package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/egoist/mygo"
	"strings"
	"time"
)

type webAccessConfig struct {
	Owner        string `json:"owner"`
	Revision     string `json:"revision"`
	Salt         string `json:"salt"`
	Hash         string `json:"hash"`
	SessionKey   string `json:"session_key"`
	Iterations   int    `json:"iterations"`
	APITokenHash string `json:"api_token_hash,omitempty"`
}
type WebAccessStatus struct {
	Protected    bool   `json:"protected"`
	APIProtected bool   `json:"apiProtected"`
	APIBaseURL   string `json:"apiBaseURL,omitempty"`
	Runtime      bool   `json:"runtime"`
	Email        string `json:"email"`
	Message      string `json:"message"`
}

var webTemplateFiles = map[string]string{
	"toybox_zh_cn.py":  "templates/toybox_zh_cn.py",
	"toybox-zh-cn.json": "templates/toybox-zh-cn.json",
	"toybox_auth.py":   "templates/toybox_auth.py",
	"toybox_openai.py": "templates/toybox_openai.py",
	"web_runner.py":    "templates/toybox_web_runner.py",
}

func newAPIToken() string { return "sk-toybox-" + random() }

func hashAPIToken(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func storedSecret(id, prefix, readErr string, generate func() string) (string, error) {
	if id == "" {
		return "", errors.New("服务身份尚未创建")
	}
	key := prefix + id
	b, e := store.Get(key)
	if e == nil && len(b) > 0 {
		return string(b), nil
	}
	if e != nil && !errors.Is(e, mygo.ErrSecretNotFound) {
		return "", errors.New(readErr)
	}
	value := generate()
	if e = store.Set(key, []byte(value)); e != nil {
		return "", e
	}
	return value, nil
}

func webPassword(id string) (string, error) {
	return storedSecret(id, "toybox-web-", "无法读取加密保存的网页密码", random)
}

func apiToken(id string) (string, error) {
	return storedSecret(id, "toybox-api-", "无法读取加密保存的 API Token", newAPIToken)
}

func makeWebAccessConfig(id, password string, apiTokens ...string) (webAccessConfig, error) {
	salt := make([]byte, 16)
	session := make([]byte, 32)
	if _, e := rand.Read(salt); e != nil {
		return webAccessConfig{}, e
	}
	if _, e := rand.Read(session); e != nil {
		return webAccessConfig{}, e
	}
	hash, e := pbkdf2.Key(sha256.New, password, salt, 210000, 32)
	if e != nil {
		return webAccessConfig{}, e
	}
	var tokenHash string
	if len(apiTokens) > 0 && apiTokens[0] != "" {
		tokenHash = hashAPIToken(apiTokens[0])
	}
	return webAccessConfig{Owner: id, Revision: random(), Salt: hex.EncodeToString(salt), Hash: hex.EncodeToString(hash), SessionKey: hex.EncodeToString(session), Iterations: 210000, APITokenHash: tokenHash}, nil
}

func apiBaseURL(r ServiceRecord) string {
	u, e := urlForToybox(r.URL)
	if e != nil {
		return ""
	}
	return strings.TrimRight(u, "/") + "/v1"
}

func (m *serviceManager) ensureWebIdentity() (ServiceRecord, error) {
	r := m.get()
	p, e := m.identity(r)
	if e != nil {
		return r, e
	}
	if r.ID == "" {
		r.ID = random()
		r.Account = p.Email
		r.Binding = accountBinding(p.Email)
		if e = m.put(r); e != nil {
			return r, e
		}
	}
	return r, nil
}

func (s *Studio) readServiceSecret(busyMsg string, reader func(string) (string, error)) (string, error) {
	m := s.services()
	if m.err != nil {
		return "", m.err
	}
	if !m.op.TryLock() {
		return "", errors.New(busyMsg)
	}
	defer m.op.Unlock()
	r, e := m.ensureWebIdentity()
	if e != nil {
		return "", e
	}
	return reader(r.ID)
}

func (s *Studio) ToyboxPassword() (string, error) {
	return s.readServiceSecret("服务操作正在进行，请稍后查看访问密码", webPassword)
}

func (s *Studio) CopyToyboxPassword() error {
	p, e := s.ToyboxPassword()
	if e != nil {
		return e
	}
	mygo.Clipboard.WriteText(p)
	return nil
}

// ToyboxAPIToken returns the locally stored OpenAI-compatible API token, creating one if needed.
func (s *Studio) ToyboxAPIToken() (string, error) {
	return s.readServiceSecret("服务操作正在进行，请稍后查看 API Token", apiToken)
}

// CopyToyboxAPIToken copies the OpenAI-compatible API token to the system clipboard.
func (s *Studio) CopyToyboxAPIToken() error {
	t, e := s.ToyboxAPIToken()
	if e != nil {
		return e
	}
	mygo.Clipboard.WriteText(t)
	return nil
}

// CopyToyboxAPIBaseURL copies the validated OpenAI-compatible /v1 base URL to the system clipboard.
func (s *Studio) CopyToyboxAPIBaseURL() error {
	m := s.services()
	if m.err != nil {
		return m.err
	}
	r := m.get()
	base := apiBaseURL(r)
	if base == "" {
		return errors.New("服务尚无已确认的安全访问地址")
	}
	mygo.Clipboard.WriteText(base)
	return nil
}

func webAccessStatus(p runtimeProxy, r ServiceRecord) (WebAccessStatus, error) {
	status := WebAccessStatus{Runtime: r.Runtime != "", Email: r.Account, APIBaseURL: apiBaseURL(r)}
	var cfg webAccessConfig
	if e := remoteJSON(p, r.RemoteRoot, "web-auth.json", &cfg); e != nil {
		status.Message = "此实例尚未确认网页密码保护，请点击启用网页鉴权"
		return status, nil
	}
	var marker struct {
		Owner        string  `json:"owner"`
		Revision     string  `json:"revision"`
		Protected    bool    `json:"protected"`
		APIProtected bool    `json:"api_protected"`
		Updated      float64 `json:"updated"`
	}
	if e := remoteJSON(p, r.RemoteRoot, "auth-status.json", &marker); e != nil {
		status.Message = "密码配置已写入，等待网页进程确认"
		return status, nil
	}
	age := time.Since(time.Unix(int64(marker.Updated), 0))
	status.Protected = age >= -10*time.Second && cfg.Owner == r.ID && marker.Owner == r.ID && cfg.Revision == marker.Revision && marker.Protected && age < 60*time.Second
	status.APIProtected = status.Protected && cfg.APITokenHash != "" && marker.APIProtected
	if status.Protected && status.APIProtected {
		status.Message = "网页密码与 OpenAI API Token 保护均已启用，两者独立鉴权互不冲突"
	} else if status.Protected {
		status.Message = "访问密码保护已启用；点击下方“启用 / 更新鉴权配置”可同步开启 OpenAI API Token"
	} else {
		status.Message = "网页鉴权尚未确认，暂不允许从应用打开网页"
	}
	return status, nil
}
func (s *Studio) ToyboxAccessStatus() (WebAccessStatus, error) {
	m := s.services()
	r := m.get()
	status := WebAccessStatus{Runtime: r.Runtime != "", Email: r.Account, APIBaseURL: apiBaseURL(r), Message: "新部署默认启用访问密码与 OpenAI API Token；凭据仅在本机查看或复制"}
	if r.Runtime == "" || r.RemoteRoot == "" {
		return status, nil
	}
	if _, e := m.identity(r); e != nil {
		return status, e
	}
	p, e := s.proxy(r.Runtime)
	if e != nil {
		return status, e
	}
	var owner remoteState
	if e = remoteJSON(p, r.RemoteRoot, "state.json", &owner); e != nil {
		return status, e
	}
	if owner.Owner != r.ID || owner.Binding != r.Binding {
		return status, errors.New("远端身份不匹配，未读取网页鉴权状态")
	}
	return webAccessStatus(p, r)
}
func (s *Studio) ApplyToyboxAccess(confirmed bool) error {
	return s.applyToyboxWeb(confirmed, false)
}

func (s *Studio) ApplyToyboxLocalization(confirmed bool) error {
	return s.applyToyboxWeb(confirmed, true)
}

func (s *Studio) applyToyboxWeb(confirmed, localization bool) error {
	_ = localization
	if !confirmed {
		return errors.New("请确认在空闲时启用鉴权配置，必要时只重启 Toybox 网页进程")
	}
	m := s.services()
	if m.err != nil {
		return m.err
	}
	if !m.op.TryLock() {
		return errors.New("服务操作正在进行，请稍后重试")
	}
	defer m.op.Unlock()
	r, e := m.ensureWebIdentity()
	if e != nil {
		return e
	}
	if r.Runtime == "" || r.RemoteRoot == "" {
		return errors.New("请先部署服务，新部署会自动启用网页与 API 鉴权")
	}
	if r.StopRequested {
		return errors.New("服务正在停止，暂不能更新鉴权配置")
	}
	state, e := m.state(r)
	if e != nil {
		return e
	}
	if state.Busy {
		return errors.New("仍有生成任务或队列状态不明，请完成任务后更新鉴权配置")
	}
	p, e := s.proxy(r.Runtime)
	if e != nil {
		return e
	}
	password, e := webPassword(r.ID)
	if e != nil {
		return e
	}
	token, e := apiToken(r.ID)
	if e != nil {
		return e
	}
	return syncRemoteWebAccess(p, r, password, token)
}

// RotateToyboxAPIToken generates a new OpenAI API token locally and syncs its digest when deployed.
func (s *Studio) RotateToyboxAPIToken(confirmed bool) error {
	if !confirmed {
		return errors.New("请确认重新生成 API Token；旧 Token 将立即失效")
	}
	m := s.services()
	if m.err != nil {
		return m.err
	}
	if !m.op.TryLock() {
		return errors.New("服务操作正在进行，请稍后重试")
	}
	defer m.op.Unlock()
	r, e := m.ensureWebIdentity()
	if e != nil {
		return e
	}
	nextToken := newAPIToken()
	if r.Runtime == "" || r.RemoteRoot == "" {
		return store.Set("toybox-api-"+r.ID, []byte(nextToken))
	}
	if r.StopRequested {
		return errors.New("服务正在停止，暂不能重置 API Token")
	}
	state, e := m.state(r)
	if e != nil {
		return e
	}
	if state.Busy {
		return errors.New("仍有生成任务或队列状态不明，请完成任务后重置 API Token")
	}
	p, e := s.proxy(r.Runtime)
	if e != nil {
		return e
	}
	password, e := webPassword(r.ID)
	if e != nil {
		return e
	}
	if e = store.Set("toybox-api-"+r.ID, []byte(nextToken)); e != nil {
		return e
	}
	return syncRemoteWebAccess(p, r, password, nextToken)
}

func syncRemoteWebAccess(p runtimeProxy, r ServiceRecord, password, token string) error {
	cfg, e := makeWebAccessConfig(r.ID, password, token)
	if e != nil {
		return e
	}
	value, _ := json.Marshal(cfg)
	for target, source := range webTemplateFiles {
		data, _ := templates.ReadFile(source)
		if e = putRemote(p, r.RemoteRoot, target, data); e != nil {
			return e
		}
	}
	if e = putRemote(p, r.RemoteRoot, "web-auth.json", value); e != nil {
		return e
	}
	path, _ := json.Marshal("/content/.colabstudio/" + r.ID)
	owner, _ := json.Marshal(r.ID)
	binding, _ := json.Marshal(r.Binding)
	code := fmt.Sprintf(`import pathlib,json,urllib.request,fcntl,os,signal,time
p=pathlib.Path(%s)
c=json.loads((p/'deployment.json').read_text())
assert c['id']==%s and c['binding']==%s,'owner mismatch'
assert not (p/'draining').exists(),'shutdown in progress'
frozen=p/'frozen'
with frozen.open('x'):pass
try:
 with open(p/'admission.lock','a') as admission:
  fcntl.flock(admission,fcntl.LOCK_EX)
  def get(port,path):
   with urllib.request.urlopen('http://127.0.0.1:'+str(port)+path,timeout=8) as resp:return json.load(resp)
  q=get(8188,'/queue');jobs=get(8190,'/api/jobs');agents=get(8190,'/api/agent/runs')
  assert isinstance(q.get('queue_running'),list) and isinstance(q.get('queue_pending'),list)
  assert not q['queue_running'] and not q['queue_pending'],'queue busy'
  assert all(j.get('status') in ('done','error','cancelled','stopped') for j in jobs),'jobs busy'
  assert all(j.get('status') in ('done','error','cancelled','stopped') for j in agents),'agent busy'
  entry=json.loads((p/'processes.json').read_text())['toybox'];pid=entry['pid']
  stat=pathlib.Path('/proc/'+str(pid)+'/stat').read_text().rsplit(')',1)[1].split()
  cmd=pathlib.Path('/proc/'+str(pid)+'/cmdline').read_bytes().hex()
  assert stat[19]==entry['signature']['start'] and cmd==entry['signature']['command'],'process changed'
  os.chmod(p/'web-auth.json',0o600)
  os.kill(pid,signal.SIGTERM)
  for i in range(25):
   time.sleep(1)
   try:
    get(8190,'/api/config')
    status=json.loads((p/'auth-status.json').read_text())
    if status.get('revision')==json.loads((p/'web-auth.json').read_text())['revision'] and status.get('protected') and status.get('api_protected'):break
   except Exception:pass
  else:raise RuntimeError('web restart not confirmed')
finally:
 frozen.unlink(missing_ok=True)
`, string(path), string(owner), string(binding))
	if e = executeOwned(p, code); e != nil {
		return errors.New("鉴权配置未确认启用；任务可能变为忙碌或进程身份不明，请查看日志后重试。未重建机器或重启 ComfyUI")
	}
	return nil
}
