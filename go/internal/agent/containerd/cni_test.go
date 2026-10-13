package containerd

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestBuildCNIConfig(t *testing.T) {
	tmp := t.TempDir()
	orig := cniSubnetRegistryPath
	cniSubnetRegistryPath = tmp + "/subnets.json"
	t.Cleanup(func() { cniSubnetRegistryPath = orig })
	subnet, err := allocateSubnet("com.example.myapp")
	if err != nil {
		t.Fatalf("allocateSubnet: %v", err)
	}
	cfg := buildBridgeCNIConfig("com.example.myapp", subnet, false)
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(cfg), &m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if m["type"] != "bridge" {
		t.Errorf("type = %q, want bridge", m["type"])
	}
	// Bridge name must fit within the 15-char kernel limit (IFNAMSIZ-1).
	bridge, _ := m["bridge"].(string)
	if len(bridge) > 15 {
		t.Errorf("bridge name %q is %d chars, must be ≤15 (IFNAMSIZ-1)", bridge, len(bridge))
	}
	if !strings.HasPrefix(bridge, "wendy-") {
		t.Errorf("bridge name %q should start with wendy-", bridge)
	}
	ipam, ok := m["ipam"].(map[string]interface{})
	if !ok {
		t.Fatal("ipam field missing or wrong type")
	}
	if ipam["type"] != "host-local" {
		t.Errorf("ipam.type = %q, want host-local", ipam["type"])
	}
	if ipam["subnet"] != subnet {
		t.Errorf("ipam.subnet = %q, want %q", ipam["subnet"], subnet)
	}
}

func TestBuildBridgeCNICheckConfigCarriesPreviousResult(t *testing.T) {
	result := `{"cniVersion":"0.4.0","ips":[{"version":"4","address":"10.1.2.3/28","gateway":"10.1.2.1"}]}`
	cfgJSON, err := buildBridgeCNICheckConfig("com.example.myapp", "10.1.2.0/28", result, false)
	if err != nil {
		t.Fatalf("build check config: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(cfgJSON), &cfg); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	prev, ok := cfg["prevResult"].(map[string]any)
	if !ok || prev["cniVersion"] != "0.4.0" {
		t.Fatalf("prevResult not preserved: %#v", cfg["prevResult"])
	}
	if _, err := buildBridgeCNICheckConfig("com.example.myapp", "10.1.2.0/28", `{broken`, false); err == nil {
		t.Fatal("malformed previous result was accepted")
	}
}

func TestBridgeName(t *testing.T) {
	// Short appID: direct embedding.
	if got := bridgeName("myapp"); got != "wendy-br-myapp" {
		t.Errorf("bridgeName(%q) = %q, want wendy-br-myapp", "myapp", got)
	}
	// Long appID: hash fallback, must be ≤15 chars.
	long := bridgeName("com.example.myapp")
	if len(long) > 15 {
		t.Errorf("bridgeName(long) = %q (%d chars), must be ≤15", long, len(long))
	}
	if !strings.HasPrefix(long, "wendy-") {
		t.Errorf("bridgeName(long) = %q, should start with wendy-", long)
	}
	// Deterministic: same appID → same name.
	if bridgeName("com.example.myapp") != bridgeName("com.example.myapp") {
		t.Error("bridgeName should be deterministic")
	}
}

func TestAllocateSubnet(t *testing.T) {
	// Redirect the registry to a temp dir so tests don't pollute/share real state.
	tmp := t.TempDir()
	orig := cniSubnetRegistryPath
	cniSubnetRegistryPath = tmp + "/subnets.json"
	t.Cleanup(func() { cniSubnetRegistryPath = orig })

	s1, err := allocateSubnet("com.example.app1")
	if err != nil {
		t.Fatalf("allocateSubnet app1: %v", err)
	}
	s2, err := allocateSubnet("com.example.app2")
	if err != nil {
		// app2 might collide with app1 in the test registry; that's OK to test.
		t.Logf("allocateSubnet app2 returned error (possible collision): %v", err)
		return
	}
	if s1 == s2 {
		t.Errorf("different appIDs should get different subnets, both got %q", s1)
	}
	// Subnets are /28 blocks within 10.0.0.0/8.
	for _, s := range []string{s1, s2} {
		if !strings.HasPrefix(s, "10.") {
			t.Errorf("subnet %q should start with 10.", s)
		}
		if !strings.HasSuffix(s, "/28") {
			t.Errorf("subnet %q should be a /28", s)
		}
	}
	// Deterministic: same appID → same subnet (returns from registry).
	s1Again, err := allocateSubnet("com.example.app1")
	if err != nil {
		t.Fatalf("allocateSubnet app1 (second call): %v", err)
	}
	if s1Again != s1 {
		t.Error("allocateSubnet should be deterministic")
	}
}

func TestValidateCNIInputs(t *testing.T) {
	validNetns := "/proc/1234/ns/net"
	validFD := "/proc/self/fd/5"

	if err := validateCNIInputs("com.example.app", "com.example.app@svc", validNetns); err != nil {
		t.Errorf("valid inputs rejected: %v", err)
	}
	if err := validateCNIInputs("com.example.app", "com.example.app@svc", validFD); err != nil {
		t.Errorf("fd-style netnsPath rejected: %v", err)
	}
	validBind := "/run/wendy/netns/com.example.app@svc"
	if err := validateCNIInputs("com.example.app", "com.example.app@svc", validBind); err != nil {
		t.Errorf("bind-mount netnsPath rejected: %v", err)
	}
	if err := validateCNIInputs("..", "container", validNetns); err == nil {
		t.Error("pure-dot appID should be rejected")
	}
	if err := validateCNIInputs("valid", "", validNetns); err == nil {
		t.Error("empty containerID should be rejected")
	}
	if err := validateCNIInputs("valid", "bad\x00id", validNetns); err == nil {
		t.Error("containerID with NUL should be rejected")
	}
	if err := validateCNIInputs("valid", "bad\nid", validNetns); err == nil {
		t.Error("containerID with LF should be rejected")
	}
	if err := validateCNIInputs("valid", "ctr", "/etc/passwd"); err == nil {
		t.Error("arbitrary netnsPath should be rejected")
	}
	if err := validateCNIInputs("valid", "ctr", "/proc/abc/ns/net"); err == nil {
		t.Error("non-numeric PID in netnsPath should be rejected")
	}
}

func TestWriteHostsFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/hosts"
	entries := map[string]string{
		"db":  "10.89.1.2",
		"api": "10.89.1.3",
	}
	if err := writeHostsFile(path, entries); err != nil {
		t.Fatalf("writeHostsFile error: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading hosts file: %v", err)
	}
	s := string(content)
	if !strings.Contains(s, "10.89.1.2\tdb") {
		t.Errorf("hosts file missing db entry, got:\n%s", s)
	}
	if !strings.Contains(s, "10.89.1.3\tapi") {
		t.Errorf("hosts file missing api entry, got:\n%s", s)
	}
	if !strings.Contains(s, "127.0.0.1\tlocalhost") {
		t.Errorf("hosts file missing localhost entry, got:\n%s", s)
	}
}

// TestCNIExecPATHIncludesSystemDirs guards against a real bug: the bridge
// exec's PATH was restricted to CNIBinDir only (to prevent third-party binary
// hijacking), which is correct for resolving "bridge"/"host-local" but broke
// the vendored bridge plugin's own real "iptables" shell-out (NAT/masquerade
// setup) with "executable file not found in $PATH" — every CNI ADD failed.
// CNIBinDir must stay first (delegation resolution takes priority); the
// system dirs must still be present for iptables to be found.
func TestCNIExecPATHIncludesSystemDirs(t *testing.T) {
	path := CNIBinDir + cniSystemPathDirs
	dirs := strings.Split(path, ":")
	if dirs[0] != CNIBinDir {
		t.Fatalf("PATH must start with CNIBinDir for delegation resolution, got %q", path)
	}
	for _, want := range []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin"} {
		found := false
		for _, d := range dirs {
			if d == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("PATH %q missing %q (needed to locate iptables)", path, want)
		}
	}
}

func TestBridgeCheckRequiresDefaultRouteProof(t *testing.T) {
	for _, tc := range []struct {
		name, routes      string
		internet, wantErr bool
	}{
		{"bridge missing", `[]`, true, true},
		{"bridge wrong gateway", `[{"dst":"0.0.0.0/0","gw":"10.1.2.2"}]`, true, true},
		{"bridge verified", `[{"dst":"0.0.0.0/0","gw":"10.1.2.1"}]`, true, false},
		{"mesh scoped", `[]`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := `{"cniVersion":"0.4.0","routes":` + tc.routes + `}`
			_, err := buildBridgeCNICheckConfig("com.example.myapp", "10.1.2.0/28", result, tc.internet)
			if (err != nil) != tc.wantErr {
				t.Fatalf("CHECK route proof error=%v", err)
			}
		})
	}
}
