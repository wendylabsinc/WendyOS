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
	for _, tc := range []struct {
		vendor string
		caps   *agentpb.GpuCapabilities
		want   string
	}{
		{"broadcom", &agentpb.GpuCapabilities{}, "false"},
		{"nvidia", &agentpb.GpuCapabilities{}, "false"},
		{"nvidia", &agentpb.GpuCapabilities{ComputeBackends: []string{"cuda"}}, "true"},
		{"amd", &agentpb.GpuCapabilities{ComputeBackends: []string{"rocm"}}, "false"},
		{"apple", &agentpb.GpuCapabilities{ComputeBackends: []string{"metal"}}, "false"},
		{"nvidia", nil, "true"}, {"", nil, "false"},
	} {
		args := map[string]string{}
		applyDeviceBuildArgHints(args, &agentpb.GetAgentVersionResponse{GpuVendor: &tc.vendor, GpuCapabilities: tc.caps})
		if args["WENDY_HAS_CUDA"] != tc.want {
			t.Fatalf("%s %+v: %v", tc.vendor, tc.caps, args)
		}
	}
}

func strptr(s string) *string { return &s }
