package oci

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/board"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
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

func TestDefaultSpec_NoDangerousCapabilities(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	if hasCapability(spec, "CAP_SYS_CHROOT") {
		t.Error("default spec must not grant CAP_SYS_CHROOT (WDY-1099)")
	}
	if hasCapability(spec, "CAP_SYS_PTRACE") {
		t.Error("default spec must not grant CAP_SYS_PTRACE (WDY-1099)")
	}
}

func TestDefaultSpec_SeccompProfile(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	if spec.Linux.Seccomp == nil {
		t.Fatal("default spec must have a seccomp profile (WDY-1099)")
	}
	if spec.Linux.Seccomp.DefaultAction != ActAllow {
		t.Errorf("seccomp DefaultAction = %q, want %q", spec.Linux.Seccomp.DefaultAction, ActAllow)
	}

	deniedSyscalls := make(map[string]bool)
	for _, sc := range spec.Linux.Seccomp.Syscalls {
		if sc.Action == ActErrno {
			for _, name := range sc.Names {
				deniedSyscalls[name] = true
			}
		}
	}
	for _, required := range []string{"ptrace", "unshare"} {
		if !deniedSyscalls[required] {
			t.Errorf("seccomp profile must deny syscall %q (WDY-1099)", required)
		}
	}

	// clone must be restricted for CLONE_NEWUSER (0x10000000).
	const cloneNewuser = uint64(0x10000000)
	for _, sc := range spec.Linux.Seccomp.Syscalls {
		if sc.Action != ActErrno {
			continue
		}
		for _, name := range sc.Names {
			if name != "clone" {
				continue
			}
			for _, arg := range sc.Args {
				if arg.Op == OpMaskedEqual && arg.Value == cloneNewuser {
					return
				}
			}
		}
	}
	t.Error("seccomp profile must deny clone with CLONE_NEWUSER (WDY-1099)")
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

	// On a generic (non-Pi) host, the GPU entitlement must not expose the
	// Raspberry Pi VideoCore mailbox device.
	if hasMountDest(spec, "/dev/vcio") {
		t.Error("GPU entitlement must not mount /dev/vcio on a non-Raspberry-Pi host")
	}
}

func TestApplyGPU_JetsonAddsResolvedRenderGID(t *testing.T) {
	origBoard := boardDetect
	origRender := lookupRenderGID
	t.Cleanup(func() {
		boardDetect = origBoard
		lookupRenderGID = origRender
	})
	boardDetect = func() board.Info { return board.Info{Kind: board.Jetson} }
	lookupRenderGID = func() (uint32, bool) { return 104, true }

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID:        "test-app",
		Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementGPU}},
	}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}

	if !hasGID(spec, nvidiaGroupGID) {
		t.Errorf("Jetson GPU entitlement missing NVIDIA/video GID %d", nvidiaGroupGID)
	}
	if !hasGID(spec, 104) {
		t.Error("Jetson GPU entitlement missing resolved render GID 104")
	}
}

func TestApplyGPU_JetsonWithoutRenderGroupKeepsVideoGID(t *testing.T) {
	origBoard := boardDetect
	origRender := lookupRenderGID
	t.Cleanup(func() {
		boardDetect = origBoard
		lookupRenderGID = origRender
	})
	boardDetect = func() board.Info { return board.Info{Kind: board.Jetson} }
	lookupRenderGID = func() (uint32, bool) { return 0, false }

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID:        "test-app",
		Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementGPU}},
	}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}

	if !hasGID(spec, nvidiaGroupGID) {
		t.Errorf("Jetson GPU entitlement missing NVIDIA/video GID %d", nvidiaGroupGID)
	}
	if hasGID(spec, 0) {
		t.Error("Jetson GPU entitlement added a render GID when lookup failed")
	}
}

func TestApplyGPU_NonJetsonDoesNotAddRenderGID(t *testing.T) {
	origBoard := boardDetect
	origRender := lookupRenderGID
	t.Cleanup(func() {
		boardDetect = origBoard
		lookupRenderGID = origRender
	})
	boardDetect = func() board.Info { return board.Info{Kind: board.Generic} }
	lookupRenderGID = func() (uint32, bool) {
		t.Fatal("non-Jetson GPU entitlement must not resolve the render group")
		return 0, false
	}

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID:        "test-app",
		Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementGPU}},
	}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}
}

// installFakeVCIO points vcioDevicePath at a temp file, makes statMajor report
// the given major for it, and reports the host as a Raspberry Pi — so the
// vcio branch of applyGPU can be exercised without a real /dev/vcio (which only
// exists on a Pi and whose major needs root/mknod to fake). Restored on cleanup.
func installFakeVCIO(t *testing.T, major int64) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "vcio")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}

	origPath := vcioDevicePath
	origStat := statMajor
	origBoard := boardDetect
	t.Cleanup(func() {
		vcioDevicePath = origPath
		statMajor = origStat
		boardDetect = origBoard
	})

	vcioDevicePath = p
	statMajor = func(q string) (int64, error) {
		if q == p {
			return major, nil
		}
		return 0, os.ErrNotExist
	}
	boardDetect = func() board.Info { return board.Info{Kind: board.RaspberryPi} }
	return p
}

func TestApplyGPU_RaspberryPiExposesVCIO(t *testing.T) {
	source := installFakeVCIO(t, 249)

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID:        "test-app",
		Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementGPU}},
	}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}

	// /dev/vcio must be bind-mounted from the host node.
	m, ok := mountForDest(spec, "/dev/vcio")
	if !ok {
		t.Fatal("GPU entitlement did not mount /dev/vcio on a Raspberry Pi")
	}
	if m.Type != "bind" || m.Source != source {
		t.Errorf("/dev/vcio mount = {Type:%q Source:%q}, want bind from %q", m.Type, m.Source, source)
	}

	// The dynamic major must be allowed, exactly once, with "rw" (no mknod).
	if !hasMajorRule(spec, 249) {
		t.Error("GPU entitlement did not allow the /dev/vcio major on a Raspberry Pi")
	}
	if got := countMajorRule(spec, 249); got != 1 {
		t.Errorf("vcio major rule should be deduplicated, got %d entries", got)
	}
	if acc, _ := majorRuleAccess(spec, 249); acc != "rw" {
		t.Errorf("vcio major Access = %q, want %q (mknod must be withheld)", acc, "rw")
	}
}

func TestApplyGPU_RaspberryPiSkipsVCIOWhenAbsent(t *testing.T) {
	// Report a Pi, but leave vcioDevicePath at its default (/dev/vcio), which
	// does not exist on the test host: the bind mount must be skipped so a
	// missing source cannot stop the container from starting.
	origBoard := boardDetect
	t.Cleanup(func() { boardDetect = origBoard })
	boardDetect = func() board.Info { return board.Info{Kind: board.RaspberryPi} }

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID:        "test-app",
		Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementGPU}},
	}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}

	if hasMountDest(spec, "/dev/vcio") {
		t.Error("GPU entitlement mounted /dev/vcio even though the host node is absent")
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

	// WDY-1094: plain "host" grants network *visibility* only. It must NOT grant
	// CAP_NET_ADMIN — that lets a container reconfigure host interfaces, routes,
	// and netfilter. Reconfiguration requires the explicit "host-admin" opt-in.
	if slices.Contains(spec.Process.Capabilities.Bounding, "CAP_NET_ADMIN") {
		t.Error("plain host network entitlement must not grant CAP_NET_ADMIN (WDY-1094)")
	}
}

// TestApplyEntitlements_Network_HostAdmin verifies the explicit opt-in: mode
// "host-admin" is host networking AND grants CAP_NET_ADMIN for apps that
// genuinely need to reconfigure the network (WDY-1094).
func TestApplyEntitlements_Network_HostAdmin(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID: "test-app",
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementNetwork, Mode: "host-admin"},
		},
	}

	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}

	// host-admin is host networking: the network namespace is removed.
	if hasNamespace(spec, "network") {
		t.Error("host-admin entitlement did not remove network namespace")
	}
	// host-admin is the opt-in that grants CAP_NET_ADMIN.
	for _, set := range [][]string{
		spec.Process.Capabilities.Bounding,
		spec.Process.Capabilities.Effective,
		spec.Process.Capabilities.Permitted,
	} {
		if !slices.Contains(set, "CAP_NET_ADMIN") {
			t.Error("host-admin entitlement must grant CAP_NET_ADMIN in all capability sets")
		}
	}
}

func TestApplyEntitlements_Network_Host_ResolvedStub(t *testing.T) {
	for _, mode := range []string{"", "host", "host-admin"} {
		name := mode
		if name == "" {
			name = "implicit-host"
		}
		t.Run(name, func(t *testing.T) {
			managed := filepath.Join(t.TempDir(), "resolv.conf")
			spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
			cfg := &appconfig.AppConfig{
				AppID: "test-app",
				Entitlements: []appconfig.Entitlement{
					{Type: appconfig.EntitlementNetwork, Mode: mode},
				},
			}

			if err := ApplyEntitlements(spec, cfg, ApplyOptions{HostResolvConfPath: managed}); err != nil {
				t.Fatalf("ApplyEntitlements() error = %v", err)
			}

			for _, m := range spec.Mounts {
				if m.Destination != "/etc/resolv.conf" {
					continue
				}
				if m.Source != managed {
					t.Fatalf("/etc/resolv.conf source = %q, want managed stub %q", m.Source, managed)
				}
				if m.Type != "bind" {
					t.Fatalf("/etc/resolv.conf mount type = %q, want bind", m.Type)
				}
				return
			}
			t.Fatal("host network entitlement did not mount the managed stub resolv.conf")
		})
	}
}

func TestApplyEntitlements_Network_Host_ResolvConfFallback(t *testing.T) {
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

// TestApplyEntitlements_Network_Bridge covers the "bridge" mode added by
// specs/2026-07-05-network-bridge-default-design.md. applyNetwork's job for
// bridge mode is limited to keeping the container's own network namespace,
// mirroring "none": no host mounts (no /sys bind, no host resolv.conf) and no
// CAP_NET_ADMIN. The private IP, NAT egress, and DNS that make "bridge"
// different from "none" are wired up by the containerd layer attaching the
// container to the per-app CNI bridge, which this oci-package-only test
// cannot exercise (no live containerd/CNI here).
func TestApplyEntitlements_Network_Bridge(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID: "test-app",
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementNetwork, Mode: "bridge"},
		},
	}

	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}

	if !hasNamespace(spec, "network") {
		t.Error("network mode 'bridge' should keep network namespace")
	}
	if hasMountDest(spec, "/etc/resolv.conf") {
		t.Error("network mode 'bridge' must not add a host resolv.conf mount (DNS is wired by the containerd layer)")
	}
	for _, m := range spec.Mounts {
		if m.Destination == "/sys" && m.Type == "bind" {
			t.Error("network mode 'bridge' must not replace /sys with a host bind mount (that is host-networking-only)")
		}
	}
	for _, set := range [][]string{
		spec.Process.Capabilities.Bounding,
		spec.Process.Capabilities.Effective,
		spec.Process.Capabilities.Permitted,
	} {
		if slices.Contains(set, "CAP_NET_ADMIN") {
			t.Error("network mode 'bridge' must not grant CAP_NET_ADMIN")
		}
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

	// The PipeWire mount is conditional: it is added only when a user session
	// socket exists at /run/user/*/pipewire-0 and is owned by the "wendy" user.
	isSocket := func(path string) bool {
		fi, err := os.Lstat(path)
		if err != nil || fi.Mode()&os.ModeSocket == 0 || fi.Mode()&os.ModeSymlink != 0 {
			return false
		}
		uid, ok := pipewireUserUID()
		if !ok {
			return false
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		return ok && st.Uid == uid
	}
	// Mirror applyAudio's socket detection.
	var pipewireSocketSource string
	userSockets, _ := filepath.Glob(pipewireUserSocketGlob)
	for _, s := range userSockets {
		if isSocket(s) {
			pipewireSocketSource = s
			break
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

// mountSourceFor returns the host source bound at dest, or "" if not mounted.
func mountSourceFor(spec *Spec, dest string) string {
	for _, m := range spec.Mounts {
		if m.Destination == dest {
			return m.Source
		}
	}
	return ""
}

// listenUnix creates a real Unix socket at path so isSocket() accepts it.
func listenUnix(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) = %v", path, err)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("Listen(unix, %s) = %v", path, err)
	}
	t.Cleanup(func() { l.Close() })
}

// Only a user session socket is mounted. The system pipewire.service has a
// socket but no WirePlumber, so a container bound to it sees an empty graph —
// no sinks, no Bluetooth, and no pulse/native to set PULSE_SERVER from. Such a
// container is better off with /dev/snd alone than with a socket that plays
// nothing.
func TestApplyEntitlements_Audio_MountsOnlyUserSessionSocket(t *testing.T) {
	// Short base path: Unix socket paths are limited to ~104 bytes.
	base, err := os.MkdirTemp("/tmp", "pw")
	if err != nil {
		t.Fatalf("MkdirTemp() = %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })

	userSocket := filepath.Join(base, "user", "1000", "pipewire-0")
	systemSocket := filepath.Join(base, "sys", "pipewire-0")
	pulseSocket := filepath.Join(base, "user", "1000", "pulse", "native")

	tests := []struct {
		name       string
		makeUser   bool
		makeSystem bool
		wantSource string
		wantPulse  bool
	}{
		{"both present, user wins", true, true, userSocket, true},
		{"only system", false, true, "", false},
		{"only user", true, false, userSocket, true},
		{"neither", false, false, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origGlob, origUID := pipewireUserSocketGlob, pipewireUserUID
			t.Cleanup(func() { pipewireUserSocketGlob, pipewireUserUID = origGlob, origUID })
			pipewireUserSocketGlob = filepath.Join(base, "user", "*", "pipewire-0")
			// The test process's own UID stands in for "wendy": creating a
			// socket actually owned by an arbitrary UID would require root.
			ownUID := uint32(os.Getuid())
			pipewireUserUID = func() (uint32, bool) { return ownUID, true }

			os.RemoveAll(filepath.Join(base, "user"))
			os.RemoveAll(filepath.Join(base, "sys"))
			if tt.makeUser {
				listenUnix(t, userSocket)
				listenUnix(t, pulseSocket)
			}
			if tt.makeSystem {
				listenUnix(t, systemSocket)
			}

			spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
			cfg := &appconfig.AppConfig{
				AppID:        "test-app",
				Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementAudio}},
			}
			if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
				t.Fatalf("ApplyEntitlements() error = %v", err)
			}

			got := mountSourceFor(spec, "/run/pipewire/pipewire-0")
			if got != tt.wantSource {
				t.Errorf("pipewire mount source = %q, want %q", got, tt.wantSource)
			}

			// PULSE_SERVER is derived from the chosen socket's directory.
			gotPulse := mountSourceFor(spec, "/run/pipewire/pulse-native")
			if tt.wantPulse && gotPulse != pulseSocket {
				t.Errorf("pulse mount source = %q, want %q", gotPulse, pulseSocket)
			}
			if !tt.wantPulse && gotPulse != "" {
				t.Errorf("pulse mounted from %q, want none", gotPulse)
			}
		})
	}
}

// A socket owned by a UID other than the expected "wendy" one must never be
// bind-mounted into a container, even though it passes the socket-type check:
// pipewireUserSocketGlob matches every UID under /run/user, and this is the
// only thing stopping a root-run agent from handing a container another local
// user's session.
func TestApplyEntitlements_Audio_RejectsUnexpectedSocketOwner(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "pw")
	if err != nil {
		t.Fatalf("MkdirTemp() = %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })

	userSocket := filepath.Join(base, "user", "1000", "pipewire-0")
	listenUnix(t, userSocket)

	origGlob, origUID := pipewireUserSocketGlob, pipewireUserUID
	t.Cleanup(func() { pipewireUserSocketGlob, pipewireUserUID = origGlob, origUID })
	pipewireUserSocketGlob = filepath.Join(base, "user", "*", "pipewire-0")
	pipewireUserUID = func() (uint32, bool) { return uint32(os.Getuid()) + 1, true }

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID:        "test-app",
		Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementAudio}},
	}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}

	if got := mountSourceFor(spec, "/run/pipewire/pipewire-0"); got != "" {
		t.Errorf("mounted a socket owned by an unexpected UID: source = %q", got)
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

	if err := ApplyEntitlements(spec, cfg, ApplyOptions{DBusProxySocketDir: ""}); err != nil {
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

	// The caller (containerd client) passes the exact directory the proxy
	// created, keyed by container name. Here a multi-service container name is
	// used to guard against the regression where the mount source was rebuilt
	// from the bare app ID and dropped the service suffix (WDY-1688).
	const proxyDir = "/run/wendy/dbus-proxy/bt-app_btscan"
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{DBusProxySocketDir: proxyDir}); err != nil {
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

	// Verify source points to the exact directory the caller supplied.
	for _, m := range spec.Mounts {
		if m.Destination == "/var/run/dbus" {
			if m.Source != proxyDir {
				t.Errorf("proxy /var/run/dbus source = %q, want %q", m.Source, proxyDir)
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
			if strings.Contains(d.Access, "m") {
				t.Errorf("%s entitlement V4L2 rule must not grant mknod, got Access=%q", entType, d.Access)
			}
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
	if hasCapability(spec, "CAP_SYS_PTRACE") {
		t.Errorf("%s entitlement must not grant CAP_SYS_PTRACE (WDY-1099)", entType)
	}
	if hasCapability(spec, "CAP_SYS_CHROOT") {
		t.Errorf("%s entitlement must not grant CAP_SYS_CHROOT (WDY-1099)", entType)
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

// TestApplyEntitlements_Camera_UdevMount verifies the camera entitlement
// bind-mounts the host udev runtime read-only. libcamera enumerates CSI
// cameras through libudev, which reads /run/udev/data; without this mount the
// in-container enumerator finds no cameras even though the device nodes and
// cgroup rules are present (WDY-1342). udevRuntimeDir is redirected to a real
// temp dir so the os.Stat guard is deterministic across CI hosts.
func TestApplyEntitlements_Camera_UdevMount(t *testing.T) {
	orig := udevRuntimeDir
	t.Cleanup(func() { udevRuntimeDir = orig })
	udevRuntimeDir = t.TempDir()

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

	m, ok := mountForDest(spec, "/run/udev")
	if !ok {
		t.Fatal("camera entitlement did not bind-mount /run/udev (libcamera CSI enumeration needs it)")
	}
	if m.Source != udevRuntimeDir {
		t.Errorf("/run/udev mount source = %q, want %q", m.Source, udevRuntimeDir)
	}
	if m.Type != "bind" {
		t.Errorf("/run/udev mount type = %q, want \"bind\"", m.Type)
	}
	if !slices.Contains(m.Options, "rbind") {
		t.Error("/run/udev mount missing rbind option")
	}
	if !slices.Contains(m.Options, "ro") {
		t.Error("/run/udev mount must be read-only (ro)")
	}
}

// TestApplyEntitlements_Camera_UdevMountSkippedWhenAbsent verifies that a host
// without a udev runtime directory does not get a /run/udev mount, so the
// container can still start (runc fails a bind mount whose source is missing).
func TestApplyEntitlements_Camera_UdevMountSkippedWhenAbsent(t *testing.T) {
	orig := udevRuntimeDir
	t.Cleanup(func() { udevRuntimeDir = orig })
	udevRuntimeDir = filepath.Join(t.TempDir(), "does-not-exist")

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

	if hasMountDest(spec, "/run/udev") {
		t.Error("camera entitlement mounted /run/udev despite host udev runtime being absent")
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

func TestApplyEntitlements_Input(t *testing.T) {
	originalLookup := lookupInputGID
	t.Cleanup(func() { lookupInputGID = originalLookup })

	tests := []struct {
		name        string
		resolvedGID uint32
		resolved    bool
		wantGID     uint32
	}{
		{name: "WendyOS host group", resolvedGID: 19, resolved: true, wantGID: 19},
		{name: "lookup fallback", resolved: false, wantGID: 105},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lookupInputGID = func() (uint32, bool) { return tc.resolvedGID, tc.resolved }
			spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
			// Input access must work for the normal non-root container user and
			// must not rewrite its primary identity.
			spec.Process.User.UID = 1000
			spec.Process.User.GID = 1000
			cfg := &appconfig.AppConfig{
				AppID: "test-app",
				Entitlements: []appconfig.Entitlement{
					{Type: appconfig.EntitlementInput},
				},
			}

			if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
				t.Fatalf("ApplyEntitlements() error = %v", err)
			}
			if !hasGID(spec, tc.wantGID) {
				t.Errorf("input entitlement GIDs = %v, want supplemental GID %d", spec.Process.User.AdditionalGids, tc.wantGID)
			}
			if spec.Process.User.UID != 1000 || spec.Process.User.GID != 1000 {
				t.Errorf("input entitlement changed non-root identity to uid=%d gid=%d", spec.Process.User.UID, spec.Process.User.GID)
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

			// Should add a cgroup rule for hot-plugged input devices (major 13).
			foundInputRule := false
			for _, d := range spec.Linux.Resources.Devices {
				if d.Major != nil && *d.Major == 13 && d.Allow && d.Access == "rw" {
					foundInputRule = true
					break
				}
			}
			if !foundInputRule {
				t.Error("input entitlement did not add rw cgroup device rule (major 13)")
			}
		})
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

// TestApplyI2C_PathTraversal verifies that a crafted device name is rejected and
// cannot escape /dev/i2c- (WDY-1015).
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
	// A crafted name is rejected by name validation before any stat, but inject a
	// permissive stat so a regression that skips the name check would still be
	// caught by the mount assertions below.
	origStat := statDeviceNode
	defer func() { statDeviceNode = origStat }()
	statDeviceNode = func(string) (int64, int64, error) { return i2cMajor, 0, nil }

	for _, device := range traversalCases {
		t.Run(device, func(t *testing.T) {
			spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
			cfg := &appconfig.AppConfig{
				AppID: "test-app",
				Entitlements: []appconfig.Entitlement{
					{Type: appconfig.EntitlementI2C, Device: device},
				},
			}
			// A crafted name must be rejected with an error; either way it must
			// never produce a mount escaping /dev or the named bad path.
			if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err == nil {
				t.Errorf("crafted device=%q was accepted; want rejection", device)
			}
			for _, m := range spec.Mounts {
				if m.Destination == "/dev/sda" || m.Destination == "/dev/mem" || m.Destination == "/etc/passwd" {
					t.Errorf("path traversal via device=%q mounted %q", device, m.Destination)
				}
				if m.Destination == "/dev/"+device {
					t.Errorf("unsanitized device=%q was mounted as %q", device, m.Destination)
				}
			}
		})
	}
}

// TestApplyI2C_ValidDevice verifies a legitimate i2c-N bus is bind-mounted and
// gets a cgroup allow rule scoped to its exact major:minor (never the whole I2C
// major), mirroring the serial entitlement (WDY-1601).
func TestApplyI2C_ValidDevice(t *testing.T) {
	const fakeMinor int64 = 3
	origStat := statDeviceNode
	defer func() { statDeviceNode = origStat }()
	statDeviceNode = func(string) (int64, int64, error) { return i2cMajor, fakeMinor, nil }

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
	if !hasMajorMinorRule(spec, i2cMajor, fakeMinor) {
		t.Errorf("i2c-1 missing scoped cgroup allow rule for %d:%d", i2cMajor, fakeMinor)
	}
	if hasWholeMajorRule(spec, i2cMajor) {
		t.Error("i2c-1 emitted a whole-major rule (no minor) — must be device-scoped")
	}
	if acc, _ := majorRuleAccess(spec, i2cMajor); strings.Contains(acc, "m") {
		t.Errorf("i2c rule must not grant mknod, got Access=%q", acc)
	}
}

// TestApplyI2C_DeviceAbsent verifies a clear error (and no mount) when the
// declared I2C bus is not present on the host.
func TestApplyI2C_DeviceAbsent(t *testing.T) {
	origStat := statDeviceNode
	defer func() { statDeviceNode = origStat }()
	statDeviceNode = func(string) (int64, int64, error) { return 0, 0, errors.New("no such device") }

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID:        "test-app",
		Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementI2C, Device: "i2c-1"}},
	}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err == nil {
		t.Fatal("ApplyEntitlements() = nil error for absent i2c bus, want error")
	}
	if hasMountDest(spec, "/dev/i2c-1") {
		t.Error("absent i2c bus should not be mounted")
	}
}

// TestEntitlements_OmitMknod verifies the whole-major device entitlements grant
// "rw", never "rwm": the host owns the device nodes and bind-mounts them, so the
// container never needs the mknod bit (WDY-1601). The minor-scoped entitlements
// (serial, i2c) and camera are covered by their own tests.
func TestEntitlements_OmitMknod(t *testing.T) {
	// applyGPU branches on the board (a Raspberry Pi also exposes vcio); pin to
	// Generic so the result is deterministic regardless of the host running the test.
	origBoard := boardDetect
	defer func() { boardDetect = origBoard }()
	boardDetect = func() board.Info { return board.Info{Kind: board.Generic} }

	cases := []struct {
		name  string
		ent   appconfig.Entitlement
		major int64
	}{
		{"usb", appconfig.Entitlement{Type: appconfig.EntitlementUSB}, 189},
		{"gpio", appconfig.Entitlement{Type: appconfig.EntitlementGPIO}, 254},
		{"spi", appconfig.Entitlement{Type: appconfig.EntitlementSPI}, 153},
		{"input", appconfig.Entitlement{Type: appconfig.EntitlementInput}, 13},
		{"gpu", appconfig.Entitlement{Type: appconfig.EntitlementGPU}, 195},
		{"audio", appconfig.Entitlement{Type: appconfig.EntitlementAudio}, 116},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
			cfg := &appconfig.AppConfig{AppID: "test", Entitlements: []appconfig.Entitlement{tc.ent}}
			if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
				t.Fatalf("ApplyEntitlements() error = %v", err)
			}
			acc, ok := majorRuleAccess(spec, tc.major)
			if !ok {
				t.Fatalf("%s entitlement did not emit an allow rule for major %d", tc.name, tc.major)
			}
			if acc != "rw" {
				t.Errorf("%s entitlement major %d must grant \"rw\", got %q", tc.name, tc.major, acc)
			}
			if strings.Contains(acc, "m") {
				t.Errorf("%s entitlement major %d must not grant mknod, got Access=%q", tc.name, tc.major, acc)
			}
		})
	}
}

// TestApplySerial_ValidDevice verifies a legitimate serial tty is bind-mounted,
// gets a cgroup allow rule scoped to the device's exact major:minor (never the
// whole major), and adds the dialout GID.
func TestApplySerial_ValidDevice(t *testing.T) {
	cases := []struct {
		device string
		major  int64
	}{
		{"ttyACM0", 166},
		{"ttyUSB0", 188},
	}
	// Inject a fake stat so the test needs no real device nodes (which require
	// root + mknod). Returns the device's expected major and a fixed minor.
	const fakeMinor int64 = 7
	origStat := statDeviceNode
	defer func() { statDeviceNode = origStat }()

	for _, tc := range cases {
		t.Run(tc.device, func(t *testing.T) {
			statDeviceNode = func(string) (int64, int64, error) { return tc.major, fakeMinor, nil }
			spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
			cfg := &appconfig.AppConfig{
				AppID: "test-app",
				Entitlements: []appconfig.Entitlement{
					{Type: appconfig.EntitlementSerial, Device: tc.device},
				},
			}
			if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
				t.Fatalf("ApplyEntitlements() error = %v", err)
			}
			if !hasMountDest(spec, "/dev/"+tc.device) {
				t.Errorf("serial device %q was not mounted", tc.device)
			}
			if !hasMajorMinorRule(spec, tc.major, fakeMinor) {
				t.Errorf("serial device %q missing scoped cgroup allow rule for %d:%d", tc.device, tc.major, fakeMinor)
			}
			if hasWholeMajorRule(spec, tc.major) {
				t.Errorf("serial device %q emitted a whole-major rule (no minor) — must be device-scoped", tc.device)
			}
			if !hasGID(spec, dialoutGroupGID) {
				t.Errorf("serial device %q did not add dialout GID %d", tc.device, dialoutGroupGID)
			}
		})
	}
}

// TestApplySerial_DeviceAbsent verifies a clear error (and no spec mutation)
// when the declared serial device is not present on the host.
func TestApplySerial_DeviceAbsent(t *testing.T) {
	origStat := statDeviceNode
	defer func() { statDeviceNode = origStat }()
	statDeviceNode = func(string) (int64, int64, error) { return 0, 0, errors.New("no such device") }

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID:        "test-app",
		Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementSerial, Device: "ttyACM0"}},
	}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err == nil {
		t.Fatal("ApplyEntitlements() = nil error for absent serial device, want error")
	}
	if hasMountDest(spec, "/dev/ttyACM0") {
		t.Error("absent serial device should not be mounted")
	}
}

// TestStatDeviceNode_RejectsSymlink verifies the shared resolver refuses a
// symlinked node (runc can't follow symlinks through a bind mount, and a
// symlink would let the validated node differ from the bound one).
func TestStatDeviceNode_RejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := statDeviceNode(link); err == nil {
		t.Error("statDeviceNode followed a symlink; want rejection")
	}
}

// TestApplySerial_PathTraversal verifies a crafted device name cannot escape
// /dev or emit an unscoped cgroup rule.
func TestApplySerial_PathTraversal(t *testing.T) {
	traversalCases := []string{
		"ttyACM0/../sda",
		"../mem",
		"../../etc/passwd",
		"sda",
		"ttyACM",
		"ttyACMx",
	}
	// A real device node never gets stat'd for these — they're rejected by name
	// before the stat — but inject a permissive stat so a regression that skips
	// the name check would still be caught by the mount assertions below.
	origStat := statDeviceNode
	defer func() { statDeviceNode = origStat }()
	statDeviceNode = func(string) (int64, int64, error) { return 166, 0, nil }

	for _, device := range traversalCases {
		t.Run(device, func(t *testing.T) {
			spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
			cfg := &appconfig.AppConfig{
				AppID: "test-app",
				Entitlements: []appconfig.Entitlement{
					{Type: appconfig.EntitlementSerial, Device: device},
				},
			}
			// A crafted name must be rejected with an error; either way it must
			// never produce a mount escaping /dev or the named bad path.
			if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err == nil {
				t.Errorf("crafted device=%q was accepted; want rejection", device)
			}
			for _, m := range spec.Mounts {
				if m.Destination == "/dev/sda" || m.Destination == "/dev/mem" || m.Destination == "/etc/passwd" {
					t.Errorf("path traversal via device=%q mounted %q", device, m.Destination)
				}
				if m.Destination == "/dev/"+device {
					t.Errorf("unsanitized device=%q was mounted as %q", device, m.Destination)
				}
			}
		})
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

// --- Camera CSI/libcamera extra majors ---

// installFakeCameraGlobs redirects cameraExtraGlobs into a tempdir populated
// with fake device files. statMajor is overridden to return canned majors
// keyed off the file basename. boardDetect is overridden to return Generic.
// Returns the chosen majors so tests can assert on them.
func installFakeCameraGlobs(t *testing.T, files map[string]int64) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "dma_heap"), 0o755); err != nil {
		t.Fatal(err)
	}

	majorsByPath := map[string]int64{}
	for name, major := range files {
		var rel string
		switch {
		case strings.HasPrefix(name, "media"):
			rel = name
		case strings.HasPrefix(name, "v4l-subdev"):
			rel = name
		case strings.HasPrefix(name, "dma_heap/"):
			rel = name
		case strings.HasPrefix(name, "nvhost-") || name == "nvmap":
			rel = name
		default:
			t.Fatalf("unknown fake device fixture %q", name)
		}
		p := filepath.Join(dir, rel)
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		majorsByPath[p] = major
	}

	origCamera := cameraExtraGlobs
	origJetson := jetsonExtraGlobs
	origStat := statMajor
	origBoard := boardDetect
	t.Cleanup(func() {
		cameraExtraGlobs = origCamera
		jetsonExtraGlobs = origJetson
		statMajor = origStat
		boardDetect = origBoard
	})

	cameraExtraGlobs = []string{
		filepath.Join(dir, "media*"),
		filepath.Join(dir, "v4l-subdev*"),
		filepath.Join(dir, "dma_heap", "*"),
	}
	jetsonExtraGlobs = []string{
		filepath.Join(dir, "nvhost-*"),
		filepath.Join(dir, "nvmap"),
	}
	statMajor = func(p string) (int64, error) {
		if m, ok := majorsByPath[p]; ok {
			return m, nil
		}
		return 0, os.ErrNotExist
	}
}

func hasMajorMinorRule(spec *Spec, major, minor int64) bool {
	for _, d := range spec.Linux.Resources.Devices {
		if d.Allow && d.Major != nil && *d.Major == major && d.Minor != nil && *d.Minor == minor {
			return true
		}
	}
	return false
}

// hasWholeMajorRule reports an allow rule for a whole major (no minor scope).
func hasWholeMajorRule(spec *Spec, major int64) bool {
	for _, d := range spec.Linux.Resources.Devices {
		if d.Allow && d.Major != nil && *d.Major == major && d.Minor == nil {
			return true
		}
	}
	return false
}

func hasMajorRule(spec *Spec, want int64) bool {
	for _, d := range spec.Linux.Resources.Devices {
		if d.Allow && d.Major != nil && *d.Major == want {
			return true
		}
	}
	return false
}

func countMajorRule(spec *Spec, want int64) int {
	n := 0
	for _, d := range spec.Linux.Resources.Devices {
		if d.Allow && d.Major != nil && *d.Major == want {
			n++
		}
	}
	return n
}

// majorRuleAccess returns the Access mask of the first allow rule for the given
// major, and whether such a rule exists.
func majorRuleAccess(spec *Spec, want int64) (string, bool) {
	for _, d := range spec.Linux.Resources.Devices {
		if d.Allow && d.Major != nil && *d.Major == want {
			return d.Access, true
		}
	}
	return "", false
}

func TestApplyCamera_AddsMediaMajor(t *testing.T) {
	installFakeCameraGlobs(t, map[string]int64{
		"media0": 234,
	})
	boardDetect = func() board.Info { return board.Info{Kind: board.Generic} }

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{AppID: "test", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementCamera}}}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements: %v", err)
	}
	if !hasMajorRule(spec, 234) {
		t.Errorf("expected cgroup allow for media major 234")
	}
	if countMajorRule(spec, 234) != 1 {
		t.Errorf("media major rule should be deduplicated, got %d entries", countMajorRule(spec, 234))
	}
}

func TestApplyCamera_AddsDmaHeapMajor(t *testing.T) {
	installFakeCameraGlobs(t, map[string]int64{
		"dma_heap/system":   510,
		"dma_heap/cma":      510,
		"dma_heap/reserved": 511,
	})
	boardDetect = func() board.Info { return board.Info{Kind: board.Generic} }

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{AppID: "test", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementCamera}}}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements: %v", err)
	}
	if !hasMajorRule(spec, 510) {
		t.Errorf("expected cgroup allow for dma_heap major 510")
	}
	if !hasMajorRule(spec, 511) {
		t.Errorf("expected cgroup allow for dma_heap major 511")
	}
	if countMajorRule(spec, 510) != 1 {
		t.Errorf("major 510 must be deduplicated across multiple files")
	}
}

func TestApplyCamera_AddsV4l2SubdevMajor(t *testing.T) {
	installFakeCameraGlobs(t, map[string]int64{
		"v4l-subdev0": 81,
		"v4l-subdev1": 81,
	})
	boardDetect = func() board.Info { return board.Info{Kind: board.Generic} }

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{AppID: "test", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementCamera}}}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements: %v", err)
	}
	// 81 is already added by the legacy v4l2Major rule; the v4l-subdev scan
	// must not duplicate it.
	if countMajorRule(spec, 81) != 1 {
		t.Errorf("major 81 should appear exactly once, got %d", countMajorRule(spec, 81))
	}
}

func TestApplyCamera_JetsonAddsNvhostMajor(t *testing.T) {
	installFakeCameraGlobs(t, map[string]int64{
		"nvhost-ctrl": 230,
		"nvhost-vi":   230,
		"nvmap":       242,
	})
	boardDetect = func() board.Info { return board.Info{Kind: board.Jetson} }

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{AppID: "test", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementCamera}}}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements: %v", err)
	}
	if !hasMajorRule(spec, 230) {
		t.Errorf("expected nvhost major 230 to be allowed on Jetson")
	}
	if !hasMajorRule(spec, 242) {
		t.Errorf("expected nvmap major 242 to be allowed on Jetson")
	}
}

func TestApplyCamera_NonJetsonOmitsNvhost(t *testing.T) {
	installFakeCameraGlobs(t, map[string]int64{
		"nvhost-ctrl": 230,
	})
	boardDetect = func() board.Info { return board.Info{Kind: board.Generic} }

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{AppID: "test", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementCamera}}}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements: %v", err)
	}
	if hasMajorRule(spec, 230) {
		t.Errorf("nvhost major 230 must not be allowed on non-Jetson hosts")
	}
}

// installFakeDriGlobs redirects driGlobs at a tempdir of fake /dev/dri nodes and
// stubs statMajor to report their majors, mirroring installFakeCameraGlobs.
func installFakeDriGlobs(t *testing.T, files map[string]int64) {
	t.Helper()
	dir := t.TempDir()
	majorsByPath := map[string]int64{}
	for name, major := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		majorsByPath[p] = major
	}
	origGlobs := driGlobs
	origStat := statMajor
	t.Cleanup(func() {
		driGlobs = origGlobs
		statMajor = origStat
	})
	driGlobs = []string{filepath.Join(dir, "*")}
	statMajor = func(p string) (int64, error) {
		if m, ok := majorsByPath[p]; ok {
			return m, nil
		}
		return 0, os.ErrNotExist
	}
}

func TestApplyDisplay_AllowsDrmMajor(t *testing.T) {
	installFakeDriGlobs(t, map[string]int64{"card0": 226, "renderD128": 226})
	boardDetect = func() board.Info { return board.Info{Kind: board.Generic} }
	origRender := lookupRenderGID
	t.Cleanup(func() { lookupRenderGID = origRender })
	lookupRenderGID = func() (uint32, bool) { return 0, false }

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{AppID: "test", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementDisplay}}}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements: %v", err)
	}
	if !hasMajorRule(spec, 226) {
		t.Error("expected cgroup allow for DRM major 226")
	}
	if countMajorRule(spec, 226) != 1 {
		t.Errorf("DRM major rule should be deduplicated, got %d", countMajorRule(spec, 226))
	}
}

func TestApplyDisplay_AddsVideoAndRenderGIDs(t *testing.T) {
	installFakeDriGlobs(t, map[string]int64{"renderD128": 226})
	boardDetect = func() board.Info { return board.Info{Kind: board.Generic} }
	origRender := lookupRenderGID
	t.Cleanup(func() { lookupRenderGID = origRender })
	lookupRenderGID = func() (uint32, bool) { return 107, true }

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{AppID: "test", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementDisplay}}}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements: %v", err)
	}
	if !hasGID(spec, videoGroupGID) {
		t.Error("expected video GID in AdditionalGids")
	}
	if !hasGID(spec, 107) {
		t.Error("expected resolved render GID in AdditionalGids")
	}
}

func TestApplyDisplay_NonDisplayAppUnchanged(t *testing.T) {
	installFakeDriGlobs(t, map[string]int64{"renderD128": 226})
	boardDetect = func() board.Info { return board.Info{Kind: board.Generic} }

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{AppID: "test", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork}}}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements: %v", err)
	}
	if hasMajorRule(spec, 226) {
		t.Error("non-display app must not get the DRM major rule")
	}
	if hasGID(spec, 107) {
		t.Error("non-display app must not get a render GID")
	}
}

// TestApplyCamera_DeviceRulesOmitMknod locks in least privilege: every cgroup
// device rule the camera entitlement adds — the static v4l2 major and every
// dynamically-discovered media/dma-heap/v4l-subdev/nvhost/nvmap major — must
// grant "rw" only. The mknod ('m') bit is withheld because the container opens
// host-created, bind-mounted device nodes and never needs to create its own.
func TestApplyCamera_DeviceRulesOmitMknod(t *testing.T) {
	installFakeCameraGlobs(t, map[string]int64{
		"media0":          234,
		"v4l-subdev0":     235,
		"dma_heap/system": 236,
		"nvhost-ctrl":     230,
		"nvmap":           242,
	})
	boardDetect = func() board.Info { return board.Info{Kind: board.Jetson} }

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{AppID: "test", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementCamera}}}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements: %v", err)
	}

	for _, major := range []int64{81, 234, 235, 236, 230, 242} {
		acc, ok := majorRuleAccess(spec, major)
		if !ok {
			t.Errorf("expected an allow rule for camera major %d", major)
			continue
		}
		if strings.Contains(acc, "m") {
			t.Errorf("camera major %d must not grant mknod, got Access=%q", major, acc)
		}
		if acc != "rw" {
			t.Errorf("camera major %d Access = %q, want %q", major, acc, "rw")
		}
	}
}

func hasMount(spec *Spec, dest string) bool {
	for _, m := range spec.Mounts {
		if m.Destination == dest {
			return true
		}
	}
	return false
}

func TestApplyNotifications_MountsOnlyPrivateSystemSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "wendy-system-oci-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	listener, err := net.Listen("unix", filepath.Join(dir, "system.sock"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID:        "test",
		Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementNotifications}},
	}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{SystemAPISocketDir: dir}); err != nil {
		t.Fatalf("ApplyEntitlements: %v", err)
	}
	mount, ok := mountForDest(spec, "/run/wendy/system")
	if !ok || mount.Source != dir || !slices.Contains(mount.Options, "ro") {
		t.Fatalf("System API mount = %+v, found=%v", mount, ok)
	}
	if !hasEnv(spec, "WENDY_SYSTEM_SOCKET=/run/wendy/system/system.sock") {
		t.Fatal("notifications entitlement did not inject WENDY_SYSTEM_SOCKET")
	}
	if !hasGID(spec, appSystemAPIGroupGID) {
		t.Fatal("notifications entitlement did not grant the System API socket group")
	}
	if hasMount(spec, "/run/wendy/agent") || hasEnv(spec, "WENDY_AGENT_SOCKET=") {
		t.Fatal("notifications entitlement exposed the admin control socket")
	}
}

func TestApplyNotifications_AbsentWithoutEntitlement(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{AppID: "test"}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{SystemAPISocketDir: t.TempDir()}); err != nil {
		t.Fatalf("ApplyEntitlements: %v", err)
	}
	if hasMount(spec, "/run/wendy/system") || hasEnv(spec, "WENDY_SYSTEM_SOCKET=") || hasGID(spec, appSystemAPIGroupGID) {
		t.Fatal("app without notifications entitlement received System API access")
	}
}

func TestApplyAdmin_MountsSocketWhenPresent(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "s")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	origPath := AdminAgentSocketHostPath
	t.Cleanup(func() { AdminAgentSocketHostPath = origPath })
	AdminAgentSocketHostPath = sock

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{AppID: "test", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementAdmin}}}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements: %v", err)
	}
	// The socket is exposed by bind-mounting its parent *directory*, not the
	// socket file. A file bind-mount pins one inode; localsocket.Listen unlinks
	// and recreates the socket (new inode) on every agent start, so a file mount
	// strands a long-lived container on a deleted inode after an agent restart.
	// A directory mount resolves the socket name live on each dial.
	const ctrDir = "/run/wendy/agent"
	if !hasMount(spec, ctrDir) {
		t.Fatalf("expected %s directory bind mount", ctrDir)
	}
	if hasMount(spec, "/run/wendy/agent/agent.sock") {
		t.Error("must mount the directory, not the socket file")
	}
	var mounted *Mount
	for i := range spec.Mounts {
		if spec.Mounts[i].Destination == ctrDir {
			mounted = &spec.Mounts[i]
		}
	}
	if mounted.Source != filepath.Dir(sock) {
		t.Errorf("mount source = %q, want socket parent dir %q", mounted.Source, filepath.Dir(sock))
	}
	if !hasEnv(spec, "WENDY_AGENT_SOCKET=/run/wendy/agent/agent.sock") {
		t.Error("expected WENDY_AGENT_SOCKET to point at the in-container socket path")
	}
	if hasGID(spec, appSystemAPIGroupGID) {
		t.Error("admin entitlement must not grant the private System API socket group")
	}
}

// TestAdminAgentSocketPath_IsDiskBacked guards the fix for the reboot
// socket-staleness bug. The admin socket is exposed by bind-mounting its parent
// directory into the container; that bind mount pins the directory's inode.
// tmpfs (/run) is wiped on every boot, so /run/wendy/agent gets a *new* inode
// each reboot — stranding a container that survives on the orphaned old inode
// (the socket then reads as ENOENT inside the container). A disk-backed
// directory keeps a stable inode across reboots (and, under /var/lib/wendy,
// across A/B OTA too), so the mount never goes stale.
func TestAdminAgentSocketPath_IsDiskBacked(t *testing.T) {
	if strings.HasPrefix(AdminAgentSocketHostPath, "/run/") {
		t.Fatalf("admin socket %q is on tmpfs /run: its parent-directory inode is "+
			"recreated on every boot, stranding the container's bind mount (ENOENT). "+
			"Use a disk-backed path such as /var/lib/wendy/...", AdminAgentSocketHostPath)
	}
	if !strings.HasPrefix(AdminAgentSocketHostPath, "/var/lib/") {
		t.Errorf("admin socket %q should live under a persistent /var/lib path", AdminAgentSocketHostPath)
	}
}

func TestApplyAdmin_NoSocketWhenAbsent(t *testing.T) {
	origPath := AdminAgentSocketHostPath
	t.Cleanup(func() { AdminAgentSocketHostPath = origPath })
	AdminAgentSocketHostPath = filepath.Join(t.TempDir(), "missing.sock")

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{AppID: "test", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementAdmin}}}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements: %v", err)
	}
	if hasMount(spec, "/run/wendy/agent") {
		t.Error("must not mount a missing socket")
	}
}

func TestApplyAdmin_NonAdminAppUnchanged(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{AppID: "test", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork}}}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements: %v", err)
	}
	if hasMount(spec, "/run/wendy/agent") || hasEnv(spec, "WENDY_AGENT_SOCKET=/run/wendy/agent/agent.sock") {
		t.Error("non-admin app must not get the agent socket")
	}
}

func seccompDenies(spec *Spec, syscall string) bool {
	if spec.Linux == nil || spec.Linux.Seccomp == nil {
		return false
	}
	for _, r := range spec.Linux.Seccomp.Syscalls {
		if r.Action != ActErrno {
			continue
		}
		for _, n := range r.Names {
			if n == syscall {
				return true
			}
		}
	}
	return false
}

func TestApplyEntitlements_Build_RelaxesSandbox(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID:        "test-app",
		Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementBuild}},
	}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}

	// Full privileged capability set in all four sets BuildKit's runc executor
	// needs — CAP_SYS_ADMIN for mounts, plus CAP_BPF/CAP_PERFMON for the cgroup-v2
	// device controller (the build dies at the RUN step without them).
	for _, set := range [][]string{
		spec.Process.Capabilities.Bounding,
		spec.Process.Capabilities.Effective,
		spec.Process.Capabilities.Permitted,
		spec.Process.Capabilities.Inheritable,
	} {
		for _, cap := range []string{"CAP_SYS_ADMIN", "CAP_BPF", "CAP_PERFMON", "CAP_NET_ADMIN"} {
			if !slices.Contains(set, cap) {
				t.Errorf("build entitlement must grant %s in all capability sets", cap)
			}
		}
	}
	// BuildKit's runc creates a per-step cgroup, so /sys/fs/cgroup must be
	// writable (default profile mounts it read-only) and the container gets its
	// own cgroup namespace.
	if m, ok := mountForDest(spec, "/sys/fs/cgroup"); ok && slices.Contains(m.Options, "ro") {
		t.Error("build entitlement must make /sys/fs/cgroup writable (no ro)")
	}
	if !hasNamespace(spec, "cgroup") {
		t.Error("build entitlement must add a cgroup namespace")
	}
	// The namespace syscalls BuildKit needs are no longer denied...
	if seccompDenies(spec, "unshare") {
		t.Error("build entitlement must un-deny unshare")
	}
	if seccompDenies(spec, "clone") {
		t.Error("build entitlement must remove the clone(CLONE_NEWUSER) deny rule")
	}
	// ...but the kernel-attack-surface denials are KEPT.
	if !seccompDenies(spec, "init_module") || !seccompDenies(spec, "kexec_load") {
		t.Error("build entitlement must keep module-load / kexec denials")
	}
	if !seccompDenies(spec, "ptrace") {
		t.Error("build entitlement must keep ptrace denied (only unshare is removed from that rule)")
	}
}

func TestApplyEntitlements_NoBuild_LeavesSandboxHardened(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{AppID: "test-app"} // no entitlements
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}
	if hasCapability(spec, "CAP_SYS_ADMIN") {
		t.Error("a spec without build must not gain CAP_SYS_ADMIN")
	}
	if !seccompDenies(spec, "unshare") || !seccompDenies(spec, "clone") {
		t.Error("a spec without build must keep the default unshare/clone denials")
	}
}

// ---------- GPU fallback device derivation (WDY-1804) ----------

// installFakeNvidiaDevTree builds a fake /dev with NVIDIA device nodes and
// injects each node's real-world major:minor via statCharDevice (plain files
// can't carry device numbers without root/mknod). Restored on cleanup.
func installFakeNvidiaDevTree(t *testing.T, numbers map[string][2]int64) string {
	t.Helper()
	dev := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dev, "nvidia-caps"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name := range numbers {
		devicePath := filepath.Join(dev, name)
		if err := os.MkdirAll(filepath.Dir(devicePath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(devicePath, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	origGlobs := nvidiaDeviceGlobs
	origRenderGlobs := renderDeviceGlobs
	origStat := statCharDevice
	origBoard := boardDetect
	origKFD := kfdDevicePath
	t.Cleanup(func() {
		nvidiaDeviceGlobs = origGlobs
		renderDeviceGlobs = origRenderGlobs
		statCharDevice = origStat
		boardDetect = origBoard
		kfdDevicePath = origKFD
	})
	// These tests exercise the NVIDIA path; point kfd at a node that cannot
	// exist so applyGPU never takes the AMD branch even on an AMD test host.
	kfdDevicePath = filepath.Join(dev, "no-such-kfd")

	nvidiaDeviceGlobs = []string{
		filepath.Join(dev, "nvidia*"),
		filepath.Join(dev, "nvidia-caps", "*"),
		filepath.Join(dev, "nvhost-*gpu"),
		filepath.Join(dev, "nvgpu", "igpu*", "*"),
	}
	renderDeviceGlobs = []string{filepath.Join(dev, "dri", "renderD*")}
	statCharDevice = func(p string) (int64, int64, error) {
		rel, err := filepath.Rel(dev, p)
		if err != nil {
			return 0, 0, err
		}
		if nums, ok := numbers[rel]; ok {
			return nums[0], nums[1], nil
		}
		// e.g. the nvidia-caps directory itself, matched by the nvidia* glob.
		return 0, 0, fmt.Errorf("%s is not a character device node", p)
	}
	boardDetect = func() board.Info { return board.Info{Kind: board.Jetson} }
	return dev
}

func TestApplyGPU_JetsonAddsOnlyDRMRenderNodes(t *testing.T) {
	dev := installFakeNvidiaDevTree(t, map[string][2]int64{
		"nvidia0":        {195, 0},
		"dri/renderD128": {226, 128},
		"dri/renderD129": {226, 129},
		"dri/card0":      {226, 0},
		"dri/renderDfoo": {226, 200},
	})

	spec := gpuSpec(t)
	for path, minor := range map[string]int64{
		filepath.Join(dev, "dri", "renderD128"): 128,
		filepath.Join(dev, "dri", "renderD129"): 129,
	} {
		d, ok := deviceForPath(spec, path)
		if !ok {
			t.Errorf("Jetson GPU entitlement did not add %s", path)
			continue
		}
		if d.Major != 226 || d.Minor != minor {
			t.Errorf("%s = c %d:%d, want c 226:%d", path, d.Major, d.Minor, minor)
		}
		if !hasExactDeviceRule(spec, 226, minor) {
			t.Errorf("Jetson GPU entitlement did not allow c 226:%d", minor)
		}
	}

	for _, path := range []string{
		filepath.Join(dev, "dri", "card0"),
		filepath.Join(dev, "dri", "renderDfoo"),
	} {
		if _, ok := deviceForPath(spec, path); ok {
			t.Errorf("Jetson GPU entitlement granted non-render node %s", path)
		}
	}
	if hasMountDest(spec, "/dev/dri") {
		t.Error("GPU entitlement must not bind the whole /dev/dri tree")
	}
}

func TestApplyGPU_NonJetsonOmitsDRMRenderNodes(t *testing.T) {
	dev := installFakeNvidiaDevTree(t, map[string][2]int64{
		"nvidia0":        {195, 0},
		"dri/renderD128": {226, 128},
	})
	boardDetect = func() board.Info { return board.Info{Kind: board.Generic} }

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{AppID: "test", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementGPU}}}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements: %v", err)
	}
	if _, ok := deviceForPath(spec, filepath.Join(dev, "dri", "renderD128")); ok {
		t.Error("non-Jetson GPU entitlement must not add DRM render nodes")
	}
}

func TestApplyGPU_SupplementsStaleJetsonCSVWithSchedulerControlDevices(t *testing.T) {
	dev := installFakeNvidiaDevTree(t, map[string][2]int64{
		"nvhost-nvsched_ctrl_fifo-gpu":  {487, 10},
		"nvgpu/igpu0/nvsched_ctrl_fifo": {487, 22},
		"nvhost-evil-gpu":               {8, 1},
		"nvgpu/igpu0/evil":              {8, 2},
	})

	spec := gpuSpec(t)

	for path, want := range map[string][2]int64{
		filepath.Join(dev, "nvhost-nvsched_ctrl_fifo-gpu"):        {487, 10},
		filepath.Join(dev, "nvgpu", "igpu0", "nvsched_ctrl_fifo"): {487, 22},
	} {
		d, ok := deviceForPath(spec, path)
		if !ok {
			t.Errorf("GPU entitlement did not add %s", path)
			continue
		}
		if d.Major != want[0] || d.Minor != want[1] {
			t.Errorf("%s = c %d:%d, want c %d:%d", path, d.Major, d.Minor, want[0], want[1])
		}
		if !hasExactDeviceRule(spec, want[0], want[1]) {
			t.Errorf("GPU entitlement did not allow c %d:%d", want[0], want[1])
		}
	}

	for _, path := range []string{
		filepath.Join(dev, "nvhost-evil-gpu"),
		filepath.Join(dev, "nvgpu", "igpu0", "evil"),
	} {
		if _, ok := deviceForPath(spec, path); ok {
			t.Errorf("GPU entitlement granted unexpected node %s", path)
		}
	}
}

func hasExactDeviceRule(spec *Spec, major, minor int64) bool {
	for _, d := range spec.Linux.Resources.Devices {
		if d.Allow && d.Major != nil && d.Minor != nil && *d.Major == major && *d.Minor == minor && d.Access == "rw" {
			return true
		}
	}
	return false
}

func gpuSpec(t *testing.T) *Spec {
	t.Helper()
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID:        "test-app",
		Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementGPU}},
	}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}
	return spec
}

func deviceForPath(spec *Spec, path string) (LinuxDevice, bool) {
	for _, d := range spec.Linux.Devices {
		if d.Path == path {
			return d, true
		}
	}
	return LinuxDevice{}, false
}

func TestApplyGPU_DerivesDevicesFromLiveNodes(t *testing.T) {
	// Ground truth from a Jetson AGX Thor (WendyOS-0.16.1, JetPack 7.2,
	// driver 595.78): uvm/uvm-tools live on dynamic major 487, the caps
	// nodes on 501, and a second GPU node nvidia1 exists.
	dev := installFakeNvidiaDevTree(t, map[string][2]int64{
		"nvidia0":                 {195, 0},
		"nvidia1":                 {195, 1},
		"nvidiactl":               {195, 255},
		"nvidia-modeset":          {195, 254},
		"nvidia-uvm":              {487, 0},
		"nvidia-uvm-tools":        {487, 1},
		"nvidia-caps/nvidia-cap1": {501, 1},
		"nvidia-caps/nvidia-cap2": {501, 2},
	})

	spec := gpuSpec(t)

	// Every live node must be added with its real major:minor.
	for name, want := range map[string][2]int64{
		"nvidia1":                 {195, 1},
		"nvidiactl":               {195, 255},
		"nvidia-uvm":              {487, 0},
		"nvidia-uvm-tools":        {487, 1},
		"nvidia-caps/nvidia-cap1": {501, 1},
	} {
		d, ok := deviceForPath(spec, filepath.Join(dev, name))
		if !ok {
			t.Errorf("GPU entitlement did not add %s", name)
			continue
		}
		if d.Major != want[0] || d.Minor != want[1] || d.Type != "c" {
			t.Errorf("%s = c %d:%d, want c %d:%d", name, d.Major, d.Minor, want[0], want[1])
		}
	}

	// One rw (no mknod) allow rule per discovered major:minor pair — never a
	// whole-major rule: dynamic majors (487) come from a shared kernel pool,
	// so an unscoped rule could cover unrelated drivers on another boot.
	wantRules := map[[2]int64]bool{
		{195, 0}: true, {195, 1}: true, {195, 255}: true, {195, 254}: true,
		{487, 0}: true, {487, 1}: true, {501, 1}: true, {501, 2}: true,
	}
	gotRules := map[[2]int64]int{}
	for _, d := range spec.Linux.Resources.Devices {
		if !d.Allow || d.Major == nil {
			continue
		}
		if d.Minor == nil {
			t.Errorf("major %d rule has no minor; whole-major rules are forbidden on the derived path", *d.Major)
			continue
		}
		if d.Access != "rw" {
			t.Errorf("rule %d:%d Access = %q, want %q (mknod must be withheld)", *d.Major, *d.Minor, d.Access, "rw")
		}
		gotRules[[2]int64{*d.Major, *d.Minor}]++
	}
	for pair := range wantRules {
		if gotRules[pair] != 1 {
			t.Errorf("rule %d:%d count = %d, want exactly 1", pair[0], pair[1], gotRules[pair])
		}
	}

	// The nvidia-caps directory itself (matched by the nvidia* glob) must not
	// become a device.
	if _, ok := deviceForPath(spec, filepath.Join(dev, "nvidia-caps")); ok {
		t.Error("GPU entitlement added the nvidia-caps directory as a device")
	}

	// The static list must not leak in alongside the derived nodes.
	if _, ok := deviceForPath(spec, "/dev/nvidia0"); ok {
		t.Error("GPU entitlement added static /dev/nvidia0 despite live nodes")
	}
}

func TestApplyGPU_StaticFallbackWhenNoLiveNodes(t *testing.T) {
	// Empty fake /dev: the glob finds nothing (e.g. driver module not yet
	// loaded), so the historical static major-195 list must be preserved.
	installFakeNvidiaDevTree(t, map[string][2]int64{})

	spec := gpuSpec(t)

	for _, path := range []string{
		"/dev/nvidia0",
		"/dev/nvidiactl",
		"/dev/nvidia-uvm",
		"/dev/nvidia-uvm-tools",
		"/dev/nvidia-modeset",
	} {
		d, ok := deviceForPath(spec, path)
		if !ok {
			t.Errorf("static fallback did not add %s", path)
			continue
		}
		if d.Major != 195 || d.Minor != 0 {
			t.Errorf("%s = %d:%d, want 195:0", path, d.Major, d.Minor)
		}
	}
	if got := countMajorRule(spec, 195); got != 1 {
		t.Errorf("major 195 allow rules = %d, want exactly 1", got)
	}
}

func TestApplyGPU_UnexpectedNodeNameRejected(t *testing.T) {
	// A character device planted under a matching glob but with a non-NVIDIA
	// name must not be granted to the container.
	dev := installFakeNvidiaDevTree(t, map[string][2]int64{
		"nvidia0":                     {195, 0},
		"nvidia":                      {195, 3}, // bare "nvidia" is not a real node name
		"nvidia-evil":                 {8, 0},   // e.g. a block-ish major smuggled behind a char node
		"nvidia-caps/nvidia-backdoor": {501, 99},
	})

	spec := gpuSpec(t)

	if _, ok := deviceForPath(spec, filepath.Join(dev, "nvidia-evil")); ok {
		t.Error("GPU entitlement granted a node with an unexpected name")
	}
	if _, ok := deviceForPath(spec, filepath.Join(dev, "nvidia")); ok {
		t.Error("GPU entitlement granted the bare 'nvidia' name")
	}
	if _, ok := deviceForPath(spec, filepath.Join(dev, "nvidia-caps", "nvidia-backdoor")); ok {
		t.Error("GPU entitlement granted a caps node with an unexpected name")
	}
	if _, ok := deviceForPath(spec, filepath.Join(dev, "nvidia0")); !ok {
		t.Error("GPU entitlement dropped the legitimate nvidia0 node")
	}
	for _, d := range spec.Linux.Resources.Devices {
		if d.Allow && d.Major != nil && *d.Major == 8 {
			t.Error("GPU entitlement emitted an allow rule for the rejected node's major")
		}
	}
}

// installFakeAMDDevTree builds a fake /dev with an AMD ROCm layout — /dev/kfd
// (the compute device) and /dev/dri/renderD* (the GPU) — and points the apply
// path's stat/glob vars at it. Restored on cleanup.
func installFakeAMDDevTree(t *testing.T, kfd [2]int64, renderNodes map[string][2]int64) string {
	t.Helper()
	dev := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dev, "dri"), 0o755); err != nil {
		t.Fatal(err)
	}
	kfdPath := filepath.Join(dev, "kfd")
	if err := os.WriteFile(kfdPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	renderNums := map[string][2]int64{}
	for name, nums := range renderNodes {
		p := filepath.Join(dev, "dri", name)
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		renderNums[p] = nums
	}

	origKFD := kfdDevicePath
	origRenderGlobs := renderDeviceGlobs
	origStatChar := statCharDevice
	origStatNode := statDeviceNode
	origRender := lookupRenderGID
	t.Cleanup(func() {
		kfdDevicePath = origKFD
		renderDeviceGlobs = origRenderGlobs
		statCharDevice = origStatChar
		statDeviceNode = origStatNode
		lookupRenderGID = origRender
	})

	kfdDevicePath = kfdPath
	renderDeviceGlobs = []string{filepath.Join(dev, "dri", "renderD*")}
	statCharDevice = func(p string) (int64, int64, error) {
		if nums, ok := renderNums[p]; ok {
			return nums[0], nums[1], nil
		}
		return 0, 0, fmt.Errorf("%s is not a character device node", p)
	}
	statDeviceNode = func(p string) (int64, int64, error) {
		if p == kfdPath {
			return kfd[0], kfd[1], nil
		}
		if nums, ok := renderNums[p]; ok {
			return nums[0], nums[1], nil
		}
		return 0, 0, os.ErrNotExist
	}
	lookupRenderGID = func() (uint32, bool) { return 107, true }
	return dev
}

// TestApplyGPU_AMDExposesKFDAndRenderNode is the load-bearing AMD ROCm test:
// the gpu entitlement on a host with /dev/kfd must expose BOTH the compute
// device and the DRM render node (KFD alone initializes but every allocation
// fails), add the render/video groups, and NOT fall through to the NVIDIA
// static-node fallback.
func TestApplyGPU_AMDExposesKFDAndRenderNode(t *testing.T) {
	dev := installFakeAMDDevTree(t,
		[2]int64{511, 0},
		map[string][2]int64{"renderD128": {226, 128}, "card0": {226, 0}},
	)

	spec := gpuSpec(t)

	// /dev/kfd is bound and its exact major:minor is allowed (rw, no mknod).
	kfdPath := filepath.Join(dev, "kfd")
	if !hasMountDest(spec, kfdPath) {
		t.Error("AMD GPU entitlement did not bind /dev/kfd")
	}
	if !hasExactDeviceRule(spec, 511, 0) {
		t.Error("AMD GPU entitlement did not allow the kfd major:minor")
	}

	// The render node is granted; card0 (display) is not.
	if _, ok := deviceForPath(spec, filepath.Join(dev, "dri", "renderD128")); !ok {
		t.Error("AMD GPU entitlement did not add the DRM render node")
	}
	if !hasExactDeviceRule(spec, 226, 128) {
		t.Error("AMD GPU entitlement did not allow the render node major:minor")
	}
	if _, ok := deviceForPath(spec, filepath.Join(dev, "dri", "card0")); ok {
		t.Error("AMD GPU entitlement granted the display card node")
	}

	// Render + video groups.
	if !hasGID(spec, 107) {
		t.Error("AMD GPU entitlement did not add the render GID")
	}
	if !hasGID(spec, videoGroupGID) {
		t.Error("AMD GPU entitlement did not add the video GID")
	}

	// No NVIDIA fallback: the AMD branch returns before the major-195 path.
	if hasMajorRule(spec, 195) {
		t.Error("AMD host must not get the NVIDIA major-195 fallback rule")
	}
	for _, e := range spec.Process.Env {
		if strings.HasPrefix(e, "NVIDIA_") {
			t.Errorf("AMD host must not get NVIDIA env vars, got %q", e)
		}
	}
}

func TestApplyEntitlements_HTTPIsNoOp(t *testing.T) {
	base := DefaultSpec("/rootfs", []string{"/bin/sh"})
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID: "test-app",
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementHTTP, Port: 8080},
		},
	}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements() error = %v", err)
	}
	if !reflect.DeepEqual(base, spec) {
		t.Errorf("http entitlement mutated the OCI spec; want no-op.\nbase: %+v\nspec: %+v", base, spec)
	}
}

func TestApplySensors_MountsOnlyPrivateSensorSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "wendy-sensors-oci-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	listener, err := net.Listen("unix", filepath.Join(dir, "sensors.sock"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID:        "test",
		Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementSensorRead}},
	}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{SensorSocketDir: dir}); err != nil {
		t.Fatalf("ApplyEntitlements: %v", err)
	}
	mount, ok := mountForDest(spec, "/run/wendy/sensors")
	if !ok || mount.Source != dir || !slices.Contains(mount.Options, "ro") {
		t.Fatalf("sensor mount = %+v, found=%v", mount, ok)
	}
	if !hasEnv(spec, "WENDY_SENSOR_SOCKET=/run/wendy/sensors/sensors.sock") {
		t.Fatal("sensor-read entitlement did not inject WENDY_SENSOR_SOCKET")
	}
	if !hasGID(spec, appSystemAPIGroupGID) {
		t.Fatal("sensor-read entitlement did not grant the private socket group")
	}
	// The whole point of the narrow grant: no admin socket, no data socket, and
	// above all no raw device nodes. An app that reaches sensors through the
	// harness must not also be handed /dev/videoN.
	if hasMount(spec, "/run/wendy/agent") || hasEnv(spec, "WENDY_AGENT_SOCKET=") {
		t.Error("sensor-read entitlement exposed the admin control socket")
	}
	if hasMount(spec, "/run/wendy/data") || hasEnv(spec, "WENDY_DATA_SOCKET=") {
		t.Error("sensor-read entitlement exposed the data socket")
	}
	if len(spec.Linux.Devices) != 0 {
		t.Errorf("sensor-read entitlement added device nodes: %+v", spec.Linux.Devices)
	}
	// The default spec's deny-all rule is expected; an ALLOW rule is not.
	if spec.Linux.Resources != nil {
		for _, rule := range spec.Linux.Resources.Devices {
			if rule.Allow {
				t.Errorf("sensor-read entitlement added an allow device cgroup rule: %+v", rule)
			}
		}
	}
}

func TestApplySensors_AbsentWithoutEntitlement(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{AppID: "test"}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{SensorSocketDir: t.TempDir()}); err != nil {
		t.Fatalf("ApplyEntitlements: %v", err)
	}
	if hasMount(spec, "/run/wendy/sensors") || hasEnv(spec, "WENDY_SENSOR_SOCKET=") {
		t.Fatal("app without the sensor-read entitlement received sensor access")
	}
}

// TestApplySensors_DoesNotDisplaceCameraEntitlement keeps the raw path intact:
// the new first-class path is additive, and an app that wants the device node
// still gets it (and can hold both grants).
func TestApplySensors_DoesNotDisplaceCameraEntitlement(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "wendy-sensors-oci-both-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	listener, err := net.Listen("unix", filepath.Join(dir, "sensors.sock"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{AppID: "test", Entitlements: []appconfig.Entitlement{
		{Type: appconfig.EntitlementSensorRead},
		{Type: appconfig.EntitlementCamera},
	}}
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{SensorSocketDir: dir}); err != nil {
		t.Fatalf("ApplyEntitlements: %v", err)
	}
	if !hasEnv(spec, "WENDY_SENSOR_SOCKET=/run/wendy/sensors/sensors.sock") {
		t.Error("sensor socket missing when combined with the camera entitlement")
	}
	if spec.Linux.Resources == nil || len(spec.Linux.Resources.Devices) == 0 {
		t.Error("camera entitlement stopped granting device access")
	}
}
