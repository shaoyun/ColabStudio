package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const driveScope = "https://www.googleapis.com/auth/drive.file"

type driveGrant struct {
	Config  Config `json:"config"`
	Token   Token  `json:"token"`
	Refresh string `json:"refresh"`
	Email   string `json:"email"`
	Folder  string `json:"folder,omitempty"`
	// Rclone points at EasyColab's drive.file rclone config; rclone refreshes it.
	Rclone string `json:"rclone,omitempty"`
}
type driveFlow struct {
	Config                           Config
	Verifier, State, Redirect, Email string
	AuthURL                          string
	WebManual                        bool
	Until                            time.Time
	ID                               string
	Phase                            string
	Close                            func() error
	Timer                            *time.Timer
}

func loadDriveGrant() (driveGrant, error) {
	var d driveGrant
	b, e := store.Get("drive-grant")
	if e != nil {
		return d, e
	}
	if json.Unmarshal(b, &d) != nil {
		return d, errors.New("Drive 凭据格式无效")
	}
	return d, nil
}
func persistDrive(d driveGrant) error { b, _ := json.Marshal(d); return store.Set("drive-grant", b) }
func driveAccess(_ string) (string, error) {
	d, e := loadDriveGrant()
	if e == nil && d.Rclone != "" {
		return rcloneAccess(d.Rclone)
	}
	if e != nil || d.Refresh == "" {
		return "", errors.New("请先连接 Google Drive 以保存文件")
	}
	if time.Now().Before(d.Token.Until.Add(-time.Minute)) {
		return d.Token.Access, nil
	}
	t, e := exchange(url.Values{"client_id": {d.Config.ClientID}, "client_secret": {d.Config.ClientSecret}, "refresh_token": {d.Refresh}, "grant_type": {"refresh_token"}})
	if e != nil {
		return "", errors.New("Drive 授权无法刷新，请在服务页重新连接 Drive")
	}
	if t.Refresh != "" {
		d.Refresh = t.Refresh
	}
	d.Token = t
	if e = persistDrive(d); e != nil {
		return "", e
	}
	return t.Access, nil
}

var driveAuthProfile = func(s *Studio) (AccountProfile, error) { return s.Profile() }
var driveAuthConfig = loadDriveOAuthConfig
var driveAuthOpen = platformOpenExternal

func loadDriveOAuthConfig() (Config, error) {
	c, e := loadConfig()
	if b, re := store.Get("drive-oauth-client"); re == nil {
		var own Config
		if json.Unmarshal(b, &own) == nil && own.ClientID != "" {
			return own, nil
		}
	}
	return c, e
}
func stopDriveFlow(f *driveFlow) {
	if f == nil {
		return
	}
	if f.Timer != nil {
		f.Timer.Stop()
	}
	if f.Close != nil {
		_ = f.Close()
	}
}
func (s *Studio) driveFlowError(f *driveFlow, message string) error {
	s.driveMu.Lock()
	if s.driveFlow == f {
		s.driveFlow = nil
		s.driveMessage = message
	}
	s.driveMu.Unlock()
	stopDriveFlow(f)
	return errors.New(message)
}
func (s *Studio) expireDriveFlow(f *driveFlow) {
	s.driveMu.Lock()
	if s.driveFlow != f {
		s.driveMu.Unlock()
		return
	}
	s.driveFlow = nil
	s.driveMessage = "Drive 授权已超时，可以重新发起"
	s.driveMu.Unlock()
	stopDriveFlow(f)
}
func (s *Studio) CancelDriveLogin(id string) error {
	s.driveMu.Lock()
	f := s.driveFlow
	if id != "" && (f == nil || f.ID != id) {
		s.driveMu.Unlock()
		return nil
	}
	s.driveFlow = nil
	s.driveMessage = "已取消本次 Drive 授权，可重新发起；已有登录保留"
	s.driveMu.Unlock()
	stopDriveFlow(f)
	return nil
}
func (s *Studio) BeginDriveLogin(id string) (bool, error) {
	f, e := s.startDriveFlow(id, false)
	if e != nil {
		return false, e
	}
	return f.Config.Remote, nil
}

func (s *Studio) beginWebDriveLogin(id string) (map[string]any, error) {
	f, e := s.startDriveFlow(id, true)
	if e != nil {
		return nil, e
	}
	return map[string]any{
		"remote":          true,
		"authURL":         f.AuthURL,
		"desktopCallback": !f.Config.Remote,
	}, nil
}

func (s *Studio) startDriveFlow(id string, webManual bool) (*driveFlow, error) {
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{8,100}$`).MatchString(id) {
		return nil, errors.New("授权请求标识无效，请重新点击连接 Drive")
	}
	s.driveMu.Lock()
	if s.driveFlow != nil && time.Now().Before(s.driveFlow.Until) {
		s.driveMu.Unlock()
		return nil, errors.New("Drive 授权正在进行，可以取消本次授权后重新发起")
	}
	old := s.driveFlow
	f := &driveFlow{ID: id, Verifier: random(), State: random(), WebManual: webManual, Until: time.Now().Add(5 * time.Minute), Phase: "preparing"}
	f.Timer = time.AfterFunc(5*time.Minute, func() { s.expireDriveFlow(f) })
	s.driveFlow = f
	s.driveAttemptEmail = ""
	s.driveMessage = "正在准备 Drive 授权，可随时取消重试"
	s.driveMu.Unlock()
	stopDriveFlow(old)
	p, e := driveAuthProfile(s)
	if e == nil && s.svc != nil {
		r := s.svc.get()
		if r.Account != "" && !strings.EqualFold(r.Account, p.Email) {
			e = fmt.Errorf("服务绑定账号为 %s，请先切回该 Colab 账号再连接 Drive", r.Account)
		}
	}
	if e != nil {
		return nil, s.driveFlowError(f, e.Error())
	}
	c, e := driveAuthConfig()
	if e != nil {
		return nil, s.driveFlowError(f, e.Error())
	}
	var ln net.Listener
	redirect := remoteRedirect
	if !c.Remote {
		if webManual {
			redirect = webDriveRedirect
		} else {
			ln, e = net.Listen("tcp4", "127.0.0.1:0")
			if e != nil {
				return nil, s.driveFlowError(f, "无法创建 Drive 本地回调")
			}
			redirect = "http://" + ln.Addr().String() + "/drive-callback"
		}
	}
	hash := sha256.Sum256([]byte(f.Verifier))
	q := url.Values{"client_id": {c.ClientID}, "redirect_uri": {redirect}, "response_type": {"code"}, "scope": {driveScope + " openid email profile"}, "state": {f.State}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "code_challenge_method": {"S256"}, "access_type": {"offline"}, "prompt": {"select_account consent"}, "login_hint": {p.Email}}
	if c.Remote {
		q.Set("token_usage", "remote")
	}
	authURL := "https://accounts.google.com/o/oauth2/v2/auth?" + q.Encode()

	s.driveMu.Lock()
	if s.driveFlow != f || time.Now().After(f.Until) {
		s.driveMu.Unlock()
		if ln != nil {
			ln.Close()
		}
		return nil, errors.New("本次 Drive 授权已取消，请使用新流程")
	}
	f.Config = c
	f.Email = p.Email
	f.Redirect = redirect
	f.AuthURL = authURL
	f.Phase = "browser"
	if ln != nil {
		server := &http.Server{ReadHeaderTimeout: 5 * time.Second}
		mux := http.NewServeMux()
		server.Handler = mux
		f.Close = server.Close
		mux.HandleFunc("/drive-callback", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			if r.URL.Query().Get("state") != f.State {
				http.Error(w, "授权状态无效", 400)
				return
			}
			e := s.completeDriveCodeFor(f, r.URL.Query().Get("code"))
			if e != nil {
				fmt.Fprint(w, "Drive 授权未完成，请返回 Colab Studio 查看原因。")
			} else {
				fmt.Fprint(w, "Drive 已连接，请返回 Colab Studio。")
			}
		})
		go server.Serve(ln)
	}
	if webManual {
		s.driveMessage = "请在浏览器完成 Drive 授权，将结果页授权码或完整回调地址粘贴回此处"
	} else {
		s.driveMessage = "等待浏览器完成 Drive 授权；未弹出时可取消或重新发起"
	}
	s.driveMu.Unlock()

	if !webManual {
		// Opening the browser never holds the flow mutex, so cancellation remains responsive.
		if e = driveAuthOpen(authURL); e != nil {
			return nil, s.driveFlowError(f, "无法打开 Drive 授权浏览器，可重新发起")
		}
	}
	s.driveMu.Lock()
	active := s.driveFlow == f
	s.driveMu.Unlock()
	if !active {
		return nil, errors.New("本次 Drive 授权已取消，请使用新流程")
	}
	return f, nil
}
func (s *Studio) SubmitDriveCode(id, code string) error {
	s.driveMu.Lock()
	f := s.driveFlow
	s.driveMu.Unlock()
	if f == nil || f.ID != id {
		return errors.New("此授权码属于旧流程，请使用当前授权页面")
	}
	return s.completeDriveCodeFor(f, code)
}
func (s *Studio) completeDriveCodeFor(f *driveFlow, codeInput string) error {
	s.driveMu.Lock()
	if f == nil || s.driveFlow != f || time.Now().After(f.Until) {
		s.driveMu.Unlock()
		return errors.New("本次 Drive 授权已取消或失效，请使用当前流程")
	}
	if f.Phase == "preparing" || f.Phase == "exchanging" {
		s.driveMu.Unlock()
		return errors.New("Drive 授权正在准备或处理，请稍候，也可取消后重试")
	}
	code, parseErr := extractOAuthCode(codeInput, f.State, "Drive 授权未获同意或授权码无效，可以重新发起")
	if parseErr != nil {
		s.driveMu.Unlock()
		return s.driveFlowError(f, parseErr.Error())
	}
	f.Phase = "exchanging"
	s.driveMessage = "正在验证 Drive 授权，可取消"
	s.driveMu.Unlock()
	t, e := exchange(url.Values{"client_id": {f.Config.ClientID}, "client_secret": {f.Config.ClientSecret}, "redirect_uri": {f.Redirect}, "grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {f.Verifier}})
	var profile AccountProfile
	if e == nil {
		profile, e = fetchProfile(httpClient, profileEndpoint, t.Access)
		if e == nil && t.Refresh == "" {
			e = errors.New("Drive 未返回离线授权，请重新授权")
		}
	}
	s.driveMu.Lock()
	if s.driveFlow != f || time.Now().After(f.Until) {
		s.driveMu.Unlock()
		stopDriveFlow(f)
		return errors.New("本次 Drive 授权已取消，返回结果未保存")
	}
	if profile.Email != "" {
		s.driveAttemptEmail = profile.Email
	}
	if e == nil {
		folder := ""
		if prior, pe := loadDriveGrant(); pe == nil && strings.EqualFold(prior.Email, profile.Email) && prior.Config.ClientID == f.Config.ClientID {
			folder = prior.Folder
		}
		e = persistDrive(driveGrant{Config: f.Config, Token: t, Refresh: t.Refresh, Email: profile.Email, Folder: folder})
	}
	s.driveFlow = nil
	if e != nil {
		s.driveMessage = e.Error()
	} else {
		s.driveMessage = "Drive 已连接；仅访问本应用创建的文件"
	}
	s.driveMu.Unlock()
	// Close after a callback handler has had a chance to return its browser response.
	if f.Timer != nil {
		f.Timer.Stop()
	}
	if f.Close != nil {
		time.AfterFunc(150*time.Millisecond, func() { _ = f.Close() })
	}
	return e
}
func (s *Studio) DriveStatus() map[string]any {
	s.driveMu.Lock()
	msg := s.driveMessage
	attempt := s.driveAttemptEmail
	f := s.driveFlow
	phase := ""
	id := ""
	remote := false
	authURL := ""
	until := time.Time{}
	pending := false
	if f != nil && time.Now().Before(f.Until) {
		pending = true
		id = f.ID
		phase = f.Phase
		remote = f.Config.Remote || f.WebManual
		authURL = f.AuthURL
		until = f.Until
	}
	s.driveMu.Unlock()
	d, _ := loadDriveGrant()
	expected := ""
	if s.svc != nil {
		expected = s.svc.get().Account
	}
	if expected == "" {
		if p, e := s.Profile(); e == nil {
			expected = p.Email
		}
	}
	connected := d.Refresh != "" || d.Rclone != ""
	status := "disconnected"
	if connected {
		status = "connected"
	}
	if pending {
		status = "pending"
	}
	folder := d.Folder

	if !connected {
		folder = ""
	}
	client := "沿用 Colab client（其项目未启用 Drive API，需导入独立 client）"
	if d.Rclone != "" {
		client = "EasyColab 的 rclone 授权（无需自建项目）"
	} else if b, e := store.Get("drive-oauth-client"); e == nil {
		var own Config
		if json.Unmarshal(b, &own) == nil && own.ClientID != "" {
			client = "独立 client · 项目 " + strings.SplitN(own.ClientID, "-", 2)[0]
		}
	}
	out := map[string]any{"client": client, "connected": connected, "email": d.Email, "expectedEmail": expected, "attemptEmail": attempt, "status": status, "folder": folder, "folderName": "H3-Toybox", "message": msg, "pending": pending, "id": id, "phase": phase, "remote": remote, "until": until}
	if pending && authURL != "" {
		out["authURL"] = authURL
	}
	return out
}

var driveID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

func driveRequest(method, address, token string, body io.Reader, headers map[string]string) (*http.Response, error) {
	req, e := http.NewRequest(method, address, body)
	if e != nil {
		return nil, errors.New("Drive 请求无效")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	safe := *httpClient
	safe.Timeout = 10 * time.Minute
	safe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, e := safe.Do(req)
	if e != nil {
		return nil, errors.New("Drive 连接失败，实例保留且可能仍在计费")
	}
	if resp.StatusCode >= 300 {
		var detail struct {
			Error struct {
				Details []struct {
					Reason string `json:"reason"`
				} `json:"details"`
			} `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&detail)
		resp.Body.Close()
		for _, v := range detail.Error.Details {
			if v.Reason == "SERVICE_DISABLED" {
				return nil, errors.New("Drive API 未启用：请在 OAuth client 所属 Google Cloud 项目启用 Google Drive API；或点击“使用 EasyColab 的 Drive 授权”。未释放实例")
			}
		}
		return nil, fmt.Errorf("Drive HTTP %d，请检查 API 是否启用、授权或空间；未释放实例", resp.StatusCode)
	}
	return resp, nil
}
func driveJSON(method, address, token string, value, out any) error {
	var body io.Reader
	if value != nil {
		b, e := json.Marshal(value)
		if e != nil {
			return e
		}
		body = bytes.NewReader(b)
	}
	resp, e := driveRequest(method, address, token, body, map[string]string{"Content-Type": "application/json"})
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if out != nil && json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out) != nil {
		return errors.New("Drive 返回格式无效")
	}
	return nil
}
func driveFolder(token, existing string) (string, error) {
	if driveID.MatchString(existing) {
		var v struct {
			ID string `json:"id"`
		}
		e := driveJSON("GET", "https://www.googleapis.com/drive/v3/files/"+existing+"?fields=id", token, nil, &v)
		return v.ID, e
	}
	parent, e := driveManagedFolder(token, "ColabStudio", "ColabStudio-root-v1", "")
	if e != nil {
		return "", e
	}
	return driveManagedFolder(token, "H3-Toybox", "ColabStudio-Toybox-v1", parent)
}

func driveManagedFolder(token, name, marker, parent string) (string, error) {
	var listed struct {
		Files []struct {
			ID string `json:"id"`
		} `json:"files"`
	}
	q := "trashed = false and mimeType = 'application/vnd.google-apps.folder' and appProperties has { key='manager' and value='" + marker + "' }"
	if parent != "" {
		q += " and '" + parent + "' in parents"
	}
	if e := driveJSON("GET", "https://www.googleapis.com/drive/v3/files?"+url.Values{"q": {q}, "fields": {"files(id)"}, "pageSize": {"100"}}.Encode(), token, nil, &listed); e != nil {
		return "", e
	}
	if len(listed.Files) > 0 && driveID.MatchString(listed.Files[0].ID) {
		return listed.Files[0].ID, nil
	}
	var v struct {
		ID string `json:"id"`
	}
	meta := map[string]any{"name": name, "mimeType": "application/vnd.google-apps.folder", "appProperties": map[string]string{"manager": marker}}
	if parent != "" {
		meta["parents"] = []string{parent}
	}
	e := driveJSON("POST", "https://www.googleapis.com/drive/v3/files?fields=id", token, meta, &v)
	if e == nil && !driveID.MatchString(v.ID) {
		e = errors.New("Drive 未返回备份目录")
	}
	return v.ID, e
}
func backupDrive(s *Studio, r ServiceRecord, b remoteBackup) (string, string, error) {
	if !regexp.MustCompile(`^backup-[A-Za-z0-9_-]+\.tar\.gz$`).MatchString(b.File) || b.Size < 1 || b.Size > 50<<30 || !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(b.MD5) {
		return "", "", errors.New("云端备份信息无效")
	}
	token, e := driveAccess(r.Account)
	if e != nil {
		return "", "", e
	}
	existingFolder := ""
	if grant, ge := loadDriveGrant(); ge == nil {
		existingFolder = grant.Folder
	}
	folder, e := driveFolder(token, existingFolder)
	if e != nil {
		return "", "", e
	}
	if grant, ge := loadDriveGrant(); ge == nil && grant.Folder != folder {
		grant.Folder = folder
		if e = persistDrive(grant); e != nil {
			return "", folder, e
		}
	}
	if id, e := findDriveBackup(token, folder, r.ID, b.Nonce, b.MD5, b.Size); e != nil {
		return "", folder, e
	} else if id != "" {
		return id, folder, nil
	}
	p, e := s.proxy(r.Runtime)
	if e != nil {
		return "", folder, e
	}
	f, e := os.CreateTemp("", "ColabStudio-backup-*.tar.gz")
	if e != nil {
		return "", folder, e
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if e = downloadRuntime(p, r.RemoteRoot+"/"+b.File, f); e != nil {
		return "", folder, e
	}
	st, _ := f.Stat()
	if st.Size() != b.Size {
		return "", folder, errors.New("备份下载大小不匹配，未释放实例")
	}
	f.Seek(0, 0)
	h := md5.New()
	if _, e = io.Copy(h, f); e != nil {
		return "", folder, e
	}
	if hex.EncodeToString(h.Sum(nil)) != b.MD5 {
		return "", folder, errors.New("备份校验失败，未释放实例")
	}
	f.Seek(0, 0)
	meta, _ := json.Marshal(map[string]any{"name": "Toybox-" + time.Now().Format("20060102-150405") + ".tar.gz", "parents": []string{folder}, "appProperties": map[string]string{"service": r.ID, "backup": b.Nonce}})
	init, e := driveRequest("POST", "https://www.googleapis.com/upload/drive/v3/files?uploadType=resumable&fields=id,md5Checksum,size", token, bytes.NewReader(meta), map[string]string{"Content-Type": "application/json", "X-Upload-Content-Type": "application/gzip", "X-Upload-Content-Length": fmt.Sprint(b.Size)})
	if e != nil {
		return "", folder, e
	}
	location := init.Header.Get("Location")
	init.Body.Close()
	u, e := url.Parse(location)
	if e != nil || u.Scheme != "https" || u.Host != "www.googleapis.com" || u.User != nil {
		return "", folder, errors.New("Drive 上传地址无效")
	}
	// Upload in bounded chunks; a failed transfer can be safely retried using the same backup snapshot.
	chunk := make([]byte, 8<<20)
	var offset int64
	var uploaded struct {
		ID   string `json:"id"`
		MD5  string `json:"md5Checksum"`
		Size string `json:"size"`
	}
	for offset < b.Size {
		n, re := io.ReadFull(f, chunk)
		if re != nil && re != io.EOF && re != io.ErrUnexpectedEOF {
			return "", folder, re
		}
		if n == 0 {
			return "", folder, errors.New("备份文件提前结束")
		}
		req, _ := http.NewRequest("PUT", location, bytes.NewReader(chunk[:n]))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/gzip")
		req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+int64(n)-1, b.Size))
		safe := *httpClient
		safe.Timeout = 5 * time.Minute
		safe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, e := safe.Do(req)
		if e != nil {
			return "", folder, errors.New("Drive 上传中断，备份保留；未释放实例")
		}
		offset += int64(n)
		if offset < b.Size {
			expected := fmt.Sprintf("bytes=0-%d", offset-1)
			valid := resp.StatusCode == 308 && resp.Header.Get("Range") == expected
			resp.Body.Close()
			if !valid {
				return "", folder, errors.New("Drive 分块进度不一致，未释放实例")
			}
		} else {
			valid := resp.StatusCode == 200 || resp.StatusCode == 201
			if valid {
				e = json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&uploaded)
			}
			resp.Body.Close()
			if !valid || e != nil {
				return "", folder, errors.New("Drive 未确认上传成功，未释放实例")
			}
		}
	}
	if !driveID.MatchString(uploaded.ID) || uploaded.MD5 != b.MD5 || uploaded.Size != fmt.Sprint(b.Size) {
		return "", folder, errors.New("Drive 备份大小或校验值不匹配，未释放实例")
	}
	return uploaded.ID, folder, nil
}
func restoreDrive(p runtimeProxy, r ServiceRecord) error {
	if !driveID.MatchString(r.Archive) {
		return errors.New("Drive 备份标识无效")
	}
	token, e := driveAccess(r.Account)
	if e != nil {
		return e
	}
	var meta struct {
		MD5  string `json:"md5Checksum"`
		Size string `json:"size"`
	}
	if e = driveJSON("GET", "https://www.googleapis.com/drive/v3/files/"+r.Archive+"?fields=md5Checksum,size", token, nil, &meta); e != nil {
		return e
	}
	resp, e := driveRequest("GET", "https://www.googleapis.com/drive/v3/files/"+r.Archive+"?alt=media", token, nil, nil)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	h := md5.New()
	reader := io.TeeReader(io.LimitReader(resp.Body, 50<<30), h)
	chunk := make([]byte, 4<<20)
	parts := 0
	var size int64
	for {
		n, re := io.ReadFull(reader, chunk)
		if n > 0 {
			if e = putRemote(p, r.RemoteRoot, fmt.Sprintf("restore.part%06d", parts), chunk[:n]); e != nil {
				return e
			}
			parts++
			size += int64(n)
		}
		if re == io.EOF || re == io.ErrUnexpectedEOF {
			break
		}
		if re != nil {
			return errors.New("Drive 恢复下载失败")
		}
	}
	if hex.EncodeToString(h.Sum(nil)) != meta.MD5 || fmt.Sprint(size) != meta.Size {
		return errors.New("Drive 恢复校验失败，未启动服务")
	}
	b, _ := json.Marshal(map[string]any{"parts": parts, "md5": meta.MD5})
	return putRemote(p, r.RemoteRoot, "restore.json", b)
}

var driveFolderOpen = platformOpenExternal

func (s *Studio) ensureDriveFolderURL() (string, error) {
	m := s.services()
	if m.err != nil {
		return "", m.err
	}
	if !m.op.TryLock() {
		return "", errors.New("服务操作正在进行，请稍后打开保存目录")
	}
	defer m.op.Unlock()
	r := m.get()
	p, e := m.identity(r)
	if e != nil {
		return "", e
	}
	token, e := driveAccess(p.Email)
	if e != nil {
		return "", e
	}
	d, e := loadDriveGrant()
	if e != nil {
		return "", e
	}
	folder := d.Folder
	folder, e = driveFolder(token, folder)
	if e != nil {
		return "", e
	}
	d.Folder = folder
	if e = persistDrive(d); e != nil {
		return "", e
	}
	if r.Binding != "" {
		r.Folder = folder
		if e = m.put(r); e != nil {
			return "", e
		}
	}
	return "https://drive.google.com/drive/folders/" + folder, nil
}

func (s *Studio) OpenDriveFolder() error {
	address, e := s.ensureDriveFolderURL()
	if e != nil {
		return e
	}
	return driveFolderOpen(address)
}

// Reuse a completed upload after an interrupted local response or release failure.
func findDriveBackup(token, folder, service, nonce, md5sum string, size int64) (string, error) {
	for _, v := range []string{folder, service, nonce} {
		if !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(v) {
			return "", errors.New("备份查询标识无效")
		}
	}
	q := "trashed = false and '" + folder + "' in parents and appProperties has { key='service' and value='" + service + "' } and appProperties has { key='backup' and value='" + nonce + "' }"
	var result struct {
		Files []struct {
			ID   string `json:"id"`
			MD5  string `json:"md5Checksum"`
			Size string `json:"size"`
		} `json:"files"`
	}
	if e := driveJSON("GET", "https://www.googleapis.com/drive/v3/files?"+url.Values{"q": {q}, "fields": {"files(id,md5Checksum,size)"}, "pageSize": {"100"}}.Encode(), token, nil, &result); e != nil {
		return "", e
	}
	for _, f := range result.Files {
		if f.MD5 == md5sum && f.Size == fmt.Sprint(size) && driveID.MatchString(f.ID) {
			return f.ID, nil
		}
	}
	return "", nil
}

// A separate Desktop client is optional when the reused Colab client's project
// cannot enable Drive API. It never changes Colab's OAuth configuration.
func (s *Studio) ImportDriveClient() error {
	paths, e := platformOpenJSONDialog("选择 Drive Desktop OAuth client", "选择已启用 Google Drive API 的自建 Desktop app JSON；不改变 Colab 登录。", false)
	if e != nil {
		if isWebServerMode() {
			return e
		}
		return errors.New("无法打开 Drive 配置选择窗口")
	}
	if len(paths) == 0 {
		return errors.New("已取消导入 Drive 配置")
	}
	b, e := os.ReadFile(paths[0])
	if e != nil {
		return errors.New("无法读取 Drive OAuth 配置")
	}
	return s.importDriveClientBytes(b)
}

func (s *Studio) importDriveClientBytes(b []byte) error {
	var doc struct {
		Installed Config `json:"installed"`
	}
	if json.Unmarshal(b, &doc) != nil || !strings.HasSuffix(doc.Installed.ClientID, ".apps.googleusercontent.com") || doc.Installed.ClientSecret == "" || strings.HasPrefix(doc.Installed.ClientID, "764086051850-") {
		return errors.New("请选择自建 Desktop app 的 client JSON，不使用 Web client 或 Cloud SDK client")
	}
	doc.Installed.Remote = false
	value, _ := json.Marshal(doc.Installed)
	if e := store.Set("drive-oauth-client", value); e != nil {
		return e
	}
	_ = s.CancelDriveLogin("")
	s.driveMu.Lock()
	s.driveMessage = "独立 Drive client 已导入，请点击连接 Google Drive"
	s.driveMu.Unlock()
	return nil
}

var rcloneDriveConf = func() string {
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".config/easycolab/drive_rclone.conf")
}

func parseRcloneDriveConf(b []byte) (name, access string, until time.Time, err error) {
	kv, sections := map[string]string{}, 0
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			sections++
			name = line[1 : len(line)-1]
		} else if k, v, ok := strings.Cut(line, "="); ok {
			kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	if sections != 1 || !driveID.MatchString(name) || kv["type"] != "drive" || kv["scope"] != "drive.file" {
		return "", "", until, errors.New("EasyColab Drive 授权须为单个 drive.file 权限的 rclone 配置")
	}
	var t struct {
		Access  string    `json:"access_token"`
		Refresh string    `json:"refresh_token"`
		Expiry  time.Time `json:"expiry"`
	}
	if json.Unmarshal([]byte(kv["token"]), &t) != nil || t.Refresh == "" {
		return "", "", until, errors.New("EasyColab Drive 授权缺少 refresh token，请在 EasyColab 重新授权")
	}
	return name, t.Access, t.Expiry, nil
}

// readRcloneDrive accepts only the single drive.file remote EasyColab uses.
func readRcloneDrive(path string) (name, access string, until time.Time, err error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return "", "", until, errors.New("未找到 EasyColab 的 Drive 授权（~/.config/easycolab/drive_rclone.conf）")
	}
	return parseRcloneDriveConf(b)
}

var rcloneRefresh = func(path, name string) error {
	bin, e := exec.LookPath("rclone")
	for _, p := range []string{"/opt/homebrew/bin/rclone", "/usr/local/bin/rclone", "/usr/bin/rclone"} {
		if e != nil {
			if _, se := os.Stat(p); se == nil {
				bin, e = p, nil
			}
		}
	}
	if e != nil {
		return errors.New("未找到 rclone，请先安装 rclone（macOS: brew install rclone；Linux: apt/yum install rclone）")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// Any authenticated call makes rclone refresh and write back the token.
	if exec.CommandContext(ctx, bin, "--config", path, "about", name+":").Run() != nil {
		return errors.New("rclone 刷新 Drive 授权失败，请在 EasyColab 重新授权 Drive")
	}
	return nil
}

func rcloneAccess(path string) (string, error) {
	name, access, until, e := readRcloneDrive(path)
	if e != nil {
		return "", e
	}
	if access != "" && time.Now().Before(until.Add(-5*time.Minute)) {
		return access, nil
	}
	if e = rcloneRefresh(path, name); e != nil {
		return "", e
	}
	if _, access, until, e = readRcloneDrive(path); e != nil || access == "" || !time.Now().Before(until) {
		return "", errors.New("rclone 未返回有效 Drive 授权")
	}
	return access, nil
}

func (s *Studio) useRcloneDrivePath(path string) error {
	token, e := rcloneAccess(path)
	if e != nil {
		return e
	}
	var about struct {
		User struct {
			Email string `json:"emailAddress"`
		} `json:"user"`
	}
	if e = driveJSON("GET", "https://www.googleapis.com/drive/v3/about?fields=user(emailAddress)", token, nil, &about); e != nil {
		return e
	}
	if about.User.Email == "" {
		return errors.New("无法确认 EasyColab Drive 授权对应的账号")
	}
	folder := ""
	if prior, pe := loadDriveGrant(); pe == nil && prior.Rclone != "" && strings.EqualFold(prior.Email, about.User.Email) {
		folder = prior.Folder
	}
	_ = s.CancelDriveLogin("")
	if e = persistDrive(driveGrant{Rclone: path, Email: about.User.Email, Folder: folder}); e != nil {
		return e
	}
	s.driveMu.Lock()
	s.driveAttemptEmail = ""
	s.driveMessage = "已使用 EasyColab 的 Drive 授权（drive.file，仅访问本应用创建的文件）"
	s.driveMu.Unlock()
	return nil
}

func (s *Studio) importRcloneDriveBytes(b []byte) error {
	if _, _, _, e := parseRcloneDriveConf(b); e != nil {
		return e
	}
	dir := dataDir()
	if e := os.MkdirAll(dir, 0o700); e != nil {
		return e
	}
	tmp, e := os.CreateTemp(dir, ".drive_rclone.*.conf")
	if e != nil {
		return e
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if e = tmp.Chmod(0o600); e != nil {
		_ = tmp.Close()
		return e
	}
	if _, e = tmp.Write(b); e != nil {
		_ = tmp.Close()
		return e
	}
	if e = tmp.Close(); e != nil {
		return e
	}
	target := filepath.Join(dir, "drive_rclone.conf")
	if e = os.Rename(tmpName, target); e != nil {
		return e
	}
	cleanup = false
	return s.useRcloneDrivePath(target)
}

// UseEasyColabDrive reuses EasyColab's rclone Drive grant: rclone's own client
// already has Drive API enabled, so no Cloud project is needed.
func (s *Studio) UseEasyColabDrive() error {
	return s.useRcloneDrivePath(rcloneDriveConf())
}
