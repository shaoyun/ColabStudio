package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestWeeklyWindowCrossMidnight(t *testing.T) {
	r := ServiceRule{Enabled: true, Days: []int{1}, Start: "23:00", End: "02:00", Zone: "Asia/Shanghai"}
	tests := []struct {
		date   string
		active bool
	}{{"2026-11-16T22:59:00+08:00", false}, {"2026-11-16T23:00:00+08:00", true}, {"2026-11-17T01:59:00+08:00", true}, {"2026-11-17T02:00:00+08:00", false}, {"2026-11-18T01:00:00+08:00", false}}
	for _, tt := range tests {
		n, _ := time.Parse(time.RFC3339, tt.date)
		key, active, _ := ruleWindow(r, n)
		if active != tt.active {
			t.Errorf("%s active=%v", tt.date, active)
		}
		if active && !strings.HasPrefix(key, "2026-11-16/") {
			t.Fatal(key)
		}
	}
}
func TestRuleValidation(t *testing.T) {
	for _, r := range []ServiceRule{{Days: []int{1}, Start: "09:00", End: "09:00", Zone: "UTC"}, {Days: []int{1, 1}, Start: "09:00", End: "18:00", Zone: "UTC"}, {Days: []int{7}, Start: "09:00", End: "18:00", Zone: "UTC"}, {Days: []int{1}, Start: "9:90", End: "18:00", Zone: "UTC"}} {
		if validateRule(r) == nil {
			t.Fatal(r)
		}
	}
}
func TestAssignmentChallengeNeverPosts(t *testing.T) {
	n := 0
	id := randomNotebookID()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if r.Method != "GET" || r.URL.Query().Get("nbh") != id || r.URL.Query().Get("accelerator") != "L4" {
			t.Error("unexpected allocation request")
		}
		if r.Header.Get("Authorization") != "Bearer test" {
			t.Error("auth missing")
		}
		w.Write([]byte(")]}'\n{\"token\":\"challenge\"}"))
	}))
	defer server.Close()
	a, e := assignChallenge(server.Client(), server.URL+"/", "test", id)
	if e != nil || a.Token != "challenge" || n != 1 {
		t.Fatalf("%+v %v", a, e)
	}
	if len(id) != 44 || !strings.HasSuffix(id, "........") {
		t.Fatal(id)
	}
}
func TestServiceJournalAndCrossProcessLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "services.json")
	m := &serviceManager{path: path}
	r := ServiceRecord{ID: "owned", Operation: "persistent", PostSent: true, State: "uncertain"}
	if e := m.put(r); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(path)
	var restored ServiceRecord
	json.Unmarshal(b, &restored)
	if restored.Operation != "persistent" || !restored.PostSent {
		t.Fatal("lost uncertain assignment")
	}
	f, _ := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	defer f.Close()
	if e := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		t.Fatal(e)
	}
	g, _ := os.OpenFile(path+".lock", os.O_RDWR, 0600)
	defer g.Close()
	if e := syscall.Flock(int(g.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e == nil {
		t.Fatal("second manager took lock")
	}
}
func TestToyboxURLRestrictsSecretAndOrigin(t *testing.T) {
	for _, s := range []string{"http://demo.trycloudflare.com", "https://demo.trycloudflare.com.evil.test", "https://user@demo.trycloudflare.com", "https://demo.trycloudflare.com?token=secret", "https://demo.trycloudflare.com:443"} {
		if _, e := urlForToybox(s); e == nil {
			t.Fatal(s)
		}
	}
	if _, e := urlForToybox("https://toybox-test.trycloudflare.com"); e != nil {
		t.Fatal(e)
	}
}
func TestPinnedDeploymentManifest(t *testing.T) {
	var m struct {
		Toybox string `json:"toybox"`
		Models []struct {
			Revision string `json:"revision"`
			SHA      string `json:"sha256"`
			Size     int64  `json:"size"`
		} `json:"models"`
		Cloud struct {
			SHA string `json:"sha256"`
		} `json:"cloudflared"`
	}
	b, _ := templates.ReadFile("templates/toybox-lock.json")
	if json.Unmarshal(b, &m) != nil || m.Toybox != toyboxVersion || len(m.Models) != 16 || len(m.Cloud.SHA) != 64 {
		t.Fatal("invalid lock")
	}
	for _, f := range m.Models {
		if len(f.Revision) != 40 || len(f.SHA) != 64 || f.Size < 1 {
			t.Fatal("unverified model entry")
		}
	}
}
