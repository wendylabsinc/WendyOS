package appconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wendy.json")

	content := `{
		"appId": "com.example.myapp",
		"version": "1.0.0",
		"language": "python",
		"entitlements": [
			{"type": "network", "mode": "host"},
			{"type": "gpu"},
			{"type": "persist", "name": "data", "path": "/app/data"},
			{"type": "audio"}
		],
		"python": {"sourceRoot": "src"}
	}`

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	cfg, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}

	if cfg.AppID != "com.example.myapp" {
		t.Errorf("AppID = %q, want %q", cfg.AppID, "com.example.myapp")
	}
	if cfg.Version != "1.0.0" {
		t.Errorf("Version = %q, want %q", cfg.Version, "1.0.0")
	}
	if cfg.Language != "python" {
		t.Errorf("Language = %q, want %q", cfg.Language, "python")
	}
	if len(cfg.Entitlements) != 4 {
		t.Fatalf("Entitlements count = %d, want 4", len(cfg.Entitlements))
	}
	if cfg.Entitlements[0].Type != "network" {
		t.Errorf("Entitlements[0].Type = %q, want %q", cfg.Entitlements[0].Type, "network")
	}
	if cfg.Entitlements[0].Mode != "host" {
		t.Errorf("Entitlements[0].Mode = %q, want %q", cfg.Entitlements[0].Mode, "host")
	}
	if cfg.Entitlements[2].Name != "data" {
		t.Errorf("Entitlements[2].Name = %q, want %q", cfg.Entitlements[2].Name, "data")
	}
	if cfg.Entitlements[2].Path != "/app/data" {
		t.Errorf("Entitlements[2].Path = %q, want %q", cfg.Entitlements[2].Path, "/app/data")
	}
	if cfg.Python == nil {
		t.Fatal("Python config is nil")
	}
	if cfg.Python.SourceRoot != "src" {
		t.Errorf("Python.SourceRoot = %q, want %q", cfg.Python.SourceRoot, "src")
	}
}

func TestLoadFromFile_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wendy.json")

	if err := os.WriteFile(path, []byte(`{invalid json}`), 0o644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	_, err := LoadFromFile(path)
	if err == nil {
		t.Fatal("LoadFromFile() expected error for invalid JSON, got nil")
	}
}

func TestLoadFromFile_FileNotFound(t *testing.T) {
	_, err := LoadFromFile("/nonexistent/path/wendy.json")
	if err == nil {
		t.Fatal("LoadFromFile() expected error for missing file, got nil")
	}
}

func TestValidate_ValidConfig(t *testing.T) {
	cfg := &AppConfig{
		AppID: "com.example.app",
		Entitlements: []Entitlement{
			{Type: EntitlementNetwork, Mode: "host"},
			{Type: EntitlementGPU},
			{Type: EntitlementPersist, Name: "vol1", Path: "/data"},
			{Type: EntitlementGPIO, Pins: []int{12, 13}},
		},
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() unexpected error: %v", err)
	}
}

// TestValidate_NetworkHostAdminMode covers the WDY-1094 opt-in: "host-admin"
// is a valid network mode (host networking + CAP_NET_ADMIN), while an unknown
// mode is still rejected.
func TestValidate_NetworkHostAdminMode(t *testing.T) {
	valid := &AppConfig{
		AppID:        "com.example.app",
		Entitlements: []Entitlement{{Type: EntitlementNetwork, Mode: "host-admin"}},
	}
	if err := valid.Validate(); err != nil {
		t.Errorf("Validate() rejected host-admin mode: %v", err)
	}

	invalid := &AppConfig{
		AppID:        "com.example.app",
		Entitlements: []Entitlement{{Type: EntitlementNetwork, Mode: "bogus"}},
	}
	if err := invalid.Validate(); err == nil {
		t.Error("Validate() accepted an unknown network mode; want error")
	}
}

// TestValidate_NetworkMeshMode covers the "mesh" network mode added for the
// wendy-mesh CNI chaining feature: mesh mode requires a valid, non-empty
// serviceCIDR, and serviceCIDR must not be set on any other mode.
func TestValidate_NetworkMeshMode(t *testing.T) {
	tests := []struct {
		name    string
		ent     Entitlement
		wantErr bool
	}{
		{
			name:    "mesh mode with valid serviceCIDR is valid",
			ent:     Entitlement{Type: EntitlementNetwork, Mode: "mesh", ServiceCIDR: "10.42.0.0/16"},
			wantErr: false,
		},
		{
			name:    "mesh mode with missing serviceCIDR errors",
			ent:     Entitlement{Type: EntitlementNetwork, Mode: "mesh"},
			wantErr: true,
		},
		{
			name:    "mesh mode with invalid serviceCIDR errors",
			ent:     Entitlement{Type: EntitlementNetwork, Mode: "mesh", ServiceCIDR: "not-a-cidr"},
			wantErr: true,
		},
		{
			name:    "mesh mode with non-CIDR IP errors",
			ent:     Entitlement{Type: EntitlementNetwork, Mode: "mesh", ServiceCIDR: "10.42.0.1"},
			wantErr: true,
		},
		{
			name:    "host mode still valid without serviceCIDR",
			ent:     Entitlement{Type: EntitlementNetwork, Mode: "host"},
			wantErr: false,
		},
		{
			name:    "host-admin mode still valid without serviceCIDR",
			ent:     Entitlement{Type: EntitlementNetwork, Mode: "host-admin"},
			wantErr: false,
		},
		{
			name:    "none mode still valid without serviceCIDR",
			ent:     Entitlement{Type: EntitlementNetwork, Mode: "none"},
			wantErr: false,
		},
		{
			name:    "serviceCIDR on host mode errors",
			ent:     Entitlement{Type: EntitlementNetwork, Mode: "host", ServiceCIDR: "10.42.0.0/16"},
			wantErr: true,
		},
		{
			name:    "serviceCIDR on none mode errors",
			ent:     Entitlement{Type: EntitlementNetwork, Mode: "none", ServiceCIDR: "10.42.0.0/16"},
			wantErr: true,
		},
		{
			name:    "serviceCIDR on host-admin mode errors",
			ent:     Entitlement{Type: EntitlementNetwork, Mode: "host-admin", ServiceCIDR: "10.42.0.0/16"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &AppConfig{
				AppID:        "com.example.app",
				Entitlements: []Entitlement{tt.ent},
			}
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Errorf("Validate() expected error for %+v, got nil", tt.ent)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("Validate() unexpected error for %+v: %v", tt.ent, err)
			}
		})
	}
}

// TestValidate_NetworkBridgeMode covers the "bridge" network mode added by
// specs/2026-07-05-network-bridge-default-design.md: an isolated network
// namespace with NAT egress. Unlike "mesh", "bridge" takes no serviceCIDR.
func TestValidate_NetworkBridgeMode(t *testing.T) {
	tests := []struct {
		name    string
		ent     Entitlement
		wantErr bool
	}{
		{
			name:    "bridge mode without serviceCIDR is valid",
			ent:     Entitlement{Type: EntitlementNetwork, Mode: "bridge"},
			wantErr: false,
		},
		{
			name:    "serviceCIDR on bridge mode errors",
			ent:     Entitlement{Type: EntitlementNetwork, Mode: "bridge", ServiceCIDR: "10.42.0.0/16"},
			wantErr: true,
		},
		{
			name:    "an unrelated unknown mode is still rejected",
			ent:     Entitlement{Type: EntitlementNetwork, Mode: "bridged"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &AppConfig{
				AppID:        "com.example.app",
				Entitlements: []Entitlement{tt.ent},
			}
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Errorf("Validate() expected error for %+v, got nil", tt.ent)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("Validate() unexpected error for %+v: %v", tt.ent, err)
			}
		})
	}
}

func TestValidate_MissingAppID(t *testing.T) {
	cfg := &AppConfig{}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() expected error for missing appId, got nil")
	}
	if got := err.Error(); got != "appId is required" {
		t.Errorf("error = %q, want %q", got, "appId is required")
	}
}

func TestValidate_AppIDCharset(t *testing.T) {
	valid := []string{
		"my-app",
		"com.example.app",
		"sh.wendy.App",
		"app_123",
	}
	for _, id := range valid {
		cfg := &AppConfig{AppID: id}
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() unexpected error for valid appId %q: %v", id, err)
		}
	}

	invalid := []string{
		"app,with,commas",  // would corrupt OTEL_RESOURCE_ATTRIBUTES
		"app=value",        // would corrupt env-var parsing
		"app with spaces",  // disallowed
		"app\nnewline",     // would inject an env entry
		"emoji-\U0001F600", // non-ASCII
		// containerd filter-grammar special characters (SOC2-CC6, NIST-SI-10):
		// ValidateAppID must reject all of these so containersForApp can safely
		// interpolate appID into a label filter string via fmt.Sprintf/%q.
		`app"quoted`, // double-quote — closes the %q-quoted value early
		`app\slash`,  // backslash — escape in filter grammar
		"app~tilde",  // tilde — used as regex operator in containerd filters
		"app/slash",  // forward-slash — path separator in container names
		"app@at",     // @ — snapshot key separator used by SnapshotKey
		// Path-traversal guards (SOC2-CC6, NIST-SI-10):
		".",  // single dot — resolves to CWD in filesystem paths
		"..", // double dot — directory traversal
		"...",
	}
	for _, id := range invalid {
		cfg := &AppConfig{AppID: id}
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate() expected error for invalid appId %q, got nil", id)
		}
	}
}

func TestValidateServiceName(t *testing.T) {
	valid := []string{
		"api",    // typical multi-service name
		"db",     // two chars — boundary minimum
		"a",      // single char — intentionally allowed (not a DNS-minimum violation)
		"my-svc", // hyphen in middle
		"svc1",   // trailing digit
		"ab",     // two chars
	}
	for _, name := range valid {
		if err := ValidateServiceName(name); err != nil {
			t.Errorf("ValidateServiceName(%q) unexpected error: %v", name, err)
		}
	}

	invalid := []string{
		"",                      // empty
		"Api",                   // uppercase rejected
		"api-",                  // trailing hyphen (RFC 1123)
		"-api",                  // leading hyphen
		"api\nnewline",          // would break env-var injection
		"api\x00null",           // null byte
		"api=value",             // equals sign
		strings.Repeat("a", 58), // too long (> 57 chars)
	}
	for _, name := range invalid {
		if err := ValidateServiceName(name); err == nil {
			t.Errorf("ValidateServiceName(%q) expected error, got nil", name)
		}
	}
}

func TestValidate_UnknownEntitlementType(t *testing.T) {
	cfg := &AppConfig{
		AppID: "com.example.app",
		Entitlements: []Entitlement{
			{Type: "banana"},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() expected error for unknown entitlement type, got nil")
	}
}

func TestValidate_PersistMissingFields(t *testing.T) {
	tests := []struct {
		name string
		ent  Entitlement
	}{
		{
			name: "missing name",
			ent:  Entitlement{Type: EntitlementPersist, Path: "/data"},
		},
		{
			name: "missing path",
			ent:  Entitlement{Type: EntitlementPersist, Name: "vol1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &AppConfig{
				AppID:        "com.example.app",
				Entitlements: []Entitlement{tt.ent},
			}
			if err := cfg.Validate(); err == nil {
				t.Error("Validate() expected error, got nil")
			}
		})
	}
}

func TestValidate_PersistPathUsesContainerPathSemantics(t *testing.T) {
	cfg := &AppConfig{
		AppID: "com.example.app",
		Entitlements: []Entitlement{
			{Type: EntitlementPersist, Name: "vol1", Path: "/data"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() unexpected error for POSIX container path: %v", err)
	}

	cfg.Entitlements[0].Path = `C:\data`
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() expected error for host-style absolute path, got nil")
	}
}

func TestValidate_GPIOWithoutPins(t *testing.T) {
	cfg := &AppConfig{
		AppID: "com.example.app",
		Entitlements: []Entitlement{
			{Type: EntitlementGPIO},
		},
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() unexpected error for gpio without pins: %v", err)
	}
}

func TestValidate_AllEntitlementTypes(t *testing.T) {
	cfg := &AppConfig{
		AppID: "com.example.app",
		Entitlements: []Entitlement{
			{Type: EntitlementNetwork},
			{Type: EntitlementBluetooth},
			{Type: EntitlementVideo},
			{Type: EntitlementGPU},
			{Type: EntitlementPersist, Name: "data", Path: "/data"},
			{Type: EntitlementAudio},
			{Type: EntitlementCamera},
			{Type: EntitlementUSB},
			{Type: EntitlementI2C, Device: "i2c-1"},
			{Type: EntitlementGPIO, Pins: []int{7}},
			{Type: EntitlementInput},
			{Type: EntitlementSerial, Device: "ttyACM0"},
			{Type: EntitlementMCP, Port: 3000},
		},
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() unexpected error: %v", err)
	}
}

func TestValidate_SerialEntitlement(t *testing.T) {
	valid := []string{"ttyACM0", "ttyUSB0", "ttyUSB12"}
	for _, device := range valid {
		t.Run("valid/"+device, func(t *testing.T) {
			cfg := &AppConfig{
				AppID:        "com.example.app",
				Entitlements: []Entitlement{{Type: EntitlementSerial, Device: device}},
			}
			if err := cfg.Validate(); err != nil {
				t.Errorf("Validate() unexpected error for serial device %q: %v", device, err)
			}
		})
	}

	// ttyAMA0/ttyS0 are on-board UARTs: USB-only entitlement rejects them.
	invalid := []string{"", "ttyACM", "tty", "sda", "ttyACMx", "ttyACM0/../sda", "../mem", "ttyACM-1", "ttyAMA0", "ttyS0"}
	for _, device := range invalid {
		t.Run("invalid/"+device, func(t *testing.T) {
			cfg := &AppConfig{
				AppID:        "com.example.app",
				Entitlements: []Entitlement{{Type: EntitlementSerial, Device: device}},
			}
			if err := cfg.Validate(); err == nil {
				t.Errorf("Validate() expected error for serial device %q, got nil", device)
			}
		})
	}
}

func TestValidate_InputEntitlement(t *testing.T) {
	cfg := &AppConfig{
		AppID: "com.example.app",
		Entitlements: []Entitlement{
			{Type: EntitlementInput},
		},
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() unexpected error for input entitlement: %v", err)
	}
}

func TestValidateJSON_InputNoWarnings(t *testing.T) {
	data := []byte(`{
		"appId": "com.example.app",
		"entitlements": [
			{"type": "input"}
		]
	}`)

	warnings := ValidateJSON(data)
	if len(warnings) != 0 {
		t.Errorf("ValidateJSON() got %d warnings for valid input entitlement, want 0", len(warnings))
	}
}

func TestValidateJSON_InputUnknownKeys(t *testing.T) {
	data := []byte(`{
		"appId": "com.example.app",
		"entitlements": [
			{"type": "input", "device": "/dev/input/event4"}
		]
	}`)

	warnings := ValidateJSON(data)
	if len(warnings) == 0 {
		t.Fatal("ValidateJSON() expected warning for unknown key on input entitlement, got none")
	}
}

func TestValidateJSON_MCPNoWarnings(t *testing.T) {
	data := []byte(`{
		"appId": "com.example.app",
		"entitlements": [
			{"type": "mcp", "port": 3000}
		]
	}`)

	warnings := ValidateJSON(data)
	if len(warnings) != 0 {
		t.Errorf("ValidateJSON() got %d warnings for valid mcp entitlement, want 0", len(warnings))
	}
}

func TestValidateJSON_MCPUnknownKeys(t *testing.T) {
	data := []byte(`{
		"appId": "com.example.app",
		"entitlements": [
			{"type": "mcp", "port": 3000, "typo": 1}
		]
	}`)

	warnings := ValidateJSON(data)
	if len(warnings) == 0 {
		t.Fatal("ValidateJSON() expected warning for unknown key on mcp entitlement, got none")
	}
}

func TestValidateJSON_VideoEntitlementDeprecated(t *testing.T) {
	data := []byte(`{
		"appId": "com.example.app",
		"entitlements": [
			{"type": "video"}
		]
	}`)

	warnings := ValidateJSON(data)
	if len(warnings) != 1 {
		t.Fatalf("ValidateJSON() got %d warnings, want 1", len(warnings))
	}
	if got := warnings[0]; got != `entitlement[0]: "video" is deprecated; use "camera" instead` {
		t.Fatalf("ValidateJSON() warning = %q", got)
	}
}

func TestValidateJSON_CameraLegacyKeysNoWarnings(t *testing.T) {
	data := []byte(`{
		"appId": "com.example.app",
		"entitlements": [
			{
				"type": "camera",
				"mode": "legacy",
				"allowlist": ["/dev/video0"]
			}
		]
	}`)

	warnings := ValidateJSON(data)
	if len(warnings) != 0 {
		t.Fatalf("ValidateJSON() got %d warnings, want 0", len(warnings))
	}
}

func TestLoadFromFile_WithHooksPostStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wendy.json")

	content := `{
		"appId": "com.example.webapp",
		"entitlements": [{"type": "network"}],
		"hooks": {
			"postStart": {
				"cli": "open http://${WENDY_HOSTNAME}:3000",
				"agent": "xdg-open http://localhost:3000"
			}
		}
	}`

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	cfg, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}

	if cfg.Hooks == nil {
		t.Fatal("Hooks is nil, expected non-nil")
	}
	if cfg.Hooks.PostStart == nil {
		t.Fatal("Hooks.PostStart is nil, expected non-nil")
	}
	if cfg.Hooks.PostStart.CLI != "open http://${WENDY_HOSTNAME}:3000" {
		t.Errorf("Hooks.PostStart.CLI = %q, want %q", cfg.Hooks.PostStart.CLI, "open http://${WENDY_HOSTNAME}:3000")
	}
	if cfg.Hooks.PostStart.Agent != "xdg-open http://localhost:3000" {
		t.Errorf("Hooks.PostStart.Agent = %q, want %q", cfg.Hooks.PostStart.Agent, "xdg-open http://localhost:3000")
	}
}

func TestLoadFromFile_WithoutHooks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wendy.json")

	content := `{
		"appId": "com.example.app",
		"entitlements": [{"type": "gpu"}]
	}`

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	cfg, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}

	if cfg.Hooks != nil {
		t.Errorf("Hooks = %+v, want nil", cfg.Hooks)
	}
}

func TestLoadFromFile_HooksPostStartOpenURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wendy.json")

	content := `{
		"appId": "com.example.webapp",
		"hooks": {
			"postStart": {
				"openURL": "http://${WENDY_HOSTNAME}:3000"
			}
		}
	}`

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	cfg, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}
	if cfg.Hooks == nil || cfg.Hooks.PostStart == nil {
		t.Fatal("Hooks.PostStart is nil")
	}
	if got, want := cfg.Hooks.PostStart.OpenURL, "http://${WENDY_HOSTNAME}:3000"; got != want {
		t.Errorf("Hooks.PostStart.OpenURL = %q, want %q", got, want)
	}
	if cfg.Hooks.PostStart.CLI != "" {
		t.Errorf("Hooks.PostStart.CLI = %q, want empty", cfg.Hooks.PostStart.CLI)
	}
}

func TestValidateJSON_PostStartCLILegacyOpener(t *testing.T) {
	tests := []struct {
		name       string
		cli        string
		wantOpener string
		wantPlatfm string
	}{
		{"open", "open http://localhost:3000", "open", "macOS"},
		{"xdg-open", "xdg-open http://localhost:3000", "xdg-open", "Linux"},
		{"start", "start http://localhost:3000", "start", "Windows"},
		{"open with leading whitespace", "  open http://localhost:3000", "open", "macOS"},
		{"open with tab separator", "open\thttp://localhost:3000", "open", "macOS"},
		{"bare open", "open", "open", "macOS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := []byte(`{
				"appId": "com.example.app",
				"hooks": {
					"postStart": {
						"cli": ` + jsonString(tt.cli) + `
					}
				}
			}`)

			warnings := ValidateJSON(data)
			if len(warnings) != 1 {
				t.Fatalf("ValidateJSON() got %d warnings, want 1: %v", len(warnings), warnings)
			}
			if !strings.Contains(warnings[0], `"`+tt.wantOpener+`"`) {
				t.Errorf("warning %q does not mention opener %q", warnings[0], tt.wantOpener)
			}
			if !strings.Contains(warnings[0], tt.wantPlatfm) {
				t.Errorf("warning %q does not mention platform %q", warnings[0], tt.wantPlatfm)
			}
			if !strings.Contains(warnings[0], "openURL") {
				t.Errorf("warning %q does not recommend openURL", warnings[0])
			}
		})
	}
}

func TestValidateJSON_PostStartCLIPortableNoWarning(t *testing.T) {
	tests := []struct {
		name string
		cli  string
	}{
		{"echo", "echo hello"},
		{"openssl is not open", "openssl version"},
		{"started is not start", "started --foo"},
		{"empty", ""},
		{"openURL only", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := []byte(`{
				"appId": "com.example.app",
				"hooks": {
					"postStart": {
						"cli": ` + jsonString(tt.cli) + `
					}
				}
			}`)

			warnings := ValidateJSON(data)
			for _, w := range warnings {
				if strings.Contains(w, "hooks.postStart.cli") {
					t.Errorf("unexpected warning: %q", w)
				}
			}
		})
	}
}

func TestValidateJSON_PostStartOpenURLNoWarning(t *testing.T) {
	data := []byte(`{
		"appId": "com.example.app",
		"hooks": {
			"postStart": {
				"openURL": "http://localhost:3000"
			}
		}
	}`)

	warnings := ValidateJSON(data)
	for _, w := range warnings {
		if strings.Contains(w, "hooks.postStart") {
			t.Errorf("unexpected warning: %q", w)
		}
	}
}

func TestValidateJSON_NoEntitlementsStillValidatesHooks(t *testing.T) {
	// Regression: ValidateJSON used to early-return when entitlements were
	// missing, silently skipping hook validation.
	data := []byte(`{
		"appId": "com.example.app",
		"hooks": {
			"postStart": {
				"cli": "open http://localhost:3000"
			}
		}
	}`)

	warnings := ValidateJSON(data)
	if len(warnings) != 1 {
		t.Fatalf("ValidateJSON() got %d warnings, want 1", len(warnings))
	}
}

func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestLoadFromFile_HooksPostStartCLIOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wendy.json")

	content := `{
		"appId": "com.example.app",
		"hooks": {
			"postStart": {
				"cli": "echo hello"
			}
		}
	}`

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	cfg, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}

	if cfg.Hooks == nil || cfg.Hooks.PostStart == nil {
		t.Fatal("Hooks.PostStart is nil")
	}
	if cfg.Hooks.PostStart.CLI != "echo hello" {
		t.Errorf("Hooks.PostStart.CLI = %q, want %q", cfg.Hooks.PostStart.CLI, "echo hello")
	}
	if cfg.Hooks.PostStart.Agent != "" {
		t.Errorf("Hooks.PostStart.Agent = %q, want empty", cfg.Hooks.PostStart.Agent)
	}
}

func TestLoadFromFile_WithReadiness(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wendy.json")

	content := `{
		"appId": "com.example.app",
		"readiness": {
			"tcpSocket": { "port": 3002 },
			"timeoutSeconds": 15
		}
	}`

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	cfg, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}

	if cfg.Readiness == nil {
		t.Fatal("Readiness is nil")
	}
	if cfg.Readiness.TCPSocket == nil {
		t.Fatal("Readiness.TCPSocket is nil")
	}
	if cfg.Readiness.TCPSocket.Port != 3002 {
		t.Errorf("Readiness.TCPSocket.Port = %d, want 3002", cfg.Readiness.TCPSocket.Port)
	}
	if cfg.Readiness.TimeoutSeconds != 15 {
		t.Errorf("Readiness.TimeoutSeconds = %d, want 15", cfg.Readiness.TimeoutSeconds)
	}
}

func TestValidate_ReadinessInvalidPort(t *testing.T) {
	tests := []struct {
		name string
		port int
	}{
		{"zero port", 0},
		{"negative port", -1},
		{"port too high", 70000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &AppConfig{
				AppID: "com.example.app",
				Readiness: &ReadinessConfig{
					TCPSocket: &TCPSocketProbe{Port: tt.port},
				},
			}
			if err := cfg.Validate(); err == nil {
				t.Error("Validate() expected error for invalid port, got nil")
			}
		})
	}
}

func TestValidate_ReadinessNegativeTimeout(t *testing.T) {
	cfg := &AppConfig{
		AppID: "com.example.app",
		Readiness: &ReadinessConfig{
			TCPSocket:      &TCPSocketProbe{Port: 3000},
			TimeoutSeconds: -5,
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() expected error for negative timeout, got nil")
	}
}

func TestValidate_ReadinessValidConfig(t *testing.T) {
	cfg := &AppConfig{
		AppID: "com.example.app",
		Readiness: &ReadinessConfig{
			TCPSocket:      &TCPSocketProbe{Port: 3002},
			TimeoutSeconds: 30,
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() unexpected error: %v", err)
	}
}

func TestLoadFromFile_ServiceReadinessHooks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wendy.json")

	content := `{
		"appId": "com.example.app",
		"services": {
			"web": {
				"context": "web",
				"readiness": {
					"tcpSocket": { "port": 3002 },
					"timeoutSeconds": 15
				},
				"hooks": {
					"postStart": {
						"openURL": "http://${WENDY_HOSTNAME}:3000",
						"agent": "echo ready"
					}
				}
			}
		}
	}`

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	cfg, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}

	web := cfg.Services["web"]
	if web == nil {
		t.Fatal(`services["web"] is nil`)
	}
	if web.Readiness == nil || web.Readiness.TCPSocket == nil {
		t.Fatal(`services["web"].Readiness.TCPSocket is nil`)
	}
	if web.Readiness.TCPSocket.Port != 3002 {
		t.Errorf("Readiness.TCPSocket.Port = %d, want 3002", web.Readiness.TCPSocket.Port)
	}
	if web.Readiness.TimeoutSeconds != 15 {
		t.Errorf("Readiness.TimeoutSeconds = %d, want 15", web.Readiness.TimeoutSeconds)
	}
	if web.Hooks == nil || web.Hooks.PostStart == nil {
		t.Fatal(`services["web"].Hooks.PostStart is nil`)
	}
	if web.Hooks.PostStart.OpenURL != "http://${WENDY_HOSTNAME}:3000" {
		t.Errorf("Hooks.PostStart.OpenURL = %q, want %q", web.Hooks.PostStart.OpenURL, "http://${WENDY_HOSTNAME}:3000")
	}
	if web.Hooks.PostStart.Agent != "echo ready" {
		t.Errorf("Hooks.PostStart.Agent = %q, want %q", web.Hooks.PostStart.Agent, "echo ready")
	}
}

func TestValidate_ServiceReadinessInvalidPort(t *testing.T) {
	cfg := &AppConfig{
		AppID: "com.example.app",
		Services: map[string]*ServiceConfig{
			"web": {
				Context: "web",
				Readiness: &ReadinessConfig{
					TCPSocket: &TCPSocketProbe{Port: 70000},
				},
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() expected error for invalid service readiness port, got nil")
	}
	if !strings.Contains(err.Error(), `services["web"].readiness`) {
		t.Errorf(`error %q does not mention services["web"].readiness`, err.Error())
	}
}

func TestValidate_ServiceReadinessNegativeTimeout(t *testing.T) {
	cfg := &AppConfig{
		AppID: "com.example.app",
		Services: map[string]*ServiceConfig{
			"web": {
				Context: "web",
				Readiness: &ReadinessConfig{
					TCPSocket:      &TCPSocketProbe{Port: 3000},
					TimeoutSeconds: -5,
				},
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() expected error for negative service readiness timeout, got nil")
	}
	if !strings.Contains(err.Error(), `services["web"].readiness`) {
		t.Errorf(`error %q does not mention services["web"].readiness`, err.Error())
	}
}

func TestRunArgs_RoundTripJSON(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantRunNil  bool
		wantRunArgs []string
	}{
		{
			name:       "no run",
			input:      `{"appId":"sh.wendy.App"}`,
			wantRunNil: true,
		},
		{
			name:        "one arg",
			input:       `{"appId":"sh.wendy.App","run":{"args":["--verbose"]}}`,
			wantRunArgs: []string{"--verbose"},
		},
		{
			name:        "empty args",
			input:       `{"appId":"sh.wendy.App","run":{"args":[]}}`,
			wantRunArgs: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := LoadFromBytes([]byte(tt.input))
			if err != nil {
				t.Fatalf("LoadFromBytes: %v", err)
			}

			if tt.wantRunNil {
				if cfg.Run != nil {
					t.Fatalf("Run = %#v, want nil", cfg.Run)
				}
			} else {
				if cfg.Run == nil {
					t.Fatal("Run = nil, want non-nil")
				}
				if len(cfg.Run.Args) != len(tt.wantRunArgs) {
					t.Fatalf("Run.Args len = %d, want %d", len(cfg.Run.Args), len(tt.wantRunArgs))
				}
				for i, want := range tt.wantRunArgs {
					if got := cfg.Run.Args[i]; got != want {
						t.Fatalf("Run.Args[%d] = %q, want %q", i, got, want)
					}
				}
			}

			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}

			var decoded AppConfig
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}

			if tt.wantRunNil {
				if decoded.Run != nil {
					t.Fatalf("decoded.Run = %#v, want nil", decoded.Run)
				}
			} else {
				if decoded.Run == nil {
					t.Fatal("decoded.Run = nil, want non-nil")
				}
				if len(decoded.Run.Args) != len(tt.wantRunArgs) {
					t.Fatalf("decoded.Run.Args len = %d, want %d", len(decoded.Run.Args), len(tt.wantRunArgs))
				}
				for i, want := range tt.wantRunArgs {
					if got := decoded.Run.Args[i]; got != want {
						t.Fatalf("decoded.Run.Args[%d] = %q, want %q", i, got, want)
					}
				}
			}
		})
	}
}

// --- Files field tests ---

func TestLoadFromFile_WithFiles_BothFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wendy.json")

	content := `{
		"appId": "sh.wendy.MyApp",
		"files": [
			{"path": "models/weights.bin", "to": "models/w.bin"},
			{"path": "config/prod.json"}
		]
	}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}
	if len(cfg.Files) != 2 {
		t.Fatalf("Files count = %d, want 2", len(cfg.Files))
	}
	if cfg.Files[0].Path != "models/weights.bin" {
		t.Errorf("Files[0].Path = %q, want %q", cfg.Files[0].Path, "models/weights.bin")
	}
	if cfg.Files[0].To != "models/w.bin" {
		t.Errorf("Files[0].To = %q, want %q", cfg.Files[0].To, "models/w.bin")
	}
	if cfg.Files[1].Path != "config/prod.json" {
		t.Errorf("Files[1].Path = %q, want %q", cfg.Files[1].Path, "config/prod.json")
	}
	if cfg.Files[1].To != "" {
		t.Errorf("Files[1].To = %q, want empty", cfg.Files[1].To)
	}
}

func TestLoadFromFile_WithFiles_PathOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wendy.json")

	content := `{"appId": "sh.wendy.App", "files": [{"path": "data/model"}]}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}
	if len(cfg.Files) != 1 {
		t.Fatalf("Files count = %d, want 1", len(cfg.Files))
	}
	if cfg.Files[0].Path != "data/model" {
		t.Errorf("Files[0].Path = %q, want %q", cfg.Files[0].Path, "data/model")
	}
	if cfg.Files[0].To != "" {
		t.Errorf("Files[0].To should be empty, got %q", cfg.Files[0].To)
	}
}

func TestLoadFromFile_WithoutFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wendy.json")

	content := `{"appId": "sh.wendy.App"}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}
	if len(cfg.Files) != 0 {
		t.Errorf("Files = %v, want nil/empty", cfg.Files)
	}
}

func TestValidate_Files_EmptyPath(t *testing.T) {
	cfg := &AppConfig{
		AppID: "sh.wendy.App",
		Files: []FileSyncEntry{{Path: ""}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() expected error for empty path, got nil")
	}
}

func TestValidate_Files_AbsolutePath(t *testing.T) {
	cfg := &AppConfig{
		AppID: "sh.wendy.App",
		Files: []FileSyncEntry{{Path: "/absolute/path"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for absolute path")
	}
}

func TestValidate_Files_DotDotPath(t *testing.T) {
	cfg := &AppConfig{
		AppID: "sh.wendy.App",
		Files: []FileSyncEntry{{Path: "../../etc/passwd"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for dotdot path")
	}
}

func TestValidate_Files_AbsoluteTo(t *testing.T) {
	cfg := &AppConfig{
		AppID: "sh.wendy.App",
		Files: []FileSyncEntry{{Path: "data/file", To: "/absolute/dest"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for absolute to")
	}
}

func TestValidate_Files_DotDotTo(t *testing.T) {
	cfg := &AppConfig{
		AppID: "sh.wendy.App",
		Files: []FileSyncEntry{{Path: "data/file", To: "../escaped"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for dotdot to")
	}
}

func TestValidate_Files_Valid(t *testing.T) {
	cfg := &AppConfig{
		AppID: "sh.wendy.App",
		Files: []FileSyncEntry{
			{Path: "models/gemma"},
			{Path: "config/prod.json", To: "config/app.json"},
			{Path: "./data/file"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() unexpected error: %v", err)
	}
}

func TestFiles_RoundTripJSON(t *testing.T) {
	original := &AppConfig{
		AppID: "sh.wendy.MyApp",
		Files: []FileSyncEntry{
			{Path: "models/gemma-3-27b"},
			{Path: "config/prod.json", To: "config/app.json"},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded AppConfig
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(decoded.Files) != 2 {
		t.Fatalf("Files count = %d, want 2", len(decoded.Files))
	}
	if decoded.Files[0].Path != original.Files[0].Path {
		t.Errorf("Files[0].Path = %q, want %q", decoded.Files[0].Path, original.Files[0].Path)
	}
	if decoded.Files[1].To != original.Files[1].To {
		t.Errorf("Files[1].To = %q, want %q", decoded.Files[1].To, original.Files[1].To)
	}
}

func TestValidate_Brewfile_Valid(t *testing.T) {
	for _, path := range []string{"ops/Brewfile", "./Brewfile.wendy"} {
		t.Run(path, func(t *testing.T) {
			cfg := &AppConfig{AppID: "sh.wendy.App", Brewfile: path}
			if err := cfg.Validate(); err != nil {
				t.Errorf("Validate() unexpected error: %v", err)
			}
		})
	}
}

func TestValidate_Brewfile_AbsolutePath(t *testing.T) {
	cfg := &AppConfig{AppID: "sh.wendy.App", Brewfile: "/tmp/Brewfile"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for absolute brewfile path")
	}
}

func TestValidate_Brewfile_UnsafeComponents(t *testing.T) {
	for _, path := range []string{"../Brewfile", "././Brewfile", "ops/./Brewfile", "ops//Brewfile", "ops/", `ops\\Brewfile`, "ops%2fBrewfile", "ops\r/Brewfile"} {
		t.Run(path, func(t *testing.T) {
			cfg := &AppConfig{AppID: "sh.wendy.App", Brewfile: path}
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected error for unsafe brewfile path")
			}
		})
	}
}

func TestBrewfile_RoundTripJSON(t *testing.T) {
	original := &AppConfig{AppID: "sh.wendy.MyApp", Brewfile: "ops/Brewfile"}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded AppConfig
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.Brewfile != original.Brewfile {
		t.Errorf("Brewfile = %q, want %q", decoded.Brewfile, original.Brewfile)
	}
}

func TestValidateJSON_UnknownKeys(t *testing.T) {
	data := []byte(`{
		"appId": "com.example.app",
		"entitlements": [
			{"type": "gpu", "foobar": true},
			{"type": "network", "mode": "host"},
			{"type": "persist", "name": "vol", "path": "/data", "unknownField": 42}
		]
	}`)

	warnings := ValidateJSON(data)
	if len(warnings) == 0 {
		t.Fatal("ValidateJSON() expected warnings for unknown keys, got none")
	}

	// Should have warnings for entitlement[0] (foobar) and entitlement[2] (unknownField)
	if len(warnings) != 2 {
		t.Errorf("ValidateJSON() got %d warnings, want 2", len(warnings))
	}
}

func TestValidateJSON_UnknownROS2Keys(t *testing.T) {
	data := []byte(`{
		"appId": "com.example.app",
		"frameworks": { "ros2": { "domainId": 42, "domian_id": 7, "rmw": "cyclonedds" } }
	}`)
	warnings := ValidateJSON(data)
	if len(warnings) != 1 {
		t.Fatalf("got %d warnings, want 1 (for domian_id): %v", len(warnings), warnings)
	}
}

func TestValidateJSON_CleanROS2_NoWarnings(t *testing.T) {
	data := []byte(`{"appId":"com.example.app","frameworks":{"ros2":{"domainId":0,"rmw":"cyclonedds","distro":"humble","discoveryScope":"host"}}}`)
	if got := ValidateJSON(data); len(got) != 0 {
		t.Errorf("clean ros2 config: got %d warnings, want 0: %v", len(got), got)
	}
}

func TestMCPEntitlementValid(t *testing.T) {
	cfg := &AppConfig{
		AppID: "test",
		Entitlements: []Entitlement{
			{Type: EntitlementMCP, Port: 3000},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestMCPEntitlementPortRequired(t *testing.T) {
	cfg := &AppConfig{
		AppID: "test",
		Entitlements: []Entitlement{
			{Type: EntitlementMCP, Port: 0},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for missing port")
	}
	if !strings.Contains(err.Error(), "port") {
		t.Fatalf("expected error to mention port, got: %v", err)
	}
}

func TestMCPEntitlementDuplicateRejected(t *testing.T) {
	cfg := &AppConfig{
		AppID: "test",
		Entitlements: []Entitlement{
			{Type: EntitlementMCP, Port: 3000},
			{Type: EntitlementMCP, Port: 4000},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for duplicate mcp entitlement")
	}
}

func TestMCPEntitlementPortOutOfRange(t *testing.T) {
	cfg := &AppConfig{
		AppID: "test",
		Entitlements: []Entitlement{
			{Type: EntitlementMCP, Port: 99999},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for out-of-range port")
	}
}

func TestValidateJSON_HTTPNoWarnings(t *testing.T) {
	data := []byte(`{
		"appId": "com.example.app",
		"entitlements": [
			{"type": "http", "port": 8080}
		]
	}`)

	warnings := ValidateJSON(data)
	if len(warnings) != 0 {
		t.Errorf("ValidateJSON() got %d warnings for valid http entitlement, want 0", len(warnings))
	}
}

func TestValidateJSON_HTTPUnknownKeys(t *testing.T) {
	data := []byte(`{
		"appId": "com.example.app",
		"entitlements": [
			{"type": "http", "port": 8080, "typo": 1}
		]
	}`)

	warnings := ValidateJSON(data)
	if len(warnings) == 0 {
		t.Fatal("ValidateJSON() expected warning for unknown key on http entitlement, got none")
	}
}

func TestHTTPEntitlementValid(t *testing.T) {
	cfg := &AppConfig{
		AppID: "test",
		Entitlements: []Entitlement{
			{Type: EntitlementHTTP, Port: 8080},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestValidateJSON_NPUNoWarnings(t *testing.T) {
	data := []byte(`{
		"appId": "com.example.app",
		"entitlements": [
			{"type": "npu"}
		]
	}`)

	warnings := ValidateJSON(data)
	if len(warnings) != 0 {
		t.Errorf("ValidateJSON() got %d warnings for valid npu entitlement, want 0", len(warnings))
	}
}

func TestValidateJSON_NPUUnknownKeys(t *testing.T) {
	data := []byte(`{
		"appId": "com.example.app",
		"entitlements": [
			{"type": "npu", "device": "/dev/fastrpc-cdsp"}
		]
	}`)

	warnings := ValidateJSON(data)
	if len(warnings) == 0 {
		t.Fatal("ValidateJSON() expected warning for unknown key on npu entitlement, got none")
	}
}

func TestNPUEntitlementValid(t *testing.T) {
	cfg := &AppConfig{
		AppID: "test",
		Entitlements: []Entitlement{
			{Type: EntitlementNPU},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestHTTPEntitlementPortRequired(t *testing.T) {
	cfg := &AppConfig{
		AppID: "test",
		Entitlements: []Entitlement{
			{Type: EntitlementHTTP, Port: 0},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for missing port")
	}
	if !strings.Contains(err.Error(), "port") {
		t.Fatalf("expected error to mention port, got: %v", err)
	}
}

func TestHTTPEntitlementDuplicateRejected(t *testing.T) {
	cfg := &AppConfig{
		AppID: "test",
		Entitlements: []Entitlement{
			{Type: EntitlementHTTP, Port: 8080},
			{Type: EntitlementHTTP, Port: 9090},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for duplicate http entitlement")
	}
}

func TestHTTPEntitlementPortOutOfRange(t *testing.T) {
	cfg := &AppConfig{
		AppID: "test",
		Entitlements: []Entitlement{
			{Type: EntitlementHTTP, Port: 99999},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for out-of-range port")
	}
}

func TestDisplayEntitlementValid(t *testing.T) {
	cfg := &AppConfig{
		AppID:        "test",
		Entitlements: []Entitlement{{Type: EntitlementDisplay}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestDisplayEntitlementDuplicateRejected(t *testing.T) {
	cfg := &AppConfig{
		AppID: "test",
		Entitlements: []Entitlement{
			{Type: EntitlementDisplay},
			{Type: EntitlementDisplay},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for duplicate display entitlement")
	}
}

func TestDisplayEntitlementJSONNoWarnings(t *testing.T) {
	warnings := ValidateJSON([]byte(`{
		"appId": "test",
		"entitlements": [ {"type": "display"} ]
	}`))
	if len(warnings) != 0 {
		t.Fatalf("got %d warnings for valid display entitlement, want 0: %v", len(warnings), warnings)
	}
}

func TestDisplayEntitlementJSONUnknownKeyWarns(t *testing.T) {
	warnings := ValidateJSON([]byte(`{
		"appId": "test",
		"entitlements": [ {"type": "display", "bogus": 1} ]
	}`))
	if len(warnings) == 0 {
		t.Fatal("expected warning for unknown key on display entitlement, got none")
	}
}

func TestServiceConfigValidation(t *testing.T) {
	t.Run("valid services", func(t *testing.T) {
		cfg := &AppConfig{
			AppID: "com.example.app",
			Services: map[string]*ServiceConfig{
				"api":      {Context: "api", DependsOn: []string{"db"}},
				"db":       {Context: "db"},
				"frontend": {Context: "frontend", DependsOn: []string{"api"}},
			},
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("missing context", func(t *testing.T) {
		cfg := &AppConfig{
			AppID: "com.example.app",
			Services: map[string]*ServiceConfig{
				"api": {Context: ""},
			},
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error for missing context")
		}
		if !strings.Contains(err.Error(), "context is required") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("unknown dependsOn", func(t *testing.T) {
		cfg := &AppConfig{
			AppID: "com.example.app",
			Services: map[string]*ServiceConfig{
				"api": {Context: "api", DependsOn: []string{"ghost"}},
			},
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error for unknown dependsOn")
		}
		if !strings.Contains(err.Error(), "ghost") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("dotdot in context rejected", func(t *testing.T) {
		cfg := &AppConfig{
			AppID: "com.example.app",
			Services: map[string]*ServiceConfig{
				"svc": {Context: "../escape"},
			},
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error for dotdot context")
		}
	})

	t.Run("services parsed from JSON", func(t *testing.T) {
		data := `{
			"appId": "com.example.app",
			"services": {
				"api":  {"context": "api",  "dependsOn": ["db"]},
				"db":   {"context": "db"}
			}
		}`
		cfg, err := LoadFromBytes([]byte(data))
		if err != nil {
			t.Fatalf("parse error: %v", err)
		}
		if len(cfg.Services) != 2 {
			t.Fatalf("want 2 services, got %d", len(cfg.Services))
		}
		if cfg.Services["api"].Context != "api" {
			t.Errorf("api context = %q, want %q", cfg.Services["api"].Context, "api")
		}
		if len(cfg.Services["api"].DependsOn) != 1 || cfg.Services["api"].DependsOn[0] != "db" {
			t.Errorf("api dependsOn = %v, want [db]", cfg.Services["api"].DependsOn)
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("validate error: %v", err)
		}
	})

	t.Run("valid service entitlements", func(t *testing.T) {
		cfg := &AppConfig{
			AppID: "com.example.app",
			Services: map[string]*ServiceConfig{
				"api": {
					Context: "api",
					Entitlements: []Entitlement{
						{Type: EntitlementNetwork, Mode: "host"},
						{Type: EntitlementPersist, Name: "data", Path: "/app/data"},
					},
				},
			},
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("unexpected error for valid service entitlements: %v", err)
		}
	})

	t.Run("unknown entitlement type in service", func(t *testing.T) {
		cfg := &AppConfig{
			AppID: "com.example.app",
			Services: map[string]*ServiceConfig{
				"api": {
					Context:      "api",
					Entitlements: []Entitlement{{Type: "banana"}},
				},
			},
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error for unknown entitlement type in service")
		}
		if !strings.Contains(err.Error(), "banana") {
			t.Fatalf("expected error to mention unknown type, got: %v", err)
		}
	})

	t.Run("persist entitlement missing name in service", func(t *testing.T) {
		cfg := &AppConfig{
			AppID: "com.example.app",
			Services: map[string]*ServiceConfig{
				"api": {
					Context: "api",
					Entitlements: []Entitlement{
						{Type: EntitlementPersist, Path: "/data"},
					},
				},
			},
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error for persist missing name in service")
		}
		if !strings.Contains(err.Error(), "name") {
			t.Fatalf("expected error to mention name, got: %v", err)
		}
	})

	t.Run("persist entitlement missing path in service", func(t *testing.T) {
		cfg := &AppConfig{
			AppID: "com.example.app",
			Services: map[string]*ServiceConfig{
				"svc": {
					Context: "svc",
					Entitlements: []Entitlement{
						{Type: EntitlementPersist, Name: "vol"},
					},
				},
			},
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error for persist missing path in service")
		}
		if !strings.Contains(err.Error(), "path") {
			t.Fatalf("expected error to mention path, got: %v", err)
		}
	})

	t.Run("mcp entitlement invalid port in service", func(t *testing.T) {
		cfg := &AppConfig{
			AppID: "com.example.app",
			Services: map[string]*ServiceConfig{
				"svc": {
					Context: "svc",
					Entitlements: []Entitlement{
						{Type: EntitlementMCP, Port: 0},
					},
				},
			},
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error for invalid mcp port in service")
		}
		if !strings.Contains(err.Error(), "port") {
			t.Fatalf("expected error to mention port, got: %v", err)
		}
	})

	t.Run("i2c entitlement invalid device in service", func(t *testing.T) {
		cfg := &AppConfig{
			AppID: "com.example.app",
			Services: map[string]*ServiceConfig{
				"svc": {
					Context: "svc",
					Entitlements: []Entitlement{
						{Type: EntitlementI2C, Device: "baddevice"},
					},
				},
			},
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error for invalid i2c device in service")
		}
		if !strings.Contains(err.Error(), "i2c") {
			t.Fatalf("expected error to mention i2c, got: %v", err)
		}
	})

	t.Run("network entitlement invalid mode in service", func(t *testing.T) {
		cfg := &AppConfig{
			AppID: "com.example.app",
			Services: map[string]*ServiceConfig{
				"svc": {
					Context: "svc",
					Entitlements: []Entitlement{
						{Type: EntitlementNetwork, Mode: "bogus"},
					},
				},
			},
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error for invalid network mode in service")
		}
		if !strings.Contains(err.Error(), "mode") {
			t.Fatalf("expected error to mention mode, got: %v", err)
		}
	})

	t.Run("network entitlement bridge mode in service is valid", func(t *testing.T) {
		cfg := &AppConfig{
			AppID: "com.example.app",
			Services: map[string]*ServiceConfig{
				"svc": {
					Context: "svc",
					Entitlements: []Entitlement{
						{Type: EntitlementNetwork, Mode: "bridge"},
					},
				},
			},
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate() rejected bridge network mode in service: %v", err)
		}
	})

	t.Run("duplicate mcp entitlement in service", func(t *testing.T) {
		cfg := &AppConfig{
			AppID: "com.example.app",
			Services: map[string]*ServiceConfig{
				"svc": {
					Context: "svc",
					Entitlements: []Entitlement{
						{Type: EntitlementMCP, Port: 3000},
						{Type: EntitlementMCP, Port: 4000},
					},
				},
			},
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error for duplicate mcp entitlement in service")
		}
	})
}

func TestLoadComposeCompanion_NotFound(t *testing.T) {
	cfg, warnings, err := LoadComposeCompanion(t.TempDir())
	if err != nil {
		t.Fatalf("expected nil error for missing wendy.json, got %v", err)
	}
	if cfg != nil {
		t.Fatalf("expected nil config for missing wendy.json, got %+v", cfg)
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", warnings)
	}
}

func TestLoadComposeCompanion_Valid(t *testing.T) {
	dir := t.TempDir()
	data := `{
		"appId": "com.example.robot",
		"isolation": "shared-ipc",
		"frameworks": { "ros2": { "domainId": 5, "rmw": "cyclonedds" } },
		"entitlements": [{ "type": "gpu" }]
	}`
	if err := os.WriteFile(filepath.Join(dir, "wendy.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, warnings, err := LoadComposeCompanion(dir)
	if err != nil {
		t.Fatalf("LoadComposeCompanion: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	if cfg.AppID != "com.example.robot" {
		t.Errorf("AppID = %q, want %q", cfg.AppID, "com.example.robot")
	}
	if cfg.Isolation != "shared-ipc" {
		t.Errorf("Isolation = %q, want %q", cfg.Isolation, "shared-ipc")
	}
	if cfg.Frameworks == nil || cfg.Frameworks.ROS2 == nil {
		t.Fatal("Frameworks.ROS2 is nil")
	}
	if cfg.Frameworks.ROS2.DomainID == nil || *cfg.Frameworks.ROS2.DomainID != 5 {
		t.Errorf("ROS2.DomainID = %v, want 5", cfg.Frameworks.ROS2.DomainID)
	}
	if cfg.Frameworks.ROS2.RMW != "cyclonedds" {
		t.Errorf("ROS2.RMW = %q, want %q", cfg.Frameworks.ROS2.RMW, "cyclonedds")
	}
	if len(cfg.Entitlements) != 1 || cfg.Entitlements[0].Type != "gpu" {
		t.Errorf("Entitlements = %+v", cfg.Entitlements)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
}

func TestLoadComposeCompanion_WithServices(t *testing.T) {
	dir := t.TempDir()
	data := `{
		"appId": "com.example.robot",
		"services": {
			"camera": {
				"entitlements": [{ "type": "camera" }, { "type": "gpu" }],
				"frameworks": { "ros2": { "domainId": 42 } }
			},
			"detector": {
				"entitlements": [{ "type": "gpu" }]
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(dir, "wendy.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := LoadComposeCompanion(dir)
	if err != nil {
		t.Fatalf("LoadComposeCompanion: %v", err)
	}
	if len(cfg.Services) != 2 {
		t.Fatalf("want 2 services, got %d", len(cfg.Services))
	}
	camera := cfg.Services["camera"]
	if camera == nil {
		t.Fatal("camera service is nil")
	}
	if len(camera.Entitlements) != 2 {
		t.Errorf("camera entitlements = %d, want 2", len(camera.Entitlements))
	}
	if camera.Frameworks == nil || camera.Frameworks.ROS2 == nil || camera.Frameworks.ROS2.DomainID == nil || *camera.Frameworks.ROS2.DomainID != 42 {
		t.Errorf("camera.Frameworks.ROS2.DomainID mismatch")
	}
	if cfg.Services["detector"] == nil {
		t.Fatal("detector service is nil")
	}
}

func TestLoadComposeCompanion_ServiceReadinessHooks(t *testing.T) {
	dir := t.TempDir()
	data := `{
		"appId": "com.example.robot",
		"services": {
			"web": {
				"readiness": { "tcpSocket": { "port": 8080 }, "timeoutSeconds": 20 },
				"hooks": { "postStart": { "openURL": "http://localhost:8080" } }
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(dir, "wendy.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := LoadComposeCompanion(dir)
	if err != nil {
		t.Fatalf("LoadComposeCompanion: %v", err)
	}
	web := cfg.Services["web"]
	if web == nil {
		t.Fatal("web service is nil")
	}
	if web.Readiness == nil || web.Readiness.TCPSocket == nil || web.Readiness.TCPSocket.Port != 8080 {
		t.Errorf("web.Readiness mismatch: %+v", web.Readiness)
	}
	if web.Readiness.TimeoutSeconds != 20 {
		t.Errorf("web.Readiness.TimeoutSeconds = %d, want 20", web.Readiness.TimeoutSeconds)
	}
	if web.Hooks == nil || web.Hooks.PostStart == nil || web.Hooks.PostStart.OpenURL != "http://localhost:8080" {
		t.Errorf("web.Hooks mismatch: %+v", web.Hooks)
	}
}

func TestLoadComposeCompanion_InvalidServiceReadinessFails(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{"zero port", `{"appId": "com.example.robot", "services": {"web": {"readiness": {"tcpSocket": {"port": 0}}}}}`},
		{"port too high", `{"appId": "com.example.robot", "services": {"web": {"readiness": {"tcpSocket": {"port": 70000}}}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "wendy.json"), []byte(tt.json), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, _, err := LoadComposeCompanion(dir); err == nil {
				t.Fatal("expected error for invalid service readiness port")
			}
		})
	}
}

func TestLoadComposeCompanion_InvalidEntitlement(t *testing.T) {
	dir := t.TempDir()
	data := `{
		"appId": "com.example.robot",
		"entitlements": [{ "type": "banana" }]
	}`
	if err := os.WriteFile(filepath.Join(dir, "wendy.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadComposeCompanion(dir); err == nil {
		t.Fatal("expected error for unknown entitlement type")
	}
}

func TestLoadComposeCompanion_NullServiceRejected(t *testing.T) {
	dir := t.TempDir()
	data := `{
		"appId": "com.example.robot",
		"services": { "camera": null }
	}`
	if err := os.WriteFile(filepath.Join(dir, "wendy.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadComposeCompanion(dir); err == nil {
		t.Fatal("expected error for null service")
	}
}

func TestLoadComposeCompanion_MissingAppID(t *testing.T) {
	dir := t.TempDir()
	data := `{ "isolation": "shared-ipc" }`
	if err := os.WriteFile(filepath.Join(dir, "wendy.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadComposeCompanion(dir); err == nil {
		t.Fatal("expected error for missing appId")
	}
}

func TestLoadComposeCompanion_ContextNotRequired(t *testing.T) {
	// Unlike Validate(), LoadComposeCompanion does not require context in services.
	dir := t.TempDir()
	data := `{
		"appId": "com.example.robot",
		"services": {
			"camera": { "entitlements": [{ "type": "camera" }] }
		}
	}`
	if err := os.WriteFile(filepath.Join(dir, "wendy.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := LoadComposeCompanion(dir)
	if err != nil {
		t.Fatalf("LoadComposeCompanion must not require context: %v", err)
	}
	if cfg.Services["camera"] == nil {
		t.Fatal("camera service missing")
	}
}

func TestFrameworksConfig_ParseJSON(t *testing.T) {
	data := `{
		"appId": "com.example.app",
		"frameworks": {
			"ros2": { "domainId": 10, "rmw": "fastrtps" }
		}
	}`
	cfg, err := LoadFromBytes([]byte(data))
	if err != nil {
		t.Fatalf("LoadFromBytes: %v", err)
	}
	if cfg.Frameworks == nil {
		t.Fatal("Frameworks is nil")
	}
	if cfg.Frameworks.ROS2 == nil {
		t.Fatal("Frameworks.ROS2 is nil")
	}
	if cfg.Frameworks.ROS2.DomainID == nil || *cfg.Frameworks.ROS2.DomainID != 10 {
		t.Errorf("DomainID = %v, want 10", cfg.Frameworks.ROS2.DomainID)
	}
	if cfg.Frameworks.ROS2.RMW != "fastrtps" {
		t.Errorf("RMW = %q, want %q", cfg.Frameworks.ROS2.RMW, "fastrtps")
	}
}

func TestAppConfig_Isolation_RoundTrip(t *testing.T) {
	data := `{"appId": "com.example.app", "isolation": "shared-ipc"}`
	cfg, err := LoadFromBytes([]byte(data))
	if err != nil {
		t.Fatalf("LoadFromBytes: %v", err)
	}
	if cfg.Isolation != "shared-ipc" {
		t.Errorf("Isolation = %q, want %q", cfg.Isolation, "shared-ipc")
	}
}

func TestValidateJSON_ServiceEntitlements(t *testing.T) {
	t.Run("unknown key in service entitlement warns", func(t *testing.T) {
		data := []byte(`{
			"appId": "com.example.app",
			"services": {
				"api": {
					"context": "api",
					"entitlements": [{"type": "gpu", "unknownKey": true}]
				}
			}
		}`)
		warnings := ValidateJSON(data)
		if len(warnings) == 0 {
			t.Fatal("ValidateJSON() expected warning for unknown key in service entitlement, got none")
		}
		found := false
		for _, w := range warnings {
			if strings.Contains(w, "unknownKey") {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected warning mentioning 'unknownKey', got: %v", warnings)
		}
	})

	t.Run("deprecated entitlement type in service warns", func(t *testing.T) {
		data := []byte(`{
			"appId": "com.example.app",
			"services": {
				"svc": {
					"context": "svc",
					"entitlements": [{"type": "video"}]
				}
			}
		}`)
		warnings := ValidateJSON(data)
		if len(warnings) == 0 {
			t.Fatal("ValidateJSON() expected deprecation warning for service entitlement, got none")
		}
		found := false
		for _, w := range warnings {
			if strings.Contains(w, "video") && strings.Contains(w, "deprecated") {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected deprecation warning mentioning 'video', got: %v", warnings)
		}
	})

	t.Run("valid service entitlements produce no warnings", func(t *testing.T) {
		data := []byte(`{
			"appId": "com.example.app",
			"services": {
				"api": {
					"context": "api",
					"entitlements": [{"type": "network", "mode": "host"}]
				}
			}
		}`)
		warnings := ValidateJSON(data)
		if len(warnings) != 0 {
			t.Fatalf("ValidateJSON() expected no warnings for valid service entitlement, got: %v", warnings)
		}
	})
}

func TestValidateJSON_ServiceHooksCLILegacyOpener(t *testing.T) {
	data := []byte(`{
		"appId": "com.example.app",
		"services": {
			"web": {
				"context": "web",
				"hooks": {
					"postStart": {
						"cli": "open http://localhost:3000"
					}
				}
			}
		}
	}`)

	warnings := ValidateJSON(data)
	if len(warnings) != 1 {
		t.Fatalf("ValidateJSON() got %d warnings, want 1: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], `services["web"].hooks`) {
		t.Errorf("warning %q does not carry the service prefix", warnings[0])
	}

	clean := []byte(`{
		"appId": "com.example.app",
		"hooks": { "postStart": { "openURL": "http://localhost:3000" } }
	}`)
	if warnings := ValidateJSON(clean); len(warnings) != 0 {
		t.Errorf("ValidateJSON() expected no warnings for clean top-level config, got: %v", warnings)
	}
}

func TestValidateJSON_TopLevelAgentHookOnMultiServiceWarns(t *testing.T) {
	const warningSubstr = "top-level hooks.postStart.agent is ignored for multi-service apps"

	t.Run("warns with services map and top-level agent hook", func(t *testing.T) {
		data := []byte(`{
			"appId": "com.example.app",
			"hooks": { "postStart": { "agent": "echo ready" } },
			"services": {
				"web": { "context": "web" }
			}
		}`)
		warnings := ValidateJSON(data)
		found := false
		for _, w := range warnings {
			if strings.Contains(w, warningSubstr) {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected warning about ignored top-level agent hook, got: %v", warnings)
		}
	})

	t.Run("no warning without services map", func(t *testing.T) {
		data := []byte(`{
			"appId": "com.example.app",
			"hooks": { "postStart": { "agent": "echo ready" } }
		}`)
		warnings := ValidateJSON(data)
		for _, w := range warnings {
			if strings.Contains(w, warningSubstr) {
				t.Fatalf("unexpected warning without services map: %q", w)
			}
		}
	})

	t.Run("no warning when top-level hook is openURL/cli only", func(t *testing.T) {
		data := []byte(`{
			"appId": "com.example.app",
			"hooks": { "postStart": { "openURL": "http://localhost:3000", "cli": "echo hi" } },
			"services": {
				"web": { "context": "web" }
			}
		}`)
		warnings := ValidateJSON(data)
		for _, w := range warnings {
			if strings.Contains(w, warningSubstr) {
				t.Fatalf("unexpected warning for openURL/cli-only top-level hook: %q", w)
			}
		}
	})
}

func TestValidate_ROS2_RejectsBadDomainID(t *testing.T) {
	bad := 500
	cfg := &AppConfig{AppID: "com.example.app", Frameworks: &FrameworksConfig{ROS2: &ROS2Config{DomainID: &bad}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for out-of-range domainId, got nil")
	}
}

func TestValidate_ROS2_RejectsUnknownRMW(t *testing.T) {
	cfg := &AppConfig{AppID: "com.example.app", Frameworks: &FrameworksConfig{ROS2: &ROS2Config{RMW: "rmw_bogus"}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for unknown rmw, got nil")
	}
}

func TestValidate_ROS2_RejectsUnknownDiscoveryScope(t *testing.T) {
	cfg := &AppConfig{AppID: "com.example.app", Frameworks: &FrameworksConfig{ROS2: &ROS2Config{DiscoveryScope: "global"}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for unknown discoveryScope, got nil")
	}
}

func TestValidate_ROS2_AcceptsValid(t *testing.T) {
	id := 42
	cfg := &AppConfig{AppID: "com.example.app", Frameworks: &FrameworksConfig{ROS2: &ROS2Config{DomainID: &id, RMW: "cyclonedds", Distro: "humble", DiscoveryScope: "host"}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid ros2 config rejected: %v", err)
	}
}

func TestValidate_ROS2_PerServiceDomainID(t *testing.T) {
	bad := -5
	cfg := &AppConfig{
		AppID: "com.example.app",
		Services: map[string]*ServiceConfig{
			"talker": {Context: "./talker", Frameworks: &FrameworksConfig{ROS2: &ROS2Config{DomainID: &bad}}},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for out-of-range per-service domainId, got nil")
	}
}

func TestValidate_ROS2_PerServiceUnknownRMW(t *testing.T) {
	cfg := &AppConfig{
		AppID: "com.example.app",
		Services: map[string]*ServiceConfig{
			"talker": {Context: "./talker", Frameworks: &FrameworksConfig{ROS2: &ROS2Config{RMW: "rmw_bogus"}}},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for unknown per-service rmw, got nil")
	}
}

func TestNotificationsEntitlementValid(t *testing.T) {
	cfg := &AppConfig{AppID: "test", Entitlements: []Entitlement{{Type: EntitlementNotifications}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if warnings := ValidateJSON([]byte(`{"appId":"test","entitlements":[{"type":"notifications"}]}`)); len(warnings) != 0 {
		t.Fatalf("got warnings for notifications entitlement: %v", warnings)
	}
}

func TestNotificationsEntitlementDuplicateRejected(t *testing.T) {
	cfg := &AppConfig{AppID: "test", Entitlements: []Entitlement{
		{Type: EntitlementNotifications}, {Type: EntitlementNotifications},
	}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for duplicate notifications entitlement")
	}
}

func TestAdminEntitlementValid(t *testing.T) {
	cfg := &AppConfig{AppID: "test", Entitlements: []Entitlement{{Type: EntitlementAdmin}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestAdminEntitlementDuplicateRejected(t *testing.T) {
	cfg := &AppConfig{AppID: "test", Entitlements: []Entitlement{
		{Type: EntitlementAdmin}, {Type: EntitlementAdmin},
	}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for duplicate admin entitlement")
	}
}

func TestAdminEntitlementJSONNoWarnings(t *testing.T) {
	warnings := ValidateJSON([]byte(`{"appId":"test","entitlements":[{"type":"admin"}]}`))
	if len(warnings) != 0 {
		t.Fatalf("got %d warnings, want 0: %v", len(warnings), warnings)
	}
}

func TestAdminEntitlementJSONUnknownKeyWarns(t *testing.T) {
	warnings := ValidateJSON([]byte(`{"appId":"test","entitlements":[{"type":"admin","bogus":1}]}`))
	if len(warnings) == 0 {
		t.Fatal("expected warning for unknown key on admin entitlement")
	}
}

func TestValidateJSON_BuildEntitlement(t *testing.T) {
	warnings := ValidateJSON([]byte(`{"appId":"test","entitlements":[{"type":"build"}]}`))
	if len(warnings) != 0 {
		t.Fatalf("expected build entitlement to validate, got warnings: %v", warnings)
	}
}

func TestValidateJSON_BuildEntitlementRejectsExtraKeys(t *testing.T) {
	warnings := ValidateJSON([]byte(`{"appId":"test","entitlements":[{"type":"build","bogus":1}]}`))
	if len(warnings) == 0 {
		t.Fatal("expected a warning for an unknown key on the build entitlement")
	}
}

// TestAppConfig_TopLevelEnv covers WDY-2040: a single-container app declares
// env at the top level, which is the only place it can.
func TestAppConfig_TopLevelEnv(t *testing.T) {
	cfg, err := LoadFromBytes([]byte(`{
		"appId": "envapp",
		"env": {"OTEL_LOGS_EXPORTER": "console", "TOKEN": "${HOST_TOKEN}"}
	}`))
	if err != nil {
		t.Fatalf("LoadFromBytes: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got, want := cfg.Env["OTEL_LOGS_EXPORTER"], "console"; got != want {
		t.Errorf("env[OTEL_LOGS_EXPORTER] = %q, want %q", got, want)
	}
	if got, want := cfg.Env["TOKEN"], "${HOST_TOKEN}"; got != want {
		t.Errorf("env[TOKEN] = %q, want %q (host expansion happens at deploy time)", got, want)
	}
}

func TestValidateEnv_RejectsMalformedKeys(t *testing.T) {
	for _, key := range []string{"BAD-KEY", "1LEADING", "has space", "WITH.DOT", ""} {
		cfg := &AppConfig{AppID: "envapp", Env: map[string]string{key: "1"}}
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate() accepted env key %q, want an error", key)
		}
	}

	for _, key := range []string{"OK", "_UNDERSCORE", "MIXED_case9"} {
		cfg := &AppConfig{AppID: "envapp", Env: map[string]string{key: "1"}}
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() rejected env key %q: %v", key, err)
		}
	}
}

func TestValidateEnv_RejectsMalformedServiceKeys(t *testing.T) {
	cfg := &AppConfig{
		AppID: "envapp",
		Services: map[string]*ServiceConfig{
			"api": {Context: ".", Env: map[string]string{"BAD-KEY": "1"}},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() accepted a malformed service env key, want an error")
	}
	if !strings.Contains(err.Error(), `services["api"].env`) {
		t.Errorf("error %q does not name the offending service env", err)
	}
}

// TestValidateJSON_UnknownTopLevelKey covers WDY-2040: keys the config does not
// decode are discarded by encoding/json, so they are reported as warnings.
func TestValidateJSON_UnknownTopLevelKey(t *testing.T) {
	warnings := ValidateJSON([]byte(`{
		"appId": "app",
		"$schema": "https://wendy.dev/schemas/wendy.json",
		"env": {"OK": "1"},
		"enviroment": {"OK": "1"},
		"local": {"darwin-arm64": "python3 app.py"}
	}`))

	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly 1", warnings)
	}
	// The warning also lists the allowed keys, so check the flagged half only.
	flagged, _, _ := strings.Cut(warnings[0], "Allowed keys are:")
	for _, want := range []string{"enviroment", "local"} {
		if !strings.Contains(flagged, want) {
			t.Errorf("warning %q does not mention %q", warnings[0], want)
		}
	}
	for _, unwanted := range []string{"appId", "$schema", "env,"} {
		if strings.Contains(flagged, unwanted) {
			t.Errorf("warning %q flags known key %s", warnings[0], unwanted)
		}
	}
}

func TestValidateJSON_UnknownServiceKey(t *testing.T) {
	warnings := ValidateJSON([]byte(`{
		"appId": "app",
		"services": {"api": {"context": ".", "environment": {"OK": "1"}}}
	}`))

	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly 1", warnings)
	}
	if !strings.Contains(warnings[0], "environment") || !strings.Contains(warnings[0], `services["api"]`) {
		t.Errorf("warning %q does not name the service and key", warnings[0])
	}
}

// A fleet manifest has its own keys and its own validation; ValidateJSON must
// not report them as unknown.
func TestValidateJSON_FleetManifestKeysNotFlagged(t *testing.T) {
	warnings := ValidateJSON([]byte(`{
		"appId": "fleet",
		"components": {"cam": {"path": "./cam", "tags": ["camera-*"]}}
	}`))
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
}

func TestValidateJSON_KnownKeysProduceNoWarnings(t *testing.T) {
	warnings := ValidateJSON([]byte(`{
		"appId": "app",
		"version": "0.1.0",
		"platform": "linux",
		"language": "swift",
		"xcode": {"scheme": "App"},
		"run": {"args": ["--flag"]},
		"entitlements": [{"type": "network", "mode": "host"}],
		"readiness": {"tcpSocket": {"port": 8080}},
		"hooks": {"postStart": {"openURL": "http://localhost:8080"}},
		"python": {"sourceRoot": "src"},
		"debug": false,
		"files": [{"path": "assets"}],
		"brewfile": "Brewfile.wendy",
		"isolation": "shared-ipc",
		"frameworks": {"ros2": {"distro": "humble"}},
		"resources": {"cpus": 1},
		"env": {"OK": "1"},
		"services": {"api": {"context": ".", "env": {"OK": "1"}}}
	}`))
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
}
