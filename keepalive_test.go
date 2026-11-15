package main

import (
	"net/http"
	"strings"
	"testing"
)

// A running keep-alive kernel is left alone and other kernels are never touched.
func TestKeepAliveLeavesRunningKernel(t *testing.T) {
	old := httpClient
	t.Cleanup(func() { httpClient = old })
	httpClient = &http.Client{Transport: serviceRoundTrip(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "api/kernels"):
			return wakeResponse(`[{"id":"user-kernel","execution_state":"idle"},{"id":"mine-kernel","execution_state":"busy"}]`), nil
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "keepalive.json"):
			return wakeResponse(`{"format":"text","content":"{\"id\":\"mine-kernel\"}"}`), nil
		}
		t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		return nil, nil
	})}
	if e := ensureKeepAlive(runtimeProxy{URL: "https://x.r.colab.dev", Token: "t"}, "content/root"); e != nil {
		t.Fatal(e)
	}
}
