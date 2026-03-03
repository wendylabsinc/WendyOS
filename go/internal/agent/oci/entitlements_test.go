package oci

import (
	"slices"
	"testing"

	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
)

func hasGID(spec *Spec, gid uint32) bool {
	return slices.Contains(spec.Process.User.AdditionalGids, gid)
}

func hasMountDest(spec *Spec, dest string) bool {
	for _, m := range spec.Mounts {
		if m.Destination == dest {
			return true
		}
	}
	return false
}

func hasNamespace(spec *Spec, nsType string) bool {
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == nsType {
			return true
		}
	}
	return false
}

func hasEnv(spec *Spec, envPrefix string) bool {
	for _, e := range spec.Process.Env {
		if len(e) >= len(envPrefix) && e[:len(envPrefix)] == envPrefix {
			return true
		}
	}
	return false
}

func TestApplyEntitlements_GPU(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID: "test-app",
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementGPU},
		},
	}

	if err := ApplyEntitlements(spec, cfg); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}

	// Should add nvidia group GID 44.
	if !hasGID(spec, 44) {
		t.Error("GPU entitlement did not add GID 44")
	}

	// Should have NVIDIA device nodes.
	foundNvidiaDevice := false
	for _, dev := range spec.Linux.Devices {
		if dev.Path == "/dev/nvidia0" {
			foundNvidiaDevice = true
			break
		}
	}
	if !foundNvidiaDevice {
		t.Error("GPU entitlement did not add /dev/nvidia0 device")
	}

	// Should have NVIDIA env vars.
	if !hasEnv(spec, "NVIDIA_VISIBLE_DEVICES") {
		t.Error("GPU entitlement did not set NVIDIA_VISIBLE_DEVICES")
	}
}

func TestApplyEntitlements_Network_Host(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID: "test-app",
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementNetwork, Mode: "host"},
		},
	}

	if err := ApplyEntitlements(spec, cfg); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}

	// Network namespace should be removed for host networking.
	if hasNamespace(spec, "network") {
		t.Error("host network entitlement did not remove network namespace")
	}

	// CAP_NET_ADMIN should be added.
	if !slices.Contains(spec.Process.Capabilities.Bounding, "CAP_NET_ADMIN") {
		t.Error("host network entitlement did not add CAP_NET_ADMIN")
	}
}

func TestApplyEntitlements_Network_Default(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID: "test-app",
		Entitlements: []appconfig.Entitlement{
			// Empty mode defaults to "host" per the code.
			{Type: appconfig.EntitlementNetwork, Mode: "none"},
		},
	}

	if err := ApplyEntitlements(spec, cfg); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}

	// With mode "none", the network namespace should remain (namespaced networking).
	if !hasNamespace(spec, "network") {
		t.Error("network mode 'none' should keep network namespace")
	}
}

func TestApplyEntitlements_Audio(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID: "test-app",
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementAudio},
		},
	}

	if err := ApplyEntitlements(spec, cfg); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}

	// Should add audio group GID 29.
	if !hasGID(spec, 29) {
		t.Error("audio entitlement did not add GID 29")
	}

	// Should mount /dev/snd.
	if !hasMountDest(spec, "/dev/snd") {
		t.Error("audio entitlement did not add /dev/snd mount")
	}

	// Should mount PipeWire socket.
	if !hasMountDest(spec, "/run/pipewire") {
		t.Error("audio entitlement did not add /run/pipewire mount")
	}

	if !hasEnv(spec, "PIPEWIRE_RUNTIME_DIR") {
		t.Error("audio entitlement did not set PIPEWIRE_RUNTIME_DIR")
	}
}

func TestApplyEntitlements_Persist(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID: "my-app",
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementPersist, Name: "data", Path: "/app/data"},
		},
	}

	if err := ApplyEntitlements(spec, cfg); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}

	// Should add a bind mount for /app/data.
	if !hasMountDest(spec, "/app/data") {
		t.Error("persist entitlement did not add /app/data mount")
	}

	// Verify the source path includes the app ID and volume name.
	for _, m := range spec.Mounts {
		if m.Destination == "/app/data" {
			expected := "/var/lib/wendy/volumes/my-app/data"
			if m.Source != expected {
				t.Errorf("persist mount source = %q, want %q", m.Source, expected)
			}
			if m.Type != "bind" {
				t.Errorf("persist mount type = %q, want %q", m.Type, "bind")
			}
			break
		}
	}
}

func TestApplyEntitlements_Bluetooth(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID: "test-app",
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementBluetooth},
		},
	}

	if err := ApplyEntitlements(spec, cfg); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}

	// Should mount D-Bus sockets.
	if !hasMountDest(spec, "/var/run/dbus") {
		t.Error("bluetooth entitlement did not add /var/run/dbus mount")
	}
	if !hasMountDest(spec, "/run/dbus") {
		t.Error("bluetooth entitlement did not add /run/dbus mount")
	}

	if !hasEnv(spec, "DBUS_SYSTEM_BUS_ADDRESS") {
		t.Error("bluetooth entitlement did not set DBUS_SYSTEM_BUS_ADDRESS")
	}
}

func TestApplyEntitlements_Video(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID: "test-app",
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementVideo},
		},
	}

	if err := ApplyEntitlements(spec, cfg); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}

	// Should add video group GID 44.
	if !hasGID(spec, 44) {
		t.Error("video entitlement did not add GID 44")
	}

	// Should mount /dev/video0.
	if !hasMountDest(spec, "/dev/video0") {
		t.Error("video entitlement did not add /dev/video0 mount")
	}
}

func TestApplyEntitlements_Multiple(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID: "multi-app",
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementGPU},
			{Type: appconfig.EntitlementAudio},
			{Type: appconfig.EntitlementNetwork, Mode: "host"},
			{Type: appconfig.EntitlementPersist, Name: "models", Path: "/models"},
		},
	}

	if err := ApplyEntitlements(spec, cfg); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}

	// GPU
	if !hasGID(spec, 44) {
		t.Error("missing GPU GID 44")
	}

	// Audio
	if !hasGID(spec, 29) {
		t.Error("missing audio GID 29")
	}
	if !hasMountDest(spec, "/dev/snd") {
		t.Error("missing /dev/snd mount")
	}

	// Network host
	if hasNamespace(spec, "network") {
		t.Error("network namespace should be removed for host mode")
	}

	// Persist
	if !hasMountDest(spec, "/models") {
		t.Error("missing /models mount")
	}
}

func TestApplyEntitlements_Empty(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID:        "test-app",
		Entitlements: []appconfig.Entitlement{},
	}

	originalMountCount := len(spec.Mounts)
	originalNSCount := len(spec.Linux.Namespaces)

	if err := ApplyEntitlements(spec, cfg); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}

	// Spec should remain unchanged.
	if len(spec.Mounts) != originalMountCount {
		t.Errorf("mount count changed from %d to %d with no entitlements",
			originalMountCount, len(spec.Mounts))
	}
	if len(spec.Linux.Namespaces) != originalNSCount {
		t.Errorf("namespace count changed from %d to %d with no entitlements",
			originalNSCount, len(spec.Linux.Namespaces))
	}
}
