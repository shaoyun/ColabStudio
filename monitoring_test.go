package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type monitorTransport func(*http.Request) (*http.Response, error)

func (f monitorTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestResourceProxySafetyAndMissingMetrics(t *testing.T) {
	p := runtimeProxy{URL: "https://example.r.colab.dev", Token: "private-proxy-token"}
	client := &http.Client{Transport: monitorTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/api/colab/resources" || r.Header.Get("X-Colab-Runtime-Proxy-Token") != "private-proxy-token" || strings.Contains(r.URL.String(), "private-proxy-token") {
			t.Fatal("incorrect request")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(")]}'\n" + `{"memory":{"totalBytes":1024,"freeBytes":512},"gpus":[{"gpuUtilization":0}],"disks":[]}`)), Header: make(http.Header)}, nil
	})}
	var result Resources
	if e := proxyRequest(client, p, "GET", "api/colab/resources", nil, &result); e != nil {
		t.Fatal(e)
	}
	if result.Memory.Total == nil || *result.Memory.Total != 1024 || result.GPUs[0].Utilization == nil || *result.GPUs[0].Utilization != 0 || result.GPUs[0].Total != nil {
		t.Fatal("missing versus zero lost")
	}
	for _, url := range []string{"http://example.r.colab.dev", "https://evil.test", "https://example.r.colab.dev.evil.test", "https://user@example.r.colab.dev", "https://example.r.colab.dev:444"} {
		if _, e := proxyURL(runtimeProxy{url, "secret"}, "api/kernels"); e == nil {
			t.Fatal("unsafe proxy accepted")
		}
	}
}
func TestProxyErrorsRedactBodyAndUsageMissingIsNotZero(t *testing.T) {
	client := &http.Client{Transport: monitorTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("private-token")), Header: make(http.Header)}, nil
	})}
	if e := proxyRequest(client, runtimeProxy{"https://example.r.colab.dev", "secret"}, "GET", "api/colab/resources", nil, &Resources{}); e == nil || strings.Contains(e.Error(), "private-token") {
		t.Fatal("unsafe error")
	}
	var u Usage
	if e := json.Unmarshal([]byte(`{"currentBalance":0}`), &u); e != nil || u.Balance == nil || *u.Balance != 0 || u.Rate != nil {
		t.Fatal("unknown balance conflated with zero")
	}
}

func TestCPUOnlyUsesIdleKernel(t *testing.T) {
	id, e := selectIdleKernel(nil)
	if e != nil || id != "" {
		t.Fatal("empty instance should allow a temporary diagnostic kernel")
	}
	id, e = selectIdleKernel([]diagnosticKernel{{ID: "user-kernel", State: "idle"}})
	if e != nil || id != "user-kernel" {
		t.Fatal("idle kernel should be reused")
	}
	if id, e = selectIdleKernel([]diagnosticKernel{{ID: "user-kernel", State: "busy"}}); e != nil || id != "" {
		t.Fatal("busy kernel must not receive diagnostic code; use a temporary kernel")
	}
	if id, e = selectIdleKernel([]diagnosticKernel{{ID: "../bad", State: "idle"}}); e != nil || id != "" {
		t.Fatal("unsafe kernel name must never be used")
	}
}

func TestCPUChannelUsesColabProxyProtocol(t *testing.T) {
	c, e := diagnosticSocketConfig(runtimeProxy{"https://example.r.colab.dev", "proxy-test-token"}, "kernel-test", "session-test")
	if e != nil {
		t.Fatal(e)
	}
	q := c.Location.Query()
	if c.Location.Scheme != "wss" || q.Get("colab-runtime-proxy-token") != "proxy-test-token" || q.Get("token") != "proxy-test-token" || q.Get("session_id") != "session-test" || c.Header.Get("X-Colab-Runtime-Proxy-Token") != "proxy-test-token" {
		t.Fatal("missing Colab WebSocket authentication or session routing")
	}
	if _, e = diagnosticSocketConfig(runtimeProxy{"https://example.r.colab.dev", "private"}, "../bad", "session"); e == nil {
		t.Fatal("unsafe kernel identifier")
	}
}
