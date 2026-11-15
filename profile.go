package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

const oauthScopes = scope + " openid email profile"
const profileEndpoint = "https://www.googleapis.com/oauth2/v2/userinfo"

type AccountProfile struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}
type cachedProfile struct {
	Binding string         `json:"binding"`
	Fetched time.Time      `json:"fetched"`
	Profile AccountProfile `json:"profile"`
}

func credentialBinding(refresh []byte) string {
	sum := sha256.Sum256(refresh)
	return hex.EncodeToString(sum[:])
}
func fetchProfile(client *http.Client, endpoint, token string) (AccountProfile, error) {
	var p AccountProfile
	req, e := http.NewRequest("GET", endpoint, nil)
	if e != nil {
		return p, errors.New("无法构造账号信息请求")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	safe := *client
	safe.Timeout = 8 * time.Second
	safe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, e := safe.Do(req)
	if e != nil {
		return p, errors.New("账号信息暂时无法读取，请稍后重试")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return p, errors.New("当前授权未提供邮箱访问权限，可在连接设置重新授权；实例功能仍可使用")
	}
	if resp.StatusCode != 200 {
		return p, errors.New("Google 账号信息暂时不可用")
	}
	if e = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&p); e != nil || p.Email == "" {
		return AccountProfile{}, errors.New("Google 未返回账号邮箱")
	}
	return p, nil
}
func cachedProfileFor(data []byte, binding string, now time.Time) (AccountProfile, bool) {
	var c cachedProfile
	if json.Unmarshal(data, &c) != nil || c.Binding != binding || c.Profile.Email == "" || now.Before(c.Fetched) || now.Sub(c.Fetched) > 24*time.Hour {
		return AccountProfile{}, false
	}
	return c.Profile, true
}

// Profile uses the current OAuth credential, never the browser's default account.
// Cache and binding are encrypted by the existing vault, with no extra Keychain item.
func (s *Studio) Profile() (AccountProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	refresh, e := store.Get("refresh-token")
	if e != nil {
		return AccountProfile{}, errors.New("尚未连接 Google 账号")
	}
	binding := credentialBinding(refresh)
	if b, e := store.Get("account-profile"); e == nil {
		if p, ok := cachedProfileFor(b, binding, time.Now()); ok {
			return p, nil
		}
	}
	token, e := access()
	if e != nil {
		return AccountProfile{}, e
	}
	p, e := fetchProfile(httpClient, profileEndpoint, token)
	if e != nil {
		return AccountProfile{}, e
	}
	// A refresh grant can rotate its refresh token; bind to the persisted current one.
	if current, e := store.Get("refresh-token"); e == nil {
		binding = credentialBinding(current)
	}
	b, _ := json.Marshal(cachedProfile{Binding: binding, Fetched: time.Now(), Profile: p})
	if e = store.Set("account-profile", b); e != nil {
		return AccountProfile{}, errors.New("已读取账号信息，但无法保存加密缓存")
	}
	return p, nil
}
