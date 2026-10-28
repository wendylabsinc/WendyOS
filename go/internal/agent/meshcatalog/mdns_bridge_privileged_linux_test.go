//go:build linux

package meshcatalog

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// This uses real Avahi responders, including a second daemon sharing the
// bridge's host port. Run only in a disposable privileged Linux container.
func TestAppMDNSMulticastLifetimesWithAvahi(t *testing.T) {
	if os.Getenv("WENDY_LAN_PRIVILEGED_TEST") != "1" {
		t.Skip("requires disposable privileged Linux network namespace and Avahi")
	}
	run := func(program string, args ...string) {
		t.Helper()
		if out, err := exec.Command(program, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v: %s", program, args, err, out)
		}
	}
	suffix := fmt.Sprint(os.Getpid())
	ns, hostIF, appIF := "ma"+suffix, "mh"+suffix, "mc"+suffix
	t.Cleanup(func() {
		_ = exec.Command("ip", "netns", "del", ns).Run()
		_ = exec.Command("ip", "link", "del", hostIF).Run()
	})
	run("ip", "netns", "add", ns)
	run("ip", "link", "add", hostIF, "type", "veth", "peer", "name", appIF)
	run("ip", "link", "set", appIF, "netns", ns)
	run("ip", "addr", "add", "192.168.42.1/24", "dev", hostIF)
	run("ip", "link", "set", hostIF, "up")
	run("ip", "netns", "exec", ns, "ip", "link", "set", "lo", "up")
	run("ip", "netns", "exec", ns, "ip", "addr", "add", "192.168.42.2/24", "dev", appIF)
	run("ip", "netns", "exec", ns, "ip", "link", "set", appIF, "up")
	start := func(program string, args ...string) {
		t.Helper()
		log, err := os.CreateTemp(t.TempDir(), "daemon-*.log")
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(program, args...)
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			_ = log.Close()
			if t.Failed() {
				data, _ := os.ReadFile(log.Name())
				t.Logf("%s: %s", program, data)
			}
		})
	}
	daemon := func(name string, prefix []string) *dbus.Conn {
		t.Helper()
		dir := t.TempDir()
		bus := filepath.Join(dir, "bus.sock")
		config := `<busconfig><type>session</type><listen>unix:path=` + bus + `</listen><policy context="default"><allow user="*"/><allow own="*"/><allow send_destination="*"/><allow receive_sender="*"/></policy></busconfig>`
		if err := os.WriteFile(filepath.Join(dir, "bus.conf"), []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		start("dbus-daemon", "--nofork", "--nopidfile", "--config-file="+filepath.Join(dir, "bus.conf"))
		var conn *dbus.Conn
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var err error
			conn, err = dbus.Connect("unix:path=" + bus)
			if err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if conn == nil {
			t.Fatal("private bus unavailable")
		}
		t.Cleanup(func() { _ = conn.Close() })
		for _, sub := range []string{"run", "services"} {
			if err := os.Mkdir(filepath.Join(dir, sub), 0700); err != nil {
				t.Fatal(err)
			}
		}
		avahiConfig := "[server]\nhost-name=" + name + "\nuse-ipv6=no\ndisallow-other-stacks=no\n[wide-area]\nenable-wide-area=no\n[publish]\npublish-hinfo=no\npublish-workstation=no\n[reflector]\nenable-reflector=no\n"
		if err := os.WriteFile(filepath.Join(dir, "avahi.conf"), []byte(avahiConfig), 0600); err != nil {
			t.Fatal(err)
		}
		// Avahi has a fixed PID/service directory: isolate those mounts exactly as
		// the agent's private app daemon does, without changing the host daemon.
		command := append(append([]string{}, prefix...), "unshare", "--mount", "--propagation", "private", "sh", "-c",
			`mkdir -p /run/avahi-daemon /etc/avahi/services && mount --bind "$1/run" /run/avahi-daemon && mount --bind "$1/services" /etc/avahi/services && exec env DBUS_SYSTEM_BUS_ADDRESS="unix:path=$1/bus.sock" avahi-daemon --no-chroot --no-drop-root --no-rlimits -f "$1/avahi.conf"`, "avahi-test", dir)
		start(command[0], command[1:]...)
		server := conn.Object("org.freedesktop.Avahi", "/")
		deadline = time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			var state int32
			if err := server.Call("org.freedesktop.Avahi.Server.GetState", 0).Store(&state); err == nil && state == 2 {
				return conn
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("Avahi did not become ready")
		return nil
	}
	_ = daemon("host-responder", nil)
	app := daemon("native-app", []string{"ip", "netns", "exec", ns})
	var path dbus.ObjectPath
	if err := app.Object("org.freedesktop.Avahi", "/").Call("org.freedesktop.Avahi.Server.EntryGroupNew", 0).Store(&path); err != nil {
		t.Fatal(err)
	}
	group := app.Object("org.freedesktop.Avahi", path)
	call := func(method string, args ...interface{}) {
		t.Helper()
		if err := group.Call("org.freedesktop.Avahi.EntryGroup."+method, 0, args...).Err; err != nil {
			t.Fatal(err)
		}
	}
	call("AddService", int32(-1), int32(0), uint32(0), "Stable Service", "_http._tcp", "local", "", uint16(8080), [][]byte{[]byte("path=/initial")})
	call("Commit")
	// The service predates the bridge, so discovery cannot depend on its startup
	// announcements. Avahi will answer the bridge's subsequent browse queries.
	time.Sleep(4 * time.Second)
	iface, err := net.InterfaceByName(hostIF)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	catalog, _ := f.newCatalog(t, p533, "default", nil, nil)
	emitted := make(chan Record, 64)
	bridge, err := NewMDNSBridge(AppScope{AppID: "com.wendy.test", AppIP: net.ParseIP("192.168.42.2"), BridgeIndex: iface.Index, Ports: map[uint16]uint16{8080: 18080}}, catalog, testVIPs(t), func(string, Record) bool { return true }, func(w Record) { emitted <- w }, emptyBridgeRouteView)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- bridge.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("bridge: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("bridge did not stop")
		}
	})
	next := func(timeout time.Duration) Record {
		t.Helper()
		select {
		case w := <-emitted:
			return w
		case err := <-done:
			t.Fatalf("bridge exited: %v", err)
		case <-time.After(timeout):
			t.Fatal("signed service update missing")
		}
		return Record{}
	}
	first := next(12 * time.Second)
	lease := time.Until(time.UnixMilli(first.Expires))
	if first.Withdraw || lease < 60*time.Second || lease > MaxLease {
		t.Fatalf("native mDNS lifetime lost: lease=%s record=%+v", lease, first)
	}
	// At least two more query cycles must not cause five-second generations.
	select {
	case w := <-emitted:
		t.Fatalf("unchanged service renewed within12s: %s", w)
	case <-time.After(12 * time.Second):
	}
	call("UpdateServiceTxt", int32(-1), int32(0), uint32(0), "Stable Service", "_http._tcp", "local", [][]byte{[]byte("path=/changed")})
	changed := next(4 * time.Second)
	if changed.Withdraw || changed.Generation != first.Generation+1 || len(changed.TXT) != 1 || changed.TXT[0] != "path=/changed" {
		t.Fatalf("live TXT change: %+v", changed)
	}
	call("Reset")
	gone := next(4 * time.Second)
	if !gone.Withdraw || gone.Generation != changed.Generation+1 {
		t.Fatalf("goodbye missing: %+v", gone)
	}
	t.Logf("real Avahi with host coexistence: initial lease=%s; stable12s; immediate TXT update and goodbye", lease)
}
