package containerd

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestContainerName_SingleContainer(t *testing.T) {
	// Single-container apps: name must equal the appID unchanged.
	if got := ContainerName("com.example.app", ""); got != "com.example.app" {
		t.Errorf("ContainerName(%q, %q) = %q; want %q", "com.example.app", "", got, "com.example.app")
	}
}

func TestContainerName_MultiService(t *testing.T) {
	got := ContainerName("com.example.app", "api")
	want := "com.example.app_api"
	if got != want {
		t.Errorf("ContainerName(%q, %q) = %q; want %q", "com.example.app", "api", got, want)
	}
}

func TestContainerName_ValidContainerdID(t *testing.T) {
	// Containerd identifiers must match ^[A-Za-z0-9]+(?:[._-](?:[A-Za-z0-9]+))*$
	// (max 76 chars). Verify multi-service names pass this constraint.
	containerdRe := regexp.MustCompile(`^[A-Za-z0-9]+(?:[._-](?:[A-Za-z0-9]+))*$`)
	cases := []struct{ appID, svc string }{
		{"sh.wendy.examples.hellocompose", "api"},
		{"com.example.myapp", "camera"},
		{"sh.wendy.robot", "slam"},
	}
	for _, tc := range cases {
		name := ContainerName(tc.appID, tc.svc)
		if !containerdRe.MatchString(name) {
			t.Errorf("ContainerName(%q, %q) = %q does not match containerd identifier regex", tc.appID, tc.svc, name)
		}
		if len(name) > 76 {
			t.Errorf("ContainerName(%q, %q) = %q exceeds containerd max length 76", tc.appID, tc.svc, name)
		}
	}
}

func TestSnapshotKey_SingleContainer(t *testing.T) {
	// Single-container apps: snapshot key must equal "wendy-{appID}" unchanged.
	got := SnapshotKey("com.example.app", "")
	want := "wendy-com.example.app"
	if got != want {
		t.Errorf("SnapshotKey(%q, %q) = %q; want %q", "com.example.app", "", got, want)
	}
}

func TestSnapshotKey_MultiService(t *testing.T) {
	got := SnapshotKey("com.example.app", "api")
	want := "wendy-com.example.app@api"
	if got != want {
		t.Errorf("SnapshotKey(%q, %q) = %q; want %q", "com.example.app", "api", got, want)
	}
}

func TestSnapshotKey_NoSlash(t *testing.T) {
	// Snapshot keys must never contain a slash (filesystem safety).
	key := SnapshotKey("com.example.app", "worker")
	if strings.Contains(key, "/") {
		t.Errorf("SnapshotKey must not contain '/'; got %q", key)
	}
}

func TestSnapshotKey_NoCollision(t *testing.T) {
	// "wendy-foo-bar@baz" must differ from "wendy-foo@bar-baz": without "@
	// separation a "-" separator would make both produce "wendy-foo-bar-baz".
	a := SnapshotKey("foo-bar", "baz")
	b := SnapshotKey("foo", "bar-baz")
	if a == b {
		t.Errorf("SnapshotKey collision: SnapshotKey(%q,%q) == SnapshotKey(%q,%q) == %q",
			"foo-bar", "baz", "foo", "bar-baz", a)
	}
}

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
	labels := wendyLabels("myapp", "", "1.0.0", nil, nil, "", nil)

	if v, ok := labels[labelKeyAppVersion]; !ok {
		t.Error("missing app version label")
	} else if v != "1.0.0" {
		t.Errorf("app version = %q; want %q", v, "1.0.0")
	}

	// No restart policy should mean no restart policy label.
	if _, ok := labels[labelKeyRestartPolicy]; ok {
		t.Error("should not have restart policy label when policy is nil")
	}

	// Single-container apps must not get a service label.
	if _, ok := labels[labelKeyServiceName]; ok {
		t.Error("single-container app must not have service label")
	}
}

func TestWendyLabels_MultiService(t *testing.T) {
	labels := wendyLabels("com.example.app", "api", "2.0", nil, nil, "", nil)

	if v := labels[labelKeyServiceName]; v != "api" {
		t.Errorf("service label = %q; want %q", v, "api")
	}
	if v := labels[labelKeyAppVersion]; v != "2.0" {
		t.Errorf("version label = %q; want %q", v, "2.0")
	}
}

// TestWendyLabels_WithIsolation is the regression test for the reboot-networking
// bug: isolation mode must be persisted as a container label so it survives an
// agent restart / device reboot, when c.appIsolation (in-memory only) starts
// out empty (WDY reboot-fix).
func TestWendyLabels_WithIsolation(t *testing.T) {
	labels := wendyLabels("app", "", "1.0", nil, nil, "isolated", nil)
	if v, ok := labels[labelKeyIsolation]; !ok {
		t.Error("missing isolation label")
	} else if v != "isolated" {
		t.Errorf("isolation label = %q; want %q", v, "isolated")
	}
}

// TestWendyLabels_EmptyIsolationOmitsLabel mirrors the "no service label for
// single-container apps" behavior: an app with no declared isolation mode must
// not carry the label at all (distinguishing "not isolated" from "isolated
// mode not yet known").
func TestWendyLabels_EmptyIsolationOmitsLabel(t *testing.T) {
	labels := wendyLabels("app", "", "1.0", nil, nil, "", nil)
	if v, ok := labels[labelKeyIsolation]; ok {
		t.Errorf("should not have isolation label when isolation is empty, got %q", v)
	}
}

func TestWendyLabels_WithRestartPolicyUnlessStopped(t *testing.T) {
	rp := &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_UNLESS_STOPPED}
	labels := wendyLabels("app", "", "2.0", rp, nil, "", nil)

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
	labels := wendyLabels("app", "", "1.0", rp, nil, "", nil)

	if v := labels[labelKeyRestartPolicy]; v != "on-failure:3" {
		t.Errorf("restart policy = %q; want %q", v, "on-failure:3")
	}
}

func TestWendyLabels_WithRestartPolicyNo(t *testing.T) {
	rp := &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_NO}
	labels := wendyLabels("app", "", "1.0", rp, nil, "", nil)

	if v := labels[labelKeyRestartPolicy]; v != "no" {
		t.Errorf("restart policy = %q; want %q", v, "no")
	}
}

func TestWendyLabels_WithRestartPolicyDefault(t *testing.T) {
	rp := &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_DEFAULT}
	labels := wendyLabels("app", "", "1.0", rp, nil, "", nil)

	if v := labels[labelKeyRestartPolicy]; v != "unless-stopped" {
		t.Errorf("restart policy = %q; want %q (DEFAULT maps to unless-stopped)", v, "unless-stopped")
	}
}

func TestWendyLabels_WithMCPEntitlement(t *testing.T) {
	entitlements := []appconfig.Entitlement{{Type: appconfig.EntitlementMCP, Port: 3000}}
	labels := wendyLabels("app", "", "1.0", nil, entitlements, "", nil)
	if v, ok := labels[labelKeyMCPPort]; !ok {
		t.Error("missing mcp port label")
	} else if v != "3000" {
		t.Errorf("mcp port label = %q; want %q", v, "3000")
	}
}

func TestWendyLabels_WithMCPPortZero(t *testing.T) {
	entitlements := []appconfig.Entitlement{{Type: appconfig.EntitlementMCP, Port: 0}}
	labels := wendyLabels("app", "", "1.0", nil, entitlements, "", nil)
	if _, ok := labels[labelKeyMCPPort]; ok {
		t.Error("should not have mcp port label when port is 0")
	}
}

func TestWendyLabels_EntitlementsStoredAsKeyValue(t *testing.T) {
	entitlements := []appconfig.Entitlement{
		{Type: appconfig.EntitlementNetwork, Mode: "host"},
		{Type: appconfig.EntitlementGPU},
	}
	labels := wendyLabels("app", "", "1.0", nil, entitlements, "", nil)

	cases := []struct {
		key     string
		wantVal string
	}{
		{appconfig.EntitlementAnnotationKeyPrefix + appconfig.EntitlementNetwork, "mode=host"},
		{appconfig.EntitlementAnnotationKeyPrefix + appconfig.EntitlementGPU, "enabled=true"},
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

func TestWendyLabels_TypeOnlyEntitlementsUseNonEmptyValues(t *testing.T) {
	entitlements := []appconfig.Entitlement{
		{Type: appconfig.EntitlementCamera},
		{Type: appconfig.EntitlementNotifications},
	}
	labels := wendyLabels("app", "", "1.0", nil, entitlements, "", nil)

	for _, entitlement := range entitlements {
		key := appconfig.EntitlementAnnotationKeyPrefix + entitlement.Type
		raw, ok := labels[key]
		if !ok {
			t.Fatalf("missing entitlement label %q", key)
		}
		if raw == "" {
			t.Fatalf("entitlement label %q has an empty value; containerd drops empty-valued labels", key)
		}
		got := appconfig.ParseEntitlementAnnotation(entitlement.Type, raw)
		if got.Type != entitlement.Type {
			t.Fatalf("parsed entitlement type = %q, want %q", got.Type, entitlement.Type)
		}
	}
}

func TestWendyLabels_DuplicateEntitlementType(t *testing.T) {
	entitlements := []appconfig.Entitlement{
		{Type: appconfig.EntitlementPersist, Name: "data", Path: "/data"},
		{Type: appconfig.EntitlementPersist, Name: "logs", Path: "/logs"},
	}
	labels := wendyLabels("app", "", "1.0", nil, entitlements, "", nil)

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

func TestWendyLabels_IsolationAndDependsOn(t *testing.T) {
	labels := wendyLabels("myapp", "web", "1.0.0", nil, nil, "isolated", []string{"db", "cache"})
	if got := labels[labelKeyIsolation]; got != "isolated" {
		t.Fatalf("isolation label = %q, want %q", got, "isolated")
	}
	if got := labels[labelKeyDependsOn]; got != "db,cache" {
		t.Fatalf("depends-on label = %q, want %q", got, "db,cache")
	}
}

func TestWendyLabels_OmitsWhenEmpty(t *testing.T) {
	labels := wendyLabels("myapp", "", "1.0.0", nil, nil, "", nil)
	if _, ok := labels[labelKeyIsolation]; ok {
		t.Fatal("isolation label should be absent when isolation is empty")
	}
	if _, ok := labels[labelKeyDependsOn]; ok {
		t.Fatal("depends-on label should be absent when dependsOn is empty")
	}
}

func TestWendyLabels_WithHTTPPort(t *testing.T) {
	ents := []appconfig.Entitlement{{Type: appconfig.EntitlementHTTP, Port: 8080}}
	labels := wendyLabels("app", "", "1.0", nil, ents, "", nil)
	if v, ok := labels[labelKeyHTTPPort]; !ok {
		t.Error("missing http port label")
	} else if v != "8080" {
		t.Errorf("http port label = %q; want %q", v, "8080")
	}
}

func TestWendyLabels_WithoutHTTPEntitlementOmitsLabel(t *testing.T) {
	labels := wendyLabels("app", "", "1.0", nil, nil, "", nil)
	if v, ok := labels[labelKeyHTTPPort]; ok {
		t.Errorf("should not have http port label when no http entitlement declared, got %q", v)
	}
}

// TestWendyLabels_WithMCPPort closes a pre-existing test gap: wendyLabels has
// written labelKeyMCPPort since the mcp entitlement shipped, but nothing
// asserted it. Added here because this task touches the same loop shape.
func TestWendyLabels_WithMCPPort(t *testing.T) {
	ents := []appconfig.Entitlement{{Type: appconfig.EntitlementMCP, Port: 3000}}
	labels := wendyLabels("app", "", "1.0", nil, ents, "", nil)
	if v, ok := labels[labelKeyMCPPort]; !ok {
		t.Error("missing mcp port label")
	} else if v != "3000" {
		t.Errorf("mcp port label = %q; want %q", v, "3000")
	}
}

func TestParseDependsOn(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"db", []string{"db"}},
		{"db,cache", []string{"db", "cache"}},
		{"db,,cache", []string{"db", "cache"}}, // tolerate stray empties
	}
	for _, tc := range cases {
		got := parseDependsOn(tc.in)
		if len(got) != len(tc.want) {
			t.Fatalf("parseDependsOn(%q) = %v, want %v", tc.in, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("parseDependsOn(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestWendyLabels_NoEntitlementsLabel(t *testing.T) {
	labels := wendyLabels("app", "", "1.0", nil, nil, "", nil)
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
		{Type: appconfig.EntitlementSerial, Device: "ttyUSB0"},
		{Type: appconfig.EntitlementSerial, Device: "ttyUSB1"},
		{Type: appconfig.EntitlementGPU},
	}

	labels := wendyLabels("app", "", "1.0", nil, original, "", nil)
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
	serial := byType[appconfig.EntitlementSerial]
	if len(serial) != 2 || serial[0].Device != "ttyUSB0" || serial[1].Device != "ttyUSB1" {
		t.Errorf("serial entitlement round-trip failed: %+v", serial)
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

func TestSafeJoin(t *testing.T) {
	base := "/run/wendy/hosts"

	// Valid component.
	got, err := safeJoin(base, "com.example.app")
	if err != nil {
		t.Errorf("safeJoin valid: unexpected error: %v", err)
	}
	if got != base+"/com.example.app" {
		t.Errorf("safeJoin valid: got %q, want %q", got, base+"/com.example.app")
	}

	// Reject path separator in component.
	if _, err := safeJoin(base, "a/b"); err == nil {
		t.Error("safeJoin: separator in component should be rejected")
	}
	// Reject dot component.
	if _, err := safeJoin(base, "."); err == nil {
		t.Error("safeJoin: dot component should be rejected")
	}
	// Reject dotdot component.
	if _, err := safeJoin(base, ".."); err == nil {
		t.Error("safeJoin: dotdot component should be rejected")
	}
	// Reject traversal via filepath.Join normalisation.
	if _, err := safeJoin(base, "sub/../../../etc/passwd"); err == nil {
		t.Error("safeJoin: traversal via .. should be rejected")
	}
}

func TestContainerArgs(t *testing.T) {
	entrypointOnly := ocispec.ImageConfig{Entrypoint: []string{"/app/server"}}
	cmdOnly := ocispec.ImageConfig{Cmd: []string{"python3", "main.py"}}
	both := ocispec.ImageConfig{Entrypoint: []string{"/usr/bin/entry"}, Cmd: []string{"--serve"}}

	cases := []struct {
		name     string
		cmd      string
		userArgs []string
		cfg      ocispec.ImageConfig
		want     []string
	}{
		{
			// The bug this guards: --user-args used to replace the image
			// entrypoint entirely, so the container exec'd "--port".
			name:     "user args append to image entrypoint",
			userArgs: []string{"--port", "8080"},
			cfg:      entrypointOnly,
			want:     []string{"/app/server", "--port", "8080"},
		},
		{
			name:     "user args append to image cmd",
			userArgs: []string{"--verbose"},
			cfg:      cmdOnly,
			want:     []string{"python3", "main.py", "--verbose"},
		},
		{
			name:     "user args append after entrypoint and cmd",
			userArgs: []string{"--extra"},
			cfg:      both,
			want:     []string{"/usr/bin/entry", "--serve", "--extra"},
		},
		{
			name: "no user args keeps image config",
			cfg:  both,
			want: []string{"/usr/bin/entry", "--serve"},
		},
		{
			// Explicit Cmd still overrides the image, with user args after it.
			name:     "explicit cmd replaces image config",
			cmd:      "/bin/custom",
			userArgs: []string{"--flag"},
			cfg:      both,
			want:     []string{"/bin/custom", "--flag"},
		},
		{
			name: "explicit cmd is field-split",
			cmd:  "sh -c",
			cfg:  both,
			want: []string{"sh", "-c"},
		},
		{
			name:     "empty image config falls back to /bin/sh",
			userArgs: []string{"-c", "echo hi"},
			want:     []string{"/bin/sh", "-c", "echo hi"},
		},
		{
			name: "empty everything falls back to /bin/sh",
			want: []string{"/bin/sh"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := containerArgs(tc.cmd, tc.userArgs, tc.cfg)
			if len(got) != len(tc.want) {
				t.Fatalf("containerArgs() = %q; want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("containerArgs() = %q; want %q", got, tc.want)
				}
			}
		})
	}
}

// The image's Entrypoint/Cmd slices come straight out of the shared image spec,
// so containerArgs must never append into their backing arrays.
func TestContainerArgs_DoesNotMutateImageConfig(t *testing.T) {
	entrypoint := make([]string, 1, 4)
	entrypoint[0] = "/app/server"
	cfg := ocispec.ImageConfig{Entrypoint: entrypoint}

	if got := containerArgs("", []string{"--first"}, cfg); got[1] != "--first" {
		t.Fatalf("containerArgs() = %q", got)
	}
	got := containerArgs("", []string{"--second"}, cfg)
	if len(got) != 2 || got[0] != "/app/server" || got[1] != "--second" {
		t.Fatalf("containerArgs() = %q; want [/app/server --second]", got)
	}
	if len(cfg.Entrypoint) != 1 || cfg.Entrypoint[0] != "/app/server" {
		t.Fatalf("image config entrypoint mutated: %q", cfg.Entrypoint)
	}
}

// dropEmptyLabelValues mimics containerd's bolt metadata store, which silently
// deletes any label whose value is the empty string when a container record is
// written (boltutil writeMap). Tests that assert labels survive a container
// create must round-trip through this, not just through the in-memory map.
func dropEmptyLabelValues(labels map[string]string) map[string]string {
	persisted := make(map[string]string, len(labels))
	for k, v := range labels {
		if v == "" {
			continue
		}
		persisted[k] = v
	}
	return persisted
}

// TestChunkDiffCreateLabels_RoundTripThroughContainerd covers the label set a
// chunk-diff deploy produces end to end: the CLI marshals the AppConfig
// (deployByChunkDiff), the agent's RunContainer handler unmarshals it, and
// CreateContainerWithProgress builds the container labels via wendyLabels.
// containerd then persists them, dropping empty-valued labels. Restart
// reconciliation (RestoreAppSystemAPISockets) reconstructs entitlements from
// what actually survives that store, so every entitlement — including type-only
// ones like camera and episode-write, whose annotation value used to encode as
// "" — must round-trip through parseEntitlementsFromAnnotations.
func TestChunkDiffCreateLabels_RoundTripThroughContainerd(t *testing.T) {
	appConfigJSON := []byte(`{
		"appId": "sh.wendy.examples.flightrecorder",
		"version": "1.0.0",
		"entitlements": [
			{"type": "camera"},
			{"type": "episode-write"},
			{"type": "serial", "device": "ttyACM0"}
		]
	}`)
	var cfg appconfig.AppConfig
	if err := json.Unmarshal(appConfigJSON, &cfg); err != nil {
		t.Fatalf("unmarshaling app config: %v", err)
	}

	labels := wendyLabels(cfg.AppID, "", cfg.Version, nil, cfg.Entitlements, cfg.Isolation, nil)
	persisted := dropEmptyLabelValues(labels)

	got := parseEntitlementsFromAnnotations(persisted)
	for _, want := range []string{
		appconfig.EntitlementCamera,
		appconfig.EntitlementEpisodeWrite,
		appconfig.EntitlementSerial,
	} {
		if !entitlementsContain(got, want) {
			t.Errorf("entitlement %q did not survive the containerd label round-trip; persisted labels: %v", want, persisted)
		}
	}
	// The other half: an entitlement carrying a payload must keep it, not just
	// survive as a bare type.
	serial, ok := entitlementOfType(got, appconfig.EntitlementSerial)
	if !ok || serial.Device != "ttyACM0" {
		t.Errorf("serial device did not survive the round-trip: %+v", serial)
	}
}
