package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestWeightsMustBeVerifiedNotOnlyDownloaded(t *testing.T) {
	p := TransferProgress{TotalFiles: 16, VerifiedFiles: 15, TotalBytes: 1000, CompletedBytes: 1000}
	if weightsVerified(p) {
		t.Fatal("download 100% falsely means all verified")
	}
	p.VerifiedFiles = 16
	if !weightsVerified(p) {
		t.Fatal("verified manifest not accepted")
	}
	p.CompletedBytes = 900
	if weightsVerified(p) {
		t.Fatal("incomplete byte count accepted")
	}
	if weightsVerified(TransferProgress{Phase: "ready"}) {
		t.Fatal("ready phase with no proof accepted")
	}
}
func TestReadinessRequiresFreshHeartbeatAndSafeAddress(t *testing.T) {
	now := time.Now()
	r := remoteState{Stage: "online", Health: true, Heartbeat: float64(now.Unix()), URL: "https://owned-demo.trycloudflare.com"}
	if v := readinessFromState(r, now); !v.Fresh || !v.Health || v.URL != r.URL {
		t.Fatal(v)
	}
	r.Heartbeat = float64(now.Add(-91 * time.Second).Unix())
	if readinessFromState(r, now).Fresh {
		t.Fatal("stale considered fresh")
	}
	r.Heartbeat = float64(now.Add(time.Minute).Unix())
	if readinessFromState(r, now).Fresh {
		t.Fatal("future considered fresh")
	}
	for _, address := range []string{"http://owned-demo.trycloudflare.com", "https://evil.example", "https://owned-demo.trycloudflare.com?token=secret", "https://user:password@owned-demo.trycloudflare.com"} {
		r.URL = address
		if readinessFromState(r, now).URL != "" {
			t.Fatal("unsafe address exposed")
		}
	}
}
func TestCopyToyboxURLUsesVerifiedRemoteStateRatherThanStaleCache(t *testing.T) {
	cases := []struct {
		name, address, owner string
		age                  time.Duration
		want                 bool
	}{{"current", "https://new-demo.trycloudflare.com", "owned", 0, true}, {"unsafe", "https://evil.example", "owned", 0, false}, {"foreign", "https://new-demo.trycloudflare.com", "other", 0, false}, {"expired", "https://new-demo.trycloudflare.com", "owned", time.Minute * 2, false}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := wakeFixture(t, func(req *http.Request) (*http.Response, error) {
				if req.Method != "GET" {
					t.Fatal("copy mutated cloud")
				}
				if strings.HasSuffix(req.URL.Path, "assignments") {
					return wakeResponse(`{"assignments":[{"endpoint":"owned","runtimeProxyInfo":{"url":"https://owned.r.colab.dev","token":"private-proxy-token"}}]}`), nil
				}
				if strings.HasSuffix(req.URL.Path, "state.json") {
					state := remoteState{Owner: tc.owner, Binding: accountBinding("test@example.com"), Version: toyboxVersion, Stage: "online", Health: true, URL: tc.address, Heartbeat: float64(time.Now().Add(-tc.age).Unix())}
					content, _ := json.Marshal(state)
					body, _ := json.Marshal(map[string]string{"format": "text", "content": string(content)})
					return wakeResponse(string(body)), nil
				}
				t.Fatalf("unexpected read %s", req.URL.Path)
				return nil, nil
			})
			m.record = ServiceRecord{ID: "owned", Binding: accountBinding("test@example.com"), Runtime: "compat/b3duZWQ", RemoteRoot: "content/owned", URL: "https://old-demo.trycloudflare.com"}
			m.studio.svc = m
			m.studio.svcOnce.Do(func() {})
			old := copyToyboxAddress
			t.Cleanup(func() { copyToyboxAddress = old })
			copied := ""
			copyToyboxAddress = func(s string) { copied = s }
			err := m.studio.CopyToyboxURL()
			if tc.want {
				if err != nil || copied != tc.address {
					t.Fatalf("copy got %q %v", copied, err)
				}
			} else if err == nil || copied != "" {
				t.Fatal("unconfirmed remote URL copied")
			}
		})
	}
}

func TestDeploymentProgressReportsIndependentReadinessEvidence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		verified  int
		installed string
		want      bool
		evidence  string
	}{{"verified", 16, "", true, "sha256"}, {"still-verifying", 15, "wrong-version", false, ""}, {"restored-installation", 0, toyboxVersion, true, "installation"}} {
		t.Run(tc.name, func(t *testing.T) {
			envelope := func(v any) *http.Response {
				content, _ := json.Marshal(v)
				body, _ := json.Marshal(map[string]string{"format": "text", "content": string(content)})
				return wakeResponse(string(body))
			}
			m := wakeFixture(t, func(req *http.Request) (*http.Response, error) {
				if req.Method != "GET" {
					t.Fatal("readiness mutated cloud")
				}
				switch {
				case strings.HasSuffix(req.URL.Path, "assignments"):
					return wakeResponse(`{"assignments":[{"endpoint":"owned","runtimeProxyInfo":{"url":"https://owned.r.colab.dev","token":"private-proxy"}}]}`), nil
				case strings.HasSuffix(req.URL.Path, "state.json"):
					return envelope(remoteState{Owner: "owned", Binding: accountBinding("test@example.com"), Version: toyboxVersion, Stage: "online", Health: true, Heartbeat: float64(time.Now().Unix()), URL: "https://ready-demo.trycloudflare.com"}), nil
				case strings.HasSuffix(req.URL.Path, "progress.json"):
					return envelope(TransferProgress{Phase: "ready", TotalFiles: 16, VerifiedFiles: tc.verified, TotalBytes: 1000, CompletedBytes: 1000}), nil
				case strings.HasSuffix(req.URL.Path, "installed.json"):
					return envelope(map[string]string{"version": tc.installed}), nil
				case strings.HasSuffix(req.URL.Path, "comfyui.log"):
					return wakeResponse("Requested to load H3Model\nloaded partially; 8192 MB loaded\n"), nil
				default:
					t.Fatalf("unexpected readiness read: %s", req.URL.Path)
					return nil, nil
				}
			})
			m.record = ServiceRecord{ID: "owned", Binding: accountBinding("test@example.com"), Runtime: "compat/b3duZWQ", RemoteRoot: "content/owned"}
			m.studio.svc = m
			m.studio.svcOnce.Do(func() {})
			out, e := m.studio.DeploymentProgress()
			if e != nil || out.WeightsReady != tc.want || out.WeightsEvidence != tc.evidence || out.Remote == nil || !out.Remote.Fresh || !out.Remote.Health || out.Remote.URL != "https://ready-demo.trycloudflare.com" || out.Model.Phase != "partial" {
				t.Fatalf("bad readiness evidence: %+v %v", out, e)
			}
		})
	}
}
