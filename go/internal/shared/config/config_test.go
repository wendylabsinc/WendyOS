package config

import (
	"os"
	"path/filepath"
	"testing"
)

// overrideConfigDir overrides the config directory for testing by setting HOME
// to a temp directory, ensuring ConfigDir returns a path within it.
func overrideHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	return dir
}

func TestLoad_NoFile(t *testing.T) {
	overrideHome(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg == nil {
		t.Fatal("Load() returned nil config")
	}
	if len(cfg.Auth) != 0 {
		t.Errorf("Load() Auth length = %d, want 0", len(cfg.Auth))
	}
}

func TestSave_And_Load(t *testing.T) {
	overrideHome(t)

	original := &Config{
		DefaultDevice: "wendy-test.local",
		Auth: []AuthConfig{
			{
				CloudDashboard: "https://dashboard.example.com",
				CloudGRPC:      "grpc.example.com:443",
				Certificates: []CertificateInfo{
					{
						PemCertificate: "-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----",
						OrganizationID: 42,
						UserID:         "user-123",
					},
				},
			},
		},
		Analytics: &AnalyticsConfig{Enabled: true},
	}

	if err := Save(original); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if loaded.DefaultDevice != original.DefaultDevice {
		t.Errorf("DefaultDevice = %q, want %q", loaded.DefaultDevice, original.DefaultDevice)
	}
	if len(loaded.Auth) != 1 {
		t.Fatalf("Auth length = %d, want 1", len(loaded.Auth))
	}
	if loaded.Auth[0].CloudDashboard != "https://dashboard.example.com" {
		t.Errorf("CloudDashboard = %q, want %q", loaded.Auth[0].CloudDashboard, "https://dashboard.example.com")
	}
	if loaded.Auth[0].CloudGRPC != "grpc.example.com:443" {
		t.Errorf("CloudGRPC = %q, want %q", loaded.Auth[0].CloudGRPC, "grpc.example.com:443")
	}
	if len(loaded.Auth[0].Certificates) != 1 {
		t.Fatalf("Certificates length = %d, want 1", len(loaded.Auth[0].Certificates))
	}
	if loaded.Auth[0].Certificates[0].OrganizationID != 42 {
		t.Errorf("OrganizationID = %d, want 42", loaded.Auth[0].Certificates[0].OrganizationID)
	}
	if loaded.Analytics == nil || !loaded.Analytics.Enabled {
		t.Error("Analytics.Enabled = false, want true")
	}
}

func TestAddAuth_NewEntry(t *testing.T) {
	cfg := &Config{}
	auth := AuthConfig{
		CloudDashboard: "https://dash.example.com",
		CloudGRPC:      "grpc.example.com:443",
	}

	cfg.AddAuth(auth)

	if len(cfg.Auth) != 1 {
		t.Fatalf("Auth length = %d, want 1", len(cfg.Auth))
	}
	if cfg.Auth[0].CloudDashboard != auth.CloudDashboard {
		t.Errorf("CloudDashboard = %q, want %q", cfg.Auth[0].CloudDashboard, auth.CloudDashboard)
	}
}

func TestAddAuth_ReplaceExisting(t *testing.T) {
	cfg := &Config{
		Auth: []AuthConfig{
			{
				CloudDashboard: "https://dash.example.com",
				CloudGRPC:      "grpc.example.com:443",
				Certificates: []CertificateInfo{
					{OrganizationID: 1},
				},
			},
		},
	}

	replacement := AuthConfig{
		CloudDashboard: "https://dash.example.com",
		CloudGRPC:      "grpc.example.com:443",
		Certificates: []CertificateInfo{
			{OrganizationID: 99, UserID: "new-user"},
		},
	}

	cfg.AddAuth(replacement)

	if len(cfg.Auth) != 1 {
		t.Fatalf("Auth length = %d, want 1 (should replace, not append)", len(cfg.Auth))
	}
	if cfg.Auth[0].Certificates[0].OrganizationID != 99 {
		t.Errorf("OrganizationID = %d, want 99", cfg.Auth[0].Certificates[0].OrganizationID)
	}
	if cfg.Auth[0].Certificates[0].UserID != "new-user" {
		t.Errorf("UserID = %q, want %q", cfg.Auth[0].Certificates[0].UserID, "new-user")
	}
}

func TestConfigDir(t *testing.T) {
	home := overrideHome(t)

	dir, err := ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir() error = %v", err)
	}

	expected := filepath.Join(home, ".wendy")
	if dir != expected {
		t.Errorf("ConfigDir() = %q, want %q", dir, expected)
	}

	// Should have created the directory.
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat config dir: %v", err)
	}
	if !info.IsDir() {
		t.Error("ConfigDir() path is not a directory")
	}
}
