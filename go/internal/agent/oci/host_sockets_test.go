package oci

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// listenInTempDir returns a directory holding a listening socket of the given
// name, as the agent's socket providers leave it.
func listenInTempDir(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "wendy-host-socket-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	l, err := net.Listen("unix", filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return dir
}

func countEnv(spec *Spec, entry string) int {
	n := 0
	for _, e := range spec.Process.Env {
		if e == entry {
			n++
		}
	}
	return n
}

// WDY-3850: CreateContainer builds the spec before it removes the old
// container, and only then prepares the new container's host sockets.
// ApplyHostSockets adds what ApplyEntitlements left out for lack of them.
func TestApplyHostSockets_CompletesSpecBuiltWithoutSockets(t *testing.T) {
	const proxyDir = "/run/wendy/dbus-proxy/bt-app_btscan"
	systemAPIDir := listenInTempDir(t, "system.sock")
	dataDir := listenInTempDir(t, "data.sock")
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{
		AppID: "bt-app",
		Entitlements: []appconfig.Entitlement{
			{Type: appconfig.EntitlementBluetooth},
			{Type: appconfig.EntitlementNotifications},
			{Type: appconfig.EntitlementEpisodeWrite},
		},
	}

	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyEntitlements: %v", err)
	}
	ApplyHostSockets(spec, cfg, ApplyOptions{
		DBusProxySocketDir: proxyDir,
		SystemAPISocketDir: systemAPIDir,
		DataSocketDir:      dataDir,
	})

	for dest, src := range map[string]string{
		"/var/run/dbus":     proxyDir,
		"/run/wendy/system": systemAPIDir,
		"/run/wendy/data":   dataDir,
	} {
		if m, ok := mountForDest(spec, dest); !ok || m.Source != src {
			t.Errorf("mount at %s = %+v (found %v), want source %s", dest, m, ok, src)
		}
	}
	for _, entry := range []string{
		"DBUS_SYSTEM_BUS_ADDRESS=unix:path=/var/run/dbus/system_bus_socket",
		"WENDY_SYSTEM_SOCKET=/run/wendy/system/system.sock",
		"WENDY_DATA_SOCKET=/run/wendy/data/data.sock",
	} {
		if n := countEnv(spec, entry); n != 1 {
			t.Errorf("env %s appears %d times, want once", entry, n)
		}
	}
	if !hasGID(spec, appSystemAPIGroupGID) {
		t.Error("socket group not granted")
	}
}

// A socket directory is only mounted for the entitlement that grants it.
func TestApplyHostSockets_IgnoresSocketsWithoutEntitlement(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	cfg := &appconfig.AppConfig{AppID: "plain-app"}

	ApplyHostSockets(spec, cfg, ApplyOptions{
		DBusProxySocketDir: "/run/wendy/dbus-proxy/plain-app",
		SystemAPISocketDir: listenInTempDir(t, "system.sock"),
		DataSocketDir:      listenInTempDir(t, "data.sock"),
	})

	for _, dest := range []string{"/var/run/dbus", "/run/wendy/system", "/run/wendy/data"} {
		if hasMount(spec, dest) {
			t.Errorf("app without the entitlement got a mount at %s", dest)
		}
	}
	if hasGID(spec, appSystemAPIGroupGID) {
		t.Error("app without the entitlement got the socket group")
	}
}

// notifications and data-read grant the same one System API socket: each
// mounts it alone, and both together mount it once.
func TestApplyHostSockets_SystemAPIMountedOncePerApp(t *testing.T) {
	systemAPIDir := listenInTempDir(t, "system.sock")
	for _, entitlements := range [][]appconfig.Entitlement{
		{{Type: appconfig.EntitlementDataRead}},
		{{Type: appconfig.EntitlementNotifications}, {Type: appconfig.EntitlementDataRead, Allowlist: []string{"apps/com.example.detector"}}},
	} {
		spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
		cfg := &appconfig.AppConfig{AppID: "viewer", Entitlements: entitlements}
		if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
			t.Fatalf("ApplyEntitlements: %v", err)
		}
		ApplyHostSockets(spec, cfg, ApplyOptions{SystemAPISocketDir: systemAPIDir})
		mounts := 0
		for _, m := range spec.Mounts {
			if m.Destination == "/run/wendy/system" {
				mounts++
			}
		}
		if mounts != 1 || countEnv(spec, "WENDY_SYSTEM_SOCKET=/run/wendy/system/system.sock") != 1 {
			t.Errorf("%v: %d System API mounts, want one with one WENDY_SYSTEM_SOCKET", entitlements, mounts)
		}
		if hasMount(spec, "/run/wendy/agent") || hasMount(spec, "/run/wendy/data") {
			t.Errorf("%v: data-read mounted the admin or data socket", entitlements)
		}
	}
}
