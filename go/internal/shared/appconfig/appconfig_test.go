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
			{Type: EntitlementMCP, Port: 3000},
		},
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() unexpected error: %v", err)
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
