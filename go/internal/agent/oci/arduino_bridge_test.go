package oci

import (
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

func withArduinoRouterDir(t *testing.T, dir string) {
	t.Helper()
	orig := ArduinoRouterDir
	t.Cleanup(func() { ArduinoRouterDir = orig })
	ArduinoRouterDir = dir
}

func withArduinoRouterGroup(t *testing.T, gid string, err error) {
	t.Helper()
	orig := lookupArduinoRouterGroup
	t.Cleanup(func() { lookupArduinoRouterGroup = orig })
	lookupArduinoRouterGroup = func(name string) (*user.Group, error) {
		if err != nil {
			return nil, err
		}
		return &user.Group{Name: name, Gid: gid}, nil
	}
}

func bridgeConfig() *appconfig.AppConfig {
	return &appconfig.AppConfig{AppID: "test",
		Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementArduinoBridge}}}
}

func TestApplyArduinoBridge_MountsDirAndSetsSocket(t *testing.T) {
	dir := t.TempDir()
	withArduinoRouterDir(t, dir)
	withArduinoRouterGroup(t, "4242", nil)

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	if err := ApplyEntitlements(spec, bridgeConfig(), ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements: %v", err)
	}
	m, ok := mountForDest(spec, "/run/arduino-router")
	if !ok {
		t.Fatalf("no /run/arduino-router mount in %+v", spec.Mounts)
	}
	if m.Source != dir || m.Type != "bind" {
		t.Errorf("mount = %+v, want bind of %s", m, dir)
	}
	if !hasEnv(spec, "APP_SOCKET=unix:///run/arduino-router/router.sock") {
		t.Errorf("env %v lacks APP_SOCKET", spec.Process.Env)
	}
}

func TestApplyArduinoBridge_KeepsAppSocket(t *testing.T) {
	withArduinoRouterDir(t, t.TempDir())
	withArduinoRouterGroup(t, "4242", nil)

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	spec.Process.Env = append(spec.Process.Env, "APP_SOCKET=tcp://x:1")
	if err := ApplyEntitlements(spec, bridgeConfig(), ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements: %v", err)
	}
	if got, _ := envValue(spec.Process.Env, "APP_SOCKET"); got != "tcp://x:1" {
		t.Errorf("APP_SOCKET = %q, want the app's tcp://x:1", got)
	}
}

func TestApplyArduinoBridge_MissingDirFails(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	withArduinoRouterDir(t, missing)
	withArduinoRouterGroup(t, "4242", nil)

	err := ApplyEntitlements(DefaultSpec("/rootfs", []string{"/bin/sh"}), bridgeConfig(), ApplyOptions{})
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("ApplyEntitlements() = %v, want an error naming %s", err, missing)
	}
}

func TestApplyArduinoBridge_AddsGroupGID(t *testing.T) {
	withArduinoRouterDir(t, t.TempDir())
	withArduinoRouterGroup(t, "4242", nil)

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	if err := ApplyEntitlements(spec, bridgeConfig(), ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements: %v", err)
	}
	if !hasGID(spec, 4242) {
		t.Errorf("AdditionalGids = %v, want 4242", spec.Process.User.AdditionalGids)
	}
}

func TestApplyArduinoBridge_MissingGroupFails(t *testing.T) {
	withArduinoRouterDir(t, t.TempDir())
	withArduinoRouterGroup(t, "", user.UnknownGroupError("arduino-router"))

	err := ApplyEntitlements(DefaultSpec("/rootfs", []string{"/bin/sh"}), bridgeConfig(), ApplyOptions{})
	if err == nil || !strings.Contains(err.Error(), "arduino-router") {
		t.Fatalf("ApplyEntitlements() = %v, want an arduino-router group error", err)
	}
}

func TestArduinoBridgeAvailable(t *testing.T) {
	withArduinoRouterDir(t, t.TempDir())
	if !ArduinoBridgeAvailable() {
		t.Error("ArduinoBridgeAvailable() = false with the directory present")
	}
	withArduinoRouterDir(t, filepath.Join(t.TempDir(), "absent"))
	if ArduinoBridgeAvailable() {
		t.Error("ArduinoBridgeAvailable() = true with the directory missing")
	}
}

func TestApplyArduinoBridge_MountsDirReadOnly(t *testing.T) {
	withArduinoRouterDir(t, t.TempDir())
	withArduinoRouterGroup(t, "4242", nil)
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	if err := ApplyEntitlements(spec, bridgeConfig(), ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	m, _ := mountForDest(spec, "/run/arduino-router")
	if !slices.Contains(m.Options, "ro") || slices.Contains(m.Options, "rw") {
		t.Fatalf("mount options = %v, want ro and not rw", m.Options)
	}
}
