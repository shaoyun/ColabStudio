package main

import (
	"encoding/json"
	"net/http"
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
