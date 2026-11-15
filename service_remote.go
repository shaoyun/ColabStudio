package main

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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

// toyboxVersion 标识部署来源：fork shaoyun/minimax-h3-colab（基线 44708d4 + 中文化 + UI 调整 + 浅色界面与新 Logo）。
const toyboxVersion = "b7a686836680a77fa604e3469d940d5f63c37391"

var pinnedCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func validRemoteVersion(v string) bool {
	return pinnedCommitPattern.MatchString(v)
}
func shortVersion(v string) string {
	if len(v) > 7 {
		return v[:7]
	}
	return v
}

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
var runCode = func(p runtimeProxy, kernel, code string, untilBusy bool) error {
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

// Keep-alive management now lives in keepalive.go (persistent runtime proxy WebSocket + tunnel heartbeat).

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
func stopRemoteSupervisor(p runtimeProxy, r ServiceRecord) error {
	path, _ := json.Marshal("/content/.colabstudio/" + r.ID)
	owner, _ := json.Marshal(r.ID)
	binding, _ := json.Marshal(r.Binding)
	nonce, _ := json.Marshal(random())
	code := fmt.Sprintf(`import fcntl,json,os,pathlib,signal,time,urllib.error,urllib.request
def _stop_supervisor():
    p=pathlib.Path(%s)
    if not p.exists():
        return
    dep=p/'deployment.json'
    if dep.exists():
        c=json.loads(dep.read_text())
        assert c.get('id')==%s and c.get('binding')==%s,'owner mismatch'
    assert not (p/'draining').exists(),'shutdown in progress'
    st_file=p/'state.json'
    if st_file.exists():
        st=json.loads(st_file.read_text())
        assert st.get('owner')==%s and st.get('binding')==%s,'state owner mismatch'
        if st.get('stage')=='online':
            def get(port,path):
                with urllib.request.urlopen('http://127.0.0.1:'+str(port)+path,timeout=8) as resp:
                    return json.load(resp)
            try:
                q=get(8188,'/queue')
                assert isinstance(q.get('queue_running'),list) and isinstance(q.get('queue_pending'),list)
                assert not q['queue_running'] and not q['queue_pending'],'queue busy'
            except urllib.error.URLError as e:
                if not isinstance(getattr(e,'reason',None),ConnectionRefusedError):
                    raise
            try:
                jobs=get(8190,'/api/jobs');agents=get(8190,'/api/agent/runs')
                assert isinstance(jobs,list) and isinstance(agents,list)
                assert all(j.get('status') in ('done','error','cancelled','stopped') for j in jobs),'jobs busy'
                assert all(a.get('status') in ('done','error','cancelled','stopped') for a in agents),'agent busy'
            except urllib.error.URLError as e:
                if not isinstance(getattr(e,'reason',None),ConnectionRefusedError):
                    raise
    stop_nonce=%s
    cmd_file=p/'command.json'
    tmp=cmd_file.with_suffix('.tmp')
    tmp.write_text(json.dumps({'action':'stop_supervisor','nonce':stop_nonce}),encoding='utf-8')
    tmp.replace(cmd_file)
    target=str((p/'supervisor.py').resolve())
    targets=[]
    me=os.getpid()
    proc_root=pathlib.Path('/proc')
    if proc_root.is_dir():
        for entry in proc_root.iterdir():
            if not entry.name.isdigit():
                continue
            pid=int(entry.name)
            if pid==me:
                continue
            try:
                raw=(entry/'cmdline').read_bytes()
                if not raw:
                    continue
                args=[a.decode('utf-8','replace') for a in raw.split(b'\x00') if a]
                cwd=os.path.realpath(str(entry/'cwd'))
                matched=False
                for arg in args[1:]:
                    if arg in ('-c','-m'):
                        break
                    if arg.startswith('-'):
                        continue
                    cand=arg if os.path.isabs(arg) else os.path.join(cwd,arg)
                    if os.path.realpath(cand)==target:
                        matched=True
                    break
                if matched:
                    stat=(entry/'stat').read_text().rsplit(')',1)[1].split()
                    if stat[0]!='Z':
                        targets.append((pid,(stat[19],raw.hex())))
            except Exception:
                continue
    for pid,_ in targets:
        try:
            os.kill(pid,signal.SIGTERM)
        except OSError:
            pass
    with open(p/'supervisor.lock','a') as lf:
        acquired=False
        for i in range(20):
            try:
                fcntl.flock(lf,fcntl.LOCK_EX|fcntl.LOCK_NB)
                acquired=True
                break
            except BlockingIOError:
                if i==10:
                    for pid,sig in targets:
                        try:
                            stat=pathlib.Path('/proc/'+str(pid)+'/stat').read_text().rsplit(')',1)[1].split()
                            raw=pathlib.Path('/proc/'+str(pid)+'/cmdline').read_bytes()
                            if stat[0]!='Z' and (stat[19],raw.hex())==sig:
                                os.kill(pid,signal.SIGKILL)
                        except Exception:
                            pass
                time.sleep(0.5)
        if not acquired:
            raise RuntimeError('previous supervisor still holds lock')
        try:
            if cmd_file.exists():
                cur=json.loads(cmd_file.read_text())
                if cur.get('action')=='stop_supervisor' and cur.get('nonce')==stop_nonce:
                    cmd_file.unlink(missing_ok=True)
        except Exception:
            pass
_stop_supervisor()
`, string(path), string(owner), string(binding), string(owner), string(binding), string(nonce))
	if e := executeOwned(p, code); e != nil {
		return errors.New("无法安全停止旧版监督进程（可能仍有任务运行或正在关闭），已取消升级；未改动原实例")
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
	root := r.RemoteRoot
	var v any
	if root == "" || proxyRequest(httpClient, p, "GET", "api/contents/"+root, nil, &v) != nil {
		code := "import pathlib\np=pathlib.Path(" + string(quoted) + ")\np.mkdir(parents=True,exist_ok=True)\n"
		if e = executeOwned(p, code); e != nil {
			return m.fail(*r, e)
		}
		root = "content/.colabstudio/" + r.ID
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
	}
	// A persistent marker is checked before touching files or restarting anything.
	var prior struct {
		ID      string `json:"id"`
		Binding string `json:"binding"`
	}
	priorExists := false
	if e = remoteJSON(p, root, "deployment.json", &prior); e == nil {
		priorExists = true
		if prior.ID != r.ID || prior.Binding != r.Binding {
			return m.fail(*r, errors.New("远端服务标记不匹配，未覆盖部署"))
		}
	} else if !strings.Contains(e.Error(), "HTTP 404") {
		return m.fail(*r, errors.New("远端部署标记读取失败，未覆盖部署"))
	}
	if priorExists {
		var owner remoteState
		if se := remoteJSON(p, root, "state.json", &owner); se == nil {
			if owner.Owner != r.ID || owner.Binding != r.Binding {
				return m.fail(*r, errors.New("远端服务身份不匹配，已暂停自动操作"))
			}
			if !validRemoteVersion(owner.Version) {
				return m.fail(*r, errors.New("远端服务版本标记无效，已暂停自动操作"))
			}
			age := time.Since(time.Unix(int64(owner.Heartbeat), 0))
			if age >= -10*time.Second && age <= 90*time.Second {
				if r.StopRequested || owner.Stage == "draining" || owner.Stage == "backing_up" || owner.Stage == "backup_ready" {
					return m.fail(*r, errors.New("服务正在停止或备份中，暂不能热升级"))
				}
				if owner.Stage == "installing" {
					return m.fail(*r, errors.New("远端正在执行安装或下载模型，进度已恢复显示；请等待安装完成后再升级版本"))
				}
				if owner.Stage != "failed" && owner.Busy {
					return m.fail(*r, errors.New("远端仍有运行中的任务或队列未确认空闲，请等待任务完成后再热升级"))
				}
			}
		} else if !strings.Contains(se.Error(), "HTTP 404") {
			return m.fail(*r, errors.New("远端状态读取失败，暂不替换监督进程"))
		}
		if e = m.startStep(r, "replacing", "正在安全停止旧版远端监督进程（保留已加载模型与当前作品）"); e != nil {
			return e
		}
		if e = stopRemoteSupervisor(p, *r); e != nil {
			return m.fail(*r, e)
		}
	}
	if e = m.startStep(r, "uploading", "上传固定版本的部署脚本与配置"); e != nil {
		return e
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
	launchCode := "import pathlib,subprocess,sys\np=pathlib.Path(" + string(quoted) + ")\nsubprocess.Popen([sys.executable,'-u',str(p/'supervisor.py')],cwd=p,stdout=open(p/'deployment.log','a'),stderr=subprocess.STDOUT,start_new_session=True)\n"
	if e = executeOwned(p, launchCode); e != nil {
		return m.fail(*r, e)
	}
	r.State = "installing"
	r.RemoteVersion = ""
	if m.ka != nil {
		m.ka.syncRoot(p, r.Runtime, root)
	}
	if e = ensureKeepAlive(p, root); e != nil {
		r.StartupLog = boundedStartupLog(r.StartupLog, "云端保活暂未启动，App 在线时会自动重试")
	}
	if m.ka != nil {
		m.ka.notify()
	}
	doneMsg := "远端安装进程已提交；等待环境检查、下载与实时日志，尚未确认网页就绪"
	if hasData || priorExists {
		doneMsg = "远端热升级已提交；正在更新固定版本并平滑重启网页进程（保留已下载模型与引擎）"
	}
	return m.startStep(r, "done", doneMsg)
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
	if v.Owner != r.ID || v.Binding != r.Binding {
		return v, errors.New("远端服务身份不匹配，已暂停自动操作")
	}
	if !validRemoteVersion(v.Version) {
		return v, errors.New("远端服务版本标记无效，已暂停自动操作")
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
	r.RemoteVersion = v.Version
	r.Heartbeat = time.Unix(int64(v.Heartbeat), 0)
	r.Message = map[string]string{"installing": "正在安装并校验模型，请查看部署日志", "starting": "正在启动服务", "online": "服务在线；网页访问需验证当前账号邮箱", "failed": "部署失败；实例可能仍在计费，请查看日志", "draining": "正在等待任务完成", "backing_up": "正在打包作品", "backup_ready": "正在等待 Drive 保存校验"}[v.Stage]
	if !v.Health && v.Stage == "online" {
		r.Message = "服务健康检查异常，暂不释放实例"
	}
	if r.StartupError == "远端服务身份或版本不匹配，已暂停自动操作" || (v.Version == toyboxVersion && v.Stage == "online" && v.Health) {
		r.StartupError = ""
		if r.StartupPhase == "failed" {
			r.StartupPhase = "done"
		}
	}
	if v.Version != toyboxVersion {
		if v.Stage == "online" && v.Health {
			r.Message = "服务在线（远端运行旧版本 " + shortVersion(v.Version) + "，空闲时可点击热升级更新至 " + shortVersion(toyboxVersion) + "，或直接停止并保存释放）"
		} else if r.Message != "" {
			r.Message += "（远端旧版本 " + shortVersion(v.Version) + "）"
		} else {
			r.Message = "远端运行旧版本 " + shortVersion(v.Version)
		}
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
	r.RemoteVersion = ""
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
	if v, e := m.state(r); e == nil && v.Version == toyboxVersion && v.Stage != "failed" {
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, "GET", tunnelBase+url.PathEscape(endpoint)+"/keep-alive/?authuser=0", nil)
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Colab-Tunnel", "Google")
	req.Header.Set("X-Colab-Client-Agent", "colab-cli")
	req.Header.Set("Accept", "application/json")
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
	r.RemoteVersion = ""
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
	r.RemoteVersion = ""
	r.State = "stopped"
	r.Message = "机器已释放，未部署服务"
	return m.put(*r)
}
