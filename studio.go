package main

import (
	"bytes"
	"context"
	"crypto/rand"
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
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const scope = "https://www.googleapis.com/auth/colaboratory"
const apiBase = "https://colaboratory.googleapis.com/v1beta/"

var httpClient = &http.Client{Timeout: 30 * time.Second}
var store = newCredentialVault(vaultPath(), defaultKeyStore())

type Config struct {
	Remote       bool   `json:"remote,omitempty"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}
type Token struct {
	Access  string    `json:"access_token"`
	Refresh string    `json:"refresh_token"`
	Expires int       `json:"expires_in"`
	Until   time.Time `json:"until"`
}
type Spec struct {
	Variant     string `json:"variant"`
	Accelerator string `json:"accelerator"`
	Shape       string `json:"shape"`
}
type Runtime struct {
	DisplayName    string `json:"displayName,omitempty"`
	Backend        string `json:"backend,omitempty"`
	Name           string `json:"name"`
	RuntimeSpec    Spec   `json:"runtimeSpec"`
	Version        string `json:"version"`
	ConnectionInfo struct {
		URL        string `json:"url"`
		ExpireTime string `json:"expireTime"`
	} `json:"connectionInfo"`
}
type Eligible struct {
	Key      Spec `json:"key"`
	Eligible bool `json:"eligible"`
}
type Studio struct {
	svcOnce           sync.Once
	svc               *serviceManager
	driveMu           sync.Mutex
	driveFlow         *driveFlow
	driveMessage      string
	driveAttemptEmail string
	compatible        bool
	mu                sync.Mutex
	login             bool
	verifier          string
	loginRedirect     string
	loginState        string
	remoteConfig      Config
	remoteUntil       time.Time
}

func loadConfig() (Config, error) {
	var c Config
	b, e := os.ReadFile(configPath())
	if e != nil {
		return c, errors.New("请先保存 OAuth Desktop app Client ID")
	}
	e = json.Unmarshal(b, &c)
	if e != nil {
		return c, errors.New("连接设置损坏，请重新保存")
	}
	b, e = store.Get("client-secret")
	if e == nil {
		c.ClientSecret = string(b)
	} else if !errors.Is(e, errSecretNotFound) {
		return c, errors.New("无法读取加密密钥，请检查密钥存储后重试")
	}
	return c, nil
}
func (s *Studio) Status() map[string]any {
	// One-time upgrade from the original three-item store. Prefer the already
	// authorized EasyColab file, never reread three legacy Keychain items.
	if _, e := os.Stat(store.path); os.IsNotExist(e) && os.Getenv("COLABSTUDIO_SMOKE_VAULT") != "1" {
		if home, he := os.UserHomeDir(); he == nil {
			path := filepath.Join(home, ".config/colab-cli/token.json")
			if b, re := os.ReadFile(path); re == nil {
				if prior, pe := parseExistingCredential(b); pe == nil {
					if configBytes, ce := os.ReadFile(configPath()); ce == nil {
						var old Config
						if json.Unmarshal(configBytes, &old) == nil && old.ClientID == prior.ClientID {
							_ = s.ReuseExistingLogin()
						}
					}
				}
			}
		}
	}

	c, _ := loadConfig()
	_, e := store.Get("refresh-token")
	return map[string]any{"clientID": c.ClientID, "connected": e == nil, "remote": c.Remote}
}
func (s *Studio) SaveConfig(id, secret string) error {
	unlock, guardErr := s.lockAccountChange()
	if guardErr != nil {
		return guardErr
	}
	defer unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.verifier = ""
	s.loginRedirect = ""
	s.loginState = ""
	s.remoteConfig = Config{}
	s.compatible = false
	id = strings.TrimSpace(id)
	if !strings.HasSuffix(id, ".apps.googleusercontent.com") {
		return errors.New("请输入 Desktop app 的有效 Client ID")
	}
	if err := store.Set("client-secret", []byte(secret)); err != nil {
		return errors.New("无法写入 Keychain，设置未保存")
	}
	p := configPath()
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		return e
	}
	b, _ := json.Marshal(Config{ClientID: id})
	if e := os.WriteFile(p, b, 0600); e != nil {
		return e
	}
	_ = store.Delete("account-profile")
	_ = store.Delete("refresh-token")
	return store.Delete("access-token")
}
func (s *Studio) Disconnect() error {
	unlock, guardErr := s.lockAccountChange()
	if guardErr != nil {
		return guardErr
	}
	defer unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.verifier = ""
	s.loginRedirect = ""
	s.loginState = ""
	s.remoteConfig = Config{}
	s.compatible = false
	if e := store.Delete("account-profile"); e != nil {
		return e
	}
	if e := store.Delete("refresh-token"); e != nil {
		return e
	}
	return store.Delete("access-token")
}
func random() string {
	b := make([]byte, 32)
	_, e := rand.Read(b)
	if e != nil {
		panic(e)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func exchange(v url.Values) (Token, error) {
	var t Token
	r, e := httpClient.PostForm("https://oauth2.googleapis.com/token", v)
	if e != nil {
		return t, errors.New("无法连接 Google OAuth，请检查网络")
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return t, fmt.Errorf("OAuth 失败（HTTP %d），请检查客户端配置或重新授权", r.StatusCode)
	}
	e = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&t)
	if e != nil || t.Access == "" {
		return t, errors.New("OAuth 返回无效凭据")
	}
	t.Until = time.Now().Add(time.Duration(t.Expires) * time.Second)
	return t, nil
}
func saveToken(t Token) error {
	if t.Refresh != "" {
		if e := store.Set("refresh-token", []byte(t.Refresh)); e != nil {
			return errors.New("无法将授权保存到 Keychain")
		}
	}
	b, _ := json.Marshal(t)
	if e := store.Set("access-token", b); e != nil {
		return errors.New("无法将凭据保存到 Keychain")
	}
	return nil
}
func (s *Studio) Login() error {
	unlock, guardErr := s.lockAccountChange()
	if guardErr != nil {
		return guardErr
	}
	defer unlock()
	s.mu.Lock()
	if s.login {
		s.mu.Unlock()
		return errors.New("授权正在进行，请完成浏览器操作")
	}
	s.login = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.login = false; s.mu.Unlock() }()
	c, e := loadConfig()
	if e != nil {
		return e
	}
	if c.Remote {
		return errors.New("当前使用 EasyColab 配置，请点击浏览器授权并回填授权码")
	}
	ln, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		return errors.New("无法创建本地 OAuth 回调")
	}
	defer ln.Close()
	redirect := "http://" + ln.Addr().String() + "/callback"
	state, verifier := random(), random()
	hash := sha256.Sum256([]byte(verifier))
	result := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if r.URL.Query().Get("state") != state {
			http.Error(w, "授权状态无效", 400)
			return
		}
		code := r.URL.Query().Get("code")
		select {
		case result <- code:
		default:
		}
		fmt.Fprint(w, "已收到授权结果，请返回 ColabStudio。")
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go server.Serve(ln)
	defer server.Close()
	q := url.Values{"client_id": {c.ClientID}, "redirect_uri": {redirect}, "response_type": {"code"}, "scope": {oauthScopes}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "code_challenge_method": {"S256"}, "access_type": {"offline"}, "prompt": {"consent"}}
	if e = platformOpenExternal("https://accounts.google.com/o/oauth2/v2/auth?" + q.Encode()); e != nil {
		return errors.New("无法打开授权浏览器")
	}
	select {
	case code := <-result:
		if code == "" {
			return errors.New("授权已取消")
		}
		t, e := exchange(url.Values{"client_id": {c.ClientID}, "client_secret": {c.ClientSecret}, "redirect_uri": {redirect}, "grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier}})
		if e != nil {
			return e
		}
		return saveToken(t)
	case <-time.After(3 * time.Minute):
		return errors.New("授权超时，请重试")
	}
}
func access() (string, error) {
	b, e := store.Get("access-token")
	var t Token
	if e == nil && json.Unmarshal(b, &t) == nil && time.Now().Before(t.Until.Add(-time.Minute)) {
		return t.Access, nil
	}
	b, e = store.Get("refresh-token")
	if e != nil {
		return "", errors.New("尚未连接，请先授权 Google 账号")
	}
	c, e := loadConfig()
	if e != nil {
		return "", e
	}
	t, e = exchange(url.Values{"client_id": {c.ClientID}, "client_secret": {c.ClientSecret}, "refresh_token": {string(b)}, "grant_type": {"refresh_token"}})
	if e != nil {
		return "", e
	}
	if e = saveToken(t); e != nil {
		return "", e
	}
	return t.Access, nil
}
func request(ctx context.Context, client *http.Client, base, token, method, path string, body any, out any) error {
	var b bytes.Buffer
	if body != nil {
		json.NewEncoder(&b).Encode(body)
	}
	r, e := http.NewRequestWithContext(ctx, method, base+path, &b)
	if e != nil {
		return e
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	resp, e := client.Do(r)
	if e != nil {
		return errors.New("无法连接 Colab API，请检查网络后重试")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var details struct {
			Error struct {
				Details []struct {
					Reason string `json:"reason"`
				} `json:"details"`
			} `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&details)
		for _, d := range details.Error.Details {
			switch d.Reason {
			case "SERVICE_DISABLED":
				return errPublicAPIDisabled
			case "ACCESS_TOKEN_SCOPE_INSUFFICIENT":
				return errors.New("Colab API HTTP 403 / ACCESS_TOKEN_SCOPE_INSUFFICIENT：已有授权范围不足，请重新授予 Colab scope")
			case "CONSUMER_INVALID":
				return errors.New("Colab API HTTP 403 / CONSUMER_INVALID：OAuth 所属项目不可用，请改用有效项目的 OAuth client")
			}
		}
		return fmt.Errorf("Colab API HTTP %d：%s", resp.StatusCode, map[int]string{401: "授权过期，请重新连接", 403: "请启用 Colab API、检查授权 scope 与账号资格", 404: "实例不存在，请刷新", 429: "请求过多或配额不足，请稍后重试", 500: "服务暂时不可用", 503: "服务暂时不可用"}[resp.StatusCode])
	}
	if out == nil {
		return nil
	}
	if e = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); e != nil {
		return errors.New("Colab API 返回格式无效")
	}
	return nil
}
func (s *Studio) call(method, path string, body, out any) error {
	t, e := access()
	if e != nil {
		return e
	}
	return request(context.Background(), httpClient, apiBase, t, method, path, body, out)
}
func (s *Studio) Runtimes() ([]Runtime, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.compatible {
		return s.compatibleList()
	}
	token, e := access()
	if e != nil {
		return nil, e
	}
	rows, compatible, e := fetchRuntimes(httpClient, apiBase, tunnelBase, token)
	if e == nil {
		s.compatible = compatible
	}
	return rows, e
}
func (s *Studio) Specs() ([]Eligible, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.compatible {
		return nil, errors.New("当前使用 EasyColab 兼容接口，可查看、打开和释放已有实例；此接口不提供 runtimeSpecs 资格查询。申请新实例请先使用 Colab 网页，或配置已启用 Colab API 的自建客户端")
	}
	var r struct {
		Specs []Eligible `json:"runtimeSpecs"`
	}
	e := s.call("GET", "runtimespecs", nil, &r)
	if r.Specs == nil {
		r.Specs = []Eligible{}
	}
	return r.Specs, e
}

var runtimeName = regexp.MustCompile(`^runtimes/[a-zA-Z0-9_-]+$`)

func (s *Studio) Release(name string, confirmed bool) error {
	if confirmed && strings.HasPrefix(name, "compat/") {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.compatibleRelease(name)
	}
	if !confirmed || !runtimeName.MatchString(name) {
		return errors.New("请确认要释放的实例")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.call("DELETE", name, nil, nil)
}
func (s *Studio) Create(spec Spec) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.compatible {
		return "", errors.New("兼容接口不提供配置资格查询，请在 Colab 网页申请新实例")
	}
	var list struct {
		Specs []Eligible `json:"runtimeSpecs"`
	}
	if e := s.call("GET", "runtimespecs", nil, &list); e != nil {
		return "", e
	}
	ok := false
	for _, v := range list.Specs {
		if v.Eligible && v.Key == spec {
			ok = true
		}
	}
	if !ok {
		return "", errors.New("该配置当前没有申请资格，请重新查询")
	}
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	h := hex.EncodeToString(b)
	id := h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
	var op struct {
		Name  string `json:"name"`
		Done  bool   `json:"done"`
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if e := s.call("POST", "runtimes?requestId="+id, map[string]any{"runtimeSpec": spec}, &op); e != nil {
		return "", e
	}
	if op.Error != nil {
		return "", fmt.Errorf("申请失败（code %d），可能没有容量或配额", op.Error.Code)
	}
	return op.Name, nil
}
func (s *Studio) runtimeOpenURL(name string) (string, error) {
	if strings.HasPrefix(name, "compat/") {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.compatibleURL(name)
	}
	if !runtimeName.MatchString(name) {
		return "", errors.New("无效实例")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var r Runtime
	if e := s.call("GET", name, nil, &r); e != nil {
		return "", e
	}
	return validateRuntimeURL(r.ConnectionInfo.URL)
}
func (s *Studio) Open(name string) error {
	u, e := s.runtimeOpenURL(name)
	if e != nil {
		return e
	}
	return platformOpenExternal(u)
}
func validateRuntimeURL(address string) (string, error) {
	u, e := url.Parse(address)
	if e != nil || u.Scheme != "https" || !(u.Hostname() == "googleusercontent.com" || strings.HasSuffix(u.Hostname(), ".googleusercontent.com") || u.Hostname() == "colab.research.google.com" || strings.HasSuffix(u.Hostname(), ".r.colab.dev")) {
		return "", errors.New("实例没有可安全打开的 Google HTTPS 地址")
	}
	return u.String(), nil
}
func openRuntimeURL(address string) error {
	u, e := validateRuntimeURL(address)
	if e != nil {
		return e
	}
	return platformOpenExternal(u)
}

// Operation reports completion without resubmitting a potentially billable request.
func (s *Studio) Operation(name string) (map[string]any, error) {
	if !regexp.MustCompile(`^operations/[a-zA-Z0-9_/-]+$`).MatchString(name) || strings.Contains(name, "..") {
		return nil, errors.New("无效 Operation 名称")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, e := access()
	if e != nil {
		return nil, e
	}
	var op struct {
		Done  bool `json:"done"`
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	e = request(context.Background(), httpClient, "https://colaboratory.googleapis.com/v1/", t, "GET", name, nil, &op)
	if e != nil {
		return nil, e
	}
	if op.Error != nil {
		return nil, fmt.Errorf("实例申请失败（code %d），请检查配额、资格或实时容量", op.Error.Code)
	}
	return map[string]any{"done": op.Done}, nil
}
