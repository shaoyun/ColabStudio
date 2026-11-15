package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	remoteRedirect     = "https://sdk.cloud.google.com/applicationdefaultauthcode.html"
	webManualRedirect  = "http://127.0.0.1:53682/callback"
	webDriveRedirect   = "http://127.0.0.1:53682/drive-callback"
)

func extractOAuthCode(raw, expectedState, emptyErrMsg string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || len(trimmed) > 8192 || strings.ContainsAny(trimmed, "\r\n") {
		return "", errors.New(emptyErrMsg)
	}
	code := trimmed
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		if strings.ContainsRune(trimmed, ' ') {
			return "", errors.New(emptyErrMsg)
		}
		u, err := url.Parse(trimmed)
		if err != nil {
			return "", errors.New(emptyErrMsg)
		}
		q := u.Query()
		if errParam := strings.TrimSpace(q.Get("error")); errParam != "" {
			return "", errors.New("Google 授权未完成：" + errParam)
		}
		if expectedState != "" && q.Get("state") != expectedState {
			return "", errors.New("授权状态（state）不匹配，请重新发起授权")
		}
		code = strings.TrimSpace(q.Get("code"))
	}
	if code == "" || len(code) > 4096 || strings.ContainsAny(code, "\r\n ") {
		return "", errors.New(emptyErrMsg)
	}
	return code, nil
}

// parseRemoteConfig accepts the Cloud SDK client used by EasyColab/Colab CLI.
// The registered HTTPS redirect belongs to this client, not arbitrary desktop clients.
func parseRemoteConfig(b []byte) (Config, error) {
	var document struct {
		Installed Config `json:"installed"`
	}
	if json.Unmarshal(b, &document) != nil || !strings.HasPrefix(document.Installed.ClientID, "764086051850-") || !strings.HasSuffix(document.Installed.ClientID, ".apps.googleusercontent.com") || document.Installed.ClientSecret == "" {
		return Config{}, errors.New("该文件不是 EasyColab / Colab CLI 使用的 Cloud SDK OAuth 配置。自建 Desktop client 请使用高级设置的本地回调登录。")
	}
	c := document.Installed
	c.Remote = true
	return c, nil
}

func (s *Studio) applyRemoteConfig(c Config) error {
	if e := s.SaveConfig(c.ClientID, c.ClientSecret); e != nil {
		return e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c.ClientSecret = ""
	b, _ := json.Marshal(c)
	return os.WriteFile(configPath(), b, 0600)
}

func (s *Studio) importEasyColabConfigBytes(b []byte) error {
	c, e := parseRemoteConfig(b)
	if e != nil {
		return e
	}
	return s.applyRemoteConfig(c)
}

func (s *Studio) tryImportLocalEasyColabConfig() (bool, error) {
	home, e := os.UserHomeDir()
	if e != nil {
		return false, e
	}
	candidates := []string{filepath.Join(home, ".colab-cli-oauth-config.json")}
	for _, pattern := range []string{filepath.Join(home, "Library/Application Support/EasyColab/venv/lib/python*/site-packages/colab_cli/oauth_config.json"), filepath.Join(home, ".local/share/uv/tools/google-colab-cli/lib/python*/site-packages/colab_cli/oauth_config.json")} {
		matches, _ := filepath.Glob(pattern)
		candidates = append(candidates, matches...)
	}
	for _, path := range candidates {
		b, e := os.ReadFile(path)
		if e != nil {
			continue
		}
		c, e := parseRemoteConfig(b)
		if e == nil {
			if ae := s.applyRemoteConfig(c); ae != nil {
				return false, ae
			}
			return true, nil
		}
	}
	return false, nil
}

// ImportEasyColabConfig imports client configuration only; never reads another app's user tokens.
func (s *Studio) ImportEasyColabConfig() error {
	found, e := s.tryImportLocalEasyColabConfig()
	if e != nil {
		return e
	}
	if found {
		return nil
	}
	paths, e := platformOpenJSONDialog("选择 EasyColab / Colab CLI OAuth 配置", "选择 oauth_config.json；不会读取或导入现有账号 token。", true)
	if e != nil {
		return errors.New("无法打开配置选择窗口")
	}
	if len(paths) == 0 {
		return errors.New("已取消导入；可选择 Colab CLI 的 oauth_config.json，或使用自建 Desktop client")
	}
	b, e := os.ReadFile(paths[0])
	if e != nil {
		return errors.New("无法读取配置文件")
	}
	return s.importEasyColabConfigBytes(b)
}

func (s *Studio) buildRemoteAuthURL(c Config, redirect string) string {
	verifier := random()
	state := random()
	hash := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"client_id":             {c.ClientID},
		"redirect_uri":          {redirect},
		"response_type":         {"code"},
		"scope":                 {oauthScopes},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(hash[:])},
		"code_challenge_method": {"S256"},
		"access_type":           {"offline"},
		"prompt":                {"select_account consent"},
	}
	if c.Remote {
		q.Set("token_usage", "remote")
	}
	s.verifier = verifier
	s.loginRedirect = redirect
	s.loginState = state
	s.remoteConfig = c
	s.remoteUntil = time.Now().Add(10 * time.Minute)
	return "https://accounts.google.com/o/oauth2/v2/auth?" + q.Encode()
}

func (s *Studio) BeginRemoteLogin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, e := loadConfig()
	if e != nil {
		return e
	}
	if !c.Remote {
		return errors.New("请先导入 EasyColab / Colab CLI 配置")
	}
	authURL := s.buildRemoteAuthURL(c, remoteRedirect)
	if e = platformOpenExternal(authURL); e != nil {
		s.verifier = ""
		s.loginRedirect = ""
		s.loginState = ""
		return errors.New("无法打开浏览器，请重试")
	}
	return nil
}

// beginWebLogin prepares a browser-driven PKCE OAuth URL for headless/web server mode.
func (s *Studio) beginWebLogin() (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, e := loadConfig()
	if e != nil {
		return nil, e
	}
	redirect := remoteRedirect
	if !c.Remote {
		redirect = webManualRedirect
	}
	authURL := s.buildRemoteAuthURL(c, redirect)
	return map[string]any{
		"authURL":     authURL,
		"remote":      c.Remote,
		"redirectURI": redirect,
	}, nil
}

func (s *Studio) SubmitRemoteCode(codeInput string) error {
	unlock, guardErr := s.lockAccountChange()
	if guardErr != nil {
		return guardErr
	}
	defer unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.verifier == "" || time.Now().After(s.remoteUntil) {
		return errors.New("请重新点击浏览器授权，生成本次授权码")
	}
	code, e := extractOAuthCode(codeInput, s.loginState, "请输入 Google 授权结果页显示的一次性授权码")
	if e != nil {
		return e
	}
	redirect := s.loginRedirect
	if redirect == "" {
		redirect = remoteRedirect
	}
	c := s.remoteConfig
	t, e := exchange(url.Values{"client_id": {c.ClientID}, "client_secret": {c.ClientSecret}, "redirect_uri": {redirect}, "grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {s.verifier}})
	s.verifier = ""
	s.loginRedirect = ""
	s.loginState = ""
	s.remoteConfig = Config{}
	if e != nil {
		return e
	}
	if t.Refresh == "" {
		return errors.New("Google 未返回离线授权，请重新授权并允许访问")
	}
	return saveToken(t)
}
