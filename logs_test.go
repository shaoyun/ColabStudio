package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func logResponse(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
func TestLogsReadOnlyTailAndRedaction(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: monitorTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "GET" {
			t.Fatal("unexpected mutation")
		}
		if r.Header.Get("X-Colab-Runtime-Proxy-Token") != "private-proxy-token" {
			t.Fatal("missing proxy authentication")
		}
		switch r.URL.Path {
		case "/api/contents/content/easycolab_server.log":
			if r.URL.Query().Get("content") != "0" {
				t.Fatal("downloaded full contents")
			}
			return logResponse(200, `{"type":"file","size":100000}`), nil
		case "/files/content/easycolab_server.log":
			if r.Header.Get("Range") != "bytes=-65536" {
				t.Fatal("missing bounded tail")
			}
			return logResponse(206, "partial line\nservice started\napi_key=secret-value\nAuthorization: Bearer private-value\n"), nil
		}
		t.Fatal("unexpected endpoint")
		return nil, nil
	})}
	r, e := readInstanceLog(client, runtimeProxy{"https://vm.r.colab.dev", "private-proxy-token"}, 2)
	if e != nil {
		t.Fatal(e)
	}
	if calls != 2 || r.Lines != 2 || !r.Truncated || strings.Contains(r.Text, "secret-value") || strings.Contains(r.Text, "private-value") || !strings.Contains(r.Text, "[已隐藏]") {
		t.Fatal("incorrect tail or unsafe text")
	}
}
func TestLogsMissingAndRootFallback(t *testing.T) {
	for _, found := range []bool{true, false} {
		client := &http.Client{Transport: monitorTransport(func(r *http.Request) (*http.Response, error) {
			if r.Method != "GET" {
				t.Fatal("unexpected mutation")
			}
			if found && r.URL.Path == "/api/contents/easycolab_server.log" {
				return logResponse(200, `{"type":"file","size":0}`), nil
			}
			return logResponse(404, "private"), nil
		})}
		r, e := readInstanceLog(client, runtimeProxy{"https://vm.r.colab.dev", "private"}, 300)
		if e != nil || r.Missing == found || r.Message == "" {
			t.Fatal("missing-file state incorrect", e)
		}
	}
}
func TestLogsErrorsAndSizeLimit(t *testing.T) {
	for _, status := range []int{200, 403} {
		client := &http.Client{Transport: monitorTransport(func(r *http.Request) (*http.Response, error) {
			if strings.HasPrefix(r.URL.Path, "/api/contents/") {
				return logResponse(200, `{"type":"file","size":1000000}`), nil
			}
			if status == 403 {
				return logResponse(403, "private-token"), nil
			}
			return logResponse(200, strings.Repeat("x", maxLogBytes+1000)), nil
		})}
		_, e := readInstanceLog(client, runtimeProxy{"https://vm.r.colab.dev", "private"}, 300)
		if e == nil || strings.Contains(e.Error(), "private-token") {
			t.Fatal("unsafe error or unbounded download accepted")
		}
	}
	if _, e := readInstanceLog(nil, runtimeProxy{}, 2001); e == nil {
		t.Fatal("unbounded line request")
	}
}
func TestLogSanitizationAndTail(t *testing.T) {
	original := "\x1b[31merror\x1b[0m\npassword: password-value\n{\"client_secret\":\"client-value\"}\nhttps://example.test?token=query-value&ok=1\nknown-exact-token"
	clean := redactLog(original, "known-exact-token")
	for _, secret := range []string{"password-value", "client-value", "query-value", "known-exact-token", "\x1b"} {
		if strings.Contains(clean, secret) {
			t.Fatal("secret or ANSI escaped sanitizer")
		}
	}
	text, n, truncated := tailLog("first\r\nsecond\nthird\n", 2, false)
	if text != "second\nthird" || n != 2 || !truncated {
		t.Fatal("incorrect line trimming")
	}
}

func TestLogRedactionCoversToyboxAPITokenAndQueryKey(t *testing.T) {
	rawToken := "sk-toybox-secretTokenValue123456"
	original := strings.Join([]string{
		"Authorization: Bearer bearer-secret-999",
		"X-API-Key: header-api-secret",
		"x-goog-api-key: goog-api-secret",
		"GET /v1/images/files/openai_img_1.png?api_key=query-api-key-val HTTP/1.1",
		"GET /v1/videos/vid_1/content?token=query-token-val HTTP/1.1",
		"leaked " + rawToken + " inside logs",
	}, "\n")
	clean := redactLog(original, rawToken)
	for _, secret := range []string{
		rawToken,
		"bearer-secret-999",
		"header-api-secret",
		"goog-api-secret",
		"query-api-key-val",
		"query-token-val",
	} {
		if strings.Contains(clean, secret) {
			t.Fatalf("secret %q leaked into redacted log: %s", secret, clean)
		}
	}
	for _, preserved := range []string{"/v1/images/files/openai_img_1.png", "/v1/videos/vid_1/content"} {
		if !strings.Contains(clean, preserved) {
			t.Fatalf("expected path %q preserved in redacted log: %s", preserved, clean)
		}
	}
}
