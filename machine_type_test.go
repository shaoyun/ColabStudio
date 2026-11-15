package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestMachineTypeDefaultValidationAndLock(t *testing.T) {
	if v, e := machineType(""); e != nil || v != "L4" {
		t.Fatal(v, e)
	}
	m := wakeFixture(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("saving type must not contact cloud")
		return nil, nil
	})
	if e := m.setMachineType("A100", false); e != nil {
		t.Fatal(e)
	}
	if m.get().Accelerator != "A100" {
		t.Fatal("type not persisted")
	}
	if e := m.setMachineType("T4", false); e == nil {
		t.Fatal("unsupported type accepted")
	}
	r := m.get()
	r.Operation = "pending"
	m.record = r
	if e := m.setMachineType("L4", true); e == nil {
		t.Fatal("changed uncertain allocation")
	}
	r.Operation = ""
	r.Runtime = "compat/owned"
	m.record = r
	if e := m.setMachineType("L4", true); e == nil {
		t.Fatal("changed allocated VM")
	}
	r.Runtime = ""
	r.Rule.Enabled = true
	m.record = r
	if e := m.setMachineType("H100", false); e == nil {
		t.Fatal("scheduled type changed without confirmation")
	}
	if e := m.setMachineType("H100", true); e != nil {
		t.Fatal(e)
	}
}
func TestSelectedGPUReachesChallengeAndAllocation(t *testing.T) {
	for _, gpu := range []string{"L4", "A100", "H100"} {
		t.Run(gpu, func(t *testing.T) {
			gets, posts := 0, 0
			m := wakeFixture(t, func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "assignments") {
					return wakeResponse(`{"assignments":[]}`), nil
				}
				if r.URL.Query().Get("accelerator") != gpu {
					t.Fatalf("wanted %s, got %s", gpu, r.URL.Query().Get("accelerator"))
				}
				if r.Method == "GET" {
					gets++
					return wakeResponse(fmt.Sprintf(`{"token":"challenge","variant":"GPU","acc":%q}`, gpu)), nil
				}
				posts++
				return wakeResponse(fmt.Sprintf(`{"endpoint":"machine","variant":1,"accelerator":%q}`, gpu)), nil
			})
			if e := m.setMachineType(gpu, true); e != nil {
				t.Fatal(e)
			}
			if e := m.wake(false); e != nil {
				t.Fatal(e)
			}
			if gets != 1 || posts != 1 || m.get().Accelerator != gpu || m.get().State != "allocated" {
				t.Fatalf("GET=%d POST=%d state=%+v", gets, posts, m.get())
			}
		})
	}
}
func TestAssignmentVariantAcceptsBothWireFormats(t *testing.T) {
	for _, input := range []string{`{"token":"challenge","variant":"GPU","acc":"A100"}`, `{"endpoint":"machine","variant":1,"accelerator":"A100"}`} {
		var a assignResponse
		if e := json.Unmarshal([]byte(input), &a); e != nil {
			t.Fatal(e)
		}
		if a.Variant != 1 || a.Accelerator != "A100" {
			t.Fatal("lost variant/accelerator")
		}
	}
}
