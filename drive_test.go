package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type serviceRoundTrip func(*http.Request) (*http.Response, error)

func (f serviceRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestVerifiedDriveSavePrecedesRelease(t *testing.T) {
	for _, badChecksum := range []bool{false, true} {
		t.Run(fmt.Sprint(badChecksum), func(t *testing.T) {
			oldStore, oldClient := store, httpClient
			defer func() { store = oldStore; httpClient = oldClient }()
			store = newCredentialVault(filepath.Join(t.TempDir(), "vault"), &countingKeyStore{})
			tok := Token{Access: "test-access", Until: time.Now().Add(time.Hour)}
			raw, _ := json.Marshal(tok)
			store.Set("access-token", raw)
			store.Set("refresh-token", []byte("test-refresh"))
			profile, _ := json.Marshal(cachedProfile{Binding: credentialBinding([]byte("test-refresh")), Fetched: time.Now(), Profile: AccountProfile{Email: "test@example.com"}})
			store.Set("account-profile", profile)
			persistDrive(driveGrant{Token: tok, Refresh: "test-drive-refresh", Email: "test@example.com", Folder: "folder"})
			data := []byte("archive bytes")
			h := md5.Sum(data)
			md5sum := hex.EncodeToString(h[:])
			r := ServiceRecord{ID: "ownedservice", Binding: accountBinding("test@example.com"), Account: "test@example.com", Runtime: mapAssignment(assignment{Endpoint: "owned-endpoint"}).Name, RemoteRoot: "content/.colabstudio/ownedservice", StopRequested: true, BackupNonce: "snapshot", Folder: "folder"}
			released := false
			uploaded := false
			httpClient = &http.Client{Transport: serviceRoundTrip(func(req *http.Request) (*http.Response, error) {
				code := 200
				body := "{}"
				headers := make(http.Header)
				switch {
				case strings.HasSuffix(req.URL.Path, "assignments"):
					body = `{"assignments":[{"endpoint":"owned-endpoint","runtimeProxyInfo":{"url":"https://owned.r.colab.dev","token":"proxy-secret"}}]}`
				case strings.HasSuffix(req.URL.Path, "state.json"):
					v := remoteState{Owner: r.ID, Binding: r.Binding, Version: toyboxVersion, Heartbeat: float64(time.Now().Unix()), Backup: &remoteBackup{File: "backup-snapshot.tar.gz", Size: int64(len(data)), MD5: md5sum, Nonce: "snapshot"}}
					text, _ := json.Marshal(v)
					out, _ := json.Marshal(map[string]string{"format": "text", "content": string(text)})
					body = string(out)
				case strings.HasSuffix(req.URL.Path, "backup-snapshot.tar.gz"):
					body = string(data)
				case req.URL.Host == "www.googleapis.com" && req.URL.Path == "/drive/v3/files/folder":
					body = `{"id":"folder"}`
				case req.URL.Host == "www.googleapis.com" && req.URL.Path == "/drive/v3/files":
					body = `{"files":[]}`
				case req.URL.Path == "/upload/drive/v3/files" && req.Method == "POST":
					headers.Set("Location", "https://www.googleapis.com/upload-session")
				case req.URL.Path == "/upload-session":
					got, _ := io.ReadAll(req.Body)
					if string(got) != string(data) {
						t.Fatal("wrong bytes uploaded")
					}
					uploaded = true
					sum := md5sum
					if badChecksum {
						sum = strings.Repeat("0", 32)
					}
					body = fmt.Sprintf(`{"id":"archive","md5Checksum":%q,"size":%q}`, sum, fmt.Sprint(len(data)))
				case strings.Contains(req.URL.Path, "unassign/"):
					if req.Method == "GET" {
						body = `{"token":"challenge"}`
					} else {
						if !uploaded || badChecksum {
							t.Error("released before verified upload")
						}
						released = true
					}
				default:
					t.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
					code = 500
				}
				return &http.Response{StatusCode: code, Header: headers, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			m := &serviceManager{studio: &Studio{}, path: filepath.Join(t.TempDir(), "service.json"), record: r}
			e := m.finishStop(&r)
			if badChecksum {
				if e == nil || released || r.Runtime == "" {
					t.Fatal("failed backup released instance")
				}
			} else if e != nil || !released || r.Runtime != "" || r.Archive != "archive" {
				t.Fatalf("successful stop: released=%v err=%v record=%+v", released, e, r)
			}
		})
	}
}

func TestCreateAndOpenFolderBeforeMachineExists(t *testing.T) {
	created, opened := false, false
	createdCount := 0
	m := wakeFixture(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/drive/v3/files" {
			t.Fatal("unexpected cloud endpoint")
		}
		if r.Method == "GET" {
			return wakeResponse(`{"files":[]}`), nil
		}
		var meta map[string]any
		if e := json.NewDecoder(r.Body).Decode(&meta); e != nil {
			t.Fatal(e)
		}
		createdCount++
		expectedName := "ColabStudio"
		if createdCount == 2 {
			expectedName = "H3-Toybox"
			if fmt.Sprint(meta["parents"]) != "[root-folder]" {
				t.Fatal("incorrect parent")
			}
		}
		if meta["name"] != expectedName || meta["mimeType"] != "application/vnd.google-apps.folder" {
			t.Fatal("incorrect save folder")
		}
		created = true
		if createdCount == 1 {
			return wakeResponse(`{"id":"root-folder"}`), nil
		}
		return wakeResponse(`{"id":"created-folder"}`), nil
	})
	m.studio.svc = m
	m.studio.svcOnce.Do(func() {})
	persistDrive(driveGrant{Refresh: "saved-drive", Email: "test@example.com", Token: Token{Access: "drive-token", Until: time.Now().Add(time.Hour)}})
	prior := driveFolderOpen
	t.Cleanup(func() { driveFolderOpen = prior })
	driveFolderOpen = func(address string) error {
		opened = address == "https://drive.google.com/drive/folders/created-folder"
		return nil
	}
	if e := m.studio.OpenDriveFolder(); e != nil {
		t.Fatal(e)
	}
	d, _ := loadDriveGrant()
	if !created || !opened || d.Folder != "created-folder" || m.get().Runtime != "" {
		t.Fatal("folder not created independently of VM")
	}
}
