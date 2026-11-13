package appconfig

import (
	"strings"
	"testing"
)

func TestROS2AutoDomainID_StableAndInRange(t *testing.T) {
	first := ROS2AutoDomainID("com.example.robot")
	second := ROS2AutoDomainID("com.example.robot")
	if first != second {
		t.Errorf("auto domain ID not stable: %d vs %d", first, second)
	}
	for _, appID := range []string{"", "a", "com.example.robot", "com.other.app", "x.y.z"} {
		id := ROS2AutoDomainID(appID)
		if id < ROS2DomainIDMin || id > ROS2DomainIDMax {
			t.Errorf("ROS2AutoDomainID(%q) = %d, want in [%d,%d]", appID, id, ROS2DomainIDMin, ROS2DomainIDMax)
		}
	}
}

func TestROS2Config_ResolvedDomainID(t *testing.T) {
	explicit := 42
	if got := (&ROS2Config{DomainID: &explicit}).ResolvedDomainID("app"); got != 42 {
		t.Errorf("explicit domain ID = %d, want 42", got)
	}
	// Newly valid band (0–232): values above the old 101 cap resolve to themselves.
	for _, valid := range []int{102, 150, 232} {
		v := valid
		if got := (&ROS2Config{DomainID: &v}).ResolvedDomainID("app"); got != valid {
			t.Errorf("domain ID %d = %d, want %d", valid, got, valid)
		}
	}
	invalid := 233
	if got := (&ROS2Config{DomainID: &invalid}).ResolvedDomainID("app"); got != -1 {
		t.Errorf("out-of-range domain ID = %d, want -1", got)
	}
	negative := -1
	if got := (&ROS2Config{DomainID: &negative}).ResolvedDomainID("app"); got != -1 {
		t.Errorf("negative domain ID = %d, want -1", got)
	}
	if got := (&ROS2Config{}).ResolvedDomainID("app"); got != ROS2AutoDomainID("app") {
		t.Errorf("nil domain ID = %d, want auto %d", got, ROS2AutoDomainID("app"))
	}
}

func TestROS2Config_ResolvedRMW(t *testing.T) {
	cases := map[string]string{
		"":                  "rmw_cyclonedds_cpp",
		"cyclonedds":        "rmw_cyclonedds_cpp",
		"CycloneDDS":        "rmw_cyclonedds_cpp",
		"fastrtps":          "rmw_fastrtps_cpp",
		"fastdds":           "rmw_fastrtps_cpp",
		"rmw_connextdds":    "rmw_connextdds",
		"gurumdds":          "rmw_gurumdds_cpp",
		"evil; rm -rf /":    "",
		"rmw_unknown_thing": "",
	}
	for in, want := range cases {
		if got := (&ROS2Config{RMW: in}).ResolvedRMW(); got != want {
			t.Errorf("ResolvedRMW(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestROS2Config_ResolvedDistro(t *testing.T) {
	if got := (&ROS2Config{}).ResolvedDistro(); got != "humble" {
		t.Errorf("default distro = %q, want humble", got)
	}
	if got := (&ROS2Config{Distro: "Jazzy"}).ResolvedDistro(); got != "jazzy" {
		t.Errorf("distro = %q, want jazzy", got)
	}
}

func TestROS2Config_ResolvedDiscoveryScope(t *testing.T) {
	cases := map[string]string{
		"":       ROS2DiscoveryScopeApp,
		"app":    ROS2DiscoveryScopeApp,
		"APP":    ROS2DiscoveryScopeApp,
		"host":   ROS2DiscoveryScopeHost,
		"HOST":   ROS2DiscoveryScopeHost,
		"global": "",
	}
	for in, want := range cases {
		if got := (&ROS2Config{DiscoveryScope: in}).ResolvedDiscoveryScope(); got != want {
			t.Errorf("ResolvedDiscoveryScope(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveROS2ConfigForService(t *testing.T) {
	groupID, svcID := 1, 7
	group := &ROS2Config{DomainID: &groupID}
	svc := &ROS2Config{DomainID: &svcID}
	cfg := &AppConfig{
		Frameworks: &FrameworksConfig{ROS2: group},
		Services: map[string]*ServiceConfig{
			"detector": {Frameworks: &FrameworksConfig{ROS2: svc}},
			"camera":   {},
		},
	}
	if got := cfg.ResolveROS2ConfigForService("detector"); got != svc {
		t.Errorf("detector should use service-level config")
	}
	if got := cfg.ResolveROS2ConfigForService("camera"); got != group {
		t.Errorf("camera should inherit group-level config")
	}
	if got := cfg.ResolveROS2ConfigForService(""); got != group {
		t.Errorf("single-container app should use group-level config")
	}
	if got := (&AppConfig{}).ResolveROS2ConfigForService("x"); got != nil {
		t.Errorf("no frameworks should resolve to nil, got %+v", got)
	}
}

func TestROS2Annotation_RoundTrip(t *testing.T) {
	explicit := 42
	value := ROS2AnnotationValue(&ROS2Config{DomainID: &explicit, Distro: "jazzy"}, "com.example.app")
	if value != "distro=jazzy,domain_id=42" {
		t.Errorf("annotation value = %q, want distro=jazzy,domain_id=42", value)
	}
	distro, domainID, ok := ParseROS2Annotation(value)
	if !ok || distro != "jazzy" || domainID != 42 {
		t.Errorf("ParseROS2Annotation(%q) = (%q, %d, %v)", value, distro, domainID, ok)
	}
}

func TestROS2AnnotationValue_Defaults(t *testing.T) {
	value := ROS2AnnotationValue(&ROS2Config{}, "com.example.app")
	distro, domainID, ok := ParseROS2Annotation(value)
	if !ok || distro != "humble" || domainID != ROS2AutoDomainID("com.example.app") {
		t.Errorf("default annotation = %q parsed to (%q, %d, %v)", value, distro, domainID, ok)
	}
}

func TestROS2AnnotationValue_InvalidDomain(t *testing.T) {
	invalid := 999
	if got := ROS2AnnotationValue(&ROS2Config{DomainID: &invalid}, "app"); got != "" {
		t.Errorf("annotation for invalid domain = %q, want empty", got)
	}
}

func TestParseROS2Annotation_Malformed(t *testing.T) {
	for _, value := range []string{"", "distro=humble", "domain_id=5", "distro=humble,domain_id=999", "garbage"} {
		if _, _, ok := ParseROS2Annotation(value); ok {
			t.Errorf("ParseROS2Annotation(%q) ok = true, want false", value)
		}
	}
}

func TestIsValidRMWImplementation(t *testing.T) {
	valid := []string{"rmw_cyclonedds_cpp", "rmw_fastrtps_cpp", "rmw_connextdds", "rmw_gurumdds_cpp"}
	for _, s := range valid {
		if !IsValidRMWImplementation(s) {
			t.Errorf("IsValidRMWImplementation(%q) = false, want true", s)
		}
	}
	invalid := []string{"", "cyclonedds", "fastrtps", "rmw_fastrtps", "rmw_evil_cpp; rm -rf", "RMW_FASTRTPS_CPP"}
	for _, s := range invalid {
		if IsValidRMWImplementation(s) {
			t.Errorf("IsValidRMWImplementation(%q) = true, want false", s)
		}
	}
}

// TestValidateJSON_UnknownFrameworkKey covers the gap this change closes: a
// typo'd top-level key under "frameworks" (e.g. "ross2" instead of "ros2")
// used to be silently dropped by encoding/json with no warning at all, unlike
// entitlements' "unknown type" error. See validateFrameworksJSON.
func TestValidateJSON_UnknownFrameworkKey(t *testing.T) {
	data := []byte(`{
		"appId": "com.example.robot",
		"frameworks": { "ross2": { "distro": "humble" } }
	}`)

	warnings := ValidateJSON(data)
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "Unknown key(s) in frameworks") && strings.Contains(w, "ross2") && strings.Contains(w, "ros2") {
			found = true
		}
	}
	if !found {
		t.Errorf("ValidateJSON() warnings = %v; want one flagging unknown top-level framework key %q", warnings, "ross2")
	}
}

// TestValidateJSON_KnownFrameworkKeyNoWarning ensures the new top-level check
// doesn't false-positive on the one framework that is actually supported.
func TestValidateJSON_KnownFrameworkKeyNoWarning(t *testing.T) {
	data := []byte(`{
		"appId": "com.example.robot",
		"frameworks": { "ros2": { "distro": "humble" } }
	}`)

	warnings := ValidateJSON(data)
	for _, w := range warnings {
		if strings.Contains(w, "Unknown key(s) in frameworks:") {
			t.Errorf("unexpected unknown-framework warning for a valid config: %v", warnings)
		}
	}
}

// TestValidateJSON_FrameworksBothWarnings covers a wendy.json with both an
// unrecognized top-level framework key and an unrecognized key nested under
// frameworks.ros2, confirming validateFrameworksJSON reports both instead of
// stopping at the first.
func TestValidateJSON_FrameworksBothWarnings(t *testing.T) {
	data := []byte(`{
		"appId": "com.example.robot",
		"frameworks": {
			"ross2": {},
			"ros2": { "domian_id": 5 }
		}
	}`)

	warnings := ValidateJSON(data)
	var sawTopLevel, sawNested bool
	for _, w := range warnings {
		if strings.Contains(w, "Unknown key(s) in frameworks:") && strings.Contains(w, "ross2") {
			sawTopLevel = true
		}
		if strings.Contains(w, "Unknown key(s) in frameworks.ros2:") && strings.Contains(w, "domian_id") {
			sawNested = true
		}
	}
	if !sawTopLevel {
		t.Errorf("missing top-level unknown-framework warning: %v", warnings)
	}
	if !sawNested {
		t.Errorf("missing nested frameworks.ros2 unknown-key warning: %v", warnings)
	}
}

// TestValidateJSON_ServiceFrameworksUnknownKey confirms the top-level check
// also applies to service-level "frameworks" objects (services["x"].frameworks),
// not just the app-level one.
func TestValidateJSON_ServiceFrameworksUnknownKey(t *testing.T) {
	data := []byte(`{
		"appId": "com.example.robot",
		"services": {
			"camera": {
				"context": ".",
				"frameworks": { "ross2": {} }
			}
		}
	}`)

	warnings := ValidateJSON(data)
	found := false
	for _, w := range warnings {
		if strings.Contains(w, `services["camera"].frameworks`) && strings.Contains(w, "ross2") {
			found = true
		}
	}
	if !found {
		t.Errorf("ValidateJSON() warnings = %v; want one flagging services[\"camera\"].frameworks unknown key", warnings)
	}
}

func TestROS2Config_ScopeTakesPrecedence(t *testing.T) {
	cfg := ROS2Config{Scope: "network", DiscoveryScope: "host"}
	if got := cfg.ResolvedScope(); got != ROS2ScopeNetwork {
		t.Errorf("ResolvedScope() = %q, want network", got)
	}
	cfg = ROS2Config{Scope: "device", DiscoveryScope: "host"}
	if got := cfg.ResolvedScope(); got != ROS2ScopeDevice {
		t.Errorf("ResolvedScope() = %q, want scope to win over discoveryScope", got)
	}
}

func TestROS2Config_ResolvedScope(t *testing.T) {
	for _, tc := range []struct {
		cfg  ROS2Config
		want string
	}{
		{ROS2Config{}, ROS2ScopeDevice},
		{ROS2Config{Scope: "app"}, ROS2ScopeApp},
		{ROS2Config{Scope: "Device"}, ROS2ScopeDevice},
		{ROS2Config{Scope: "peripherals"}, ROS2ScopePeripherals},
		{ROS2Config{Scope: "network"}, ROS2ScopeNetwork},
		{ROS2Config{Scope: "host"}, ""},
		// The deprecated field keeps its behavior: "app" was always loopback
		// discovery shared through a common domain, so it is "device".
		{ROS2Config{DiscoveryScope: "app"}, ROS2ScopeDevice},
		{ROS2Config{DiscoveryScope: "host"}, ROS2ScopeNetwork},
		{ROS2Config{DiscoveryScope: "bogus"}, ""},
	} {
		if got := tc.cfg.ResolvedScope(); got != tc.want {
			t.Errorf("ResolvedScope(%+v) = %q, want %q", tc.cfg, got, tc.want)
		}
	}
}

func TestValidateROS2Config_Scope(t *testing.T) {
	zero := 0
	bridge := []Entitlement{{Type: EntitlementNetwork, Mode: "bridge"}}
	host := []Entitlement{{Type: EntitlementNetwork, Mode: "host"}}
	for _, tc := range []struct {
		name         string
		cfg          ROS2Config
		entitlements []Entitlement
		wantErr      string
	}{
		{"each scope", ROS2Config{Scope: "network"}, nil, ""},
		{"unknown scope", ROS2Config{Scope: "host"}, nil, "must be one of app, device, peripherals, network"},
		{"both fields, safe fallback", ROS2Config{Scope: "device", DiscoveryScope: "app"}, nil, ""},
		{"both fields, peripherals falls back to loopback", ROS2Config{Scope: "peripherals", DiscoveryScope: "app"}, nil, ""},
		{"both fields, network matches host", ROS2Config{Scope: "network", DiscoveryScope: "host"}, nil, ""},
		{"both fields, older agent would be wider", ROS2Config{Scope: "device", DiscoveryScope: "host"}, nil, "conflicts with scope"},
		{"both fields, older agent would be narrower", ROS2Config{Scope: "network", DiscoveryScope: "app"}, nil, "conflicts with scope"},
		{"app with a domain", ROS2Config{Scope: "app", DomainID: &zero}, nil, "remove domainId"},
		{"app without a domain", ROS2Config{Scope: "app"}, bridge, ""},
		{"peripherals on FastDDS", ROS2Config{Scope: "peripherals", RMW: "fastdds"}, nil, "requires rmw"},
		{"device off the host network", ROS2Config{Scope: "device"}, bridge, "needs the host network"},
		{"peripherals on the host network", ROS2Config{Scope: "peripherals"}, host, ""},
		{"deprecated field off the host network", ROS2Config{DiscoveryScope: "app"}, bridge, ""},
	} {
		err := validateROS2Config("frameworks.ros2", &tc.cfg, tc.entitlements)
		if tc.wantErr == "" && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
			t.Errorf("%s: error %v, want it to mention %q", tc.name, err, tc.wantErr)
		}
	}
}

func TestValidateROS2Config_ServiceUsesEffectiveEntitlements(t *testing.T) {
	for _, tc := range []struct {
		name       string
		frameworks *FrameworksConfig
	}{
		{"inherited scope", nil},
		{"service scope", &FrameworksConfig{ROS2: &ROS2Config{Scope: ROS2ScopeDevice}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &AppConfig{
				AppID:        "com.example.app",
				Entitlements: []Entitlement{{Type: EntitlementNetwork, Mode: "host"}},
				Frameworks:   &FrameworksConfig{ROS2: &ROS2Config{Scope: ROS2ScopeDevice}},
				Services: map[string]*ServiceConfig{
					"worker": {
						Context:      ".",
						Entitlements: []Entitlement{{Type: EntitlementNetwork, Mode: "bridge"}},
						Frameworks:   tc.frameworks,
					},
				},
			}
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), `services["worker"].frameworks.ros2`) || !strings.Contains(err.Error(), "needs the host network") {
				t.Fatalf("Validate() error = %v, want the service's effective bridge network rejected", err)
			}
		})
	}
}

func TestROS2Config_AppScopeGetsItsOwnDomain(t *testing.T) {
	zero := 0
	cfg := ROS2Config{Scope: "app", DomainID: &zero}
	if got := cfg.ResolvedDomainID("com.example.app"); got != ROS2AutoDomainID("com.example.app") {
		t.Errorf("scope app domain = %d, want the app's own %d", got, ROS2AutoDomainID("com.example.app"))
	}
}
