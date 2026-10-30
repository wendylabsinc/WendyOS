package mcp

import (
	"context"
	"errors"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func useConfigDir(t *testing.T) {
	t.Helper()
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
}

func enableReload(t *testing.T, load func() (*config.Config, error)) {
	t.Helper()
	prev := reloadConfigFn
	EnableConfigReload(load)
	t.Cleanup(func() { reloadConfigFn = prev })
}

var laterLogin = config.AuthConfig{CloudGRPC: "grpc.a.sh:443", Certificates: []config.CertificateInfo{{OrganizationID: 7}}}

// device_set_default stays registered so published relay catalogs that list it
// still pair, but Wendy no longer saves a default device: the call fails and
// leaves the config alone.
func TestDeviceSetDefaultIsUnsupported(t *testing.T) {
	useConfigDir(t)
	srv := New(&config.Config{}, nil)
	if err := config.Save(&config.Config{Auth: []config.AuthConfig{laterLogin}}); err != nil {
		t.Fatal(err)
	}
	result, err := srv.callTool(context.Background(), "device_set_default", map[string]any{"address": "thor.local:50051"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || structuredMap(t, result)["error_code"] != string(errCodeUnsupported) {
		t.Fatalf("device_set_default = %+v, want an UNSUPPORTED error", result)
	}
	onDisk, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(onDisk.Auth) != 1 || onDisk.Auth[0].CloudGRPC != "grpc.a.sh:443" {
		t.Fatalf("device_set_default changed the config: %+v", onDisk.Auth)
	}
}

func TestCloudAuthEntryReadsALoginWrittenAfterStartup(t *testing.T) {
	useConfigDir(t)
	enableReload(t, config.Load)
	srv := New(&config.Config{}, nil)
	if _, err := srv.cloudAuthEntry(""); err == nil {
		t.Fatal("not logged in yet: want an error")
	}
	if err := config.Save(&config.Config{Auth: []config.AuthConfig{laterLogin}}); err != nil {
		t.Fatal(err)
	}
	auth, err := srv.cloudAuthEntry("")
	if err != nil {
		t.Fatalf("cloudAuthEntry after a login in another process: %v", err)
	}
	if auth.CloudGRPC != "grpc.a.sh:443" {
		t.Fatalf("auth = %+v, want the new login", auth)
	}
}

func TestCurrentConfigFallsBackToTheStartupSnapshot(t *testing.T) {
	startup := &config.Config{DefaultBuildHost: "startup.local"}
	srv := New(startup, nil)

	prev := reloadConfigFn
	t.Cleanup(func() { reloadConfigFn = prev })
	reloadConfigFn = nil
	if got := srv.currentConfig(); got != startup {
		t.Fatal("with reloading off (tests, in-memory servers) currentConfig must be the startup snapshot")
	}
	enableReload(t, func() (*config.Config, error) { return nil, errors.New("config.json unreadable") })
	if got := srv.currentConfig(); got != startup {
		t.Fatal("an unreadable config.json must fall back to the startup snapshot, not nil")
	}
}
