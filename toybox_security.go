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
	"time"
)

type webAccessConfig struct {
	Owner      string `json:"owner"`
	Revision   string `json:"revision"`
	Salt       string `json:"salt"`
	Hash       string `json:"hash"`
	SessionKey string `json:"session_key"`
	Iterations int    `json:"iterations"`
}
type WebAccessStatus struct {
	Protected bool   `json:"protected"`
	Runtime   bool   `json:"runtime"`
	Email     string `json:"email"`
	Message   string `json:"message"`
}

func webPassword(id string) (string, error) {
	if id == "" {
		return "", errors.New("服务身份尚未创建")
	}
	key := "toybox-web-" + id
	b, e := store.Get(key)
	if e == nil {
		return string(b), nil
	}
	if !errors.Is(e, mygo.ErrSecretNotFound) {
		return "", errors.New("无法读取加密保存的网页密码")
	}
	value := random()
	if e = store.Set(key, []byte(value)); e != nil {
		return "", e
	}
	return value, nil
}
func makeWebAccessConfig(id, password string) (webAccessConfig, error) {
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
	return webAccessConfig{Owner: id, Revision: random(), Salt: hex.EncodeToString(salt), Hash: hex.EncodeToString(hash), SessionKey: hex.EncodeToString(session), Iterations: 210000}, nil
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
func (s *Studio) ToyboxPassword() (string, error) {
	m := s.services()
	if m.err != nil {
		return "", m.err
	}
	if !m.op.TryLock() {
		return "", errors.New("服务操作正在进行，请稍后查看访问密码")
	}
	defer m.op.Unlock()
	r, e := m.ensureWebIdentity()
	if e != nil {
		return "", e
	}
	return webPassword(r.ID)
}
func (s *Studio) CopyToyboxPassword() error {
	p, e := s.ToyboxPassword()
	if e != nil {
		return e
	}
	mygo.Clipboard.WriteText(p)
	return nil
}
func webAccessStatus(p runtimeProxy, r ServiceRecord) (WebAccessStatus, error) {
	status := WebAccessStatus{Runtime: r.Runtime != "", Email: r.Account}
	var cfg webAccessConfig
	if e := remoteJSON(p, r.RemoteRoot, "web-auth.json", &cfg); e != nil {
		status.Message = "此实例尚未确认网页密码保护，请点击启用网页鉴权"
		return status, nil
	}
	var marker struct {
		Owner     string  `json:"owner"`
		Revision  string  `json:"revision"`
		Protected bool    `json:"protected"`
		Updated   float64 `json:"updated"`
	}
	if e := remoteJSON(p, r.RemoteRoot, "auth-status.json", &marker); e != nil {
		status.Message = "密码配置已写入，等待网页进程确认"
		return status, nil
	}
	age := time.Since(time.Unix(int64(marker.Updated), 0))
	status.Protected = age >= -10*time.Second && cfg.Owner == r.ID && marker.Owner == r.ID && cfg.Revision == marker.Revision && marker.Protected && time.Since(time.Unix(int64(marker.Updated), 0)) < 60*time.Second
	if status.Protected {
		status.Message = "访问密码保护已启用；链接本身不能使用生成接口"
	} else {
		status.Message = "网页鉴权尚未确认，暂不允许从应用打开网页"
	}
	return status, nil
}
func (s *Studio) ToyboxAccessStatus() (WebAccessStatus, error) {
	m := s.services()
	r := m.get()
	status := WebAccessStatus{Runtime: r.Runtime != "", Email: r.Account, Message: "新部署默认启用访问密码；密码仅在本机查看或复制"}
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
	if !confirmed {
		return errors.New("请确认在空闲时启用网页鉴权，必要时只重启 Toybox 网页进程")
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
		return errors.New("请先部署服务，新部署会自动启用网页鉴权")
	}
	if r.StopRequested {
		return errors.New("服务正在停止，暂不能更新网页鉴权")
	}
	state, e := m.state(r)
	if e != nil {
		return e
	}
	if state.Busy {
		return errors.New("仍有生成任务或队列状态不明，请完成任务后启用网页鉴权")
	}
	p, e := s.proxy(r.Runtime)
	if e != nil {
		return e
	}
	previous, _ := webAccessStatus(p, r)
	password, e := webPassword(r.ID)
	if e != nil {
		return e
	}
	cfg, e := makeWebAccessConfig(r.ID, password)
	if e != nil {
		return e
	}
	value, _ := json.Marshal(cfg)
	for target, source := range map[string]string{"toybox_zh_cn.py": "templates/toybox_zh_cn.py", "toybox-zh-cn.json": "templates/toybox-zh-cn.json", "toybox_auth.py": "templates/toybox_auth.py", "web_runner.py": "templates/toybox_web_runner.py"} {
		data, _ := templates.ReadFile(source)
		if e = putRemote(p, r.RemoteRoot, target, data); e != nil {
			return e
		}
	}
	if e = putRemote(p, r.RemoteRoot, "web-auth.json", value); e != nil {
		return e
	}
	_ = previous
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
    if status.get('revision')==json.loads((p/'web-auth.json').read_text())['revision'] and status.get('protected'):break
   except Exception:pass
  else:raise RuntimeError('web restart not confirmed')
finally:
 frozen.unlink(missing_ok=True)
`, string(path), string(owner), string(binding))
	if e = executeOwned(p, code); e != nil {
		return errors.New("网页鉴权未确认启用；任务可能变为忙碌或进程身份不明，请查看日志后重试。未重建机器或重启 ComfyUI")
	}
	return nil
}
