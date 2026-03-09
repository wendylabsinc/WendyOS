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

	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
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

	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
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

	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
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

	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
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

	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
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

func TestApplyEntitlements_Bluetooth_NoProxy(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID: "test-app",
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementBluetooth},
		},
	}

	if err := ApplyEntitlements(spec, cfg, ApplyOptions{DBusProxyAvailable: false}); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}

	// Without the proxy, raw host D-Bus sockets must NOT be mounted
	// (they expose NetworkManager and other privileged services).
	if hasMountDest(spec, "/var/run/dbus") {
		t.Error("bluetooth without proxy should not mount /var/run/dbus")
	}
	if hasMountDest(spec, "/run/dbus") {
		t.Error("bluetooth without proxy should not mount /run/dbus")
	}

	// The env var should still be set so apps know the expected path.
	if !hasEnv(spec, "DBUS_SYSTEM_BUS_ADDRESS") {
		t.Error("bluetooth entitlement did not set DBUS_SYSTEM_BUS_ADDRESS")
	}
}

func TestApplyEntitlements_Bluetooth_Proxy(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID: "bt-app",
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementBluetooth},
		},
	}

	if err := ApplyEntitlements(spec, cfg, ApplyOptions{DBusProxyAvailable: true}); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}

	// Proxy mode should mount from the proxy directory.
	if !hasMountDest(spec, "/var/run/dbus") {
		t.Error("bluetooth proxy did not add /var/run/dbus mount")
	}

	// Should NOT have /run/dbus (only one mount with proxy).
	if hasMountDest(spec, "/run/dbus") {
		t.Error("bluetooth proxy should not add /run/dbus mount")
	}

	// Verify source points to proxy directory.
	for _, m := range spec.Mounts {
		if m.Destination == "/var/run/dbus" {
			expected := "/run/wendy/dbus-proxy/bt-app"
			if m.Source != expected {
				t.Errorf("proxy /var/run/dbus source = %q, want %q", m.Source, expected)
			}
		}
	}

	if !hasEnv(spec, "DBUS_SYSTEM_BUS_ADDRESS") {
		t.Error("bluetooth entitlement did not set DBUS_SYSTEM_BUS_ADDRESS")
	}
}

// TestBluetoothEntitlementDoesNotExposeNetworkManager verifies that enabling
// only the Bluetooth entitlement does not give the container unrestricted
// access to the D-Bus system bus. Mounting the raw host D-Bus socket
// (/var/run/dbus, /run/dbus) lets the container talk to every D-Bus service,
// including NetworkManager — effectively granting root-level network control
// to a container that only asked for Bluetooth.
func TestBluetoothEntitlementDoesNotExposeNetworkManager(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID: "bt-only-app",
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementBluetooth},
		},
	}

	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}

	// The raw host D-Bus system socket must NOT be bind-mounted into the
	// container. Doing so exposes every D-Bus service (NetworkManager,
	// systemd, polkit, etc.) — not just BlueZ. D-Bus access should be
	// filtered/proxied so only org.bluez is reachable.
	for _, m := range spec.Mounts {
		if m.Source == "/var/run/dbus" || m.Source == "/run/dbus" {
			t.Errorf("Bluetooth entitlement bind-mounts raw D-Bus system socket %q -> %q; "+
				"this exposes NetworkManager and other privileged D-Bus services. "+
				"D-Bus access must be scoped to BlueZ only (org.bluez).",
				m.Source, m.Destination)
		}
	}

	// The network namespace must remain intact — Bluetooth should not
	// alter network isolation.
	if !hasNamespace(spec, "network") {
		t.Error("Bluetooth-only entitlement removed the network namespace")
	}

	// CAP_NET_ADMIN must not be granted by Bluetooth alone.
	if spec.Process.Capabilities != nil &&
		slices.Contains(spec.Process.Capabilities.Bounding, "CAP_NET_ADMIN") {
		t.Error("Bluetooth entitlement should not grant CAP_NET_ADMIN")
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

	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}

	// Should add video group GID 44.
	if !hasGID(spec, 44) {
		t.Error("video entitlement did not add GID 44")
	}

	// Should add a cgroup rule for V4L2 devices (major 81).
	foundV4L2Rule := false
	for _, d := range spec.Linux.Resources.Devices {
		if d.Major != nil && *d.Major == 81 && d.Allow {
			foundV4L2Rule = true
			break
		}
	}
	if !foundV4L2Rule {
		t.Error("video entitlement did not add V4L2 cgroup device rule (major 81)")
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

	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
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

	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
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
