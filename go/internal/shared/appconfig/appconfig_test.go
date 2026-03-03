package appconfig

import (
	"os"
	"path/filepath"
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

func TestValidate_GPIORequiresPins(t *testing.T) {
	cfg := &AppConfig{
		AppID: "com.example.app",
		Entitlements: []Entitlement{
			{Type: EntitlementGPIO},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() expected error for gpio without pins, got nil")
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
		},
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() unexpected error: %v", err)
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
