package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const testSampleTunnelToken = "eyJhIjoiMDEyMzQ1Njc4OWFiY2RlZiIsInQiOiIxMTExMTExMS0yMjIyLTMzMzMtNDQ0NC01NTU1NTU1NTU1NTUiLCJzIjoiYWFhYmJiY2NjZGRkZWVlZmZmIn0="

func TestTunnelNormalizationAndValidation(t *testing.T) {
	urlCases := []struct {
		raw  string
		want string
		ok   bool
	}{
		{"https://toybox.example.com", "https://toybox.example.com", true},
		{"  HTTPS://ToyBox.Example.COM/ ", "https://toybox.example.com", true},
		{"sub.toybox.example.com", "https://sub.toybox.example.com", true},
		{"", "", false},
		{"http://toybox.example.com", "", false},
		{"https://toybox.example.com:8443", "", false},
		{"https://toybox.example.com/path", "", false},
		{"https://toybox.example.com?token=1", "", false},
		{"https://user@toybox.example.com", "", false},
		{"https://127.0.0.1", "", false},
		{"https://localhost", "", false},
		{"https://demo.trycloudflare.com", "", false},
		{"https://trycloudflare.com", "", false},
		{"https://singlelabel", "", false},
	}
	for _, tc := range urlCases {
		got, err := normalizeTunnelURL(tc.raw)
		if tc.ok && (err != nil || got != tc.want) {
			t.Fatalf("normalizeTunnelURL(%q) = (%q, %v), want (%q, nil)", tc.raw, got, err, tc.want)
		}
		if !tc.ok && err == nil {
			t.Fatalf("normalizeTunnelURL(%q) succeeded with %q, want error", tc.raw, got)
		}
	}

	tokCases := []struct {
		raw  string
		want string
		ok   bool
	}{
		{testSampleTunnelToken, testSampleTunnelToken, true},
		{"sudo cloudflared service install " + testSampleTunnelToken, testSampleTunnelToken, true},
		{"cloudflared tunnel run --token '" + testSampleTunnelToken + "'", testSampleTunnelToken, true},
		{"TUNNEL_TOKEN=" + testSampleTunnelToken, testSampleTunnelToken, true},
		{"", "", false},
		{"too-short", "", false},
		{"https://toybox.example.com", "", false},
		{"bad token value!@#", "", false},
	}
	for _, tc := range tokCases {
		got, err := normalizeTunnelToken(tc.raw)
		if tc.ok && (err != nil || got != tc.want) {
			t.Fatalf("normalizeTunnelToken(%q) = (%q, %v), want (%q, nil)", tc.raw, got, err, tc.want)
		}
		if !tc.ok && err == nil {
			t.Fatalf("normalizeTunnelToken(%q) succeeded with %q, want error", tc.raw, got)
		}
	}

	if _, err := urlForToyboxWithCustom("https://quick-1.trycloudflare.com", ""); err != nil {
		t.Fatalf("expected quick tunnel allowed: %v", err)
	}
	if got, err := urlForToyboxWithCustom("https://toybox.example.com", "https://toybox.example.com/"); err != nil || got != "https://toybox.example.com" {
		t.Fatalf("expected custom tunnel allowed, got (%q, %v)", got, err)
	}
	for _, bad := range []string{"https://other.example.com", "https://toybox.example.com/v1", "https://toybox.example.com:443", "http://toybox.example.com"} {
		if _, err := urlForToyboxWithCustom(bad, "https://toybox.example.com"); err == nil {
			t.Fatalf("urlForToyboxWithCustom(%q) unexpectedly succeeded", bad)
		}
	}
}

func TestNamedTunnelPreConfigureDeployAndFallback(t *testing.T) {
	oldRunCode := runCode
	t.Cleanup(func() { runCode = oldRunCode })
	runCode = func(p runtimeProxy, kernel, code string, untilBusy bool) error { return nil }
	oldKeepAlive := ensureKeepAlive
	t.Cleanup(func() { ensureKeepAlive = oldKeepAlive })
	ensureKeepAlive = func(p runtimeProxy, root string) error { return nil }

	uploaded := map[string][]byte{}
	m := wakeFixture(t, func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, "assignments"):
			return wakeResponse(`{"assignments":[{"endpoint":"owned","runtimeProxyInfo":{"url":"https://owned.r.colab.dev","token":"proxy-secret"}}]}`), nil
		case strings.HasSuffix(req.URL.Path, "api/kernels") && req.Method == "POST":
			return wakeResponse(`{"id":"11111111-1111-1111-1111-111111111111"}`), nil
		case strings.Contains(req.URL.Path, "api/kernels/") && req.Method == "DELETE":
			return wakeResponse(`{}`), nil
		case strings.HasSuffix(req.URL.Path, "api/contents/content/.colabstudio/owned") && req.Method == "PUT":
			return wakeResponse(`{}`), nil
		case strings.HasSuffix(req.URL.Path, "api/contents/content/.colabstudio/owned") && req.Method == "GET":
			return wakeResponse(`{"type":"directory"}`), nil
		case strings.HasSuffix(req.URL.Path, "api/contents/content/.colabstudio/owned/deployment.json") && req.Method == "GET":
			resp := wakeResponse(`{}`)
			resp.StatusCode = 404
			return resp, nil
		case strings.HasSuffix(req.URL.Path, "api/contents/content/.colabstudio/owned/data") && req.Method == "GET":
			resp := wakeResponse(`{}`)
			resp.StatusCode = 404
			return resp, nil
		case req.Method == "PUT" && strings.Contains(req.URL.Path, "api/contents/content/.colabstudio/owned/"):
			name := strings.TrimPrefix(req.URL.Path, "/api/contents/content/.colabstudio/owned/")
			var putReq struct {
				Content string `json:"content"`
			}
			raw, _ := io.ReadAll(req.Body)
			_ = json.Unmarshal(raw, &putReq)
			decoded, _ := base64.StdEncoding.DecodeString(putReq.Content)
			uploaded[name] = decoded
			return wakeResponse(`{}`), nil
		default:
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
			return nil, nil
		}
	})
	m.studio.svc = m
	m.studio.svcOnce.Do(func() {})

	// Pre-configure Named Tunnel before any VM exists (ID/Runtime/RemoteRoot are empty).
	st, err := m.studio.SaveTunnelConfig("toybox.example.com/", "sudo cloudflared service install "+testSampleTunnelToken)
	if err != nil {
		t.Fatalf("SaveTunnelConfig failed before allocation: %v", err)
	}
	if !st.Configured || !st.HasToken || st.URL != "https://toybox.example.com" || st.Synced {
		t.Fatalf("unexpected pre-allocation status: %+v", st)
	}
	// Updating custom URL alone should reuse existing encrypted token.
	st, err = m.studio.SaveTunnelConfig("https://studio.example.com", "")
	if err != nil || !st.Configured || st.URL != "https://studio.example.com" {
		t.Fatalf("SaveTunnelConfig reuse-token failed: %+v %v", st, err)
	}

	svcStatus := m.studio.ServiceStatus()
	if !svcStatus.NamedTunnel || !svcStatus.TunnelHasToken || svcStatus.TunnelURL != "https://studio.example.com" {
		t.Fatalf("ServiceStatus missing tunnel flags: %+v", svcStatus)
	}

	r := m.get()
	r.ID = "owned"
	r.Account = "test@example.com"
	r.Binding = accountBinding("test@example.com")
	r.Runtime = "compat/b3duZWQ"
	r.URL = "https://stale-quick.trycloudflare.com"
	if err := m.put(r); err != nil {
		t.Fatal(err)
	}
	if err := m.deploy(&r); err != nil {
		t.Fatalf("deploy failed with Named Tunnel configured: %v", err)
	}
	var deployedTunnel remoteTunnelConfig
	if err := json.Unmarshal(uploaded["tunnel.json"], &deployedTunnel); err != nil {
		t.Fatalf("invalid uploaded tunnel.json: %v", err)
	}
	if deployedTunnel.Mode != "named" || deployedTunnel.URL != "https://studio.example.com" || deployedTunnel.Token != testSampleTunnelToken {
		t.Fatalf("unexpected named tunnel payload: %+v", deployedTunnel)
	}
	if r.URL != "" {
		t.Fatalf("expected stale quick URL cleared when deploying Named Tunnel, got %q", r.URL)
	}

	// Clear Named Tunnel and verify deployment falls back to Quick Tunnel.
	r.Runtime = ""
	r.RemoteRoot = ""
	if err := m.put(r); err != nil {
		t.Fatal(err)
	}
	if st, err := m.studio.ClearTunnelConfig(); err != nil || st.Configured || st.HasToken || st.URL != "" {
		t.Fatalf("ClearTunnelConfig unexpected status: %+v %v", st, err)
	}
	uploaded = map[string][]byte{}
	r = m.get()
	r.Runtime = "compat/b3duZWQ"
	r.URL = "https://studio.example.com"
	if err := m.put(r); err != nil {
		t.Fatal(err)
	}
	if err := m.deploy(&r); err != nil {
		t.Fatalf("deploy fallback to quick failed: %v", err)
	}
	deployedTunnel = remoteTunnelConfig{}
	if err := json.Unmarshal(uploaded["tunnel.json"], &deployedTunnel); err != nil {
		t.Fatalf("invalid fallback tunnel.json: %v", err)
	}
	if deployedTunnel.Mode != "quick" || deployedTunnel.URL != "" || deployedTunnel.Token != "" {
		t.Fatalf("expected clean quick mode payload, got %+v", deployedTunnel)
	}
	if r.URL != "" {
		t.Fatalf("expected stale custom URL cleared when deploying Quick Tunnel, got %q", r.URL)
	}
}

func TestNamedTunnelReadinessCopyAPIBaseAndRedaction(t *testing.T) {
	envelope := func(v any) *http.Response {
		b, _ := json.Marshal(v)
		body, _ := json.Marshal(map[string]string{"format": "text", "content": string(b)})
		return wakeResponse(string(body))
	}
	remoteReportedURL := "https://toybox.example.com"
	m := wakeFixture(t, func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, "assignments"):
			return wakeResponse(`{"assignments":[{"endpoint":"owned","runtimeProxyInfo":{"url":"https://owned.r.colab.dev","token":"private-proxy-token"}}]}`), nil
		case strings.HasSuffix(req.URL.Path, "state.json"):
			return envelope(remoteState{
				Owner:      "owned",
				Binding:    accountBinding("test@example.com"),
				Version:    toyboxVersion,
				Stage:      "online",
				Health:     true,
				URL:        remoteReportedURL,
				TunnelMode: "named",
				Heartbeat:  float64(time.Now().Unix()),
			}), nil
		case strings.HasSuffix(req.URL.Path, "progress.json"):
			return envelope(TransferProgress{Phase: "ready", TotalFiles: 16, VerifiedFiles: 16, TotalBytes: 1000, CompletedBytes: 1000}), nil
		case strings.HasSuffix(req.URL.Path, "comfyui.log"):
			return wakeResponse(""), nil
		case strings.HasSuffix(req.URL.Path, "tunnel.log"):
			return wakeResponse("starting named tunnel\nTUNNEL_TOKEN=" + testSampleTunnelToken + "\nraw token leak: " + testSampleTunnelToken + "\nRegistered tunnel connection"), nil
		default:
			t.Fatalf("unexpected request %s %s", req.Method, req.URL.Path)
			return nil, nil
		}
	})
	m.record = ServiceRecord{
		ID:         "owned",
		Binding:    accountBinding("test@example.com"),
		Runtime:    "compat/b3duZWQ",
		RemoteRoot: "content/.colabstudio/owned",
		TunnelURL:  "https://toybox.example.com",
		URL:        "https://toybox.example.com",
	}
	m.studio.svc = m
	m.studio.svcOnce.Do(func() {})
	if err := store.Set(cloudflareTunnelTokenKey, []byte(testSampleTunnelToken)); err != nil {
		t.Fatal(err)
	}

	if got := apiBaseURL(m.get()); got != "https://toybox.example.com/v1" {
		t.Fatalf("unexpected apiBaseURL for named tunnel: %q", got)
	}
	if got := apiBaseURL(ServiceRecord{URL: "https://toybox.example.com"}); got != "" {
		t.Fatalf("unconfigured custom domain must be rejected by apiBaseURL, got %q", got)
	}

	oldCopy := copyToyboxAddress
	t.Cleanup(func() { copyToyboxAddress = oldCopy })
	copied := ""
	copyToyboxAddress = func(s string) { copied = s }
	if err := m.studio.CopyToyboxURL(); err != nil || copied != "https://toybox.example.com" {
		t.Fatalf("CopyToyboxURL failed: %q %v", copied, err)
	}

	progress, err := m.studio.DeploymentProgress()
	if err != nil || progress.Remote == nil || progress.Remote.URL != "https://toybox.example.com" {
		t.Fatalf("DeploymentProgress rejected valid named tunnel URL: %+v %v", progress.Remote, err)
	}

	logs, err := m.studio.ToyboxLogs("tunnel")
	if err != nil {
		t.Fatalf("ToyboxLogs failed: %v", err)
	}
	if strings.Contains(logs, testSampleTunnelToken) || !strings.Contains(logs, "[凭据已隐藏]") || !strings.Contains(logs, "[已隐藏]") {
		t.Fatalf("Tunnel token was not redacted in logs: %q", logs)
	}

	// Untrusted domain must still fail CopyToyboxURL even when custom URL is configured.
	remoteReportedURL = "https://untrusted.example.com"
	copied = ""
	if err := m.studio.CopyToyboxURL(); err == nil || copied != "" {
		t.Fatalf("expected CopyToyboxURL to reject untrusted domain, got %q %v", copied, err)
	}
}

func TestNamedTunnelLiveReloadSync(t *testing.T) {
	oldRunCode := runCode
	t.Cleanup(func() { runCode = oldRunCode })
	var runCodeCalls []string
	runCode = func(p runtimeProxy, kernel, code string, untilBusy bool) error {
		runCodeCalls = append(runCodeCalls, code)
		return nil
	}
	oldKeepAlive := ensureKeepAlive
	t.Cleanup(func() { ensureKeepAlive = oldKeepAlive })
	ensureKeepAlive = func(p runtimeProxy, root string) error { return nil }

	envelope := func(v any) *http.Response {
		b, _ := json.Marshal(v)
		body, _ := json.Marshal(map[string]string{"format": "text", "content": string(b)})
		return wakeResponse(string(body))
	}
	tunnelMode := "quick"
	remoteStage := "online"
	remoteBusy := false
	stateFailuresRemaining := 0
	uploaded := map[string][]byte{}
	m := wakeFixture(t, func(req *http.Request) (*http.Response, error) {
		switch {
		case req.URL.Host == "www.googleapis.com":
			return wakeResponse(`{"files":[]}`), nil
		case strings.HasSuffix(req.URL.Path, "assignments"):
			return wakeResponse(`{"assignments":[{"endpoint":"owned","runtimeProxyInfo":{"url":"https://owned.r.colab.dev","token":"proxy-secret"}}]}`), nil
		case strings.HasSuffix(req.URL.Path, "api/kernels") && req.Method == "POST":
			return wakeResponse(`{"id":"11111111-1111-1111-1111-111111111111"}`), nil
		case strings.Contains(req.URL.Path, "api/kernels/") && req.Method == "DELETE":
			return wakeResponse(`{}`), nil
		case strings.HasSuffix(req.URL.Path, "api/contents/content/.colabstudio/owned") && req.Method == "GET":
			return wakeResponse(`{"type":"directory"}`), nil
		case strings.HasSuffix(req.URL.Path, "api/contents/content/.colabstudio/owned/data") && req.Method == "GET":
			return wakeResponse(`{"type":"directory"}`), nil
		case strings.HasSuffix(req.URL.Path, "deployment.json") && req.Method == "GET":
			return envelope(map[string]string{"id": "owned", "binding": accountBinding("test@example.com")}), nil
		case strings.HasSuffix(req.URL.Path, "state.json"):
			if stateFailuresRemaining > 0 {
				stateFailuresRemaining--
				resp := wakeResponse(`{}`)
				resp.StatusCode = 500
				return resp, nil
			}
			return envelope(remoteState{
				Owner:      "owned",
				Binding:    accountBinding("test@example.com"),
				Version:    toyboxVersion,
				Stage:      remoteStage,
				Busy:       remoteBusy,
				Health:     true,
				TunnelMode: tunnelMode,
				Heartbeat:  float64(time.Now().Unix()),
			}), nil
		case req.Method == "PUT" && strings.Contains(req.URL.Path, "api/contents/content/.colabstudio/owned/"):
			name := strings.TrimPrefix(req.URL.Path, "/api/contents/content/.colabstudio/owned/")
			var putReq struct {
				Content string `json:"content"`
			}
			raw, _ := io.ReadAll(req.Body)
			_ = json.Unmarshal(raw, &putReq)
			decoded, _ := base64.StdEncoding.DecodeString(putReq.Content)
			uploaded[name] = decoded
			return wakeResponse(`{}`), nil
		default:
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
			return nil, nil
		}
	})
	if err := persistDrive(driveGrant{
		Email:   "test@example.com",
		Refresh: "drive-refresh",
		Token:   Token{Access: "drive-access", Until: time.Now().Add(time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	m.record = ServiceRecord{
		ID:         "owned",
		Account:    "test@example.com",
		Binding:    accountBinding("test@example.com"),
		Runtime:    "compat/b3duZWQ",
		RemoteRoot: "content/.colabstudio/owned",
		URL:        "https://old-quick.trycloudflare.com",
		State:      "online",
	}
	m.studio.svc = m
	m.studio.svcOnce.Do(func() {})

	st, err := m.studio.SaveTunnelConfig("https://toybox.example.com", testSampleTunnelToken)
	if err != nil {
		t.Fatalf("live SaveTunnelConfig failed: %v", err)
	}
	if !st.Synced || !st.Configured || m.get().URL != "" {
		t.Fatalf("expected live sync & stale URL reset, got status=%+v recordURL=%q", st, m.get().URL)
	}
	var cfg remoteTunnelConfig
	if err := json.Unmarshal(uploaded["tunnel.json"], &cfg); err != nil || cfg.Mode != "named" || cfg.URL != "https://toybox.example.com" || cfg.Token != testSampleTunnelToken {
		t.Fatalf("unexpected live tunnel.json: %+v %v", cfg, err)
	}
	var cmd map[string]string
	if err := json.Unmarshal(uploaded["command.json"], &cmd); err != nil || cmd["action"] != "reload_tunnel" {
		t.Fatalf("expected reload_tunnel command, got %+v %v", cmd, err)
	}

	// Clearing config on an online modern supervisor should switch back to quick mode.
	tunnelMode = "named"
	uploaded = map[string][]byte{}
	r := m.get()
	r.URL = "https://toybox.example.com"
	if err := m.put(r); err != nil {
		t.Fatal(err)
	}
	st, err = m.studio.ClearTunnelConfig()
	if err != nil || !st.Synced || st.Configured || m.get().URL != "" {
		t.Fatalf("live ClearTunnelConfig failed: %+v recordURL=%q err=%v", st, m.get().URL, err)
	}
	cfg = remoteTunnelConfig{}
	if err := json.Unmarshal(uploaded["tunnel.json"], &cfg); err != nil || cfg.Mode != "quick" || cfg.Token != "" {
		t.Fatalf("expected quick mode payload after clear, got %+v %v", cfg, err)
	}
	cmd = nil
	if err := json.Unmarshal(uploaded["command.json"], &cmd); err != nil || cmd["action"] != "reload_tunnel" {
		t.Fatalf("expected reload_tunnel command on clear, got %+v %v", cmd, err)
	}

	// Same-version legacy supervisor (TunnelMode == "") while installing must skip mutation and mark RemoteOutdated.
	tunnelMode = ""
	remoteStage = "installing"
	remoteBusy = true
	uploaded = map[string][]byte{}
	runCodeCalls = nil
	st, err = m.studio.SaveTunnelConfig("https://toybox.example.com", testSampleTunnelToken)
	if err != nil || st.Synced || !st.Configured || !m.get().RemoteOutdated || !strings.Contains(st.Message, "安装") {
		t.Fatalf("unexpected installing legacy save status: %+v outdated=%v err=%v", st, m.get().RemoteOutdated, err)
	}
	if len(uploaded) != 0 || len(runCodeCalls) != 0 {
		t.Fatalf("installing legacy supervisor must not be mutated, uploaded=%v calls=%d", uploaded, len(runCodeCalls))
	}

	// inspect must detect same-version legacy supervisor when Named Tunnel is configured,
	// and preserve StartupError until supervisor actually upgrades.
	remoteStage = "online"
	inspected := m.get()
	inspected.RemoteOutdated = false
	inspected.StartupError = "之前的切换错误提示"
	inspected.StartupPhase = "failed"
	if err := m.inspect(&inspected); err != nil {
		t.Fatalf("inspect failed: %v", err)
	}
	if !inspected.RemoteOutdated || !strings.Contains(inspected.Message, "热升级") || inspected.StartupError != "之前的切换错误提示" || inspected.StartupPhase != "failed" {
		t.Fatalf("expected inspect to mark RemoteOutdated and preserve StartupError, got %+v", inspected)
	}

	// Busy online same-version legacy supervisor: SaveTunnelConfig hot-upgrades supervisor.py & tunnel.json without restarting web_runner.py.
	remoteBusy = true
	uploaded = map[string][]byte{}
	runCodeCalls = nil
	st, err = m.studio.SaveTunnelConfig("https://toybox.example.com", testSampleTunnelToken)
	if err != nil || !st.Synced || !st.Configured || m.get().RemoteOutdated {
		t.Fatalf("expected SaveTunnelConfig to hot-upgrade busy online legacy supervisor: %+v outdated=%v err=%v", st, m.get().RemoteOutdated, err)
	}
	if len(runCodeCalls) != 2 || !strings.Contains(runCodeCalls[0], "_stop_supervisor") || !strings.Contains(runCodeCalls[0], "allow_busy=True") || !strings.Contains(runCodeCalls[1], "supervisor.py") {
		t.Fatalf("expected allow_busy stop + relaunch kernel calls during legacy tunnel hot-upgrade, got %v", runCodeCalls)
	}
	if !strings.Contains(string(uploaded["supervisor.py"]), "load_tunnel_config") || !strings.Contains(string(uploaded["tunnel.json"]), "https://toybox.example.com") {
		t.Fatalf("expected supervisor.py and tunnel.json uploaded")
	}
	if _, touchedWeb := uploaded["web_runner.py"]; touchedWeb {
		t.Fatalf("same-version tunnel upgrade must not overwrite web_runner.py")
	}

	// m.start(false) retries transient state.json failure and runs tunnel-only upgrade even while busy.
	stateFailuresRemaining = 1
	uploaded = map[string][]byte{}
	runCodeCalls = nil
	if err := m.start(false); err != nil {
		t.Fatalf("start(false) failed to hot-upgrade busy same-version legacy tunnel after transient error: %v", err)
	}
	if stateFailuresRemaining != 0 || len(runCodeCalls) != 2 || !strings.Contains(runCodeCalls[0], "allow_busy=True") || !strings.Contains(string(uploaded["tunnel.json"]), "https://toybox.example.com") {
		t.Fatalf("expected retry + tunnel-only upgrade calls=2, got failuresLeft=%d calls=%d", stateFailuresRemaining, len(runCodeCalls))
	}
	if _, touchedWeb := uploaded["web_runner.py"]; touchedWeb {
		t.Fatalf("deployTunnelSupervisorOnly must not upload web_runner.py")
	}

	// Persistent state.json read failure in m.start(false) must fail explicitly rather than falling through silently.
	stateFailuresRemaining = 5
	if err := m.start(false); err == nil {
		t.Fatalf("expected start(false) to fail when state.json remains unreadable")
	}
	stateFailuresRemaining = 0
}
