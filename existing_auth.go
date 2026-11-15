package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type existingCredential struct {
	ClientID     string          `json:"client_id"`
	ClientSecret string          `json:"client_secret"`
	Refresh      string          `json:"refresh_token"`
	Scopes       json.RawMessage `json:"scopes"`
	TokenURI     string          `json:"token_uri"`
}

func parseExistingCredential(b []byte) (existingCredential, error) {
	var c existingCredential
	if json.Unmarshal(b, &c) != nil {
		return c, errors.New("已有登录文件格式无效，请重新授权")
	}
	if c.Refresh == "" || c.ClientSecret == "" || !strings.HasSuffix(c.ClientID, ".apps.googleusercontent.com") {
		return existingCredential{}, errors.New("已有配置不包含完整 refresh token 和匹配客户端，无法免授权")
	}
	if c.TokenURI != "" && c.TokenURI != "https://oauth2.googleapis.com/token" && c.TokenURI != "https://accounts.google.com/o/oauth2/token" {
		return existingCredential{}, errors.New("已有凭据使用未知 OAuth endpoint，未导入")
	}
	var scopes []string
	if json.Unmarshal(c.Scopes, &scopes) != nil {
		var text string
		if json.Unmarshal(c.Scopes, &text) == nil {
			scopes = strings.Fields(text)
		}
	}
	for _, v := range scopes {
		if v == scope {
			return c, nil
		}
	}
	return existingCredential{}, errors.New("已有登录没有完整 Colab scope，请重新授权")
}

func readExistingCredential() (existingCredential, error) {
	home, e := os.UserHomeDir()
	if e != nil {
		return existingCredential{}, errors.New("无法定位本机用户目录")
	}
	path := filepath.Join(home, ".config/colab-cli/token.json")
	f, e := os.Open(path)
	if e == nil {
		defer f.Close()
		b, e := io.ReadAll(io.LimitReader(f, 64<<10))
		if e != nil {
			return existingCredential{}, errors.New("无法读取 EasyColab 登录文件")
		}
		return parseExistingCredential(b)
	}
	if !os.IsNotExist(e) {
		return existingCredential{}, errors.New("无法访问 EasyColab 登录文件，请检查文件权限")
	}
	b, e := readSystemColabCLISecret()
	if e != nil {
		return existingCredential{}, errors.New("未找到可读取的 EasyColab / Colab CLI 登录，请使用浏览器授权")
	}
	return parseExistingCredential(b)
}

func (s *Studio) applyExistingCredentialLocked(c existingCredential) error {
	t, e := exchange(url.Values{"client_id": {c.ClientID}, "client_secret": {c.ClientSecret}, "refresh_token": {c.Refresh}, "grant_type": {"refresh_token"}})
	if e != nil {
		return errors.New("已有登录无法刷新：可能已撤销、过期或网络不可用。请检查网络，或使用浏览器重新授权")
	}
	if t.Refresh == "" {
		t.Refresh = c.Refresh
	}
	cfg := Config{ClientID: c.ClientID, Remote: strings.HasPrefix(c.ClientID, "764086051850-")}
	if e = store.Set("client-secret", []byte(c.ClientSecret)); e != nil {
		return errors.New("无法将已有客户端配置写入加密密钥库")
	}
	p := configPath()
	if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		return errors.New("无法创建本机配置目录")
	}
	b, _ := json.Marshal(cfg)
	if e = os.WriteFile(p, b, 0600); e != nil {
		return errors.New("无法保存本机配置")
	}
	if e = saveToken(t); e != nil {
		return e
	}
	s.compatible = false
	s.verifier = ""
	s.loginRedirect = ""
	s.loginState = ""
	s.remoteConfig = Config{}
	return nil
}

func (s *Studio) importExistingCredentialBytes(b []byte) error {
	c, e := parseExistingCredential(b)
	if e != nil {
		return e
	}
	unlock, guardErr := s.lockAccountChange()
	if guardErr != nil {
		return guardErr
	}
	defer unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyExistingCredentialLocked(c)
}

// ReuseExistingLogin validates the refresh grant before local persistence.
// API access is checked separately by the UI refresh so a 403 is not reported as a login failure.
func (s *Studio) ReuseExistingLogin() error {
	unlock, guardErr := s.lockAccountChange()
	if guardErr != nil {
		return guardErr
	}
	defer unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	c, e := readExistingCredential()
	if e != nil {
		return e
	}
	return s.applyExistingCredentialLocked(c)
}
