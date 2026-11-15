package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func startupFixture(t *testing.T, driveCheck func(*http.Request) (*http.Response, error)) *serviceManager {
	m := wakeFixture(t, func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "www.googleapis.com" {
			return driveCheck(req)
		}
		if strings.HasSuffix(req.URL.Path, "assignments") {
			return wakeResponse(`{"assignments":[{"endpoint":"owned","runtimeProxyInfo":{"url":"https://owned.r.colab.dev","token":"fake-proxy"}}]}`), nil
		}
		t.Fatalf("unexpected startup request: %s %s", req.Method, req.URL.Path)
		return nil, nil
	})
	m.record = ServiceRecord{ID: "owned", Account: "test@example.com", Binding: accountBinding("test@example.com"), Runtime: "compat/b3duZWQ", State: "allocated"}
	m.studio.svc = m
	m.studio.svcOnce.Do(func() {})
	if e := persistDrive(driveGrant{Email: "test@example.com", Refresh: "fake-drive-refresh", Token: Token{Access: "fake-drive-access", Until: time.Now().Add(time.Hour)}}); e != nil {
		t.Fatal(e)
	}
	return m
}
func TestStartupFailureSurvivesMachineHeartbeatAndReload(t *testing.T) {
	m := startupFixture(t, func(req *http.Request) (*http.Response, error) {
		resp := wakeResponse(`{}`)
		resp.StatusCode = 403
		return resp, nil
	})
	err := m.start(false)
	if err == nil {
		t.Fatal("fixture did not fail")
	}
	before := m.get()
	after := before
	if e := m.inspect(&after); e != nil {
		t.Fatal(e)
	}
	t.Logf("start error=%v; remote root empty=%v; before heartbeat=%q; after heartbeat=%q", err, before.RemoteRoot == "", before.Message, after.Message)
	if after.StartupError != err.Error() || after.StartupPhase != "failed" || !strings.Contains(after.StartupLog, "Drive HTTP 403") {
		t.Fatal("startup failure lost after machine heartbeat")
	}
	if e := m.put(after); e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(m.path)
	if e != nil {
		t.Fatal(e)
	}
	var loaded ServiceRecord
	if json.Unmarshal(raw, &loaded) != nil || loaded.StartupError != after.StartupError {
		t.Fatal("startup error lost on reload")
	}
	text, e := m.studio.ToyboxLogs("deployment")
	if e != nil || !strings.Contains(text, "检查 Drive") || !strings.Contains(text, "Drive HTTP 403") {
		t.Fatalf("no pre-directory log: %q %v", text, e)
	}
}

func TestQueuedStartupReportsLivePhaseAndRejectsSecondStart(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	m := startupFixture(t, func(req *http.Request) (*http.Response, error) {
		close(entered)
		<-release
		return nil, errors.New("fixture unavailable")
	})
	if e := m.studio.StartToybox(true); e != nil {
		t.Fatal(e)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("startup not entered")
	}
	status := m.studio.ServiceStatus()
	second := m.studio.StartToybox(true)
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.op.TryLock() {
			m.op.Unlock()
			break
		}
		time.Sleep(time.Millisecond * 5)
	}
	if m.op.TryLock() {
		m.op.Unlock()
	} else {
		t.Fatal("startup did not release operation lock")
	}
	if !status.Operating || status.StartupPhase != "checking_drive" || !strings.Contains(status.StartupLog, "检查 Drive") {
		t.Fatalf("pending startup invisible: %+v", status)
	}
	if second == nil {
		t.Fatal("duplicate startup accepted")
	}
	if m.get().StartupPhase != "failed" {
		t.Fatal("async failure not persisted")
	}
}
func TestStartupLogIsBoundedAndRedacted(t *testing.T) {
	text := ""
	for i := 0; i < 130; i++ {
		text = boundedStartupLog(text, "token=secret-value Bearer fake-access-token")
	}
	if strings.Contains(text, "secret-value") || strings.Contains(text, "fake-access-token") || len(strings.Split(text, "\n")) != 100 {
		t.Fatal("startup journal leaked credentials or grew unbounded")
	}
}

func TestRetryDeploymentFailureAlsoHasStartupDiagnostics(t *testing.T) {
	m := startupFixture(t, func(req *http.Request) (*http.Response, error) {
		resp := wakeResponse(`{}`)
		resp.StatusCode = 403
		return resp, nil
	})
	err := m.retryDeployment()
	if err == nil || m.get().StartupError != err.Error() || !strings.Contains(m.get().StartupLog, "继续部署：检查 Drive") {
		t.Fatal("retry failure invisible")
	}
}
