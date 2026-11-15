package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/egoist/mygo"
)

type ServiceRule struct {
	Enabled bool   `json:"enabled"`
	Days    []int  `json:"days"`
	Start   string `json:"start"`
	End     string `json:"end"`
	Zone    string `json:"zone"`
}
type ServiceRecord struct {
	StartupPhase  string      `json:"startupPhase,omitempty"`
	StartupError  string      `json:"startupError,omitempty"`
	StartupLog    string      `json:"startupLog,omitempty"`
	Accelerator   string      `json:"accelerator,omitempty"`
	ID            string      `json:"id"`
	Account       string      `json:"account"`
	Binding       string      `json:"binding"`
	Runtime       string      `json:"runtime"`
	Operation     string      `json:"operation"`
	PostSent      bool        `json:"postSent"`
	State         string      `json:"state"`
	Message       string      `json:"message"`
	URL           string      `json:"url"`
	Updated       time.Time   `json:"updated"`
	Heartbeat     time.Time   `json:"heartbeat"`
	Rule          ServiceRule `json:"rule"`
	RunningWindow string      `json:"runningWindow"`
	LastWindow    string      `json:"lastWindow"`
	Suppressed    string      `json:"suppressed"`
	StopRequested bool        `json:"stopRequested"`
	Folder        string      `json:"folder"`
	Archive       string      `json:"archive"`
	BackupTime    time.Time   `json:"backupTime"`
	RemoteRoot     string      `json:"remoteRoot"`
	RemoteVersion  string      `json:"remoteVersion,omitempty"`
	RemoteOutdated bool        `json:"remoteOutdated,omitempty"`
	BackupNonce    string      `json:"backupNonce"`
	TunnelURL      string      `json:"tunnelURL,omitempty"`
}
type ServiceStatus struct {
	Operating bool `json:"operating"`
	ServiceRecord
	Manager        bool   `json:"manager"`
	Drive          bool   `json:"drive"`
	DriveAccount   string `json:"driveAccount"`
	Next           string `json:"next"`
	Version        string `json:"version"`
	NamedTunnel    bool   `json:"namedTunnel"`
	TunnelHasToken bool   `json:"tunnelHasToken,omitempty"`
}
type serviceManager struct {
	metricsAttempt time.Time
	metricsRuntime string
	studio         *Studio
	ka             *keepAliveManager
	mu             sync.Mutex
	op             sync.Mutex
	record         ServiceRecord
	file           *os.File
	path           string
	err            error
}

func servicePath() string {
	if os.Getenv("COLABSTUDIO_SMOKE_VAULT") == "1" {
		return filepath.Join(filepath.Dir(store.path), "services.json")
	}
	return filepath.Join(filepath.Dir(configPath()), "services.json")
}
func (s *Studio) services() *serviceManager {
	s.svcOnce.Do(func() {
		m := &serviceManager{studio: s, path: servicePath()}
		m.record = ServiceRecord{State: "stopped", Rule: ServiceRule{Days: []int{1, 2, 3, 4, 5}, Start: "09:00", End: "18:00", Zone: "Asia/Shanghai"}}
		_ = os.MkdirAll(filepath.Dir(m.path), 0700)
		f, e := os.OpenFile(m.path+".lock", os.O_CREATE|os.O_RDWR, 0600)
		if e == nil {
			e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		}
		if e != nil {
			if f != nil {
				f.Close()
			}
			m.err = errors.New("另一份 ColabStudio 正在管理服务，请回到已打开的应用")
		} else {
			m.file = f
			b, re := os.ReadFile(m.path)
			if re == nil {
				if json.Unmarshal(b, &m.record) != nil {
					m.err = errors.New("服务记录损坏，已暂停自动操作；请保留记录后修复")
				}
			} else if !os.IsNotExist(re) {
				m.err = re
			}
		}
		m.ka = newKeepAliveManager(m)
		s.svc = m
		go m.loop()
		if m.err == nil {
			go m.ka.loop()
			m.ka.notify()
		}
	})
	return s.svc
}
func (m *serviceManager) enablePowerHooks() {
	if m == nil || m.ka == nil {
		return
	}
	m.ka.setWakeProvider(func(reason string) func() {
		return mygo.Power.KeepAwake(reason, false)
	})
	m.ka.notify()
}
func (m *serviceManager) closeKeepAlive() {
	if m == nil || m.ka == nil {
		return
	}
	m.ka.close()
}
func (m *serviceManager) get() ServiceRecord { m.mu.Lock(); defer m.mu.Unlock(); return m.record }
func (m *serviceManager) put(r ServiceRecord) error {
	r.Updated = time.Now()
	b, e := json.MarshalIndent(r, "", "  ")
	if e != nil {
		return e
	}
	tmp := m.path + ".tmp"
	f, e := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	if _, e = f.Write(b); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Rename(tmp, m.path); e != nil {
		return e
	}
	if dir, e := os.Open(filepath.Dir(m.path)); e == nil {
		_ = dir.Sync()
		dir.Close()
	}
	m.mu.Lock()
	m.record = r
	m.mu.Unlock()
	if m.ka != nil {
		m.ka.notify()
	}
	return nil
}
func (m *serviceManager) fail(r ServiceRecord, e error) error {
	r.Message = e.Error()
	_ = m.put(r)
	return e
}
func accountBinding(email string) string {
	b := sha256.Sum256([]byte(strings.ToLower(email)))
	return hex.EncodeToString(b[:])
}
func (m *serviceManager) identity(r ServiceRecord) (AccountProfile, error) {
	p, e := m.studio.Profile()
	if e != nil {
		return p, e
	}
	if r.Binding != "" && accountBinding(p.Email) != r.Binding {
		return p, errors.New("当前账号与服务绑定账号不同，自动化已暂停；请连接 " + r.Account)
	}
	return p, nil
}
func (s *Studio) ServiceStatus() ServiceStatus {
	m := s.services()
	r := m.get()
	if r.Accelerator == "" {
		r.Accelerator = "L4"
	}
	d, _ := loadDriveGrant()
	if r.Folder == "" {
		r.Folder = d.Folder
	}
	namedTunnel, hasToken := namedTunnelActive(r)
	result := ServiceStatus{
		ServiceRecord:  r,
		Manager:        m.err == nil,
		Drive:          d.Refresh != "" || d.Rclone != "",
		DriveAccount:   d.Email,
		Version:        toyboxVersion,
		NamedTunnel:    namedTunnel,
		TunnelHasToken: hasToken,
	}
	if m.err != nil {
		result.Message = m.err.Error()
	}
	if m.op.TryLock() {
		m.op.Unlock()
	} else {
		result.Operating = true
	}
	result.Next = nextWindow(r.Rule, time.Now())
	return result
}
func (s *Studio) StartToybox(confirmed bool) error {
	if !confirmed {
		return errors.New("请确认启动 L4 实例并消耗 CU")
	}
	m := s.services()
	if m.err != nil {
		return m.err
	}
	if !m.op.TryLock() {
		return errors.New("服务操作正在进行，请查看进度")
	}
	r := m.get()
	r.StartupLog = ""
	if e := m.startStep(&r, "queued", "启动请求已接收，正在检查条件；尚未确认远端启动"); e != nil {
		m.op.Unlock()
		return e
	}
	go func() { defer m.op.Unlock(); _ = m.start(false) }()
	return nil
}
func (m *serviceManager) start(scheduled bool) (err error) {
	defer func() {
		if err != nil {
			m.startFailure(err)
		}
	}()
	r := m.get()
	if e := m.startStep(&r, "checking_account", "检查 Colab 账号身份"); e != nil {
		return e
	}
	p, e := m.identity(r)
	if e != nil {
		return m.fail(r, e)
	}
	if e = m.startStep(&r, "checking_drive", "检查 Drive 授权与 API 可用性"); e != nil {
		return e
	}
	dt, e := driveAccess(p.Email)
	if e != nil {
		return m.fail(r, e)
	}
	var check any
	if e = driveJSON("GET", "https://www.googleapis.com/drive/v3/files?pageSize=1&fields=files(id)", dt, nil, &check); e != nil {
		if r.Runtime == "" {
			// Checked before waking: no machine exists, so nothing is billing.
			e = errors.New(strings.Replace(e.Error(), "未释放实例", "尚未申请机器，不产生费用", 1))
		}
		return m.fail(r, e)
	}
	if e = m.startStep(&r, "checking_runtime", "连接已分配机器；有现有机器时复用"); e != nil {
		return e
	}
	if e = m.wake(scheduled); e != nil {
		return e
	}
	r = m.get()
	// An allocated VM has no supervisor yet: deployment must precede health checks.
	if r.RemoteRoot == "" {
		return m.deploy(&r)
	}
	if !scheduled && !r.StopRequested {
		v, stateErr := m.stateWithRetry(r)
		if stateErr != nil {
			return m.fail(r, stateErr)
		}
		if v.Version == toyboxVersion && remoteTunnelNeedsUpgrade(r, v) && (v.Stage == "online" || v.Stage == "starting") {
			return m.deployTunnelSupervisorOnly(&r)
		}
		if v.Version != toyboxVersion || remoteTunnelNeedsUpgrade(r, v) {
			return m.deploy(&r)
		}
	}
	if e = m.inspect(&r); e != nil {
		return m.fail(r, e)
	}
	if r.StopRequested {
		if e = m.command(r, "resume", random()); e != nil {
			return m.fail(r, e)
		}
		r.StopRequested = false
		r.BackupNonce = ""
		r.State = "starting"
		r.Suppressed = ""
	}
	return m.startStep(&r, "done", "启动检查完成；远端运行状态继续自动更新")
}

func (s *Studio) WakeToybox(confirmed bool) error {
	if !confirmed {
		return errors.New("请确认唤醒所选计费机器；该操作只分配机器，不部署服务")
	}
	m := s.services()
	if m.err != nil {
		return m.err
	}
	if !m.op.TryLock() {
		return errors.New("服务操作正在进行，请查看进度")
	}
	go func() { defer m.op.Unlock(); _ = m.wake(false) }()
	return nil
}

func (m *serviceManager) wake(scheduled bool) error {
	r := m.get()
	accelerator, e := machineType(r.Accelerator)
	if e != nil {
		return m.fail(r, e)
	}
	r.Accelerator = accelerator
	p, e := m.identity(r)
	if e != nil {
		return m.fail(r, e)
	}
	if _, e = mail.ParseAddress(p.Email); e != nil {
		return m.fail(r, errors.New("需要有效的当前账号邮箱以保护网页访问"))
	}
	if r.ID == "" {
		r.ID = random()
		r.Account = p.Email
		r.Binding = accountBinding(p.Email)
	}
	if r.Runtime != "" {
		_, e := m.studio.proxy(r.Runtime)
		if e != nil {
			return m.fail(r, e)
		}
		return nil
	}

	token, e := access()
	if e != nil {
		return m.fail(r, e)
	}
	if r.Operation != "" {
		a, e := assignChallenge(httpClient, tunnelBase, token, r.Operation, accelerator)
		if e != nil {
			return m.fail(r, e)
		}
		if a.Endpoint == "" {
			r.State = "uncertain"
			return m.fail(r, errors.New("上次申请结果尚未确认，已阻止再次创建；请先核对实例列表"))
		}
		r.Runtime = mapAssignment(a.assignment).Name
		r.State = "allocated"
		if e = m.put(r); e != nil {
			return e
		}
		r.Message = "机器已唤醒，尚未部署；请连接 Drive 后部署并启动，或释放机器停止计费"
		return m.put(r)
	}
	if e = m.guardDuplicate(token, r); e != nil {
		return m.fail(r, e)
	}
	if key, active, _ := ruleWindow(r.Rule, time.Now()); active {
		r.RunningWindow = key
	}
	if scheduled {
		if _, active, _ := ruleWindow(r.Rule, time.Now()); !active {
			return m.fail(r, errors.New("启动检查期间运行时段已结束，未申请实例"))
		}
	}
	r.Operation = randomNotebookID()
	r.PostSent = false
	r.State = "allocating"
	r.Message = "正在申请 " + accelerator + "，不会自动更换 GPU 或重复申请"
	r.Suppressed = ""
	if e = m.put(r); e != nil {
		return e
	}
	a, e := assignChallenge(httpClient, tunnelBase, token, r.Operation, accelerator)
	if e != nil {
		return m.fail(r, e)
	}
	if a.Endpoint == "" {
		if a.Token == "" {
			return m.fail(r, errors.New("申请确认令牌缺失，未创建实例"))
		}
		if scheduled {
			if _, active, _ := ruleWindow(r.Rule, time.Now()); !active {
				r.Operation = ""
				r.State = "stopped"
				return m.fail(r, errors.New("运行时段已结束，未发送创建请求"))
			}
		}
		r.PostSent = true
		if e = m.put(r); e != nil {
			return e
		}
		e = tunnelRequest(httpClient, tunnelBase, token, "POST", assignPath(r.Operation, accelerator), a.Token, &a)
		if e != nil {
			r.State = "uncertain"
			return m.fail(r, errors.New("申请返回中断，实例可能已计费；请核对状态，程序不会重复创建"))
		}
	}
	if !endpointPattern.MatchString(a.Endpoint) {
		r.State = "uncertain"
		return m.fail(r, errors.New("申请未返回有效实例，已停止自动重试"))
	}
	r.Runtime = mapAssignment(a.assignment).Name
	r.State = "allocated"
	r.Message = "机器已唤醒，尚未部署；请连接 Drive 后部署并启动，或释放机器停止计费"
	if e = m.put(r); e != nil {
		return e
	}
	r.Message = "机器已唤醒，尚未部署；请连接 Drive 后部署并启动，或释放机器停止计费"
	return m.put(r)
}

type assignResponse struct {
	assignment
	Token string `json:"token"`
}

func randomNotebookID() string {
	h := sha256.Sum256([]byte(random()))
	b := append([]byte(nil), h[:16]...)
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	v := hex.EncodeToString(b)
	return v[:8] + "_" + v[8:12] + "_" + v[12:16] + "_" + v[16:20] + "_" + v[20:] + strings.Repeat(".", 8)
}
func assignPath(id string, types ...string) string {
	accelerator := "L4"
	if len(types) > 0 && types[0] != "" {
		accelerator = types[0]
	}
	return "assign?" + url.Values{"authuser": {"0"}, "nbh": {id}, "variant": {"GPU"}, "accelerator": {accelerator}}.Encode()
}
func assignChallenge(client *http.Client, base, token, id string, types ...string) (assignResponse, error) {
	var a assignResponse
	e := tunnelRequest(client, base, token, "GET", assignPath(id, types...), "", &a)
	return a, e
}

// A login change must not redirect an in-flight service operation to another account.
func (s *Studio) lockAccountChange() (func(), error) {
	if s.svc == nil {
		return func() {}, nil
	}
	if !s.svc.op.TryLock() {
		return nil, errors.New("服务正在申请、保存或释放，请等待当前操作完成后切换账号")
	}
	return s.svc.op.Unlock, nil
}

func machineType(value string) (string, error) {
	value = strings.ToUpper(strings.TrimSpace(value))
	if value == "" {
		value = "L4"
	}
	switch value {
	case "L4", "A100", "H100":
		return value, nil
	}
	return "", errors.New("请选择 L4、A100 或 H100 GPU")
}
func (m *serviceManager) setMachineType(value string, confirmed bool) error {
	value, e := machineType(value)
	if e != nil {
		return e
	}
	r := m.get()
	if r.Runtime != "" || r.Operation != "" {
		return errors.New("机器已唤醒或申请结果待确认，不能切换类型；请先核对并释放原机器")
	}
	old, _ := machineType(r.Accelerator)
	if r.Rule.Enabled && old != value && !confirmed {
		return errors.New("请确认后续定时启动将使用新的计费 GPU 类型")
	}
	r.Accelerator = value
	r.Message = "机器类型已设为 " + value + "，将在下次唤醒或启动时使用"
	return m.put(r)
}
func (s *Studio) SaveMachineType(value string, confirmed bool) error {
	m := s.services()
	if m.err != nil {
		return m.err
	}
	if !m.op.TryLock() {
		return errors.New("服务操作正在进行，暂不能切换机器类型")
	}
	defer m.op.Unlock()
	return m.setMachineType(value, confirmed)
}

// The challenge uses a string variant (GPU); the assignment uses its integer enum.
func (a *assignResponse) UnmarshalJSON(data []byte) error {
	var v struct {
		Endpoint    string          `json:"endpoint"`
		Token       string          `json:"token"`
		Accelerator string          `json:"accelerator"`
		Acc         string          `json:"acc"`
		Variant     json.RawMessage `json:"variant"`
		Proxy       runtimeProxy    `json:"runtimeProxyInfo"`
	}
	if e := json.Unmarshal(data, &v); e != nil {
		return e
	}
	variant := 0
	if len(v.Variant) > 0 {
		if e := json.Unmarshal(v.Variant, &variant); e != nil {
			var label string
			if json.Unmarshal(v.Variant, &label) != nil {
				return errors.New("invalid assignment variant")
			}
			switch label {
			case "GPU":
				variant = 1
			case "TPU":
				variant = 2
			case "DEFAULT", "CPU":
				variant = 0
			default:
				return errors.New("unknown assignment variant")
			}
		}
	}
	a.Endpoint = v.Endpoint
	a.Token = v.Token
	a.Accelerator = v.Accelerator
	if a.Accelerator == "" {
		a.Accelerator = v.Acc
	}
	a.Variant = variant
	a.Proxy.URL = v.Proxy.URL
	a.Proxy.Token = v.Proxy.Token
	return nil
}

// Startup diagnostics exist before a remote directory or supervisor exists.
func (m *serviceManager) startStep(r *ServiceRecord, phase, text string) error {
	r.StartupPhase = phase
	r.StartupError = ""
	r.Message = text
	r.StartupLog = boundedStartupLog(r.StartupLog, text)
	return m.put(*r)
}
func boundedStartupLog(prior, text string) string {
	text = redactLog(text)
	if len(text) > 1600 {
		text = text[:1600]
	}
	lines := strings.Split(strings.TrimSpace(prior), "\n")
	if prior == "" {
		lines = nil
	}
	lines = append(lines, time.Now().Format("15:04:05")+" "+text)
	if len(lines) > 100 {
		lines = lines[len(lines)-100:]
	}
	return strings.Join(lines, "\n")
}
func (m *serviceManager) startFailure(err error) {
	r := m.get()
	message := redactLog(err.Error())
	if r.StartupPhase == "failed" && r.StartupError == message {
		return
	}
	r.StartupPhase = "failed"
	r.StartupError = message
	r.StartupLog = boundedStartupLog(r.StartupLog, "启动失败："+message)
	r.Message = "启动失败：" + message
	_ = m.put(r)
}
