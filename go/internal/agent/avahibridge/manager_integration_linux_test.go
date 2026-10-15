//go:build linux

package avahibridge

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"go.uber.org/zap"
)

// Runs inside a disposable privileged Linux container with the production
// executables. It never uses a developer machine's system bus or Avahi daemon.
func TestRealAvahiCoexistsWithHostAndOtherApp(t *testing.T) {
	if os.Getenv("WENDY_AVAHI_INTEGRATION") != "1" {
		t.Skip("requires isolated Linux runtime")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Production apps have distinct network namespaces and IPv4 addresses.
	// Putting multiple address-publishing daemons in the same namespace makes
	// their reverse records conflict forever, despite distinct hostnames.
	namespaces := make(map[string]string)
	for i, app := range []string{"first", "second"} {
		ns := "wendy-avahi-" + app
		peer := "wav" + app
		run := func(args ...string) {
			t.Helper()
			if output, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput(); err != nil {
				t.Fatalf("ip %v: %v: %s", args, err, output)
			}
		}
		run("netns", "add", ns)
		defer exec.Command("ip", "netns", "delete", ns).Run()
		run("link", "add", peer, "type", "veth", "peer", "name", "eth0", "netns", ns)
		run("addr", "add", fmt.Sprintf("10.217.%d.1/24", i+1), "dev", peer)
		run("link", "set", peer, "up")
		run("-n", ns, "addr", "add", fmt.Sprintf("10.217.%d.2/24", i+1), "dev", "eth0")
		run("-n", ns, "link", "set", "eth0", "up")
		run("-n", ns, "link", "set", "lo", "up")
		namespaces[app] = "/run/netns/" + ns
	}
	if err := os.MkdirAll("/run/dbus", 0755); err != nil {
		t.Fatal(err)
	}
	bus := exec.CommandContext(ctx, "dbus-daemon", "--system", "--nofork", "--nopidfile")
	if err := bus.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bus.Process.Kill(); _ = bus.Wait() }()
	if err := waitForSocket(ctx, "/run/dbus/system_bus_socket", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	// A fresh image may have never started the host daemon. Private startup
	// must create its mount target instead of depending on host service order.
	if _, err := os.Stat("/run/avahi-daemon"); !os.IsNotExist(err) {
		t.Fatalf("test requires a fresh host runtime directory, got %v", err)
	}
	m := NewManagerInDir(zap.NewNop(), filepath.Join("/run", "wendy-avahi-integration"))
	defer m.StopAll()
	if _, err := m.Prepare(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	if err := m.StartDaemon(ctx, "first", namespaces["first"], func(context.Context) (bool, error) { return true, nil }); err != nil {
		t.Fatalf("private startup before host: %v", err)
	}
	hostConf := filepath.Join(t.TempDir(), "host.conf")
	if err := os.WriteFile(hostConf, []byte(avahiConfig("host")), 0644); err != nil {
		t.Fatal(err)
	}
	host := exec.CommandContext(ctx, "avahi-daemon", "-f", hostConf)
	var hostOutput daemonOutput
	host.Stderr = &hostOutput
	if err := host.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = host.Process.Kill(); _ = host.Wait() }()
	if err := waitForDaemon(ctx, "/run/dbus/system_bus_socket"); err != nil {
		t.Fatalf("host: %v: %s", err, hostOutput.String())
	}
	hostPID, err := os.ReadFile("/run/avahi-daemon/pid")
	if err != nil {
		t.Fatal(err)
	}
	for _, app := range []string{"first", "second"} {
		dir, err := m.Prepare(ctx, app)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.StartDaemon(ctx, app, namespaces[app], func(context.Context) (bool, error) { return true, nil }); err != nil {
			t.Fatalf("%s: %v", app, err)
		}
		// A repeated start must not spawn a second daemon.
		if err := m.StartDaemon(ctx, app, namespaces[app], func(context.Context) (bool, error) { t.Fatal("duplicate address check"); return false, nil }); err != nil {
			t.Fatal(err)
		}
		privatePID, err := os.ReadFile(filepath.Join(m.stateDir(app), "daemon-run", "pid"))
		if err != nil || string(privatePID) == string(hostPID) {
			t.Fatalf("private PID: %q, %v", privatePID, err)
		}
		conn, err := dbus.Connect("unix:path="+filepath.Join(dir, proxySocketName), dbus.WithContext(ctx))
		if err != nil {
			t.Fatal(err)
		}
		var state int32
		err = conn.Object(avahiBusName, "/").CallWithContext(ctx, avahiBusName+".Server.GetState", 0).Store(&state)
		_ = conn.Close()
		if err != nil || state != 2 {
			t.Fatalf("filtered proxy state=%d err=%v", state, err)
		}
	}
	after, _ := os.ReadFile("/run/avahi-daemon/pid")
	if string(after) != string(hostPID) {
		t.Fatal("host daemon PID changed")
	}
	firstDir := m.proxyDir("first")
	before, _ := os.Stat(firstDir)
	m.Close()
	afterDir, err := os.Stat(firstDir)
	if err != nil || !os.SameFile(before, afterDir) {
		t.Fatal("agent close replaced a mounted socket directory")
	}
	if _, err := m.Prepare(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	if err := m.StartDaemon(ctx, "first", namespaces["first"], func(context.Context) (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Prepare(ctx, "broken"); err != nil {
		t.Fatal(err)
	}
	err = m.StartDaemon(ctx, "broken", "/nonexistent-netns", func(context.Context) (bool, error) { return true, nil })
	if err == nil || !strings.Contains(err.Error(), "avahi startup") {
		t.Fatalf("missing early daemon failure: %v", err)
	}
}
