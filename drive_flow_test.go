package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func driveFlowFixture(t *testing.T, remote bool) *Studio {
	t.Helper()
	m := wakeFixture(t, func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("unexpected network during auth setup")
	})
	oldProfile, oldConfig, oldOpen := driveAuthProfile, driveAuthConfig, driveAuthOpen
	t.Cleanup(func() { driveAuthProfile = oldProfile; driveAuthConfig = oldConfig; driveAuthOpen = oldOpen })
	driveAuthProfile = func(*Studio) (AccountProfile, error) { return AccountProfile{Email: "test@example.com"}, nil }
	driveAuthConfig = func() (Config, error) {
		return Config{ClientID: "test.apps.googleusercontent.com", ClientSecret: "test-secret", Remote: remote}, nil
	}
	driveAuthOpen = func(string) error { return nil }
	t.Cleanup(func() { _ = m.studio.CancelDriveLogin("") })
	return m.studio
}
func activeDriveFlow(s *Studio) *driveFlow {
	s.driveMu.Lock()
	defer s.driveMu.Unlock()
	return s.driveFlow
}
func TestDriveCancelAndRestartPreservesExistingGrant(t *testing.T) {
	s := driveFlowFixture(t, true)
	if e := persistDrive(driveGrant{Refresh: "existing-drive", Email: "test@example.com"}); e != nil {
		t.Fatal(e)
	}
	if _, e := s.BeginDriveLogin("request-old"); e != nil {
		t.Fatal(e)
	}
	old := activeDriveFlow(s)
	if !s.DriveStatus()["pending"].(bool) {
		t.Fatal("pending not visible")
	}
	if e := s.CancelDriveLogin("request-old"); e != nil {
		t.Fatal(e)
	}
	if s.DriveStatus()["pending"].(bool) {
		t.Fatal("cancel still pending")
	}
	if _, e := s.BeginDriveLogin("request-new"); e != nil {
		t.Fatal(e)
	}
	current := activeDriveFlow(s)
	if current.Verifier == old.Verifier || current.State == old.State {
		t.Fatal("PKCE/state reused")
	}
	_ = s.CancelDriveLogin("request-old")
	s.expireDriveFlow(old)
	if activeDriveFlow(s) != current {
		t.Fatal("late old cancel/timeout cleared new flow")
	}
	if e := s.completeDriveCodeFor(old, "old-code"); e == nil {
		t.Fatal("cancelled callback accepted")
	}
	if e := s.SubmitDriveCode("request-old", "old-code"); e == nil {
		t.Fatal("old submission accepted")
	}
	if activeDriveFlow(s) != current {
		t.Fatal("old callback consumed new flow")
	}
	d, _ := loadDriveGrant()
	if d.Refresh != "existing-drive" {
		t.Fatal("cancel removed existing authorization")
	}
}
func TestCancelDuringTokenExchangeRejectsLateSuccess(t *testing.T) {
	s := driveFlowFixture(t, true)
	persistDrive(driveGrant{Refresh: "existing-drive", Email: "test@example.com"})
	s.BeginDriveLogin("request-old")
	entered, release := make(chan struct{}), make(chan struct{})
	httpClient = &http.Client{Transport: serviceRoundTrip(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Host, "oauth2") {
			close(entered)
			<-release
			return wakeResponse(`{"access_token":"late-access","refresh_token":"late-refresh","expires_in":3600}`), nil
		}
		return wakeResponse(`{"email":"test@example.com"}`), nil
	})}
	done := make(chan error, 1)
	go func() { done <- s.SubmitDriveCode("request-old", "old-code") }()
	<-entered
	start := time.Now()
	s.CancelDriveLogin("request-old")
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("cancel blocked on token exchange")
	}
	if _, e := s.BeginDriveLogin("request-new"); e != nil {
		t.Fatal(e)
	}
	fresh := activeDriveFlow(s)
	close(release)
	if e := <-done; e == nil {
		t.Fatal("late success saved after cancellation")
	}
	if activeDriveFlow(s) != fresh {
		t.Fatal("old result cleared new flow")
	}
	d, _ := loadDriveGrant()
	if d.Refresh != "existing-drive" {
		t.Fatal("late token overwrote existing grant")
	}
}
func TestCancelClosesLoopbackCallback(t *testing.T) {
	s := driveFlowFixture(t, false)
	var address string
	driveAuthOpen = func(value string) error {
		u, _ := url.Parse(value)
		address = u.Query().Get("redirect_uri")
		return nil
	}
	if _, e := s.BeginDriveLogin("local-flow"); e != nil {
		t.Fatal(e)
	}
	client := &http.Client{Timeout: time.Second}
	resp, e := client.Get(address + "?state=wrong")
	if e != nil {
		t.Fatal(e)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatal("missing state check")
	}
	s.CancelDriveLogin("local-flow")
	if resp, e = client.Get(address); e == nil {
		resp.Body.Close()
		t.Fatal("cancel left listener open")
	}
	if _, e = s.BeginDriveLogin("local-new"); e != nil {
		t.Fatal(e)
	}
}
func TestCancelWhilePreparingDoesNotOpenOldBrowser(t *testing.T) {
	s := driveFlowFixture(t, true)
	var calls, opens atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	driveAuthProfile = func(*Studio) (AccountProfile, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return AccountProfile{Email: "test@example.com"}, nil
	}
	driveAuthOpen = func(string) error { opens.Add(1); return nil }
	done := make(chan error, 1)
	go func() { _, e := s.BeginDriveLogin("request-old"); done <- e }()
	<-entered
	s.CancelDriveLogin("request-old")
	if _, e := s.BeginDriveLogin("request-new"); e != nil {
		t.Fatal(e)
	}
	fresh := activeDriveFlow(s)
	close(release)
	if e := <-done; e == nil {
		t.Fatal("cancelled preparation opened browser")
	}
	if opens.Load() != 1 || activeDriveFlow(s) != fresh {
		t.Fatal("old preparation affected new authorization")
	}
	status := s.DriveStatus()
	b, _ := json.Marshal(status)
	if strings.Contains(string(b), fresh.Verifier) || strings.Contains(string(b), fresh.Config.ClientSecret) {
		t.Fatal("status exposes a secret")
	}
}

func TestDriveIndependentAccount(t *testing.T) {
	s := driveFlowFixture(t, true)
	httpClient = &http.Client{Transport: serviceRoundTrip(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Host, "oauth2") {
			return wakeResponse(`{"access_token":"test-access","refresh_token":"new-refresh","expires_in":3600}`), nil
		}
		return wakeResponse(`{"email":"other@example.com"}`), nil
	})}
	if _, e := s.BeginDriveLogin("independent-flow"); e != nil {
		t.Fatal(e)
	}
	if e := s.SubmitDriveCode("independent-flow", "code"); e != nil {
		t.Fatal(e)
	}
	status := s.DriveStatus()
	if !status["connected"].(bool) || status["email"] != "other@example.com" {
		t.Fatal(status)
	}
	if _, e := driveAccess("test@example.com"); e != nil {
		t.Fatal(e)
	}
}
func TestDriveConnectedStatusIncludesAccountAndFolder(t *testing.T) {
	s := driveFlowFixture(t, true)
	persistDrive(driveGrant{Refresh: "saved-drive", Email: "test@example.com", Folder: "existing-folder"})
	status := s.DriveStatus()
	if !status["connected"].(bool) || status["email"] != "test@example.com" || status["folder"] != "existing-folder" || status["folderName"] != "H3-Toybox" {
		t.Fatal("connected account/folder missing")
	}
}
