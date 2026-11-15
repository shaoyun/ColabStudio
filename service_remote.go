package main

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/egoist/mygo"
	"golang.org/x/net/websocket"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

//go:embed templates/*
var templates embed.FS

// toyboxVersion 标识部署来源：fork shaoyun/minimax-h3-colab（基线 44708d4 + 中文化 + UI 调整）。
const toyboxVersion = "efa7310b54faaf06c4b22a6fd7b18e1c9bfefd69"

type remoteState struct {
	Owner     string        `json:"owner"`
	Binding   string        `json:"binding"`
	Version   string        `json:"version"`
	Stage     string        `json:"stage"`
	Message   string        `json:"message"`
	Busy      bool          `json:"busy"`
	Health    bool          `json:"health"`
	URL       string        `json:"url"`
	Heartbeat float64       `json:"heartbeat"`
	Backup    *remoteBackup `json:"backup"`
}
type remoteBackup struct {
	File  string `json:"file"`
	Size  int64  `json:"size"`
	MD5   string `json:"md5"`
	Nonce string `json:"nonce"`
}

func executeOwned(p runtimeProxy, code string) error {
	var k diagnosticKernel
	// A fresh VM can take well over 30s to start its first kernel.
	slow := *httpClient
	slow.Timeout = 3 * time.Minute
	if e := proxyRequest(&slow, p, "POST", "api/kernels", map[string]string{"name": "python3"}, &k); e != nil {
		return e
	}
	if !kernelID.MatchString(k.ID) {
		return errors.New("部署 kernel 无效")
	}
	defer proxyRequest(httpClient, p, "DELETE", "api/kernels/"+k.ID, nil, nil)
	return runCode(p, k.ID, code, false)
}

// runCode sends code to a kernel. With untilBusy it returns once execution has
// started and leaves it running; otherwise it waits for completion.
func runCode(p runtimeProxy, kernel, code string, untilBusy bool) error {
	session := random()
	config, e := diagnosticSocketConfig(p, kernel, session)
	if e != nil {
		return e
	}
	ws, e := websocket.DialConfig(config)
	if e != nil {
		return errors.New("部署通道无法连接，可在同一实例继续部署")
	}
	defer ws.Close()
	ws.MaxPayloadBytes = 1 << 20
	ws.SetDeadline(time.Now().Add(90 * time.Second))
	id := random()
	quoted, _ := json.Marshal(code)
	msg := map[string]any{"header": map[string]any{"msg_id": id, "username": "colabstudio", "session": session, "date": time.Now().UTC().Format(time.RFC3339Nano), "msg_type": "execute_request", "version": "5.3"}, "parent_header": map[string]any{}, "metadata": map[string]any{}, "channel": "shell", "content": map[string]any{"code": "exec(" + string(quoted) + ", {})", "silent": true, "store_history": false, "user_expressions": map[string]any{}, "allow_stdin": false, "stop_on_error": true}, "buffers": []any{}}
	if e = websocket.JSON.Send(ws, msg); e != nil {
		return errors.New("部署请求发送失败")
	}
	failed := false
	for i := 0; i < 1000; i++ {
		var reply struct {
			Parent struct {
				ID string `json:"msg_id"`
			} `json:"parent_header"`
			Header struct {
				Type string `json:"msg_type"`
			} `json:"header"`
			Content struct {
				State  string `json:"execution_state"`
				Status string `json:"status"`
			} `json:"content"`
		}
		if e = websocket.JSON.Receive(ws, &reply); e != nil {
			return errors.New("部署准备结果未确认；请继续核对同一实例，勿重复创建")
		}
		if reply.Parent.ID != id {
			continue
		}
		if reply.Header.Type == "error" || reply.Content.Status == "error" {
			failed = true
		}
		if untilBusy && reply.Header.Type == "status" && reply.Content.State == "busy" {
			return nil
		}
		if reply.Header.Type == "status" && reply.Content.State == "idle" {
			if failed || untilBusy {
				return errors.New("远端部署准备失败，请查看部署日志")
			}
			return nil
		}
	}
	return errors.New("部署准备未完成")
}

// Colab only applies its idle timeout once code stops executing (Pro+ background
// execution), so a dedicated kernel runs a sleep loop for as long as the VM lives.
// Release is unaffected: unassign removes the VM regardless of running cells.
const keepAliveCode = "import time\nwhile True:\n    time.sleep(30)\n"

var ensureKeepAlive = func(p runtimeProxy, root string) error {
	var kernels []diagnosticKernel
	if e := proxyRequest(httpClient, p, "GET", "api/kernels", nil, &kernels); e != nil {
		return e
	}
	var mark struct {
		ID string `json:"id"`
	}
	if remoteJSON(p, root, "keepalive.json", &mark) == nil && kernelID.MatchString(mark.ID) {
		for _, k := range kernels {
			if k.ID != mark.ID {
				continue
			}
			if k.State == "busy" {
				return nil
			}
			// Only our own marked kernel is ever removed.
			_ = proxyRequest(httpClient, p, "DELETE", "api/kernels/"+k.ID, nil, nil)
		}
	}
	var k diagnosticKernel
	slow := *httpClient
	slow.Timeout = 3 * time.Minute
	if e := proxyRequest(&slow, p, "POST", "api/kernels", map[string]string{"name": "python3"}, &k); e != nil {
		return e
	}
	if !kernelID.MatchString(k.ID) {
		return errors.New("保活 kernel 无效")
	}
	b, _ := json.Marshal(map[string]string{"id": k.ID})
	if e := putRemote(p, root, "keepalive.json", b); e != nil {
		return e
	}
	return runCode(p, k.ID, keepAliveCode, true)
}

func putRemote(p runtimeProxy, root, name string, data []byte) error {
	var v any
	return proxyRequest(httpClient, p, "PUT", "api/contents/"+root+"/"+name, map[string]any{"type": "file", "format": "base64", "content": base64.StdEncoding.EncodeToString(data)}, &v)
}
func remoteJSON(p runtimeProxy, root, name string, out any) error {
	var result struct {
		Content string `json:"content"`
		Format  string `json:"format"`
	}
	if e := proxyRequest(httpClient, p, "GET", "api/contents/"+root+"/"+name, nil, &result); e != nil {
		return e
	}
	b := []byte(result.Content)
	if result.Format == "base64" {
		var e error
		b, e = base64.StdEncoding.DecodeString(result.Content)
		if e != nil {
			return errors.New("服务状态编码无效")
		}
	}
	if json.Unmarshal(b, out) != nil {
		return errors.New("服务状态格式无效")
	}
	return nil
}
func (m *serviceManager) deploy(r *ServiceRecord) (err error) {
	defer func() {
		if err != nil {
			m.startFailure(err)
		}
	}()
	if e := m.startStep(r, "preparing", "连接实例代理并准备部署目录"); e != nil {
		return e
	}
	p, e := m.studio.proxy(r.Runtime)
	if e != nil {
		return m.fail(*r, e)
	}
	directory := "/content/.colabstudio/" + r.ID
	quoted, _ := json.Marshal(directory)
	code := "import pathlib\np=pathlib.Path(" + string(quoted) + ")\np.mkdir(parents=True,exist_ok=True)\n"
	if e = executeOwned(p, code); e != nil {
		return m.fail(*r, e)
	}
	root := "content/.colabstudio/" + r.ID
	var v any
	if e = proxyRequest(httpClient, p, "GET", "api/contents/"+root, nil, &v); e != nil {
		root = ".colabstudio/" + r.ID
		if e = proxyRequest(httpClient, p, "GET", "api/contents/"+root, nil, &v); e != nil {
			return m.fail(*r, e)
		}
	}
	r.RemoteRoot = root
	if e = m.put(*r); e != nil {
		return e
	}
	if e = m.startStep(r, "uploading", "上传固定版本的部署脚本与配置"); e != nil {
		return e
	}
	// A persistent marker is checked before touching files or restarting anything.
	var prior struct {
		ID      string `json:"id"`
		Binding string `json:"binding"`
	}
	if e = remoteJSON(p, root, "deployment.json", &prior); e == nil && (prior.ID != r.ID || prior.Binding != r.Binding) {
		return m.fail(*r, errors.New("远端服务标记不匹配，未覆盖部署"))
	}
	cfg, _ := json.Marshal(map[string]string{"id": r.ID, "binding": r.Binding, "email": r.Account, "managed_by": "ColabStudio"})
	files := map[string]string{
		"supervisor.py":      "templates/toybox_supervisor.py",
		"lock.json":          "templates/toybox-lock.json",
		"download.py":        "templates/toybox_download.py",
		"studio_progress.py": "templates/studio_progress.py",
	}
	for name, source := range webTemplateFiles {
		files[name] = source
	}
	for name, source := range files {
		b, _ := templates.ReadFile(source)
		if e = putRemote(p, root, name, b); e != nil {
			return m.fail(*r, e)
		}
	}
	if e = putRemote(p, root, "deployment.json", cfg); e != nil {
		return m.fail(*r, e)
	}
	password, e := webPassword(r.ID)
	if e != nil {
		return m.fail(*r, e)
	}
	token, e := apiToken(r.ID)
	if e != nil {
		return m.fail(*r, e)
	}
	security, e := makeWebAccessConfig(r.ID, password, token)
	if e != nil {
		return m.fail(*r, e)
	}
	authBytes, _ := json.Marshal(security)
	if e = putRemote(p, root, "web-auth.json", authBytes); e != nil {
		return m.fail(*r, e)
	}

	var existingData any
	hasData := proxyRequest(httpClient, p, "GET", "api/contents/"+root+"/data", nil, &existingData) == nil
	if r.Archive != "" && !hasData {
		if e = m.startStep(r, "restoring", "正在从 Drive 恢复上次作品"); e != nil {
			return e
		}
		if e = restoreDrive(p, *r); e != nil {
			return m.fail(*r, e)
		}
	}
	if e = m.startStep(r, "launching", "部署文件已上传，正在启动远端监督进程"); e != nil {
		return e
	}
	code = "import pathlib,subprocess,sys\np=pathlib.Path(" + string(quoted) + ")\nsubprocess.Popen([sys.executable,'-u',str(p/'supervisor.py')],cwd=p,stdout=open(p/'deployment.log','a'),stderr=subprocess.STDOUT,start_new_session=True)\n"
	if e = executeOwned(p, code); e != nil {
		return m.fail(*r, e)
	}
	r.State = "installing"
	if e = ensureKeepAlive(p, root); e != nil {
		r.StartupLog = boundedStartupLog(r.StartupLog, "云端保活暂未启动，App 在线时会自动重试")
	}
	return m.startStep(r, "done", "远端安装进程已提交；等待环境检查、下载与实时日志，尚未确认网页就绪")
}
func (m *serviceManager) state(r ServiceRecord) (remoteState, error) {
	var v remoteState
	if r.RemoteRoot == "" {
		return v, errors.New("远端部署尚未完成，可点击继续部署，不会再申请实例")
	}
	p, e := m.studio.proxy(r.Runtime)
	if e != nil {
		return v, e
	}
	if e = remoteJSON(p, r.RemoteRoot, "state.json", &v); e != nil {
		return v, errors.New("服务状态暂不可读；请核对实例或继续部署，不会重复创建")
	}
	if v.Owner != r.ID || v.Binding != r.Binding || v.Version != toyboxVersion {
		return v, errors.New("远端服务身份或版本不匹配，已暂停自动操作")
	}
	if time.Since(time.Unix(int64(v.Heartbeat), 0)) > 90*time.Second {
		return v, errors.New("云端服务心跳超过 90 秒未更新，未确认在线")
	}
	return v, nil
}
func (m *serviceManager) inspect(r *ServiceRecord) error {
	if r.RemoteRoot == "" && r.Runtime != "" {
		if _, e := m.studio.proxy(r.Runtime); e != nil {
			return e
		}
		r.State = "allocated"
		r.Heartbeat = time.Now()
		r.Message = "机器已唤醒，尚未部署服务；机器正在计费，可部署并启动或释放机器"
		return nil
	}
	v, e := m.state(*r)
	if e != nil {
		return e
	}
	r.State = v.Stage
	r.Heartbeat = time.Unix(int64(v.Heartbeat), 0)
	r.Message = map[string]string{"installing": "正在安装并校验模型，请查看部署日志", "starting": "正在启动服务", "online": "服务在线；网页访问需验证当前账号邮箱", "failed": "部署失败；实例可能仍在计费，请查看日志", "draining": "正在等待任务完成", "backing_up": "正在打包作品", "backup_ready": "正在等待 Drive 保存校验"}[v.Stage]
	if !v.Health && v.Stage == "online" {
		r.Message = "服务健康检查异常，暂不释放实例"
	}
	if v.URL != "" {
		if _, e = urlForToybox(v.URL); e == nil {
			r.URL = v.URL
		}
	}
	if !r.StopRequested {
		r.BackupNonce = ""
	}
	return nil
}
func (m *serviceManager) command(r ServiceRecord, action, nonce string) error {
	p, e := m.studio.proxy(r.Runtime)
	if e != nil {
		return e
	}
	b, _ := json.Marshal(map[string]string{"action": action, "nonce": nonce})
	return putRemote(p, r.RemoteRoot, "command.json", b)
}
func (m *serviceManager) finishStop(r *ServiceRecord) error {
	if r.Runtime != "" && r.RemoteRoot == "" {
		return m.releaseUninstalled(r)
	}
	v, e := m.state(*r)
	if e != nil {
		return m.fail(*r, e)
	}
	if r.BackupNonce == "" {
		if e = m.command(*r, "drain", random()); e != nil {
			return m.fail(*r, e)
		}
		r.State = "draining"
		r.Message = "停止接收新任务，等待任务完成"
		if v.Busy {
			return m.put(*r)
		}
		r.BackupNonce = random()
		if e = m.put(*r); e != nil {
			return e
		}
		if e = m.command(*r, "backup", r.BackupNonce); e != nil {
			return m.fail(*r, e)
		}
		return nil
	}
	if v.Backup == nil || v.Backup.Nonce != r.BackupNonce {
		r.Message = "等待云端生成一致的作品备份"
		return m.put(*r)
	}
	r.State = "backing_up"
	r.Message = "正在上传作品到 Google Drive；校验成功后释放"
	if e = m.put(*r); e != nil {
		return e
	}
	id, folder, e := backupDrive(m.studio, *r, *v.Backup)
	if e != nil {
		return m.fail(*r, e)
	}
	r.Archive = id
	r.Folder = folder
	r.BackupTime = time.Now()
	r.State = "releasing"
	r.Message = "Drive 备份已校验，正在释放实例"
	if e = m.put(*r); e != nil {
		return e
	}
	if _, e = m.identity(*r); e != nil {
		return m.fail(*r, e)
	}
	if e = m.studio.compatibleRelease(r.Runtime); e != nil {
		return m.fail(*r, e)
	}
	r.Runtime = ""
	r.RunningWindow = ""
	r.Operation = ""
	r.PostSent = false
	r.StopRequested = false
	r.BackupNonce = ""
	r.RemoteRoot = ""
	r.URL = ""
	r.State = "stopped"
	r.Message = "作品已保存到 Drive，实例已释放"
	return m.put(*r)
}
func (s *Studio) RetryToybox(confirmed bool) error {
	if !confirmed {
		return errors.New("请确认仅在原实例继续部署")
	}
	m := s.services()
	if m.err != nil {
		return m.err
	}
	if !m.op.TryLock() {
		return errors.New("已有服务操作正在进行")
	}
	go func() { defer m.op.Unlock(); _ = m.retryDeployment() }()
	return nil
}
func (m *serviceManager) retryDeployment() (err error) {
	defer func() {
		if err != nil {
			m.startFailure(err)
		}
	}()
	r := m.get()
	if e := m.startStep(&r, "checking_account", "继续部署：检查账号与原实例"); e != nil {
		return e
	}
	if _, e := m.identity(r); e != nil {
		return e
	}
	if r.Runtime == "" {
		return errors.New("没有已确认的原实例，不能继续部署")
	}
	if e := m.startStep(&r, "checking_drive", "继续部署：检查 Drive 授权与 API"); e != nil {
		return e
	}
	dt, e := driveAccess(r.Account)
	if e != nil {
		return e
	}
	var check any
	if e = driveJSON("GET", "https://www.googleapis.com/drive/v3/files?pageSize=1&fields=files(id)", dt, nil, &check); e != nil {
		return e
	}
	if e = m.startStep(&r, "checking_runtime", "核对远端监督进程，避免重复安装"); e != nil {
		return e
	}
	if _, e = m.state(r); e == nil {
		return errors.New("监督进程仍在线，请查看日志；不重复执行安装")
	}
	return m.deploy(&r)
}

func urlForToybox(address string) (string, error) {
	u, e := url.Parse(address)
	if e != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || !regexp.MustCompile(`^[a-z0-9-]+\.trycloudflare\.com$`).MatchString(u.Hostname()) || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("服务尚无已确认的安全网页地址，请刷新状态")
	}
	return u.String(), nil
}
func openToyboxBrowser(u string) error { return mygo.Shell.OpenExternal(u) }
func (s *Studio) ToyboxLogs(kind string) (string, error) {
	m := s.services()
	r := m.get()
	if kind == "deployment" && r.RemoteRoot == "" && r.StartupLog != "" {
		return r.StartupLog, nil
	}
	if _, e := m.identity(r); e != nil {
		return "", e
	}
	if r.RemoteRoot == "" {
		return "", errors.New("部署尚未创建日志目录")
	}
	if kind != "deployment" && kind != "toybox" && kind != "comfyui" && kind != "tunnel" {
		return "", errors.New("无效日志类型")
	}
	p, e := s.proxy(r.Runtime)
	if e != nil {
		return "", e
	}
	resp, e := logRequest(httpClient, p, "files/"+r.RemoteRoot+"/"+kind+".log", nil, "bytes=-65536")
	if e != nil {
		if kind == "deployment" && r.StartupLog != "" {
			return r.StartupLog + "\n远端日志尚不可读：" + e.Error(), nil
		}
		return "", e
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return "", errors.New("日志尚未生成或实例暂不可读")
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if e != nil || len(b) > 65536 {
		return "", errors.New("日志过大且不支持范围读取，已停止下载")
	}
	if kind == "deployment" && r.StartupLog != "" {
		return r.StartupLog + "\n──── 远端部署日志 ────\n" + redactLog(string(b), p.Token), nil
	}
	return redactLog(string(b), p.Token), nil
}

// Keep archive transfers bounded on disk, never place proxy tokens in browser URLs.
func downloadRuntime(p runtimeProxy, path string, out *os.File) error {
	address, e := proxyURL(p, "files/"+path)
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, "GET", address, nil)
	if e != nil {
		return e
	}
	req.Header.Set("X-Colab-Runtime-Proxy-Token", p.Token)
	client := *httpClient
	client.Timeout = 30 * time.Minute
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, e := client.Do(req)
	if e != nil {
		return errors.New("云端备份下载失败，未释放实例")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return errors.New("无法下载云端备份")
	}
	_, e = io.Copy(out, io.LimitReader(resp.Body, (50<<30)+1))
	return e
}

// Inspect only ColabStudio markers, never execute code or alter unknown instances.
func (m *serviceManager) guardDuplicate(token string, r ServiceRecord) error {
	var rows struct {
		Assignments []assignment `json:"assignments"`
	}
	if e := tunnelRequest(httpClient, tunnelBase, token, "GET", "assignments?authuser=0", "", &rows); e != nil {
		return e
	}
	for _, a := range rows.Assignments {
		if !endpointPattern.MatchString(a.Endpoint) {
			return errors.New("实例列表含无效 endpoint，未申请新实例")
		}
		p := runtimeProxy{a.Proxy.URL, a.Proxy.Token}
		foundRoot := false
		for _, root := range []string{"content/.colabstudio", ".colabstudio"} {
			var tree struct {
				Type    string `json:"type"`
				Content []struct {
					Name string `json:"name"`
					Type string `json:"type"`
				} `json:"content"`
			}
			e := proxyRequest(httpClient, p, "GET", "api/contents/"+root, nil, &tree)
			if e != nil {
				if strings.Contains(e.Error(), "HTTP 404") {
					continue
				}
				return errors.New("已有实例暂不可核对服务标记，为避免重复占用，未申请新实例")
			}
			foundRoot = true
			if tree.Type != "directory" || len(tree.Content) > 100 {
				return errors.New("已有服务标记目录异常，未申请新实例")
			}
			for _, entry := range tree.Content {
				if entry.Type != "directory" || !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(entry.Name) {
					continue
				}
				var cfg struct {
					ID      string `json:"id"`
					Binding string `json:"binding"`
					Managed string `json:"managed_by"`
				}
				if e = remoteJSON(p, root+"/"+entry.Name, "deployment.json", &cfg); e != nil {
					return errors.New("已有服务标记尚未确认，未申请新实例")
				}
				if cfg.Managed == "ColabStudio" && cfg.Binding == r.Binding {
					return errors.New("账号已有 ColabStudio Toybox 服务标记，已阻止重复创建；请恢复原服务记录")
				}
			}
			break
		}
		_ = foundRoot
	}
	return nil
}

func (m *serviceManager) keepAlive(r ServiceRecord) error {
	endpoint, e := decodeEndpoint(r.Runtime)
	if e != nil {
		return e
	}
	token, e := access()
	if e != nil {
		return e
	}
	req, e := http.NewRequest("GET", tunnelBase+url.PathEscape(endpoint)+"/keep-alive/?authuser=0", nil)
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Colab-Tunnel", "Google")
	client := *httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, e := client.Do(req)
	if e != nil {
		return errors.New("Colab 连接心跳失败，正在等待下次重连")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 204 {
		return errors.New("Colab 连接心跳未确认，服务可能仍在运行")
	}
	return nil
}

func (m *serviceManager) reconcileGone(r *ServiceRecord) (bool, error) {
	endpoint, e := decodeEndpoint(r.Runtime)
	if e != nil {
		return false, e
	}
	token, e := access()
	if e != nil {
		return false, e
	}
	var v struct {
		Assignments *[]assignment `json:"assignments"`
	}
	if e = tunnelRequest(httpClient, tunnelBase, token, "GET", "assignments?authuser=0", "", &v); e != nil {
		return false, e
	}
	if v.Assignments == nil {
		return false, errors.New("实例列表未返回明确结果，未推断实例已释放")
	}
	for _, a := range *v.Assignments {
		if a.Endpoint == endpoint {
			return false, nil
		}
	}
	released := r.State == "releasing" && r.Archive != ""
	r.Runtime = ""
	r.Operation = ""
	r.PostSent = false
	r.StopRequested = false
	r.BackupNonce = ""
	r.RemoteRoot = ""
	r.URL = ""
	r.RunningWindow = ""
	if key, active, _ := ruleWindow(r.Rule, time.Now()); active {
		r.LastWindow = key
	}
	if released {
		r.State = "stopped"
		r.Message = "Drive 已保存，云端列表已确认实例结束"
	} else {
		r.State = "lost"
		r.Message = "云端列表确认实例已结束；上次备份后的临时作品可能丢失，本时段不会重新申请"
	}
	return true, nil
}

// No deployment path has been persisted, so ColabStudio has never started a
// service or restored work on this VM. Only this explicitly owned VM is released.
func (m *serviceManager) releaseUninstalled(r *ServiceRecord) error {
	if r.Runtime == "" || r.RemoteRoot != "" {
		return errors.New("只允许释放尚未部署的已唤醒机器")
	}
	if _, e := m.identity(*r); e != nil {
		return m.fail(*r, e)
	}
	if _, e := m.studio.proxy(r.Runtime); e != nil {
		return m.fail(*r, e)
	}
	r.State = "releasing"
	r.Message = "正在释放尚未部署的机器"
	if e := m.put(*r); e != nil {
		return e
	}
	if e := m.studio.compatibleRelease(r.Runtime); e != nil {
		return m.fail(*r, e)
	}
	r.Runtime = ""
	r.Operation = ""
	r.PostSent = false
	r.StopRequested = false
	r.RunningWindow = ""
	r.State = "stopped"
	r.Message = "机器已释放，未部署服务"
	return m.put(*r)
}
