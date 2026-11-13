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

// A `wendy auth login` in another terminal after the MCP server started must
// survive the agent setting a default device: the handler used to save the
// startup snapshot, which had no login in it.
func TestDeviceSetDefaultKeepsConfigWrittenAfterStartup(t *testing.T) {
	useConfigDir(t)
	srv := New(&config.Config{}, nil) // startup snapshot: not logged in
	if err := config.Save(&config.Config{Auth: []config.AuthConfig{laterLogin}}); err != nil {
		t.Fatal(err)
	}
	result, err := srv.callTool(context.Background(), "device_set_default", map[string]any{"address": "thor.local:50051"})
	if err != nil || result.IsError {
		t.Fatalf("device_set_default: err=%v result=%+v", err, result)
	}
	onDisk, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.DefaultDevice != "thor.local:50051" {
		t.Fatalf("DefaultDevice = %q, want thor.local:50051", onDisk.DefaultDevice)
	}
	if len(onDisk.Auth) != 1 || onDisk.Auth[0].CloudGRPC != "grpc.a.sh:443" {
		t.Fatalf("device_set_default dropped a login written after the server started: %+v", onDisk.Auth)
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
	startup := &config.Config{DefaultDevice: "startup.local"}
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
