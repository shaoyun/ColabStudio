package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func wakeFixture(t *testing.T, transport serviceRoundTrip) *serviceManager {
	t.Helper()
	oldStore, oldHTTP := store, httpClient
	t.Cleanup(func() { store = oldStore; httpClient = oldHTTP })
	store = newCredentialVault(filepath.Join(t.TempDir(), "vault"), &countingKeyStore{})
	token, _ := json.Marshal(Token{Access: "test-access", Until: time.Now().Add(time.Hour)})
	profile, _ := json.Marshal(cachedProfile{Binding: credentialBinding([]byte("test-refresh")), Fetched: time.Now(), Profile: AccountProfile{Email: "test@example.com"}})
	for k, v := range map[string][]byte{"access-token": token, "refresh-token": []byte("test-refresh"), "account-profile": profile} {
		if e := store.Set(k, v); e != nil {
			t.Fatal(e)
		}
	}
	httpClient = &http.Client{Transport: transport}
	return &serviceManager{studio: &Studio{}, path: filepath.Join(t.TempDir(), "service.json"), record: ServiceRecord{State: "stopped"}}
}
func wakeResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
func TestWakeWithoutDriveAndReuseExistingMachine(t *testing.T) {
	posts := 0
	endpoint := ""
	notebook := ""
	m := wakeFixture(t, func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "assignments"):
			if endpoint == "" {
				return wakeResponse(`{"assignments":[]}`), nil
			}
			return wakeResponse(`{"assignments":[{"endpoint":"owned-machine","runtimeProxyInfo":{"url":"https://owned.r.colab.dev","token":"proxy-token"}}]}`), nil
		case strings.HasSuffix(r.URL.Path, "assign"):
			if r.URL.Query().Get("accelerator") != "L4" {
				t.Fatal("unexpected GPU")
			}
			if notebook == "" {
				notebook = r.URL.Query().Get("nbh")
			}
			if notebook != r.URL.Query().Get("nbh") {
				t.Fatal("notebook hash changed")
			}
			if r.Method == "GET" {
				return wakeResponse(`{"token":"challenge"}`), nil
			}
			if r.Header.Get("X-Goog-Colab-Token") != "challenge" {
				t.Fatal("missing challenge")
			}
			posts++
			endpoint = "owned-machine"
			return wakeResponse(`{"endpoint":"owned-machine","variant":1,"accelerator":"L4"}`), nil
		default:
			t.Errorf("waking unexpectedly called %s", r.URL.Host+r.URL.Path)
			return nil, errors.New("unexpected request")
		}
	})
	if e := m.wake(false); e != nil {
		t.Fatal(e)
	}
	r := m.get()
	if r.State != "allocated" || r.Runtime == "" || r.RemoteRoot != "" || posts != 1 {
		t.Fatalf("not allocated: %+v posts=%d", r, posts)
	}
	if e := m.wake(false); e != nil {
		t.Fatal(e)
	}
	if posts != 1 {
		t.Fatal("second wake allocated another VM")
	}
	if e := m.start(false); e == nil || !strings.Contains(e.Error(), "Drive") {
		t.Fatal("deployment should request Drive after waking")
	}
	if m.get().Runtime != r.Runtime {
		t.Fatal("Drive failure lost VM record")
	}
}
func TestLostAllocationResponseNeverPostsAgain(t *testing.T) {
	posts := 0
	notebook := ""
	m := wakeFixture(t, func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "assignments") {
			return wakeResponse(`{"assignments":[]}`), nil
		}
		if r.Method == "POST" {
			posts++
			return nil, errors.New("connection lost after server accepted request")
		}
		id := r.URL.Query().Get("nbh")
		if notebook == "" {
			notebook = id
		}
		if id != notebook {
			t.Fatal("new allocation id after ambiguous response")
		}
		return wakeResponse(`{"token":"challenge"}`), nil
	})
	if m.wake(false) == nil {
		t.Fatal("expected ambiguous allocation")
	}
	if m.get().State != "uncertain" {
		t.Fatal(m.get().State)
	}
	if m.wake(false) == nil {
		t.Fatal("must keep blocking ambiguous retry")
	}
	if posts != 1 {
		t.Fatalf("allocation posted %d times", posts)
	}
}
func TestEmptyWokenMachineReleasesWithoutDriveOrSupervisor(t *testing.T) {
	released := false
	m := wakeFixture(t, func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "assignments"):
			return wakeResponse(`{"assignments":[{"endpoint":"empty-machine","runtimeProxyInfo":{"url":"https://owned.r.colab.dev","token":"proxy-token"}}]}`), nil
		case strings.Contains(r.URL.Path, "unassign/"):
			if r.Method == "GET" {
				return wakeResponse(`{"token":"challenge"}`), nil
			}
			released = true
			return wakeResponse(`{}`), nil
		default:
			return nil, fmt.Errorf("empty VM must not call backup/kernel: %s", r.URL.Path)
		}
	})
	r := ServiceRecord{ID: "owned", Account: "test@example.com", Binding: accountBinding("test@example.com"), Runtime: mapAssignment(assignment{Endpoint: "empty-machine"}).Name, State: "allocated"}
	if e := m.finishStop(&r); e != nil {
		t.Fatal(e)
	}
	if !released || r.Runtime != "" || r.State != "stopped" {
		t.Fatal("empty machine was not released")
	}
}

func TestStartDeploysWokenMachineBeforeCheckingSupervisor(t *testing.T) {
	kernelRequested := false
	stateRequested := false
	m := wakeFixture(t, func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "assignments"):
			return wakeResponse(`{"assignments":[{"endpoint":"woken","runtimeProxyInfo":{"url":"https://owned.r.colab.dev","token":"proxy-token"}}]}`), nil
		case r.URL.Host == "www.googleapis.com":
			return wakeResponse(`{"files":[]}`), nil
		case strings.HasSuffix(r.URL.Path, "api/kernels") && r.Method == "POST":
			kernelRequested = true
			return nil, errors.New("intentional stop before remote execution")
		case strings.HasSuffix(r.URL.Path, "state.json"):
			stateRequested = true
			return nil, errors.New("supervisor not installed")
		default:
			return nil, fmt.Errorf("unexpected request %s", r.URL.Path)
		}
	})
	if e := persistDrive(driveGrant{Email: "test@example.com", Refresh: "drive-refresh", Token: Token{Access: "drive-access", Until: time.Now().Add(time.Hour)}}); e != nil {
		t.Fatal(e)
	}
	m.record = ServiceRecord{ID: "owned", Account: "test@example.com", Binding: accountBinding("test@example.com"), Runtime: mapAssignment(assignment{Endpoint: "woken"}).Name, State: "allocated"}
	if m.start(false) == nil {
		t.Fatal("expected intentional fixture stop")
	}
	if !kernelRequested || stateRequested {
		t.Fatalf("deployment blocked by missing supervisor: kernel=%v state=%v", kernelRequested, stateRequested)
	}
}

func TestRetryCannotBypassDriveGate(t *testing.T) {
	requests := 0
	m := wakeFixture(t, func(r *http.Request) (*http.Response, error) {
		requests++
		return nil, fmt.Errorf("retry without Drive must not execute: %s", r.URL.Path)
	})
	m.record = ServiceRecord{ID: "owned", Account: "test@example.com", Binding: accountBinding("test@example.com"), Runtime: mapAssignment(assignment{Endpoint: "woken"}).Name, State: "allocated"}
	m.studio.svc = m
	m.studio.svcOnce.Do(func() {})
	if e := m.studio.RetryToybox(true); e != nil {
		t.Fatal(e)
	}
	m.op.Lock()
	defer m.op.Unlock()
	if requests != 0 || !strings.Contains(m.get().Message, "Drive") {
		t.Fatalf("retry bypassed Drive: requests=%d message=%s", requests, m.get().Message)
	}
}

func TestRejectedAssignDoesNotLockOperation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		gpu         string
		postStatus  int
		postBody    string
		assignments string
	}{
		{
			name:        "http_200_empty_endpoint",
			gpu:         "A100",
			postStatus:  200,
			postBody:    `{}`,
			assignments: `{"assignments":[]}`,
		},
		{
			name:        "http_503_capacity_empty",
			gpu:         "A100",
			postStatus:  503,
			postBody:    `unavailable`,
			assignments: `{"assignments":[]}`,
		},
		{
			name:        "http_412_with_unrelated_browser_vm",
			gpu:         "H100",
			postStatus:  412,
			postBody:    `too many assignments`,
			assignments: `{"assignments":[{"endpoint":"browser-vm","runtimeProxyInfo":{"url":"https://other.r.colab.dev","token":"other-tok"}}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := wakeFixture(t, func(r *http.Request) (*http.Response, error) {
				switch {
				case strings.HasSuffix(r.URL.Path, "assignments"):
					return wakeResponse(tc.assignments), nil
				case strings.Contains(r.URL.Path, "api/contents/"):
					resp := wakeResponse(`{}`)
					resp.StatusCode = 404
					return resp, nil
				case strings.HasSuffix(r.URL.Path, "assign") && r.Method == "GET":
					return wakeResponse(`{"token":"challenge"}`), nil
				case strings.HasSuffix(r.URL.Path, "assign") && r.Method == "POST":
					resp := wakeResponse(tc.postBody)
					resp.StatusCode = tc.postStatus
					return resp, nil
				default:
					return nil, fmt.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
			})
			if e := m.setMachineType(tc.gpu, true); e != nil {
				t.Fatal(e)
			}
			err := m.wake(false)
			if err == nil {
				t.Fatal("expected wake error when allocation rejected")
			}
			got := m.get()
			if got.Operation != "" || got.PostSent || got.State != "stopped" {
				t.Fatalf("rejected assign leaked pending lock: op=%q postSent=%v state=%q err=%v", got.Operation, got.PostSent, got.State, err)
			}
			if e := m.setMachineType("L4", true); e != nil {
				t.Fatalf("failed to switch machine type after rejected assign: %v", e)
			}
		})
	}
}

func TestRefreshServiceReconcilesUncertainOperation(t *testing.T) {
	t.Run("clears_when_cloud_has_no_instances", func(t *testing.T) {
		m := wakeFixture(t, func(r *http.Request) (*http.Response, error) {
			switch {
			case strings.HasSuffix(r.URL.Path, "assign"):
				return wakeResponse(`{"token":"challenge"}`), nil
			case strings.HasSuffix(r.URL.Path, "assignments"):
				return wakeResponse(`{"assignments":[]}`), nil
			default:
				return nil, fmt.Errorf("unexpected request %s", r.URL.Path)
			}
		})
		m.record = ServiceRecord{
			ID:           "owned",
			Account:      "test@example.com",
			Binding:      accountBinding("test@example.com"),
			Accelerator:  "A100",
			Operation:    "stuck-op",
			PostSent:     true,
			State:        "uncertain",
			StartupPhase: "failed",
			StartupError: "上次申请结果尚未确认，已阻止再次创建；请先核对实例列表",
		}
		m.studio.svc = m
		m.studio.svcOnce.Do(func() {})
		if e := m.studio.RefreshService(); e != nil {
			t.Fatal(e)
		}
		got := m.get()
		if got.Operation != "" || got.PostSent || got.State != "stopped" || got.StartupError != "" {
			t.Fatalf("expected RefreshService to unlock uncertain state, got %+v", got)
		}
		if e := m.setMachineType("L4", true); e != nil {
			t.Fatalf("expected machine type to unlock after reconcile: %v", e)
		}
	})

	t.Run("recovers_runtime_if_operation_allocated", func(t *testing.T) {
		m := wakeFixture(t, func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "assign") {
				return wakeResponse(`{"endpoint":"recovered-vm","variant":1,"accelerator":"A100"}`), nil
			}
			return nil, fmt.Errorf("unexpected request %s", r.URL.Path)
		})
		m.record = ServiceRecord{
			ID:          "owned",
			Account:     "test@example.com",
			Binding:     accountBinding("test@example.com"),
			Accelerator: "A100",
			Operation:   "stuck-op",
			PostSent:    true,
			State:       "uncertain",
		}
		m.studio.svc = m
		m.studio.svcOnce.Do(func() {})
		if e := m.studio.RefreshService(); e != nil {
			t.Fatal(e)
		}
		got := m.get()
		if got.Runtime == "" || got.Operation != "" || got.PostSent || got.State != "allocated" {
			t.Fatalf("expected RefreshService to adopt recovered runtime, got %+v", got)
		}
	})

	t.Run("runtimes_empty_list_clears_uncertain_operation", func(t *testing.T) {
		m := wakeFixture(t, func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "assignments") {
				return wakeResponse(`{"assignments":[]}`), nil
			}
			return nil, fmt.Errorf("unexpected request %s", r.URL.Path)
		})
		m.record = ServiceRecord{
			ID:          "owned",
			Account:     "test@example.com",
			Binding:     accountBinding("test@example.com"),
			Accelerator: "L4",
			Operation:   "stuck-op",
			PostSent:    true,
			State:       "uncertain",
		}
		m.studio.compatible = true
		m.studio.svc = m
		m.studio.svcOnce.Do(func() {})
		rows, e := m.studio.Runtimes()
		if e != nil || len(rows) != 0 {
			t.Fatalf("unexpected Runtimes result: rows=%v err=%v", rows, e)
		}
		got := m.get()
		if got.Operation != "" || got.PostSent || got.State != "stopped" {
			t.Fatalf("expected Runtimes() empty check to unlock uncertain state, got %+v", got)
		}
	})
}
