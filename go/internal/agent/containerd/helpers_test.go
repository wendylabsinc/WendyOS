package containerd

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func TestComputeChainID_FirstLayer(t *testing.T) {
	diffID := "sha256:abc123"
	got := computeChainID("", diffID)
	if got != diffID {
		t.Errorf("computeChainID(\"\", %q) = %q; want %q", diffID, got, diffID)
	}
}

func TestComputeChainID_WithParent(t *testing.T) {
	parent := "sha256:aaaa"
	diffID := "sha256:bbbb"

	h := sha256.New()
	h.Write([]byte(parent + " " + diffID))
	expected := fmt.Sprintf("sha256:%x", h.Sum(nil))

	got := computeChainID(parent, diffID)
	if got != expected {
		t.Errorf("computeChainID(%q, %q) = %q; want %q", parent, diffID, got, expected)
	}
}

func TestComputeChainID_Chained(t *testing.T) {
	// Simulate chaining three layers.
	diff0 := "sha256:layer0"
	diff1 := "sha256:layer1"
	diff2 := "sha256:layer2"

	chain0 := computeChainID("", diff0)
	if chain0 != diff0 {
		t.Fatalf("chain0 should equal diff0")
	}

	chain1 := computeChainID(chain0, diff1)
	chain2 := computeChainID(chain1, diff2)

	// Verify chain2 is deterministic.
	chain2Again := computeChainID(computeChainID(diff0, diff1), diff2)
	if chain2 != chain2Again {
		t.Errorf("chaining is not deterministic: %q != %q", chain2, chain2Again)
	}

	// Verify it has the sha256: prefix.
	if !strings.HasPrefix(chain2, "sha256:") {
		t.Errorf("chain ID should have sha256: prefix, got %q", chain2)
	}
}

func TestParseRestartPolicyLabel_Simple(t *testing.T) {
	tests := []struct {
		label      string
		wantPolicy string
		wantRetry  int
	}{
		{"no", "no", 0},
		{"unless-stopped", "unless-stopped", 0},
		{"on-failure", "on-failure", 0},
		{"on-failure:5", "on-failure", 5},
		{"on-failure:0", "on-failure", 0},
		{"on-failure:100", "on-failure", 100},
		{"on-failure:abc", "on-failure", 0}, // invalid number falls back to 0
		{"", "", 0},
	}

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			policy, maxRetries := parseRestartPolicyLabel(tt.label)
			if policy != tt.wantPolicy {
				t.Errorf("policy = %q; want %q", policy, tt.wantPolicy)
			}
			if maxRetries != tt.wantRetry {
				t.Errorf("maxRetries = %d; want %d", maxRetries, tt.wantRetry)
			}
		})
	}
}

func TestIsLocalRegistryImage(t *testing.T) {
	tests := []struct {
		name      string
		imageName string
		want      bool
	}{
		{
			name:      "localhost registry",
			imageName: "localhost:5000/sh.wendy.examples.hellopython:latest",
			want:      true,
		},
		{
			name:      "loopback ipv4 registry",
			imageName: "127.0.0.1:5000/example:latest",
			want:      true,
		},
		{
			name:      "loopback ipv6 registry",
			imageName: "[::1]:5000/example:latest",
			want:      true,
		},
		{
			name:      "remote registry",
			imageName: "ghcr.io/wendylabsinc/example:latest",
			want:      false,
		},
		{
			name:      "bare local image",
			imageName: "example:latest",
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isLocalRegistryImage(tt.imageName); got != tt.want {
				t.Errorf("isLocalRegistryImage(%q) = %v; want %v", tt.imageName, got, tt.want)
			}
		})
	}
}

func TestNormalizeImageName(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"python", "docker.io/library/python:latest"},
		{"python:3.11-slim", "docker.io/library/python:3.11-slim"},
		{"library/nginx:1.27", "docker.io/library/nginx:1.27"},
		{"bitnami/redis:7", "docker.io/bitnami/redis:7"},
		// Already-qualified refs pass through (the localhost path needs
		// isLocalRegistryImage to keep working unchanged).
		{"localhost:5000/foo:bar", "localhost:5000/foo:bar"},
		{"127.0.0.1:5000/example:latest", "127.0.0.1:5000/example:latest"},
		{"ghcr.io/wendylabsinc/example:latest", "ghcr.io/wendylabsinc/example:latest"},
		{"gcr.io/google-containers/pause:3.9", "gcr.io/google-containers/pause:3.9"},
		// Digest references.
		{"python@sha256:0000000000000000000000000000000000000000000000000000000000000000", "docker.io/library/python@sha256:0000000000000000000000000000000000000000000000000000000000000000"},
		// Whitespace trimmed.
		{"  python:3.11-slim  ", "docker.io/library/python:3.11-slim"},
		// Empty input → unchanged.
		{"", ""},
		// Malformed → unchanged so the caller's error message stays useful.
		{"not a ref", "not a ref"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := normalizeImageName(tt.in); got != tt.want {
				t.Errorf("normalizeImageName(%q) = %q; want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestGCTimestamp_ValidRFC3339(t *testing.T) {
	ts := gcTimestamp()
	if ts == "" {
		t.Fatal("gcTimestamp returned empty string")
	}

	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatalf("gcTimestamp returned invalid RFC3339: %q: %v", ts, err)
	}

	// Should be within the last few seconds.
	diff := time.Since(parsed)
	if diff < 0 || diff > 5*time.Second {
		t.Errorf("gcTimestamp is not recent (diff = %v)", diff)
	}
}

func TestGCTimestamp_IsUTC(t *testing.T) {
	ts := gcTimestamp()
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Location() != time.UTC {
		t.Errorf("gcTimestamp should be UTC, got %v", parsed.Location())
	}
}

func TestWendyLabels_Basic(t *testing.T) {
	labels := wendyLabels("myapp", "1.0.0", nil, nil)

	if v, ok := labels[labelKeyAppVersion]; !ok {
		t.Error("missing app version label")
	} else if v != "1.0.0" {
		t.Errorf("app version = %q; want %q", v, "1.0.0")
	}

	// No restart policy should mean no restart policy label.
	if _, ok := labels[labelKeyRestartPolicy]; ok {
		t.Error("should not have restart policy label when policy is nil")
	}
}

func TestWendyLabels_WithRestartPolicyUnlessStopped(t *testing.T) {
	rp := &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_UNLESS_STOPPED}
	labels := wendyLabels("app", "2.0", rp, nil)

	if v, ok := labels[labelKeyRestartPolicy]; !ok {
		t.Error("missing restart policy label")
	} else if v != "unless-stopped" {
		t.Errorf("restart policy = %q; want %q", v, "unless-stopped")
	}
}

func TestWendyLabels_WithRestartPolicyOnFailure(t *testing.T) {
	rp := &agentpb.RestartPolicy{
		Mode:                agentpb.RestartPolicyMode_ON_FAILURE,
		OnFailureMaxRetries: 3,
	}
	labels := wendyLabels("app", "1.0", rp, nil)

	if v := labels[labelKeyRestartPolicy]; v != "on-failure:3" {
		t.Errorf("restart policy = %q; want %q", v, "on-failure:3")
	}
}

func TestWendyLabels_WithRestartPolicyNo(t *testing.T) {
	rp := &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_NO}
	labels := wendyLabels("app", "1.0", rp, nil)

	if v := labels[labelKeyRestartPolicy]; v != "no" {
		t.Errorf("restart policy = %q; want %q", v, "no")
	}
}

func TestWendyLabels_WithRestartPolicyDefault(t *testing.T) {
	rp := &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_DEFAULT}
	labels := wendyLabels("app", "1.0", rp, nil)

	if v := labels[labelKeyRestartPolicy]; v != "unless-stopped" {
		t.Errorf("restart policy = %q; want %q (DEFAULT maps to unless-stopped)", v, "unless-stopped")
	}
}

func TestWendyLabels_WithMCPEntitlement(t *testing.T) {
	entitlements := []appconfig.Entitlement{{Type: appconfig.EntitlementMCP, Port: 3000}}
	labels := wendyLabels("app", "1.0", nil, entitlements)
	if v, ok := labels[labelKeyMCPPort]; !ok {
		t.Error("missing mcp port label")
	} else if v != "3000" {
		t.Errorf("mcp port label = %q; want %q", v, "3000")
	}
}

func TestWendyLabels_WithMCPPortZero(t *testing.T) {
	entitlements := []appconfig.Entitlement{{Type: appconfig.EntitlementMCP, Port: 0}}
	labels := wendyLabels("app", "1.0", nil, entitlements)
	if _, ok := labels[labelKeyMCPPort]; ok {
		t.Error("should not have mcp port label when port is 0")
	}
}

func TestWendyLabels_EntitlementsStoredAsKeyValue(t *testing.T) {
	entitlements := []appconfig.Entitlement{
		{Type: appconfig.EntitlementNetwork, Mode: "host"},
		{Type: appconfig.EntitlementGPU},
	}
	labels := wendyLabels("app", "1.0", nil, entitlements)

	cases := []struct {
		key     string
		wantVal string
	}{
		{appconfig.EntitlementAnnotationKeyPrefix + appconfig.EntitlementNetwork, "mode=host"},
		{appconfig.EntitlementAnnotationKeyPrefix + appconfig.EntitlementGPU, ""},
	}
	for _, tc := range cases {
		raw, ok := labels[tc.key]
		if !ok {
			t.Fatalf("missing entitlement label %q", tc.key)
		}
		if raw != tc.wantVal {
			t.Errorf("%q value = %q; want %q", tc.key, raw, tc.wantVal)
		}
	}
}

func TestWendyLabels_DuplicateEntitlementType(t *testing.T) {
	entitlements := []appconfig.Entitlement{
		{Type: appconfig.EntitlementPersist, Name: "data", Path: "/data"},
		{Type: appconfig.EntitlementPersist, Name: "logs", Path: "/logs"},
	}
	labels := wendyLabels("app", "1.0", nil, entitlements)

	for i, want := range entitlements {
		key := fmt.Sprintf("%s%s.%d", appconfig.EntitlementAnnotationKeyPrefix, appconfig.EntitlementPersist, i)
		raw, ok := labels[key]
		if !ok {
			t.Fatalf("missing entitlement label %q", key)
		}
		got := appconfig.ParseEntitlementAnnotation(appconfig.EntitlementPersist, raw)
		if got.Name != want.Name || got.Path != want.Path {
			t.Errorf("%q: got name=%q path=%q; want name=%q path=%q", key, got.Name, got.Path, want.Name, want.Path)
		}
	}
}

func TestWendyLabels_NoEntitlementsLabel(t *testing.T) {
	labels := wendyLabels("app", "1.0", nil, nil)
	for k := range labels {
		if strings.HasPrefix(k, appconfig.EntitlementAnnotationKeyPrefix) {
			t.Errorf("should not have entitlement label when entitlements are empty, got %q", k)
		}
	}
}

func TestRestartPolicyToLabel_Nil(t *testing.T) {
	got := restartPolicyToLabel(nil)
	if got != "" {
		t.Errorf("restartPolicyToLabel(nil) = %q; want empty", got)
	}
}

func TestRestartPolicyToLabel_OnFailureNoRetries(t *testing.T) {
	rp := &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_ON_FAILURE}
	got := restartPolicyToLabel(rp)
	if got != "on-failure" {
		t.Errorf("restartPolicyToLabel = %q; want %q", got, "on-failure")
	}
}

func TestParseEntitlementsFromAnnotations_Single(t *testing.T) {
	annotations := map[string]string{
		"sh.wendy/entitlement.network": "mode=host",
		"sh.wendy/entitlement.gpu":     "",
	}
	got := parseEntitlementsFromAnnotations(annotations)

	if len(got) != 2 {
		t.Fatalf("want 2 entitlements, got %d", len(got))
	}
	// Sorted alphabetically: gpu, network.
	if got[0].Type != appconfig.EntitlementGPU {
		t.Errorf("got[0].Type = %q; want %q", got[0].Type, appconfig.EntitlementGPU)
	}
	if got[1].Type != appconfig.EntitlementNetwork || got[1].Mode != "host" {
		t.Errorf("got[1] = %+v; want type=network mode=host", got[1])
	}
}

func TestParseEntitlementsFromAnnotations_MultipleOfSameType(t *testing.T) {
	annotations := map[string]string{
		"sh.wendy/entitlement.persist.0": "name=data,path=/data",
		"sh.wendy/entitlement.persist.1": "name=logs,path=/logs",
	}
	got := parseEntitlementsFromAnnotations(annotations)

	if len(got) != 2 {
		t.Fatalf("want 2 entitlements, got %d", len(got))
	}
	if got[0].Name != "data" || got[0].Path != "/data" {
		t.Errorf("got[0] = %+v; want name=data path=/data", got[0])
	}
	if got[1].Name != "logs" || got[1].Path != "/logs" {
		t.Errorf("got[1] = %+v; want name=logs path=/logs", got[1])
	}
}

func TestParseEntitlementsFromAnnotations_RoundTrip(t *testing.T) {
	original := []appconfig.Entitlement{
		{Type: appconfig.EntitlementNetwork, Mode: "host"},
		{Type: appconfig.EntitlementPersist, Name: "data", Path: "/data"},
		{Type: appconfig.EntitlementPersist, Name: "logs", Path: "/logs"},
		{Type: appconfig.EntitlementGPU},
	}

	labels := wendyLabels("app", "1.0", nil, original)
	annotations := make(map[string]string)
	for k, v := range labels {
		if strings.HasPrefix(k, appconfig.EntitlementAnnotationKeyPrefix) {
			annotations[k] = v
		}
	}

	parsed := parseEntitlementsFromAnnotations(annotations)
	if len(parsed) != len(original) {
		t.Fatalf("round-trip: got %d entitlements, want %d", len(parsed), len(original))
	}

	byType := make(map[string][]appconfig.Entitlement)
	for _, e := range parsed {
		byType[e.Type] = append(byType[e.Type], e)
	}
	if len(byType[appconfig.EntitlementNetwork]) != 1 || byType[appconfig.EntitlementNetwork][0].Mode != "host" {
		t.Errorf("network entitlement round-trip failed: %+v", byType[appconfig.EntitlementNetwork])
	}
	if len(byType[appconfig.EntitlementPersist]) != 2 {
		t.Errorf("persist entitlement round-trip failed: %+v", byType[appconfig.EntitlementPersist])
	}
	if len(byType[appconfig.EntitlementGPU]) != 1 {
		t.Errorf("gpu entitlement round-trip failed: %+v", byType[appconfig.EntitlementGPU])
	}
}

func TestParseEntitlementsFromAnnotations_Empty(t *testing.T) {
	if got := parseEntitlementsFromAnnotations(nil); len(got) != 0 {
		t.Errorf("nil annotations: want empty, got %v", got)
	}
	if got := parseEntitlementsFromAnnotations(map[string]string{"unrelated": "value"}); len(got) != 0 {
		t.Errorf("unrelated annotations: want empty, got %v", got)
	}
}
