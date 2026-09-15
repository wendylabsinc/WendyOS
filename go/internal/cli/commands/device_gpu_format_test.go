package commands

import (
	"reflect"
	"testing"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestFormatGPUCompute(t *testing.T) {
	for name, tc := range map[string]struct {
		gpus []*agentpb.GpuCapabilities
		want string
	}{
		"older agent":      {nil, ""},
		"single with cuda": {[]*agentpb.GpuCapabilities{{Vendor: "nvidia", Path: "/dev/nvidia0", ComputeBackends: []string{"cuda"}}}, "cuda"},
		"single without":   {[]*agentpb.GpuCapabilities{{Vendor: "broadcom", Path: "/dev/dri/card0"}}, "none detected"},
		"two vendors": {[]*agentpb.GpuCapabilities{
			{Vendor: "nvidia", Path: "/dev/dri/card0", ComputeBackends: []string{"cuda"}},
			{Vendor: "amd", Path: "/dev/dri/card1", ComputeBackends: []string{"rocm"}},
		}, "cuda (nvidia /dev/dri/card0); rocm (amd /dev/dri/card1)"},
		"two with one bare": {[]*agentpb.GpuCapabilities{
			{Vendor: "intel", Path: "/dev/dri/card0"},
			{Vendor: "nvidia", Path: "/dev/dri/card1", ComputeBackends: []string{"cuda"}},
		}, "none (intel /dev/dri/card0); cuda (nvidia /dev/dri/card1)"},
		"unknown vendor and no path": {[]*agentpb.GpuCapabilities{{}, {Vendor: "amd", ComputeBackends: []string{"rocm"}}}, "none (unknown); rocm (amd)"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := formatGPUCompute(tc.gpus); got != tc.want {
				t.Fatalf("formatGPUCompute() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGPUCapabilitiesJSON(t *testing.T) {
	got := gpuCapabilitiesJSON([]*agentpb.GpuCapabilities{
		{Vendor: "nvidia", Path: "/dev/dri/card0", ComputeBackends: []string{"cuda"}},
		{Vendor: "broadcom", Path: "/dev/dri/card1"},
	})
	want := []map[string]any{
		{"vendor": "nvidia", "path": "/dev/dri/card0", "computeBackends": []string{"cuda"}},
		{"vendor": "broadcom", "path": "/dev/dri/card1", "computeBackends": []string{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gpuCapabilitiesJSON() = %#v, want %#v", got, want)
	}
}
