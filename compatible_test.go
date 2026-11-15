package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServiceDisabledFallsBackToAssignments(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Method != "GET" {
			t.Error("unexpected mutation")
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing credentials")
		}
		if r.URL.Path == "/public/runtimes" {
			w.WriteHeader(403)
			w.Write([]byte(`{"error":{"details":[{"reason":"SERVICE_DISABLED"}]}}`))
			return
		}
		if r.URL.Path != "/tunnel/assignments" {
			t.Error("unexpected path")
		}
		if r.URL.Query().Get("authuser") != "0" || r.Header.Get("X-Colab-Client-Agent") != "colab-cli" {
			t.Error("missing CLI protocol fields")
		}
		w.Write([]byte(")]}'\n" + `{"assignments":[{"endpoint":"vm-example","variant":1,"accelerator":"T4","machineShape":1,"runtimeProxyInfo":{"url":"https://example.googleusercontent.com","token":"proxy-secret"}}]}`))
	}))
	defer server.Close()
	rows, compat, e := fetchRuntimes(server.Client(), server.URL+"/public/", server.URL+"/tunnel/", "test-token")
	if e != nil || !compat || len(rows) != 1 {
		t.Fatalf("fallback failed: compatible=%v rows=%d err=%v", compat, len(rows), e)
	}
	if rows[0].RuntimeSpec.Variant != "VARIANT_GPU" || rows[0].RuntimeSpec.Shape != "SHAPE_HIGHMEM" {
		t.Fatal("incorrect mapping")
	}
	b, _ := json.Marshal(rows)
	if strings.Contains(string(b), "proxy-secret") {
		t.Fatal("proxy token leaked to frontend")
	}
	endpoint, e := decodeEndpoint(rows[0].Name)
	if e != nil || endpoint != "vm-example" {
		t.Fatal("invalid endpoint mapping")
	}
	if len(paths) != 2 {
		t.Fatal("unexpected retries")
	}
}
func TestDoesNotFallbackOnMissingScope(t *testing.T) {
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		w.WriteHeader(403)
		w.Write([]byte(`{"error":{"details":[{"reason":"ACCESS_TOKEN_SCOPE_INSUFFICIENT"}]}}`))
	}))
	defer server.Close()
	_, compat, e := fetchRuntimes(server.Client(), server.URL+"/", server.URL+"/tunnel/", "token")
	if e == nil || compat || count != 1 {
		t.Fatal("unexpected fallback for scope error")
	}
}
func TestRejectCompatibleEndpointTraversal(t *testing.T) {
	for _, v := range []string{"compat/Li4vLi4vYQ", "compat/a/b", "compat/", "runtimes/test"} {
		if _, e := decodeEndpoint(v); e == nil {
			t.Fatal("unsafe endpoint")
		}
	}
	if e := (&Studio{}).Release("compat/dm0tZXhhbXBsZQ", false); e == nil {
		t.Fatal("missing release confirmation")
	}
}
func TestCompatibleErrorDoesNotExposeBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403); w.Write([]byte("token-secret")) }))
	defer srv.Close()
	e := tunnelRequest(srv.Client(), srv.URL+"/", "test", "GET", "assignments", "", &map[string]any{})
	if e == nil || strings.Contains(e.Error(), "token-secret") {
		t.Fatal("unsafe error")
	}
}

func TestCompatibleReleaseRequiresServerChallenge(t *testing.T) {
	for _, challenge := range []string{"", "xsrf-test"} {
		t.Run(challenge, func(t *testing.T) {
			posts := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					json.NewEncoder(w).Encode(map[string]string{"token": challenge})
					return
				}
				posts++
				if r.Method != "POST" || r.Header.Get("X-Goog-Colab-Token") != challenge {
					t.Error("invalid release handshake")
				}
			}))
			defer srv.Close()
			e := releaseAssignment(srv.Client(), srv.URL+"/", "test-token", "vm-example")
			if challenge == "" {
				if e == nil || posts != 0 {
					t.Fatal("released without challenge")
				}
			} else if e != nil || posts != 1 {
				t.Fatal("release handshake failed", e)
			}
		})
	}
}
