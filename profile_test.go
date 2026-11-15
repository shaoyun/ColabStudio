package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProfileReturnsDisplayIdentityOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-test-token" {
			t.Error("missing bearer")
		}
		w.Write([]byte(`{"name":"测试账号","email":"account@example.test","access_token":"should-not-return"}`))
	}))
	defer server.Close()
	p, e := fetchProfile(server.Client(), server.URL, "private-test-token")
	if e != nil || p.Name != "测试账号" || p.Email != "account@example.test" {
		t.Fatal("profile lookup failed", e)
	}
	b, _ := json.Marshal(p)
	if strings.Contains(string(b), "should-not-return") {
		t.Fatal("unwanted identity fields returned")
	}
}
func TestProfilePermissionErrorDoesNotLeakResponse(t *testing.T) {
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		w.WriteHeader(403)
		w.Write([]byte("private-token"))
	}))
	defer server.Close()
	_, e := fetchProfile(server.Client(), server.URL, "private-token")
	if e == nil || strings.Contains(e.Error(), "private-token") || count != 1 {
		t.Fatal("unsafe error or repeated authorization request")
	}
}
func TestProfileCacheNeverCrossesAccounts(t *testing.T) {
	now := time.Now()
	binding := credentialBinding([]byte("refresh-a"))
	b, _ := json.Marshal(cachedProfile{Binding: binding, Fetched: now, Profile: AccountProfile{Name: "A", Email: "a@example.test"}})
	if _, ok := cachedProfileFor(b, binding, now); !ok {
		t.Fatal("cache miss for current account")
	}
	if _, ok := cachedProfileFor(b, credentialBinding([]byte("refresh-b")), now); ok {
		t.Fatal("identity leaked across accounts")
	}
	if _, ok := cachedProfileFor(b, binding, now.Add(25*time.Hour)); ok {
		t.Fatal("stale identity accepted")
	}
	if _, ok := cachedProfileFor(b, binding, now.Add(-time.Hour)); ok {
		t.Fatal("future cache accepted")
	}
}
