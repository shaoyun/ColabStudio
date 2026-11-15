package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWebCredentialNeverContainsPlaintextPassword(t *testing.T) {
	c, e := makeWebAccessConfig("owned", "fixture-password")
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(c)
	if strings.Contains(string(b), "fixture-password") || len(c.Hash) != 64 || len(c.Salt) != 32 || len(c.SessionKey) != 64 || c.Iterations != 210000 {
		t.Fatal("invalid password verifier")
	}
	next, e := makeWebAccessConfig("owned", "fixture-password")
	if e != nil {
		t.Fatal(e)
	}
	if c.SessionKey == next.SessionKey || c.Revision == next.Revision || c.Salt == next.Salt {
		t.Fatal("session credentials reused")
	}
}

func TestWebProtectionRequiresMatchingFreshConfirmation(t *testing.T) {
	old := httpClient
	t.Cleanup(func() { httpClient = old })
	r := ServiceRecord{ID: "owned", RemoteRoot: "content/owned", Runtime: "compat/test"}
	for _, test := range []struct {
		name            string
		owner, revision string
		when            float64
		want            bool
	}{{"valid", "owned", "current", float64(time.Now().Unix()), true}, {"wrong-owner", "other", "current", float64(time.Now().Unix()), false}, {"old-config", "owned", "old", float64(time.Now().Unix()), false}, {"stale", "owned", "current", float64(time.Now().Add(-time.Hour).Unix()), false}, {"future", "owned", "current", float64(time.Now().Add(time.Hour).Unix()), false}} {
		t.Run(test.name, func(t *testing.T) {
			httpClient = &http.Client{Transport: serviceRoundTrip(func(req *http.Request) (*http.Response, error) {
				value := any(map[string]any{"owner": "owned", "revision": "current"})
				if strings.HasSuffix(req.URL.Path, "auth-status.json") {
					value = map[string]any{"owner": test.owner, "revision": test.revision, "protected": true, "updated": test.when}
				}
				text, _ := json.Marshal(value)
				body, _ := json.Marshal(map[string]string{"format": "text", "content": string(text)})
				return wakeResponse(string(body)), nil
			})}
			status, e := webAccessStatus(runtimeProxy{URL: "https://owned.r.colab.dev", Token: "proxy-secret"}, r)
			if e != nil || status.Protected != test.want {
				t.Fatal("unconfirmed protection accepted")
			}
		})
	}
}

func TestAPITokenStoredEncryptedAndHashedInRemoteConfig(t *testing.T) {
	oldStore := store
	t.Cleanup(func() { store = oldStore })
	backend := &countingKeyStore{}
	store = newCredentialVault(filepath.Join(t.TempDir(), "credentials.enc"), backend)

	token, err := apiToken("owned")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "sk-toybox-") || len(token) < 24 {
		t.Fatalf("unexpected api token format: %q", token)
	}
	again, err := apiToken("owned")
	if err != nil {
		t.Fatal(err)
	}
	if again != token {
		t.Fatal("apiToken should be idempotent for same instance id")
	}
	if backend.writes != 1 || backend.reads != 0 {
		t.Fatalf("expected single keychain write without extra reads, got reads=%d writes=%d", backend.reads, backend.writes)
	}

	pw, err := webPassword("owned")
	if err != nil {
		t.Fatal(err)
	}
	if pw == token {
		t.Fatal("web password and API token must be distinct secrets")
	}

	cfg, err := makeWebAccessConfig("owned", pw, token)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APITokenHash != hashAPIToken(token) || len(cfg.APITokenHash) != 64 {
		t.Fatalf("unexpected api_token_hash: %q", cfg.APITokenHash)
	}
	raw, _ := json.Marshal(cfg)
	if strings.Contains(string(raw), token) || strings.Contains(string(raw), pw) {
		t.Fatal("remote web-auth config must never serialize plaintext API token or password")
	}
}

func TestAPIProtectionStatusConfirmation(t *testing.T) {
	old := httpClient
	t.Cleanup(func() { httpClient = old })
	r := ServiceRecord{ID: "owned", RemoteRoot: "content/owned", Runtime: "compat/test", URL: "https://valid-demo.trycloudflare.com"}
	for _, tc := range []struct {
		name         string
		tokenHash    string
		apiProtected bool
		wantAPI      bool
	}{
		{"enabled", hashAPIToken("sk-toybox-demo"), true, true},
		{"legacy-without-token-hash", "", true, false},
		{"unconfirmed-marker", hashAPIToken("sk-toybox-demo"), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			httpClient = &http.Client{Transport: serviceRoundTrip(func(req *http.Request) (*http.Response, error) {
				var value any
				if strings.HasSuffix(req.URL.Path, "auth-status.json") {
					value = map[string]any{
						"owner":         "owned",
						"revision":      "current",
						"protected":     true,
						"api_protected": tc.apiProtected,
						"updated":       float64(time.Now().Unix()),
					}
				} else {
					value = map[string]any{
						"owner":          "owned",
						"revision":       "current",
						"api_token_hash": tc.tokenHash,
					}
				}
				text, _ := json.Marshal(value)
				body, _ := json.Marshal(map[string]string{"format": "text", "content": string(text)})
				return wakeResponse(string(body)), nil
			})}
			status, err := webAccessStatus(runtimeProxy{URL: "https://owned.r.colab.dev", Token: "proxy-secret"}, r)
			if err != nil {
				t.Fatal(err)
			}
			if !status.Protected || status.APIProtected != tc.wantAPI {
				t.Fatalf("got Protected=%v APIProtected=%v, want Protected=true APIProtected=%v", status.Protected, status.APIProtected, tc.wantAPI)
			}
			if status.APIBaseURL != "https://valid-demo.trycloudflare.com/v1" {
				t.Fatalf("unexpected APIBaseURL: %q", status.APIBaseURL)
			}
		})
	}
}
