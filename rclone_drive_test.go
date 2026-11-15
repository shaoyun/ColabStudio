package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRcloneDriveGrant(t *testing.T) {
	dir := t.TempDir()
	write := func(scope, expiry string) string {
		p := filepath.Join(dir, "drive_rclone.conf")
		body := "[side]\ntype = drive\nscope = " + scope + "\ntoken = {\"access_token\":\"acc\",\"refresh_token\":\"ref\",\"expiry\":\"" + expiry + "\"}\n"
		if e := os.WriteFile(p, []byte(body), 0600); e != nil {
			t.Fatal(e)
		}
		return p
	}
	old := rcloneRefresh
	t.Cleanup(func() { rcloneRefresh = old })
	refreshed := 0
	rcloneRefresh = func(path, name string) error {
		refreshed++
		if name != "side" {
			t.Fatal(name)
		}
		write("drive.file", time.Now().Add(time.Hour).Format(time.RFC3339))
		return nil
	}
	if _, e := rcloneAccess(write("drive", time.Now().Add(time.Hour).Format(time.RFC3339))); e == nil || !strings.Contains(e.Error(), "drive.file") {
		t.Fatal("full drive scope must be rejected")
	}
	if a, e := rcloneAccess(write("drive.file", time.Now().Add(time.Hour).Format(time.RFC3339))); e != nil || a != "acc" || refreshed != 0 {
		t.Fatalf("fresh token: %v %q %d", e, a, refreshed)
	}
	if a, e := rcloneAccess(write("drive.file", time.Now().Add(-time.Hour).Format(time.RFC3339))); e != nil || a != "acc" || refreshed != 1 {
		t.Fatalf("expired token should refresh via rclone: %v %q %d", e, a, refreshed)
	}
}
