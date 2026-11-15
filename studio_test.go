package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPIRequestAndSecretRedaction(t *testing.T) {
	for _, status := range []int{200, 401, 403, 429, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer private-token" {
					t.Error("missing authorization")
				}
				if r.URL.Path != "/runtimes" {
					t.Error("wrong path")
				}
				w.WriteHeader(status)
				if status == 200 {
					w.Write([]byte(`{"runtimes":[{"name":"runtimes/test","connectionInfo":{"token":"secret"}}]}`))
				} else {
					w.Write([]byte(`{"error":"private-token"}`))
				}
			}))
			defer srv.Close()
			var out struct {
				Runtimes []Runtime `json:"runtimes"`
			}
			err := request(context.Background(), srv.Client(), srv.URL+"/", "private-token", "GET", "runtimes", nil, &out)
			if status == 200 {
				if err != nil || len(out.Runtimes) != 1 {
					t.Fatal(err)
				}
			} else if err == nil || strings.Contains(err.Error(), "private-token") {
				t.Fatal("unsafe error", err)
			}
		})
	}
}
func TestRejectUnsafeRuntimeNames(t *testing.T) {
	s := &Studio{}
	for _, v := range []string{"../runtimes/test", "https://evil.test", "runtimes/a/b", "runtimes/"} {
		if s.Release(v, true) == nil {
			t.Fatal("accepted", v)
		}
	}
	if s.Release("runtimes/test", false) == nil {
		t.Fatal("missing confirmation")
	}
}
func TestMalformedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`not json`)) }))
	defer srv.Close()
	var out any
	if request(context.Background(), srv.Client(), srv.URL+"/", "token", "GET", "runtimes", nil, &out) == nil {
		t.Fatal("accepted invalid JSON")
	}
}

func TestRemoteClientConfigValidation(t *testing.T) {
	good := []byte(`{"installed":{"client_id":"764086051850-example.apps.googleusercontent.com","client_secret":"test-only"}}`)
	c, e := parseRemoteConfig(good)
	if e != nil || !c.Remote {
		t.Fatal("expected remote config", e)
	}
	for _, b := range []string{`{}`, `not json`, `{"installed":{"client_id":"own.apps.googleusercontent.com","client_secret":"test-only"}}`} {
		if _, e := parseRemoteConfig([]byte(b)); e == nil {
			t.Fatal("accepted mismatched redirect client")
		}
	}
}
func TestRemoteCodeRequiresActiveFlow(t *testing.T) {
	s := &Studio{}
	if s.SubmitRemoteCode("example") == nil {
		t.Fatal("accepted code without PKCE flow")
	}
}

func TestExistingCredentials(t *testing.T) {
	good := `{"client_id":"example.apps.googleusercontent.com","client_secret":"test","refresh_token":"test","scopes":["https://www.googleapis.com/auth/colaboratory"],"token_uri":"https://oauth2.googleapis.com/token"}`
	if _, e := parseExistingCredential([]byte(good)); e != nil {
		t.Fatal(e)
	}
	for _, bad := range []string{`{}`, `not json`, strings.Replace(good, "colaboratory\"", "colaboratory.readonly\"", 1), strings.Replace(good, "https://oauth2.googleapis.com/token", "https://untrusted.test/token", 1), strings.Replace(good, `"refresh_token":"test"`, `"refresh_token":""`, 1)} {
		if _, e := parseExistingCredential([]byte(bad)); e == nil {
			t.Fatal("accepted incomplete or unsafe credentials")
		}
	}
}

func TestServiceDisabledExplainsProjectInsteadOfLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte(`{"error":{"message":"private-secret","details":[{"reason":"SERVICE_DISABLED"}]}}`))
	}))
	defer srv.Close()
	err := request(context.Background(), srv.Client(), srv.URL+"/", "test", "GET", "runtimes", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "SERVICE_DISABLED") || strings.Contains(err.Error(), "private-secret") {
		t.Fatal("incorrect error handling", err)
	}
}
