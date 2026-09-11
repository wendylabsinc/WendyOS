package commands

import (
	"testing"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// TestApplyDeviceBuildArgHints_SkipsUnsafeJetpackVersion reproduces the watch
// failure where a Jetson running an L4T release the agent's JetPack table does
// not map reports a fallback like "L4T 38.2.0". The space made the whole deploy
// abort on build-arg validation; the hint must now be skipped, not fatal, while
// the well-formed hints still pass through.
func TestApplyDeviceBuildArgHints_SkipsUnsafeJetpackVersion(t *testing.T) {
	resp := &agentpb.GetAgentVersionResponse{
		DeviceType:     strptr("nvidia-jetson"),
		HasGpu:         boolPtr(true),
		GpuVendor:      strptr("nvidia"),
		JetpackVersion: strptr("L4T 38.2.0"),
		CudaVersion:    strptr("13.0"),
	}

	buildArgs := map[string]string{}
	applyDeviceBuildArgHints(buildArgs, resp)

	if _, ok := buildArgs["WENDY_JETPACK_VERSION"]; ok {
		t.Fatalf("expected unsafe WENDY_JETPACK_VERSION to be skipped, got %q", buildArgs["WENDY_JETPACK_VERSION"])
	}
	for key, want := range map[string]string{
		"WENDY_DEVICE_TYPE":  "nvidia-jetson",
		"WENDY_HAS_GPU":      "true",
		"WENDY_GPU_VENDOR":   "nvidia",
		"WENDY_CUDA_VERSION": "13.0",
	} {
		if got := buildArgs[key]; got != want {
			t.Errorf("buildArgs[%q] = %q, want %q", key, got, want)
		}
	}
}

// TestApplyDeviceBuildArgHints_PassesMappedJetpackVersion confirms a normal
// table-mapped version (e.g. "6.1") still propagates.
func TestApplyDeviceBuildArgHints_PassesMappedJetpackVersion(t *testing.T) {
	resp := &agentpb.GetAgentVersionResponse{
		JetpackVersion: strptr("6.1"),
	}
	buildArgs := map[string]string{}
	applyDeviceBuildArgHints(buildArgs, resp)
	if got := buildArgs["WENDY_JETPACK_VERSION"]; got != "6.1" {
		t.Fatalf("WENDY_JETPACK_VERSION = %q, want %q", got, "6.1")
	}
}

// TestApplyDeviceBuildArgHints_DerivesJetpackMajor confirms the coarse
// WENDY_JETPACK_MAJOR selector is derived from a clean version ("7.2" -> "7")
// and omitted for an unmapped "L4T ..." fallback.
func TestApplyDeviceBuildArgHints_DerivesJetpackMajor(t *testing.T) {
	clean := map[string]string{}
	applyDeviceBuildArgHints(clean, &agentpb.GetAgentVersionResponse{JetpackVersion: strptr("7.2")})
	if got := clean["WENDY_JETPACK_MAJOR"]; got != "7" {
		t.Fatalf("WENDY_JETPACK_MAJOR = %q, want %q", got, "7")
	}

	fallback := map[string]string{}
	applyDeviceBuildArgHints(fallback, &agentpb.GetAgentVersionResponse{JetpackVersion: strptr("L4T 39.2.0")})
	if got, ok := fallback["WENDY_JETPACK_MAJOR"]; ok {
		t.Fatalf("expected WENDY_JETPACK_MAJOR omitted for L4T fallback, got %q", got)
	}
}

// TestApplyDeviceBuildArgHints_OmitsUnreportedHints confirms older agents that
// do not report a field leave the ARG default untouched (no empty value set).
func TestApplyDeviceBuildArgHints_OmitsUnreportedHints(t *testing.T) {
	buildArgs := map[string]string{}
	applyDeviceBuildArgHints(buildArgs, &agentpb.GetAgentVersionResponse{})
	if len(buildArgs) != 1 || buildArgs["WENDY_HAS_CUDA"] != "false" {
		t.Fatalf("expected conservative CUDA hint for empty response, got %v", buildArgs)
	}
}

func TestCUDAHintCapabilitiesAndLegacyFallback(t *testing.T) {
	nvidiaCUDA := &agentpb.GpuCapabilities{Vendor: "nvidia", Path: "/dev/dri/card1", ComputeBackends: []string{"cuda"}}
	amdROCm := &agentpb.GpuCapabilities{Vendor: "amd", Path: "/dev/dri/card0", ComputeBackends: []string{"rocm"}}
	for _, tc := range []struct {
		name   string
		vendor string
		gpus   []*agentpb.GpuCapabilities
		want   string
	}{
		{"gpu without compute", "broadcom", []*agentpb.GpuCapabilities{{Vendor: "broadcom"}}, "false"},
		{"nvidia without driver evidence", "nvidia", []*agentpb.GpuCapabilities{{Vendor: "nvidia"}}, "false"},
		{"nvidia with cuda", "nvidia", []*agentpb.GpuCapabilities{nvidiaCUDA}, "true"},
		{"amd with rocm", "amd", []*agentpb.GpuCapabilities{amdROCm}, "false"},
		{"apple with metal", "apple", []*agentpb.GpuCapabilities{{Vendor: "apple", ComputeBackends: []string{"metal"}}}, "false"},
		// The legacy vendor names the AMD card; cuda on the second GPU still counts.
		{"amd and nvidia", "amd", []*agentpb.GpuCapabilities{amdROCm, nvidiaCUDA}, "true"},
		{"older agent, nvidia", "nvidia", nil, "true"},
		{"older agent, no vendor", "", nil, "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]string{}
			applyDeviceBuildArgHints(args, &agentpb.GetAgentVersionResponse{GpuVendor: &tc.vendor, GpuCapabilities: tc.gpus})
			if args["WENDY_HAS_CUDA"] != tc.want {
				t.Fatalf("WENDY_HAS_CUDA = %q, want %q (%v)", args["WENDY_HAS_CUDA"], tc.want, args)
			}
		})
	}
}

func strptr(s string) *string { return &s }
