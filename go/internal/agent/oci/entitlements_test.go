package oci

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
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

func mountForDest(spec *Spec, dest string) (Mount, bool) {
	for _, m := range spec.Mounts {
		if m.Destination == dest {
			return m, true
		}
	}
	return Mount{}, false
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

func hasCapability(spec *Spec, cap string) bool {
	if spec.Process.Capabilities == nil {
		return false
	}
	return slices.Contains(spec.Process.Capabilities.Bounding, cap)
}

func hasAllowAllDeviceRule(spec *Spec) bool {
	for _, d := range spec.Linux.Resources.Devices {
		if d.Allow && d.Type == "" && d.Major == nil && d.Minor == nil {
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

func TestApplyEntitlements_Network_Host_ResolvConf(t *testing.T) {
	const resolvedConf = "/run/systemd/resolve/resolv.conf"
	_, errSystemd := os.Stat(resolvedConf)
	_, errHost := os.Stat("/etc/resolv.conf")
	if errSystemd != nil && errHost != nil {
		t.Skip("no resolv.conf on host; skipping DNS mount assertion")
	}

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

	// A container with host networking but its own mount namespace needs
	// /etc/resolv.conf bind-mounted from the host; otherwise the container
	// rootfs may have an empty file and all DNS lookups fail.
	if !hasMountDest(spec, "/etc/resolv.conf") {
		t.Fatal("host network entitlement did not mount /etc/resolv.conf")
	}

	for _, m := range spec.Mounts {
		if m.Destination == "/etc/resolv.conf" {
			if m.Source != resolvedConf && m.Source != "/etc/resolv.conf" {
				t.Errorf("/etc/resolv.conf source = %q, want %q or %q",
					m.Source, resolvedConf, "/etc/resolv.conf")
			}
			if m.Type != "bind" {
				t.Errorf("/etc/resolv.conf mount type = %q, want \"bind\"", m.Type)
			}
			break
		}
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

	// PipeWire mount is conditional — only added when a real socket exists
	// on the host at either /run/pipewire/pipewire-0 (system) or
	// /run/user/*/pipewire-0 (user session).
	isSocket := func(path string) bool {
		fi, err := os.Lstat(path)
		return err == nil && fi.Mode()&os.ModeSocket != 0 && fi.Mode()&os.ModeSymlink == 0
	}
	// Mirror applyAudio's socket detection: system path first, then user session.
	var pipewireSocketSource string
	if isSocket("/run/pipewire/pipewire-0") {
		pipewireSocketSource = "/run/pipewire/pipewire-0"
	} else {
		userSockets, _ := filepath.Glob("/run/user/*/pipewire-0")
		for _, s := range userSockets {
			if isSocket(s) {
				pipewireSocketSource = s
				break
			}
		}
	}
	if pipewireSocketSource != "" {
		if !hasMountDest(spec, "/run/pipewire/pipewire-0") {
			t.Error("audio entitlement did not add /run/pipewire/pipewire-0 mount")
		}
		if !hasEnv(spec, "PIPEWIRE_RUNTIME_DIR") {
			t.Error("audio entitlement did not set PIPEWIRE_RUNTIME_DIR")
		}
		// Derive pulse path from the same source directory as applyAudio does.
		sourceDir := filepath.Dir(pipewireSocketSource)
		pulseNative := filepath.Join(sourceDir, "pulse", "native")
		if isSocket(pulseNative) {
			if !hasMountDest(spec, "/run/pipewire/pulse-native") {
				t.Error("audio entitlement did not add /run/pipewire/pulse-native mount")
			}
			if !hasEnv(spec, "PULSE_SERVER") {
				t.Error("audio entitlement did not set PULSE_SERVER when pulse socket exists")
			}
		}
	} else {
		if hasMountDest(spec, "/run/pipewire/pipewire-0") || hasMountDest(spec, "/run/pipewire/pulse-native") {
			t.Error("audio entitlement should not mount /run/pipewire when socket is absent")
		}
	}

	// Audio should remain constrained to explicit sound-device rules even
	// though it calls SetDeviceCapabilities().
	foundSoundRule := false
	for _, d := range spec.Linux.Resources.Devices {
		if d.Major != nil && *d.Major == 116 && d.Allow {
			foundSoundRule = true
			break
		}
	}
	if !foundSoundRule {
		t.Error("audio entitlement did not add sound cgroup device rule (major 116)")
	}
	if hasAllowAllDeviceRule(spec) {
		t.Error("audio entitlement should not add a generic allow-all device cgroup rule")
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

	// Verify the source path uses the volume name (shared across apps).
	for _, m := range spec.Mounts {
		if m.Destination == "/app/data" {
			expected := "/var/lib/wendy/volumes/data"
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

func assertCameraEntitlement(t *testing.T, spec *Spec, entType string) {
	t.Helper()

	if !hasGID(spec, 44) {
		t.Errorf("%s entitlement did not add GID 44", entType)
	}

	foundV4L2Rule := false
	for _, d := range spec.Linux.Resources.Devices {
		if d.Major != nil && *d.Major == 81 && d.Allow {
			foundV4L2Rule = true
			break
		}
	}
	if !foundV4L2Rule {
		t.Errorf("%s entitlement did not add V4L2 cgroup device rule (major 81)", entType)
	}
	if hasAllowAllDeviceRule(spec) {
		t.Errorf("%s entitlement should not add a generic allow-all device cgroup rule", entType)
	}

	devMount, ok := mountForDest(spec, "/dev")
	if !ok {
		t.Fatalf("%s entitlement did not define /dev mount", entType)
	}
	if devMount.Source != "/dev" || devMount.Type != "bind" {
		t.Fatalf("%s entitlement /dev mount = %+v, want bind mount from host /dev", entType, devMount)
	}
	if !slices.Contains(devMount.Options, "rbind") {
		t.Errorf("%s entitlement /dev mount missing rbind option", entType)
	}
	if !slices.Contains(devMount.Options, "rw") {
		t.Errorf("%s entitlement /dev mount missing rw option", entType)
	}
	if !slices.Contains(devMount.Options, "noexec") {
		t.Errorf("%s entitlement /dev mount missing noexec option", entType)
	}
	if !hasCapability(spec, "CAP_SYS_PTRACE") {
		t.Errorf("%s entitlement did not add device capability wiring", entType)
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

	assertCameraEntitlement(t, spec, "video")
}

func TestApplyEntitlements_Camera(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID: "test-app",
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementCamera},
		},
	}

	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}

	assertCameraEntitlement(t, spec, "camera")
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

func TestApplyEntitlements_Input(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID: "test-app",
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementInput},
		},
	}

	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}

	// Should add input group GID 105.
	if !hasGID(spec, 105) {
		t.Error("input entitlement did not add GID 105")
	}

	// Should mount /dev/input.
	if !hasMountDest(spec, "/dev/input") {
		t.Error("input entitlement did not add /dev/input mount")
	}

	// Verify mount options.
	for _, m := range spec.Mounts {
		if m.Destination == "/dev/input" {
			if m.Source != "/dev/input" {
				t.Errorf("input mount source = %q, want %q", m.Source, "/dev/input")
			}
			if m.Type != "bind" {
				t.Errorf("input mount type = %q, want %q", m.Type, "bind")
			}
			if !slices.Contains(m.Options, "rbind") {
				t.Error("input mount missing rbind option")
			}
			if !slices.Contains(m.Options, "nosuid") {
				t.Error("input mount missing nosuid option")
			}
			if !slices.Contains(m.Options, "noexec") {
				t.Error("input mount missing noexec option")
			}
			break
		}
	}

	// Should add a cgroup rule for input devices (major 13).
	foundInputRule := false
	for _, d := range spec.Linux.Resources.Devices {
		if d.Major != nil && *d.Major == 13 && d.Allow {
			foundInputRule = true
			break
		}
	}
	if !foundInputRule {
		t.Error("input entitlement did not add cgroup device rule (major 13)")
	}
}

func TestApplyEntitlements_GPIO(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID: "test-app",
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementGPIO, Pins: []int{5, 6, 13}},
		},
	}

	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}

	// Should add a cgroup rule for GPIO devices (major 254).
	foundGPIORule := false
	for _, d := range spec.Linux.Resources.Devices {
		if d.Major != nil && *d.Major == 254 && d.Allow {
			foundGPIORule = true
			break
		}
	}
	if !foundGPIORule {
		t.Error("gpio entitlement did not add cgroup device rule (major 254)")
	}

	// Should mount /dev/gpiochip0 when it exists on the host.
	t.Run("mounts /dev/gpiochip0 when present", func(t *testing.T) {
		if _, err := os.Stat("/dev/gpiochip0"); err == nil {
			if !hasMountDest(spec, "/dev/gpiochip0") {
				t.Error("gpio entitlement did not add /dev/gpiochip0 mount")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat /dev/gpiochip0: %v", err)
		} else {
			t.Skip("/dev/gpiochip0 not present on this host")
		}
	})
}

func TestApplyEntitlements_SPI(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID: "test-app",
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementSPI},
		},
	}

	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}

	foundSPIRule := false
	for _, d := range spec.Linux.Resources.Devices {
		if d.Major != nil && *d.Major == 153 && d.Allow {
			foundSPIRule = true
			break
		}
	}
	if !foundSPIRule {
		t.Error("spi entitlement did not add SPI cgroup device rule (major 153)")
	}

	// SPI group GID: if the "spi" group exists on this host, verify it was added.
	// If not (e.g. macOS, ubuntu CI), verify we didn't add a bogus GID.
	if grp, err := user.LookupGroup("spi"); err == nil {
		gid, err := strconv.ParseUint(grp.Gid, 10, 32)
		if err != nil {
			t.Fatalf("failed to parse spi group GID %q: %v", grp.Gid, err)
		}
		if !hasGID(spec, uint32(gid)) {
			t.Errorf("spi entitlement did not add spi group GID %d", gid)
		}
	} else if len(spec.Process.User.AdditionalGids) != 0 {
		t.Errorf("spi group not present on host but AdditionalGids is not empty: %v", spec.Process.User.AdditionalGids)
	}

	// Should mount /dev/spidev0.0 when it exists on the host.
	t.Run("mounts /dev/spidev0.0 when present", func(t *testing.T) {
		if _, err := os.Stat("/dev/spidev0.0"); err == nil {
			if !hasMountDest(spec, "/dev/spidev0.0") {
				t.Error("spi entitlement did not add /dev/spidev0.0 mount")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat /dev/spidev0.0: %v", err)
		} else {
			t.Skip("/dev/spidev0.0 not present on this host")
		}
	})
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

// TestApplyI2C_PathTraversal verifies that a crafted device name cannot escape /dev/i2c-
// (WDY-1015).
func TestApplyI2C_PathTraversal(t *testing.T) {
	traversalCases := []string{
		"../sda",
		"../mem",
		"../../etc/passwd",
		"i2c-1/../sda",
		"sda",
		"i2c-",
		"i2c-1a",
	}
	for _, device := range traversalCases {
		t.Run(device, func(t *testing.T) {
			spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
			cfg := &appconfig.AppConfig{
				AppID: "test-app",
				Entitlements: []appconfig.Entitlement{
					{Type: appconfig.EntitlementI2C, Device: device},
				},
			}
			if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
				t.Fatalf("ApplyEntitlements() error = %v", err)
			}
			for _, m := range spec.Mounts {
				if !slices.Contains([]string{"/dev/snd", "/dev/input", "/dev/bus/usb"}, m.Destination) &&
					len(m.Destination) >= 9 && m.Destination[:9] != "/dev/i2c-" {
					if m.Destination == "/dev/sda" || m.Destination == "/dev/mem" ||
						m.Destination == "/etc/passwd" {
						t.Errorf("path traversal via device=%q mounted %q", device, m.Destination)
					}
				}
				if m.Destination == "/dev/"+device {
					t.Errorf("unsanitized device=%q was mounted as %q", device, m.Destination)
				}
			}
		})
	}
}

// TestApplyI2C_ValidDevice verifies that a legitimate i2c-N device is still mounted.
func TestApplyI2C_ValidDevice(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID: "test-app",
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementI2C, Device: "i2c-1"},
		},
	}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}
	if !hasMountDest(spec, "/dev/i2c-1") {
		t.Error("valid i2c-1 device was not mounted")
	}
}

// TestApplyPersist_PathTraversalDestination verifies that a crafted mount destination
// cannot escape the container path validation (WDY-1016).
func TestApplyPersist_PathTraversalDestination(t *testing.T) {
	traversalCases := []string{
		"relative/path",
		"../escape",
		"data",
	}
	for _, path := range traversalCases {
		t.Run(path, func(t *testing.T) {
			spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
			cfg := &appconfig.AppConfig{
				AppID: "test-app",
				Entitlements: []appconfig.Entitlement{
					{Type: appconfig.EntitlementPersist, Name: "vol", Path: path},
				},
			}
			if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
				t.Fatalf("ApplyEntitlements() error = %v", err)
			}
			if hasMountDest(spec, path) {
				t.Errorf("relative/traversal path=%q was added as a mount destination", path)
			}
		})
	}
}

// TestApplyPersist_DotDotInDestination verifies that dot-dot components in the
// mount destination are rejected (WDY-1016).
func TestApplyPersist_DotDotInDestination(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID: "test-app",
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementPersist, Name: "vol", Path: "/data/../etc"},
		},
	}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}
	// filepath.Clean resolves /data/../etc → /etc, so the cleaned path must not
	// be added as a mount destination when the original contained dot-dot.
	if hasMountDest(spec, "/etc") {
		t.Error("dot-dot in persist destination was silently resolved to /etc and mounted")
	}
	if hasMountDest(spec, "/data/../etc") {
		t.Error("raw dot-dot persist destination was mounted unchanged")
	}
}
