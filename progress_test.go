package main

import (
	"strings"
	"testing"
)

func TestModelLoadingUsesActualEngineEvents(t *testing.T) {
	if r := parseModelLoad("server ready\n"); r.Phase != "on_demand" {
		t.Fatal(r)
	}
	if r := parseModelLoad("Requested to load H3Model\nloading weights\n"); r.Phase != "loading" || r.Requested != "H3Model" {
		t.Fatal(r)
	}
	r := parseModelLoad("Requested to load H3Model\nloaded partially; 12000 MB usable, 8000 MB loaded, 4000 MB offloaded\n")
	if r.Phase != "partial" || r.LoadedMiB == nil || *r.LoadedMiB != 8000 {
		t.Fatal(r)
	}
	if r := parseModelLoad("loaded completely; 12000 MB loaded\nCUDA out of memory\n"); r.Phase != "error" {
		t.Fatal(r)
	}
	if r := parseModelLoad("Requested to load H3Model\nloaded completely; 12000 MB loaded\nRequested to load CLIP\n"); r.Phase != "loading" || r.LoadedMiB != nil {
		t.Fatal(r)
	}
}
func TestInvalidTransferCannotCreateFakePercentage(t *testing.T) {
	for _, p := range []TransferProgress{{TotalFiles: 1, FileIndex: 2}, {FileBytes: 100, CurrentBytes: 101}, {TotalBytes: 10, CompletedBytes: 11}, {CurrentFile: "../secret"}, {SpeedBytes: -1}} {
		if validateTransfer(&p) == nil {
			t.Fatal(p)
		}
	}
	p := TransferProgress{TotalFiles: 16, FileIndex: 1, FileBytes: 100, CurrentBytes: 50, TotalBytes: 1000}
	if e := validateTransfer(&p); e != nil {
		t.Fatal(e)
	}
	if m := legacyModel.FindStringSubmatch("MODEL 3 16 model.safetensors\n"); len(m) != 4 || !strings.HasSuffix(m[3], "safetensors") {
		t.Fatal(m)
	}
}
