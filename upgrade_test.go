package main

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestLegacyRemoteVersionAllowsProgressInspectAndSafeStop(t *testing.T) {
	const oldVersion = "efa7310b54faaf06c4b22a6fd7b18e1c9bfefd69"
	binding := accountBinding("test@example.com")
	backupBytes := []byte("legacy-works-tar-gz")
	sum := md5.Sum(backupBytes)
	backupMD5 := hex.EncodeToString(sum[:])

	stateOwner := "owned"
	stateBinding := binding
	stateVersion := oldVersion
	var activeBackup *remoteBackup
	released := false
	drainSent := false
	backupCmdSent := false

	envelope := func(v any) *http.Response {
		b, _ := json.Marshal(v)
		wrap, _ := json.Marshal(map[string]string{"format": "text", "content": string(b)})
		return wakeResponse(string(wrap))
	}

	m := wakeFixture(t, func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, "assignments"):
			return wakeResponse(`{"assignments":[{"endpoint":"owned","runtimeProxyInfo":{"url":"https://owned.r.colab.dev","token":"proxy-secret"}}]}`), nil
		case strings.HasSuffix(req.URL.Path, "state.json"):
			return envelope(remoteState{
				Owner:     stateOwner,
				Binding:   stateBinding,
				Version:   stateVersion,
				Stage:     "online",
				Health:    true,
				URL:       "https://legacy-demo.trycloudflare.com",
				Heartbeat: float64(time.Now().Unix()),
				Backup:    activeBackup,
			}), nil
		case strings.HasSuffix(req.URL.Path, "progress.json"):
			resp := wakeResponse(`{}`)
			resp.StatusCode = 404
			return resp, nil
		case strings.HasSuffix(req.URL.Path, "installed.json"):
			return envelope(map[string]string{"version": oldVersion}), nil
		case strings.HasSuffix(req.URL.Path, "deployment.log"):
			return wakeResponse(""), nil
		case strings.HasSuffix(req.URL.Path, "comfyui.log"):
			return wakeResponse("Requested to load H3Model\nloaded completely; 1024 MB loaded\n"), nil
		case req.Method == "PUT" && strings.HasSuffix(req.URL.Path, "command.json"):
			var putReq struct {
				Content string `json:"content"`
				Format  string `json:"format"`
			}
			raw, _ := io.ReadAll(req.Body)
			_ = json.Unmarshal(raw, &putReq)
			decoded, _ := base64.StdEncoding.DecodeString(putReq.Content)
			var cmd struct {
				Action string `json:"action"`
				Nonce  string `json:"nonce"`
			}
			_ = json.Unmarshal(decoded, &cmd)
			switch cmd.Action {
			case "drain":
				drainSent = true
			case "backup":
				backupCmdSent = true
				activeBackup = &remoteBackup{
					File:  "backup-" + cmd.Nonce + ".tar.gz",
					Size:  int64(len(backupBytes)),
					MD5:   backupMD5,
					Nonce: cmd.Nonce,
				}
			}
			return wakeResponse(`{}`), nil
		case activeBackup != nil && strings.HasSuffix(req.URL.Path, activeBackup.File):
			return wakeResponse(string(backupBytes)), nil
		case req.URL.Host == "www.googleapis.com" && req.URL.Path == "/drive/v3/files/folder":
			return wakeResponse(`{"id":"folder"}`), nil
		case req.URL.Host == "www.googleapis.com" && req.URL.Path == "/drive/v3/files":
			return wakeResponse(`{"files":[]}`), nil
		case req.URL.Path == "/upload/drive/v3/files" && req.Method == "POST":
			resp := wakeResponse(`{}`)
			resp.Header.Set("Location", "https://www.googleapis.com/upload-session")
			return resp, nil
		case req.URL.Path == "/upload-session":
			return wakeResponse(fmt.Sprintf(`{"id":"archive-legacy","md5Checksum":%q,"size":%q}`, backupMD5, fmt.Sprint(len(backupBytes)))), nil
		case strings.Contains(req.URL.Path, "unassign/"):
			if req.Method == "GET" {
				return wakeResponse(`{"token":"challenge"}`), nil
			}
			released = true
			return wakeResponse(`{}`), nil
		default:
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
			return nil, nil
		}
	})
	if e := persistDrive(driveGrant{
		Email:   "test@example.com",
		Refresh: "drive-refresh",
		Folder:  "folder",
		Token:   Token{Access: "drive-access", Until: time.Now().Add(time.Hour)},
	}); e != nil {
		t.Fatal(e)
	}
	r := ServiceRecord{
		ID:           "owned",
		Account:      "test@example.com",
		Binding:      binding,
		Runtime:      "compat/b3duZWQ",
		RemoteRoot:   "content/.colabstudio/owned",
		Folder:       "folder",
		StartupPhase: "failed",
		StartupError: "远端服务身份或版本不匹配，已暂停自动操作",
	}
	m.record = r
	m.studio.svc = m
	m.studio.svcOnce.Do(func() {})

	prog, err := m.studio.DeploymentProgress()
	if err != nil {
		t.Fatalf("DeploymentProgress failed on legacy version: %v", err)
	}
	if !prog.WeightsReady || prog.WeightsEvidence != "installation" || prog.Remote == nil || !prog.Remote.Outdated || prog.Remote.Version != oldVersion {
		t.Fatalf("unexpected progress on legacy version: %+v", prog)
	}

	if err = m.inspect(&r); err != nil {
		t.Fatalf("inspect failed on legacy version: %v", err)
	}
	if r.State != "online" || r.RemoteVersion != oldVersion || r.StartupError != "" || r.StartupPhase != "done" || !strings.Contains(r.Message, "旧版本") {
		t.Fatalf("unexpected inspect record: %+v", r)
	}

	for _, tc := range []struct {
		name, owner, bind, ver, wantErr string
	}{
		{"foreign-owner", "other", binding, oldVersion, "身份不匹配"},
		{"foreign-binding", "owned", accountBinding("other@example.com"), oldVersion, "身份不匹配"},
		{"invalid-version", "owned", binding, "not-a-sha", "版本标记无效"},
	} {
		stateOwner, stateBinding, stateVersion = tc.owner, tc.bind, tc.ver
		if _, err = m.state(r); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Fatalf("%s: state err=%v want %q", tc.name, err, tc.wantErr)
		}
		if _, err = m.studio.DeploymentProgress(); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Fatalf("%s: progress err=%v want %q", tc.name, err, tc.wantErr)
		}
	}

	stateOwner, stateBinding, stateVersion = "owned", binding, oldVersion
	r.StopRequested = true
	if err = m.finishStop(&r); err != nil {
		t.Fatalf("finishStop phase 1 failed: %v", err)
	}
	if !drainSent || !backupCmdSent || r.BackupNonce == "" {
		t.Fatalf("expected drain and backup commands: drain=%v backup=%v nonce=%q", drainSent, backupCmdSent, r.BackupNonce)
	}
	if err = m.finishStop(&r); err != nil {
		t.Fatalf("finishStop phase 2 failed: %v", err)
	}
	if !released || r.Runtime != "" || r.RemoteRoot != "" || r.RemoteVersion != "" || r.State != "stopped" || r.Archive != "archive-legacy" {
		t.Fatalf("legacy finishStop did not cleanly save and release: released=%v record=%+v", released, r)
	}
}

func TestHotUpgradeRefusesBusyAndReplacesIdleOldSupervisor(t *testing.T) {
	const oldVersion = "efa7310b54faaf06c4b22a6fd7b18e1c9bfefd69"
	binding := accountBinding("test@example.com")

	envelope := func(v any) *http.Response {
		b, _ := json.Marshal(v)
		wrap, _ := json.Marshal(map[string]string{"format": "text", "content": string(b)})
		return wakeResponse(string(wrap))
	}

	t.Run("refuses_when_busy_or_installing_without_creating_kernel", func(t *testing.T) {
		for _, tc := range []struct {
			name, stage, wantMsg string
			busy                 bool
		}{
			{"busy_online", "online", "运行中的任务", true},
			{"installing", "installing", "正在执行安装", false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				kernelPosts := 0
				m := wakeFixture(t, func(req *http.Request) (*http.Response, error) {
					switch {
					case req.URL.Host == "www.googleapis.com":
						return wakeResponse(`{"files":[]}`), nil
					case strings.HasSuffix(req.URL.Path, "assignments"):
						return wakeResponse(`{"assignments":[{"endpoint":"owned","runtimeProxyInfo":{"url":"https://owned.r.colab.dev","token":"proxy-secret"}}]}`), nil
					case strings.HasSuffix(req.URL.Path, "api/kernels") && req.Method == "POST":
						kernelPosts++
						return wakeResponse(`{"id":"11111111-1111-1111-1111-111111111111"}`), nil
					case strings.HasSuffix(req.URL.Path, "api/contents/content/.colabstudio/owned"):
						return wakeResponse(`{"type":"directory"}`), nil
					case strings.HasSuffix(req.URL.Path, "deployment.json"):
						return envelope(map[string]string{"id": "owned", "binding": binding}), nil
					case strings.HasSuffix(req.URL.Path, "state.json"):
						return envelope(remoteState{
							Owner:     "owned",
							Binding:   binding,
							Version:   oldVersion,
							Stage:     tc.stage,
							Busy:      tc.busy,
							Health:    true,
							Heartbeat: float64(time.Now().Unix()),
						}), nil
					default:
						t.Fatalf("unexpected request %s %s", req.Method, req.URL.Path)
						return nil, nil
					}
				})
				if e := persistDrive(driveGrant{
					Email:   "test@example.com",
					Refresh: "drive-refresh",
					Token:   Token{Access: "drive-access", Until: time.Now().Add(time.Hour)},
				}); e != nil {
					t.Fatal(e)
				}
				m.record = ServiceRecord{
					ID:         "owned",
					Account:    "test@example.com",
					Binding:    binding,
					Runtime:    "compat/b3duZWQ",
					RemoteRoot: "content/.colabstudio/owned",
					State:      tc.stage,
				}
				m.studio.svc = m
				m.studio.svcOnce.Do(func() {})

				if err := m.start(false); err == nil || !strings.Contains(err.Error(), tc.wantMsg) {
					t.Fatalf("start err=%v want %q", err, tc.wantMsg)
				}
				if err := m.retryDeployment(); err == nil || !strings.Contains(err.Error(), tc.wantMsg) {
					t.Fatalf("retry err=%v want %q", err, tc.wantMsg)
				}
				if kernelPosts != 0 {
					t.Fatalf("kernel created while %s: %d", tc.name, kernelPosts)
				}
			})
		}
	})

	t.Run("replaces_idle_old_supervisor_and_preserves_data", func(t *testing.T) {
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
			case strings.HasSuffix(req.URL.Path, "api/contents/content/.colabstudio/owned"):
				return wakeResponse(`{"type":"directory"}`), nil
			case strings.HasSuffix(req.URL.Path, "api/contents/content/.colabstudio/owned/data"):
				return wakeResponse(`{"type":"directory"}`), nil
			case strings.HasSuffix(req.URL.Path, "deployment.json") && req.Method == "GET":
				return envelope(map[string]string{"id": "owned", "binding": binding}), nil
			case strings.HasSuffix(req.URL.Path, "state.json") && req.Method == "GET":
				return envelope(remoteState{
					Owner:     "owned",
					Binding:   binding,
					Version:   oldVersion,
					Stage:     "online",
					Busy:      false,
					Health:    true,
					Heartbeat: float64(time.Now().Unix()),
				}), nil
			case req.Method == "PUT" && strings.Contains(req.URL.Path, "api/contents/content/.colabstudio/owned/"):
				name := strings.TrimPrefix(req.URL.Path, "/api/contents/content/.colabstudio/owned/")
				var putReq struct {
					Content string `json:"content"`
					Format  string `json:"format"`
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
		if e := persistDrive(driveGrant{
			Email:   "test@example.com",
			Refresh: "drive-refresh",
			Token:   Token{Access: "drive-access", Until: time.Now().Add(time.Hour)},
		}); e != nil {
			t.Fatal(e)
		}
		m.record = ServiceRecord{
			ID:            "owned",
			Account:       "test@example.com",
			Binding:       binding,
			Runtime:       "compat/b3duZWQ",
			RemoteRoot:    "content/.colabstudio/owned",
			RemoteVersion: oldVersion,
			Archive:       "old-drive-archive",
			State:         "online",
		}
		m.studio.svc = m
		m.studio.svcOnce.Do(func() {})

		if err := m.start(false); err != nil {
			t.Fatalf("hot upgrade start failed: %v", err)
		}
		if len(runCodeCalls) != 2 {
			t.Fatalf("expected 2 runCode calls (stop old supervisor + launch new supervisor), got %d", len(runCodeCalls))
		}
		if !strings.Contains(runCodeCalls[0], "_stop_supervisor") || !strings.Contains(runCodeCalls[0], "supervisor.lock") || !strings.Contains(runCodeCalls[0], "stop_supervisor") {
			t.Fatalf("first kernel call did not stop old supervisor cleanly: %s", runCodeCalls[0])
		}
		if !strings.Contains(runCodeCalls[1], "supervisor.py") || !strings.Contains(runCodeCalls[1], "Popen") {
			t.Fatalf("second kernel call did not launch new supervisor: %s", runCodeCalls[1])
		}
		if !strings.Contains(string(uploaded["lock.json"]), toyboxVersion) {
			t.Fatalf("uploaded lock.json missing new version %s: %s", toyboxVersion, string(uploaded["lock.json"]))
		}
		if !strings.Contains(string(uploaded["supervisor.py"]), "stop_supervisor") {
			t.Fatalf("uploaded supervisor.py missing stop_supervisor handler")
		}
		if _, hasRestore := uploaded["restore.json"]; hasRestore {
			t.Fatal("hot upgrade restored Drive backup over existing data directory")
		}
		got := m.get()
		if got.StartupPhase != "done" || got.RemoteVersion != "" || !strings.Contains(got.Message, "热升级") {
			t.Fatalf("unexpected post-upgrade record: %+v", got)
		}
	})
}
