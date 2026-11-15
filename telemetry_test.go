package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestTelemetryHistoryValidatesOwnerAgesAndUnknownValues(t *testing.T) {
	now := time.Now()
	r := ServiceRecord{ID: "owned", Binding: "account"}
	bad := 101.0
	good := 37.5
	h := ResourceHistory{Owner: r.ID, Binding: r.Binding, Samples: []ResourceSample{{Time: float64(now.Unix() - 901)}, {Time: float64(now.Unix() - 30), CPU: &bad, GPU: &good}, {Time: float64(now.Unix() - 30), CPU: &good}, {Time: float64(now.Unix()), CPU: &good}, {Time: float64(now.Unix() + 60)}}}
	samples, e := validSamples(h, r, now)
	if e != nil || len(samples) != 2 || samples[0].CPU != nil || *samples[0].GPU != 37.5 || *samples[1].CPU != 37.5 {
		t.Fatalf("incorrect filtering: %+v %v", samples, e)
	}
	h.Binding = "other"
	if _, e = validSamples(h, r, now); e == nil {
		t.Fatal("accepted foreign history")
	}
}
func TestTelemetryReadsExistingSamplerWithoutExecutingKernel(t *testing.T) {
	requests := 0
	m := wakeFixture(t, func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Method != "GET" {
			t.Fatalf("poll executed a mutation: %s", r.Method)
		}
		if strings.HasSuffix(r.URL.Path, "assignments") {
			return wakeResponse(`{"assignments":[{"endpoint":"owned","runtimeProxyInfo":{"url":"https://owned.r.colab.dev","token":"proxy"}}]}`), nil
		}
		if strings.HasSuffix(r.URL.Path, "metrics.json") {
			data, _ := json.Marshal(ResourceHistory{Owner: "service", Binding: accountBinding("test@example.com"), Samples: []ResourceSample{{Time: float64(time.Now().Unix())}}})
			body, _ := json.Marshal(map[string]string{"format": "text", "content": string(data)})
			return wakeResponse(string(body)), nil
		}
		return nil, fmt.Errorf("unexpected %s", r.URL.Path)
	})
	m.record = ServiceRecord{ID: "service", Binding: accountBinding("test@example.com"), Runtime: "compat/b3duZWQ"}
	m.studio.svc = m
	m.studio.svcOnce.Do(func() {})
	h, e := m.studio.ResourceHistory(m.record.Runtime)
	if e != nil || len(h.Samples) != 1 || !h.Managed || requests != 2 {
		t.Fatalf("unexpected history: %+v %v requests=%d", h, e, requests)
	}
}
func TestTelemetryUnownedDoesNotTouchRemoteOrInstall(t *testing.T) {
	m := wakeFixture(t, func(r *http.Request) (*http.Response, error) {
		t.Fatalf("unowned telemetry made request: %s", r.URL)
		return nil, nil
	})
	m.studio.svc = m
	m.studio.svcOnce.Do(func() {})
	h, e := m.studio.ResourceHistory("compat/unknown")
	if e != nil || h.Managed || h.Message == "" {
		t.Fatalf("unowned monitoring: %+v %v", h, e)
	}
}
