package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func mockJSONResponse(status int, body any) *http.Response {
	b, _ := json.Marshal(body)
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(b)),
	}
}

func setupIsolatedTestEnv(t *testing.T, token string, rt roundTripFunc) (*webServer, *Studio, *serviceManager, string) {
	t.Helper()
	dir := t.TempDir()
	oldStore := store
	oldTransport := httpClient.Transport
	t.Setenv("COLABSTUDIO_VAULT_KEY", "")

	if err := setDataDir(dir); err != nil {
		t.Fatalf("setDataDir: %v", err)
	}
	setWebServerMode(true)
	store = newCredentialVault(filepath.Join(dir, "credentials.enc"), newFileKeyStore(dir))

	s := &Studio{}
	m := &serviceManager{
		studio: s,
		path:   filepath.Join(dir, "services.json"),
	}
	s.svc = m
	s.svcOnce.Do(func() {})

	if rt != nil {
		httpClient.Transport = rt
	}
	srv, err := newWebServer(s, token)
	if err != nil {
		t.Fatalf("newWebServer: %v", err)
	}
	srv.pollInterval = 10 * time.Millisecond

	t.Cleanup(func() {
		// Wait for any asynchronous op started by handlers before restoring globals.
		m.op.Lock()
		m.op.Unlock()
		httpClient.Transport = oldTransport
		store = oldStore
		_ = setDataDir("")
		setWebServerMode(false)
	})

	return srv, s, m, dir
}

func seedLoggedInAccount(t *testing.T, s *Studio, remote bool) {
	t.Helper()
	cfg := Config{
		ClientID:     "123456-testclient.apps.googleusercontent.com",
		ClientSecret: "test-client-secret",
		Remote:       remote,
	}
	if remote {
		if err := s.applyRemoteConfig(cfg); err != nil {
			t.Fatalf("applyRemoteConfig: %v", err)
		}
	} else {
		if err := s.SaveConfig(cfg.ClientID, cfg.ClientSecret); err != nil {
			t.Fatalf("SaveConfig: %v", err)
		}
	}
	if err := saveToken(Token{
		Access:  "mock-access-token",
		Refresh: "mock-refresh-token",
		Until:   time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("saveToken: %v", err)
	}
}

func TestFileKeyStoreAndVaultEnv(t *testing.T) {
	dir := t.TempDir()
	ks := newFileKeyStore(dir)

	rawKey := make([]byte, 32)
	for i := range rawKey {
		rawKey[i] = byte(i + 1)
	}
	hexKey := make([]byte, 64)
	for i, b := range rawKey {
		const digits = "0123456789abcdef"
		hexKey[i*2] = digits[b>>4]
		hexKey[i*2+1] = digits[b&0x0f]
	}

	t.Run("hex env key", func(t *testing.T) {
		t.Setenv("COLABSTUDIO_VAULT_KEY", string(hexKey))
		vaultPath := filepath.Join(t.TempDir(), "cred.enc")
		v1 := newCredentialVault(vaultPath, ks)
		if err := v1.Set("k1", []byte("secret-one")); err != nil {
			t.Fatalf("Set with hex env key: %v", err)
		}
		v2 := newCredentialVault(vaultPath, ks)
		got, err := v2.Get("k1")
		if err != nil || string(got) != "secret-one" {
			t.Fatalf("Get with hex env key = %q, %v", got, err)
		}
	})

	t.Run("base64 env key and mismatch", func(t *testing.T) {
		b64Key := base64.StdEncoding.EncodeToString(rawKey)
		t.Setenv("COLABSTUDIO_VAULT_KEY", b64Key)
		vaultPath := filepath.Join(t.TempDir(), "cred.enc")
		v1 := newCredentialVault(vaultPath, ks)
		if err := v1.Set("k2", []byte("secret-two")); err != nil {
			t.Fatalf("Set with base64 env key: %v", err)
		}
		wrongKey := make([]byte, 32)
		wrongKey[0] = 0xff
		t.Setenv("COLABSTUDIO_VAULT_KEY", base64.StdEncoding.EncodeToString(wrongKey))
		vBad := newCredentialVault(vaultPath, ks)
		if _, err := vBad.Get("k2"); err == nil {
			t.Fatalf("expected error reading vault with mismatched key")
		}
	})

	t.Run("invalid env key rejected", func(t *testing.T) {
		t.Setenv("COLABSTUDIO_VAULT_KEY", "not-valid-key")
		if _, err := ks.PresetKey(); err == nil {
			t.Fatalf("expected error for invalid COLABSTUDIO_VAULT_KEY")
		}
	})

	t.Run("generated vault.key file 0600", func(t *testing.T) {
		t.Setenv("COLABSTUDIO_VAULT_KEY", "")
		genDir := t.TempDir()
		genKS := newFileKeyStore(genDir)
		vaultPath := filepath.Join(genDir, "cred.enc")
		v1 := newCredentialVault(vaultPath, genKS)
		if err := v1.Set("token", []byte("alpha-beta")); err != nil {
			t.Fatalf("Set without env key: %v", err)
		}
		info, err := os.Stat(filepath.Join(genDir, "vault.key"))
		if err != nil {
			t.Fatalf("stat vault.key: %v", err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("vault.key perm = %#o, want 0600", info.Mode().Perm())
		}
		v2 := newCredentialVault(vaultPath, genKS)
		got, err := v2.Get("token")
		if err != nil || string(got) != "alpha-beta" {
			t.Fatalf("reload token = %q, %v", got, err)
		}
	})
}

func TestResolveServerToken(t *testing.T) {
	if _, _, _, err := resolveServerToken("short", t.TempDir(), false); err == nil {
		t.Fatalf("expected error for short token")
	}
	if _, _, _, err := resolveServerToken("token with spaces 1234", t.TempDir(), false); err == nil {
		t.Fatalf("expected error for whitespace in token")
	}

	dir := t.TempDir()
	tok1, src1, gen1, err := resolveServerToken("", dir, false)
	if err != nil {
		t.Fatalf("resolveServerToken first: %v", err)
	}
	if !gen1 || !strings.HasPrefix(tok1, "cs_") || len(tok1) < 32 {
		t.Fatalf("unexpected generated token: gen=%v tok=%q", gen1, tok1)
	}
	info, err := os.Stat(src1)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("api.token stat err=%v mode=%v", err, info)
	}

	tok2, _, gen2, err := resolveServerToken("", dir, false)
	if err != nil || gen2 || tok2 != tok1 {
		t.Fatalf("expected persisted token reuse: tok2=%q gen2=%v err=%v", tok2, gen2, err)
	}

	tok3, _, gen3, err := resolveServerToken("", dir, true)
	if err != nil || !gen3 || tok3 == tok1 {
		t.Fatalf("expected rotated token: tok3=%q gen3=%v err=%v", tok3, gen3, err)
	}
}

func TestWebServerAuthCSRFAndRateLimit(t *testing.T) {
	const testToken = "test-master-token-987654"
	srv, _, _, _ := setupIsolatedTestEnv(t, testToken, nil)
	h := srv.Handler()

	// 1. Health check is unauthenticated.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d, want 200", rec.Code)
	}

	// 2. Unauthenticated status denied.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status = %d, want 401", rec.Code)
	}

	// 3. Query parameter tokens rejected even if value matches.
	for _, qp := range []string{"token", "api_token", "access_token"} {
		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodGet, "/api/v1/status?"+qp+"="+testToken, nil)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("query param %s status = %d, want 401", qp, rec.Code)
		}
	}

	// 4. Bearer and X-API-Token headers accepted on GET and mutating POST without CSRF header.
	for _, hdr := range [][2]string{
		{"Authorization", "Bearer " + testToken},
		{"X-API-Token", testToken},
	} {
		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodPost, "/api/rpc/Status", strings.NewReader(`{"args":[]}`))
		req.Header.Set(hdr[0], hdr[1])
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("header %s POST /api/rpc/Status = %d (%s)", hdr[0], rec.Code, rec.Body.String())
		}
	}

	// 5. Login issues HttpOnly SameSite=Strict cookie; Secure mirrors HTTPS.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "http://example.com/api/auth/login", strings.NewReader(`{"token":"`+testToken+`"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d: %s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookieName {
		t.Fatalf("unexpected cookies: %+v", cookies)
	}
	sessCookie := cookies[0]
	if !sessCookie.HttpOnly || sessCookie.SameSite != http.SameSiteStrictMode || sessCookie.Secure {
		t.Fatalf("unexpected plain-HTTP cookie flags: %+v", sessCookie)
	}

	recHTTPS := httptest.NewRecorder()
	reqHTTPS := httptest.NewRequest(http.MethodPost, "http://example.com/api/auth/login", strings.NewReader(`{"token":"`+testToken+`"}`))
	reqHTTPS.Header.Set("X-Forwarded-Proto", "https")
	h.ServeHTTP(recHTTPS, reqHTTPS)
	if !recHTTPS.Result().Cookies()[0].Secure {
		t.Fatalf("expected Secure cookie when X-Forwarded-Proto is https")
	}

	// 6. Cookie-authenticated GET works; POST without CSRF rejected 403; mismatched Origin rejected 403; valid passes.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "http://example.com/api/v1/status", nil)
	req.AddCookie(sessCookie)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cookie GET status = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "http://example.com/api/rpc/Status", strings.NewReader(`{"args":[]}`))
	req.AddCookie(sessCookie)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cookie POST missing CSRF status = %d, want 403", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "http://example.com/api/rpc/Status", strings.NewReader(`{"args":[]}`))
	req.Host = "example.com"
	req.Header.Set("X-Requested-With", "ColabStudio")
	req.Header.Set("Origin", "https://evil.example")
	req.AddCookie(sessCookie)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cookie POST wrong Origin status = %d, want 403", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "http://example.com/api/rpc/Status", strings.NewReader(`{"args":[]}`))
	req.Host = "example.com"
	req.Header.Set("X-Requested-With", "ColabStudio")
	req.Header.Set("Origin", "http://example.com")
	req.AddCookie(sessCookie)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cookie POST valid CSRF status = %d: %s", rec.Code, rec.Body.String())
	}

	// 7. Old cookie rejected once server token changes.
	srvRotated, err := newWebServer(srv.studio, "rotated-new-token-123456")
	if err != nil {
		t.Fatalf("newWebServer rotated: %v", err)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "http://example.com/api/v1/status", nil)
	req.AddCookie(sessCookie)
	srvRotated.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("old cookie on rotated server = %d, want 401", rec.Code)
	}

	// 8. Rate limiter triggers 429 on 13th failure from same IP while another IP stays allowed.
	for i := 0; i < authFailureLimit; i++ {
		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
		req.RemoteAddr = "198.51.100.10:1234"
		req.Header.Set("Authorization", "Bearer wrong-token")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want 401", i+1, rec.Code)
		}
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req.RemoteAddr = "198.51.100.10:5678"
	req.Header.Set("Authorization", "Bearer "+testToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "60" {
		t.Fatalf("13th attempt status = %d retry-after=%q, want 429/'60'", rec.Code, rec.Header().Get("Retry-After"))
	}

	// Unauthenticated GET /api/auth/session does not increment limiter and returns authenticated:false.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	req.RemoteAddr = "198.51.100.99:9999"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"authenticated":false`) {
		t.Fatalf("session unauth = %d %s", rec.Code, rec.Body.String())
	}
	// Clean IP still works with Bearer token.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req.RemoteAddr = "198.51.100.99:9999"
	req.Header.Set("Authorization", "Bearer "+testToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("different IP status = %d, want 200", rec.Code)
	}
}

func TestWebServerCUAndStatus(t *testing.T) {
	const testToken = "test-cu-token-123456"
	srv, s, m, _ := setupIsolatedTestEnv(t, testToken, func(req *http.Request) (*http.Response, error) {
		u := req.URL.String()
		switch {
		case strings.Contains(u, "colaboratory.googleapis.com/v1beta/runtimes"):
			return mockJSONResponse(http.StatusOK, map[string]any{
				"runtimes": []map[string]any{
					{
						"name": "runtimes/m-s-cu-demo",
						"runtimeSpec": map[string]any{
							"variant":     "VARIANT_GPU",
							"accelerator": "L4",
							"shape":       "SHAPE_STANDARD",
						},
					},
				},
			}), nil
		case strings.Contains(u, "/tun/m/ccu-info"):
			return mockJSONResponse(http.StatusOK, map[string]any{
				"currentBalance":        42.5,
				"consumptionRateHourly": 2.5,
				"assignmentsCount":      1,
			}), nil
		case strings.Contains(u, "/tun/m/assignments"):
			return mockJSONResponse(http.StatusOK, map[string]any{
				"assignments": []map[string]any{
					{
						"endpoint":    "m-s-cu-demo",
						"accelerator": "L4",
						"variant":     1,
					},
				},
			}), nil
		default:
			return mockJSONResponse(http.StatusNotFound, map[string]string{"error": "unexpected URL: " + u}), nil
		}
	})
	seedLoggedInAccount(t, s, true)
	if err := m.put(ServiceRecord{
		Accelerator: "L4",
		Runtime:     "runtimes/m-s-cu-demo",
		State:       "online",
		URL:         "https://demo.trycloudflare.com",
	}); err != nil {
		t.Fatalf("m.put: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cu", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/cu = %d: %s", rec.Code, rec.Body.String())
	}
	var cuResp struct {
		OK                    bool     `json:"ok"`
		CurrentBalance        *float64 `json:"currentBalance"`
		ConsumptionRateHourly *float64 `json:"consumptionRateHourly"`
		AssignmentsCount      *int     `json:"assignmentsCount"`
		RemainingHours        *float64 `json:"estimatedRemainingHours"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cuResp); err != nil {
		t.Fatalf("unmarshal cuResp: %v", err)
	}
	if !cuResp.OK || cuResp.CurrentBalance == nil || *cuResp.CurrentBalance != 42.5 || cuResp.RemainingHours == nil || *cuResp.RemainingHours != 17 {
		t.Fatalf("unexpected CU payload: %+v", cuResp)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/status?full=1", nil)
	req.Header.Set("X-API-Token", testToken)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/status?full=1 = %d: %s", rec.Code, rec.Body.String())
	}
	var statusResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &statusResp); err != nil {
		t.Fatalf("unmarshal statusResp: %v", err)
	}
	if statusResp["runtimes"] == nil || statusResp["usage"] == nil {
		t.Fatalf("expected runtimes and usage in full status: %v", statusResp)
	}
}

func TestWebServerToyboxStartStopAndRuntimesRelease(t *testing.T) {
	const testToken = "test-control-token-123456"
	var mu sync.Mutex
	var unassigned []string
	var deleted []string
	runtimesPayload := []map[string]any{
		{
			"name":        "runtimes/m-s-managed",
			"runtimeSpec": map[string]any{"variant": "VARIANT_GPU", "accelerator": "L4"},
		},
		{
			"name":        "runtimes/m-s-spare",
			"runtimeSpec": map[string]any{"variant": "VARIANT_GPU", "accelerator": "T4"},
		},
	}

	srv, s, m, _ := setupIsolatedTestEnv(t, testToken, func(req *http.Request) (*http.Response, error) {
		u := req.URL.String()
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.Contains(u, "/oauth2/v2/userinfo"):
			return mockJSONResponse(http.StatusOK, map[string]string{
				"email": "tester@example.com",
				"name":  "Test User",
			}), nil
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/v1beta/runtimes"):
			return mockJSONResponse(http.StatusOK, map[string]any{"runtimes": runtimesPayload}), nil
		case strings.Contains(u, "/tun/m/unassign/"):
			parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/tun/m/unassign/"), "/")
			unassigned = append(unassigned, parts[0])
			return mockJSONResponse(http.StatusOK, map[string]any{"ok": true}), nil
		case req.Method == http.MethodDelete && strings.Contains(u, "colaboratory.googleapis.com"):
			deleted = append(deleted, req.URL.Path)
			return mockJSONResponse(http.StatusOK, map[string]any{"done": true}), nil
		default:
			return mockJSONResponse(http.StatusOK, map[string]any{}), nil
		}
	})
	seedLoggedInAccount(t, s, true)
	h := srv.Handler()

	// 1. Idempotent start when already online.
	if err := m.put(ServiceRecord{
		Accelerator: "L4",
		Runtime:     "runtimes/m-s-managed",
		State:       "online",
		URL:         "https://toybox-ready.trycloudflare.com",
		Account:     "tester@example.com",
		Binding:     accountBinding("tester@example.com"),
	}); err != nil {
		t.Fatalf("m.put online: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/toybox/start", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"alreadyRunning":true`) {
		t.Fatalf("idempotent start = %d: %s", rec.Code, rec.Body.String())
	}

	// 2. Runtime release protection: cannot release managed Toybox runtime without force:true.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/runtimes/release", strings.NewReader(`{"name":"runtimes/m-s-managed"}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("protected release status = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}

	// 3. all:true skips managed Toybox runtime cleanly and releases non-managed runtime.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/runtimes/release", strings.NewReader(`{"all":true}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("release all status = %d: %s", rec.Code, rec.Body.String())
	}
	var relAllResp struct {
		OK       bool                `json:"ok"`
		Released []string            `json:"released"`
		Skipped  []map[string]string `json:"skipped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &relAllResp); err != nil {
		t.Fatalf("unmarshal relAllResp: %v", err)
	}
	if !relAllResp.OK || len(relAllResp.Released) != 1 || relAllResp.Released[0] != "runtimes/m-s-spare" || len(relAllResp.Skipped) != 1 || relAllResp.Skipped[0]["name"] != "runtimes/m-s-managed" {
		t.Fatalf("unexpected release all response: %+v", relAllResp)
	}

	// 4. Force stop Toybox releases managed runtime cleanly and resets state to stopped.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/toybox/stop", strings.NewReader(`{"force":true}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"forced":true`) {
		t.Fatalf("force stop status = %d: %s", rec.Code, rec.Body.String())
	}
	if st := s.ServiceStatus(); st.State != "stopped" || st.Runtime != "" {
		t.Fatalf("expected stopped & empty runtime after force stop, got state=%q runtime=%q", st.State, st.Runtime)
	}

	// 5. Idempotent stop when already stopped.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/toybox/stop", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"alreadyStopped":true`) {
		t.Fatalf("idempotent stop = %d: %s", rec.Code, rec.Body.String())
	}

	// 6. Wait-mode start polling when already transitioning into starting -> online.
	if err := m.put(ServiceRecord{
		Accelerator: "L4",
		Runtime:     "runtimes/m-s-booting",
		State:       "installing",
		Account:     "tester@example.com",
		Binding:     accountBinding("tester@example.com"),
	}); err != nil {
		t.Fatalf("m.put booting: %v", err)
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = m.put(ServiceRecord{
			Accelerator: "L4",
			Runtime:     "runtimes/m-s-booting",
			State:       "online",
			URL:         "https://booted.trycloudflare.com",
			Account:     "tester@example.com",
			Binding:     accountBinding("tester@example.com"),
		})
	}()
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/toybox/start", strings.NewReader(`{"wait":true,"timeoutSeconds":5}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "https://booted.trycloudflare.com") {
		t.Fatalf("wait start = %d: %s", rec.Code, rec.Body.String())
	}

	// 7. Logs endpoint surfaces Studio.ToyboxLogs validation cleanly.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/toybox/logs?kind=unknown", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid logs kind status = %d, want 422", rec.Code)
	}
}

func TestWebRPCAndHeadlessOAuthAndRclone(t *testing.T) {
	const testToken = "test-rpc-oauth-token-12345"
	srv, s, _, dir := setupIsolatedTestEnv(t, testToken, func(req *http.Request) (*http.Response, error) {
		u := req.URL.String()
		switch {
		case strings.Contains(u, "oauth2.googleapis.com/token"):
			return mockJSONResponse(http.StatusOK, map[string]any{
				"access_token":  "exchanged-access",
				"refresh_token": "exchanged-refresh",
				"expires_in":    3600,
			}), nil
		case strings.Contains(u, "www.googleapis.com/drive/v3/about"):
			return mockJSONResponse(http.StatusOK, map[string]any{
				"user": map[string]string{"emailAddress": "tester@example.com"},
			}), nil
		case strings.Contains(u, "www.googleapis.com/oauth2/v2/userinfo"):
			return mockJSONResponse(http.StatusOK, map[string]string{
				"email": "tester@example.com",
				"name":  "Test User",
			}), nil
		default:
			return mockJSONResponse(http.StatusNotFound, map[string]string{"error": u}), nil
		}
	})
	if err := s.SaveConfig("123456-custom.apps.googleusercontent.com", "custom-secret"); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	h := srv.Handler()

	// Unknown RPC -> 404.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/rpc/NoSuchMethod", strings.NewReader(`{"args":[]}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown rpc status = %d, want 404", rec.Code)
	}

	// Bad arg count -> 400.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/rpc/Release", strings.NewReader(`{"args":["runtimes/x"]}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad arg count status = %d, want 400", rec.Code)
	}

	// BeginWebLogin for custom Desktop client generates manual redirect URI.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/rpc/BeginWebLogin", strings.NewReader(`{"args":[]}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("BeginWebLogin = %d: %s", rec.Code, rec.Body.String())
	}
	var loginResp struct {
		OK     bool `json:"ok"`
		Result struct {
			AuthURL     string `json:"authURL"`
			Remote      bool   `json:"remote"`
			RedirectURI string `json:"redirectURI"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &loginResp); err != nil {
		t.Fatalf("unmarshal BeginWebLogin: %v", err)
	}
	parsedAuthURL, err := url.Parse(loginResp.Result.AuthURL)
	if err != nil {
		t.Fatalf("parse authURL: %v", err)
	}
	state := parsedAuthURL.Query().Get("state")
	if loginResp.Result.Remote || loginResp.Result.RedirectURI != webManualRedirect || state == "" {
		t.Fatalf("unexpected BeginWebLogin result: %+v", loginResp.Result)
	}

	// State mismatch rejected.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/rpc/SubmitRemoteCode", strings.NewReader(`{"args":["http://127.0.0.1:53682/oauth2callback?code=my-code&state=wrong-state"]}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("wrong state status = %d, want 422", rec.Code)
	}

	// Matching callback URL succeeds and exchanges code cleanly.
	callbackURL := "http://127.0.0.1:53682/oauth2callback?code=auth-code-xyz&state=" + url.QueryEscape(state)
	bodyBytes, _ := json.Marshal(map[string]any{"args": []string{callbackURL}})
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/rpc/SubmitRemoteCode", bytes.NewReader(bodyBytes))
	req.Header.Set("Authorization", "Bearer "+testToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("SubmitRemoteCode valid = %d: %s", rec.Code, rec.Body.String())
	}

	// BeginWebDriveLogin returns desktopCallback URL and DriveStatus does not leak secret verifier/state.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/rpc/BeginWebDriveLogin", strings.NewReader(`{"args":["attempt_01"]}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("BeginWebDriveLogin = %d: %s", rec.Code, rec.Body.String())
	}
	var driveResp struct {
		Result struct {
			AuthURL         string `json:"authURL"`
			DesktopCallback bool   `json:"desktopCallback"`
		} `json:"result"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &driveResp)
	if !driveResp.Result.DesktopCallback || !strings.Contains(driveResp.Result.AuthURL, url.QueryEscape(webDriveRedirect)) {
		t.Fatalf("unexpected BeginWebDriveLogin result: %+v", driveResp.Result)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/rpc/DriveStatus", strings.NewReader(`{"args":[]}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	h.ServeHTTP(rec, req)
	driveBody := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(driveBody, `"authURL":"https://accounts.google.com/o/oauth2/v2/auth`) || strings.Contains(driveBody, "test-client-secret") || strings.Contains(driveBody, "verifier") || strings.Contains(driveBody, "refresh_token") {
		t.Fatalf("DriveStatus leaked secrets or failed: %d %s", rec.Code, driveBody)
	}

	// ImportRcloneDriveConf rejects invalid multi-section / broad-scope config and accepts valid single drive.file conf.
	if err := s.importRcloneDriveBytes([]byte("[one]\ntype = drive\nscope = drive\n")); err == nil {
		t.Fatalf("expected invalid rclone conf to be rejected")
	}
	expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	validConf := "[easycolab_drive]\ntype = drive\nscope = drive.file\ntoken = {\"access_token\":\"fresh-drive-token\",\"refresh_token\":\"r1\",\"expiry\":\"" + expiry + "\"}\n"
	confRPC, _ := json.Marshal(map[string]any{"args": []string{validConf}})
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/rpc/ImportRcloneDriveConf", bytes.NewReader(confRPC))
	req.Header.Set("Authorization", "Bearer "+testToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ImportRcloneDriveConf = %d: %s", rec.Code, rec.Body.String())
	}
	confStat, err := os.Stat(filepath.Join(dir, "drive_rclone.conf"))
	if err != nil || confStat.Mode().Perm() != 0o600 {
		t.Fatalf("drive_rclone.conf stat err=%v mode=%v", err, confStat)
	}
}

func TestComposeWebPage(t *testing.T) {
	srv, _, _, _ := setupIsolatedTestEnv(t, "test-compose-token-12345", nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, needle := range []string{
		"connect-src 'self'",
		"window.mygo",
		"webAuthDialog",
		"data:image/png;base64,",
	} {
		if !strings.Contains(body, needle) {
			t.Fatalf("composed HTML missing %q", needle)
		}
	}
	for _, leftover := range []string{"/*__APP_CSS__*/", "/*__APP_JS__*/", "__APP_ICON__"} {
		if strings.Contains(body, leftover) {
			t.Fatalf("composed HTML still has placeholder %q", leftover)
		}
	}
}
