package main

import (
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/egoist/mygo"
)

const cloudflareTunnelTokenKey = "cloudflare-tunnel-token"

var (
	quickTunnelHostPattern  = regexp.MustCompile(`^[a-z0-9-]+\.trycloudflare\.com$`)
	customTunnelHostPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)
	tunnelTokenPattern      = regexp.MustCompile(`^[A-Za-z0-9._+/=-]+$`)
)

type remoteTunnelConfig struct {
	Mode  string `json:"mode"`
	URL   string `json:"url,omitempty"`
	Token string `json:"token,omitempty"`
}

// TunnelStatus describes the local Cloudflare Named Tunnel configuration and sync outcome.
type TunnelStatus struct {
	Configured bool   `json:"configured"`
	URL        string `json:"url"`
	HasToken   bool   `json:"hasToken"`
	ActiveMode string `json:"activeMode,omitempty"`
	Synced     bool   `json:"synced"`
	Message    string `json:"message"`
}

// normalizeTunnelURL validates and canonicalizes a user-configured Cloudflare Named Tunnel origin.
func normalizeTunnelURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("请填写 Cloudflare Tunnel 绑定的固定域名（例如 https://toybox.example.com）")
	}
	if len(s) >= 7 && strings.EqualFold(s[:7], "http://") {
		return "", errors.New("固定域名必须使用 HTTPS（https://），不支持不安全的 HTTP 地址")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, e := url.Parse(s)
	if e != nil || u.Scheme != "https" || u.User != nil {
		return "", errors.New("固定域名格式无效，请填写例如 https://toybox.example.com")
	}
	if strings.Contains(u.Host, ":") || u.Port() != "" {
		return "", errors.New("固定域名请勿包含端口号，请直接使用 Cloudflare HTTPS 域名")
	}
	if u.RawPath != "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("固定域名请填写根域名（例如 https://toybox.example.com），不要包含路径或查询参数")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" || len(host) > 253 {
		return "", errors.New("固定域名主机名无效")
	}
	if net.ParseIP(host) != nil || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return "", errors.New("固定域名请填写公网可访问的域名，不能使用 IP 或本机域名")
	}
	if host == "trycloudflare.com" || strings.HasSuffix(host, ".trycloudflare.com") {
		return "", errors.New("trycloudflare.com 为临时隧道域名；固定隧道请填写你在 Cloudflare 绑定的自有域名")
	}
	if !customTunnelHostPattern.MatchString(host) {
		return "", errors.New("固定域名主机名格式无效，请检查域名拼写")
	}
	return "https://" + host, nil
}

// normalizeTunnelToken extracts and validates a Cloudflare Tunnel Token from raw input or install command.
func normalizeTunnelToken(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("请填写 Cloudflare Tunnel Token（eyJ 开头的令牌，或直接粘贴含令牌的安装命令）")
	}
	if fields := strings.Fields(s); len(fields) > 1 {
		s = fields[len(fields)-1]
	}
	s = strings.Trim(s, `"'`)
	if len(s) > len("TUNNEL_TOKEN=") && strings.EqualFold(s[:len("TUNNEL_TOKEN=")], "TUNNEL_TOKEN=") {
		s = strings.TrimSpace(s[len("TUNNEL_TOKEN="):])
		s = strings.Trim(s, `"'`)
	}
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return "", errors.New("此处应填写 Cloudflare Tunnel Token，而不是域名或网址")
	}
	if len(s) < 32 || len(s) > 4096 || !tunnelTokenPattern.MatchString(s) {
		return "", errors.New("Tunnel Token 格式无效；请粘贴 Cloudflare 控制台提供的 eyJ 开头令牌或完整安装命令")
	}
	return s, nil
}

func storedTunnelToken() (string, bool, error) {
	b, e := store.Get(cloudflareTunnelTokenKey)
	if e != nil {
		if errors.Is(e, mygo.ErrSecretNotFound) {
			return "", false, nil
		}
		return "", false, errors.New("无法读取本机加密保存的 Tunnel Token：" + e.Error())
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return "", false, nil
	}
	tok, e := normalizeTunnelToken(string(b))
	if e != nil {
		return "", false, errors.New("本机保存的 Tunnel Token 无效，请重新保存或清除隧道配置")
	}
	return tok, true, nil
}

func loadNamedTunnel(r ServiceRecord) (customURL, token string, enabled bool, err error) {
	if strings.TrimSpace(r.TunnelURL) == "" {
		return "", "", false, nil
	}
	u, e := normalizeTunnelURL(r.TunnelURL)
	if e != nil {
		return "", "", false, e
	}
	tok, ok, e := storedTunnelToken()
	if e != nil {
		return "", "", false, e
	}
	if !ok {
		return "", "", false, nil
	}
	return u, tok, true, nil
}

func namedTunnelActive(r ServiceRecord) (configured bool, hasToken bool) {
	_, ok, e := storedTunnelToken()
	hasToken = e == nil && ok
	if !hasToken {
		return false, false
	}
	_, ue := normalizeTunnelURL(r.TunnelURL)
	return ue == nil, true
}

func tunnelRedactionSecrets() []string {
	tok, ok, e := storedTunnelToken()
	if e != nil || !ok {
		return nil
	}
	return []string{tok}
}

// urlForToyboxWithCustom accepts verified Quick Tunnel URLs or the exact user-configured Named Tunnel origin.
func urlForToyboxWithCustom(address, customURL string) (string, error) {
	u, e := url.Parse(address)
	if e != nil || u.Scheme != "https" || u.User != nil || strings.Contains(u.Host, ":") || u.Port() != "" || u.RawPath != "" || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("服务尚无已确认的安全网页地址，请刷新状态")
	}
	if quickTunnelHostPattern.MatchString(u.Hostname()) {
		return u.String(), nil
	}
	if normCustom, ce := normalizeTunnelURL(customURL); ce == nil && u.String() == normCustom {
		return u.String(), nil
	}
	return "", errors.New("服务尚无已确认的安全网页地址，请刷新状态")
}

func urlForToybox(address string) (string, error) {
	return urlForToyboxWithCustom(address, "")
}

func remoteTunnelNeedsUpgrade(r ServiceRecord, v remoteState) bool {
	named, _ := namedTunnelActive(r)
	return named && v.TunnelMode == ""
}

func (m *serviceManager) upgradeTunnelSupervisor(p runtimeProxy, r *ServiceRecord, owner remoteState, payload remoteTunnelConfig) error {
	if owner.Owner != r.ID || owner.Binding != r.Binding {
		return errors.New("远端服务身份不匹配，已暂停自动操作")
	}
	if !validRemoteVersion(owner.Version) {
		return errors.New("远端服务版本标记无效，已暂停自动操作")
	}
	age := time.Since(time.Unix(int64(owner.Heartbeat), 0))
	if age < -10*time.Second || age > 90*time.Second {
		return errors.New("远端心跳已过期，请刷新确认后再试")
	}
	if r.StopRequested || owner.Stage == "draining" || owner.Stage == "backing_up" || owner.Stage == "backup_ready" {
		return errors.New("服务正在停止或备份中，暂不能切换隧道")
	}
	if owner.Stage == "installing" {
		return errors.New("远端正在执行安装或下载模型，请等待安装完成后再切换隧道")
	}
	if owner.Stage == "failed" {
		return errors.New("远端部署处于失败状态，请先继续部署恢复后再切换隧道")
	}
	if owner.Stage != "online" && owner.Stage != "starting" {
		return errors.New("当前实例状态暂不支持切换隧道，请刷新后重试")
	}
	if e := stopRemoteSupervisorWithOptions(p, *r, true); e != nil {
		return e
	}
	for _, item := range [][2]string{
		{"supervisor.py", "templates/toybox_supervisor.py"},
		{"studio_progress.py", "templates/studio_progress.py"},
	} {
		tplBytes, _ := templates.ReadFile(item[1])
		if e := putRemote(p, r.RemoteRoot, item[0], tplBytes); e != nil {
			return errors.New("已停止旧监督进程，但上传 " + item[0] + " 失败（请点击“在原实例继续部署”）：" + e.Error())
		}
	}
	b, _ := json.Marshal(payload)
	if e := putRemote(p, r.RemoteRoot, "tunnel.json", b); e != nil {
		return errors.New("已停止旧监督进程，但上传 tunnel.json 失败（请点击“在原实例继续部署”）：" + e.Error())
	}
	quoted, _ := json.Marshal("/" + strings.TrimPrefix(r.RemoteRoot, "/"))
	launchCode := "import pathlib,subprocess,sys\np=pathlib.Path(" + string(quoted) + ")\nsubprocess.Popen([sys.executable,'-u',str(p/'supervisor.py')],cwd=p,stdout=open(p/'deployment.log','a'),stderr=subprocess.STDOUT,start_new_session=True)\n"
	if e := executeOwned(p, launchCode); e != nil {
		return errors.New("已上传新隧道配置，但启动新监督进程失败（请点击“在原实例继续部署”）：" + e.Error())
	}
	r.RemoteOutdated = false
	if payload.Mode == "named" && r.URL != payload.URL {
		r.URL = ""
	}
	_ = m.put(*r)
	return nil
}

func (m *serviceManager) deployTunnelSupervisorOnly(r *ServiceRecord) (err error) {
	defer func() {
		if err != nil {
			m.startFailure(err)
		}
	}()
	if e := m.startStep(r, "replacing", "正在热替换远端监督进程并切换至 Cloudflare Named Tunnel（保留运行中的模型与任务）"); e != nil {
		return e
	}
	customURL, tok, enabled, e := loadNamedTunnel(*r)
	if e != nil {
		return m.fail(*r, e)
	}
	if !enabled {
		return m.fail(*r, errors.New("本机未配置完整的 Cloudflare Named Tunnel，无需切换"))
	}
	p, e := m.studio.proxy(r.Runtime)
	if e != nil {
		return m.fail(*r, e)
	}
	owner, e := m.stateWithRetry(*r)
	if e != nil {
		return m.fail(*r, e)
	}
	if e := m.upgradeTunnelSupervisor(p, r, owner, remoteTunnelConfig{Mode: "named", URL: customURL, Token: tok}); e != nil {
		r.RemoteOutdated = true
		_ = m.put(*r)
		return m.fail(*r, e)
	}
	_ = ensureKeepAlive(p, r.RemoteRoot)
	r.State = "online"
	r.RemoteOutdated = false
	r.URL = ""
	r.StartupError = ""
	return m.startStep(r, "done", "远端监督进程已切换至 Cloudflare Named Tunnel（运行中的模型、任务与作品未中断），正在建立固定域名连接（约 3–5 秒）")
}

func (m *serviceManager) syncRemoteTunnel(r *ServiceRecord, payload remoteTunnelConfig) (synced bool, note string) {
	if r.Runtime == "" {
		if payload.Mode == "named" {
			return false, "固定隧道已保存；下次唤醒并启动服务时将自动启用 Cloudflare Named Tunnel"
		}
		return false, "已恢复默认临时隧道；下次启动将使用 *.trycloudflare.com 临时地址"
	}
	if r.RemoteRoot == "" {
		if payload.Mode == "named" {
			return false, "固定隧道已保存；点击“部署并启动”时将自动启用 Cloudflare Named Tunnel"
		}
		return false, "已恢复默认临时隧道；部署时将使用 *.trycloudflare.com 临时地址"
	}
	if r.StopRequested {
		return false, "本机配置已保存；当前实例正在停止，将在下次启动时生效"
	}
	if _, e := m.identity(*r); e != nil {
		return false, "本机配置已保存；当前实例未同步（" + e.Error() + "）"
	}
	p, e := m.studio.proxy(r.Runtime)
	if e != nil {
		return false, "本机配置已保存；当前实例暂不可达（" + e.Error() + "）"
	}
	var owner remoteState
	if e = remoteJSON(p, r.RemoteRoot, "state.json", &owner); e != nil {
		return false, "本机配置已保存；远端状态暂不可读，部署或重连后可应用"
	}
	if owner.Owner != r.ID || owner.Binding != r.Binding {
		return false, "本机配置已保存；远端服务身份不匹配，已跳过远端同步"
	}
	if owner.TunnelMode == "" {
		if payload.Mode == "quick" {
			r.RemoteOutdated = false
			_ = m.put(*r)
			return false, "已恢复默认临时隧道；当前实例已在运行 *.trycloudflare.com 临时隧道"
		}
		if e = m.upgradeTunnelSupervisor(p, r, owner, payload); e != nil {
			r.RemoteOutdated = true
			_ = m.put(*r)
			return false, "本机配置已保存；" + e.Error()
		}
		return true, "已自动热替换远端监督进程并切换至 Cloudflare Named Tunnel（不中断运行中的模型与任务），约 3–5 秒后固定域名即可访问"
	}
	b, _ := json.Marshal(payload)
	if e = putRemote(p, r.RemoteRoot, "tunnel.json", b); e != nil {
		return false, "本机配置已保存；上传 tunnel.json 失败：" + e.Error()
	}
	if e = m.command(*r, "reload_tunnel", random()); e != nil {
		return false, "已写入远端 tunnel.json，但通知重启隧道失败：" + e.Error()
	}
	if payload.Mode == "named" {
		return true, "固定隧道已保存并同步到运行中的实例，正在平滑切换至固定域名（不影响已加载模型与任务）"
	}
	return true, "已恢复默认临时隧道并通知运行中的实例重新生成 *.trycloudflare.com 地址（不影响已加载模型与任务）"
}

func (s *Studio) buildTunnelStatus(r ServiceRecord, synced bool, customMsg string) TunnelStatus {
	_, ok, tokErr := storedTunnelToken()
	normURL, urlErr := normalizeTunnelURL(r.TunnelURL)
	configured := ok && tokErr == nil && urlErr == nil
	st := TunnelStatus{
		Configured: configured,
		URL:        strings.TrimSpace(r.TunnelURL),
		HasToken:   ok && tokErr == nil,
		ActiveMode: "quick",
		Synced:     synced,
	}
	if configured {
		st.URL = normURL
		st.ActiveMode = "named"
	}
	if customMsg != "" {
		st.Message = customMsg
		return st
	}
	switch {
	case tokErr != nil:
		st.Message = tokErr.Error()
	case configured:
		st.Message = "已配置 Cloudflare Named Tunnel（" + normURL + "），部署与启动时自动优先使用"
	case st.URL != "" && !st.HasToken:
		st.Message = "已填写固定域名但尚未保存 Tunnel Token；当前仍走默认 *.trycloudflare.com 临时隧道"
	case st.HasToken && urlErr != nil:
		st.Message = "已保存 Tunnel Token 但固定域名尚未设置；当前仍走默认 *.trycloudflare.com 临时隧道"
	default:
		st.Message = "当前使用默认 Cloudflare 临时隧道（*.trycloudflare.com）；填写固定域名与 Token 后自动切换为 Named Tunnel"
	}
	return st
}

// TunnelConfigStatus returns the current Named Tunnel vs Quick Tunnel configuration status.
func (s *Studio) TunnelConfigStatus() (TunnelStatus, error) {
	m := s.services()
	if m.err != nil {
		return TunnelStatus{}, m.err
	}
	return s.buildTunnelStatus(m.get(), false, ""), nil
}

// SaveTunnelConfig validates and persists a Cloudflare Named Tunnel domain and encrypted token,
// and hot-reloads the tunnel process on an active modern supervisor if deployed.
func (s *Studio) SaveTunnelConfig(rawURL, rawToken string) (TunnelStatus, error) {
	m := s.services()
	if m.err != nil {
		return TunnelStatus{}, m.err
	}
	if !m.op.TryLock() {
		return TunnelStatus{}, errors.New("服务操作正在进行，请稍后保存隧道配置")
	}
	defer m.op.Unlock()

	normURL, e := normalizeTunnelURL(rawURL)
	if e != nil {
		return TunnelStatus{}, e
	}

	var token string
	if strings.TrimSpace(rawToken) == "" {
		existing, ok, te := storedTunnelToken()
		if te != nil {
			return TunnelStatus{}, te
		}
		if !ok {
			return TunnelStatus{}, errors.New("请填写 Cloudflare Tunnel Token（eyJ 开头的令牌，或直接粘贴含令牌的安装命令）")
		}
		token = existing
	} else {
		normTok, te := normalizeTunnelToken(rawToken)
		if te != nil {
			return TunnelStatus{}, te
		}
		if se := store.Set(cloudflareTunnelTokenKey, []byte(normTok)); se != nil {
			return TunnelStatus{}, se
		}
		token = normTok
	}

	r := m.get()
	r.TunnelURL = normURL
	if r.URL != "" && r.URL != normURL {
		r.URL = ""
	}
	if e = m.put(r); e != nil {
		return TunnelStatus{}, e
	}
	synced, msg := m.syncRemoteTunnel(&r, remoteTunnelConfig{Mode: "named", URL: normURL, Token: token})
	return s.buildTunnelStatus(m.get(), synced, msg), nil
}

// ClearTunnelConfig removes the saved Named Tunnel domain/token so deployments fall back to Quick Tunnel.
func (s *Studio) ClearTunnelConfig() (TunnelStatus, error) {
	m := s.services()
	if m.err != nil {
		return TunnelStatus{}, m.err
	}
	if !m.op.TryLock() {
		return TunnelStatus{}, errors.New("服务操作正在进行，请稍后清除隧道配置")
	}
	defer m.op.Unlock()

	if e := store.Delete(cloudflareTunnelTokenKey); e != nil {
		return TunnelStatus{}, e
	}
	r := m.get()
	r.TunnelURL = ""
	if r.URL != "" {
		if _, e := urlForToyboxWithCustom(r.URL, ""); e != nil {
			r.URL = ""
		}
	}
	if e := m.put(r); e != nil {
		return TunnelStatus{}, e
	}
	synced, msg := m.syncRemoteTunnel(&r, remoteTunnelConfig{Mode: "quick"})
	return s.buildTunnelStatus(m.get(), synced, msg), nil
}
