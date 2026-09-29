package avahibridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestStatePaths(t *testing.T) {
	if got := ProxySocketDir("myapp_svc"); !strings.HasSuffix(got, "/run/wendy/avahi/myapp_svc/proxy") {
		t.Fatalf("proxy dir %q", got)
	}
}

func TestBusConfigListenPath(t *testing.T) {
	cfg := busConfig("/run/wendy/avahi/x/bus.sock")
	if !strings.Contains(cfg, "unix:path=/run/wendy/avahi/x/bus.sock") {
		t.Fatalf("bus config missing listen path:\n%s", cfg)
	}
}

func TestAvahiConfigDisallowsExclusiveHold(t *testing.T) {
	cfg := avahiConfig()
	if !strings.Contains(cfg, "disallow-other-stacks=no") {
		t.Fatalf("config must allow co-binding :5353:\n%s", cfg)
	}
	if strings.Contains(cfg, "enable-reflector=yes") {
		t.Fatal("reflector must stay off")
	}
}

// stubBinaries installs fake dbus-daemon/avahi-daemon/xdg-dbus-proxy/nsenter/ip
// that behave just enough for lifecycle tests and records invocations.
func stubBinaries(t *testing.T, dir string) {
	t.Helper()
	stubs := map[string]string{
		"dbus-daemon": `#!/bin/sh
# args contain --config-file=PATH; extract socket from it is overkill:
# the manager polls for the socket, so create a file at the path given
# via AVAHI_STUB_SOCKET env.
while [ $# -gt 0 ]; do case "$1" in --config-file=*) cfg="${1#--config-file=}";; esac; shift; done
sock=$(grep -o 'unix:path=[^<]*' "$cfg" | head -1 | cut -d= -f2)
[ -n "$sock" ] && { mkdir -p "$(dirname "$sock")"; touch "$sock"; }
echo "dbus-stub $@" >> "$AVAHI_STUB_LOG"
sleep 30 &
wait
`,
		"xdg-dbus-proxy": `#!/bin/sh
echo "proxy-stub $@" >> "$AVAHI_STUB_LOG"
# args: <upstream> <socketpath> --filter ...; socket is $2
mkdir -p "$(dirname "$2")"
touch "$2"
sleep 30 &
wait
`,
		"avahi-daemon": `#!/bin/sh
echo "avahi-stub $@" >> "$AVAHI_STUB_LOG"
sleep 30 &
wait
`,
		"nsenter": `#!/bin/sh
echo "nsenter-stub $@" >> "$AVAHI_STUB_LOG"
# drop through --net=X env VAR=... to avahi-daemon and its args
while [ $# -gt 0 ]; do
  case "$1" in avahi-daemon) break;; *) shift;;
  esac
done
exec "$@"
`,
		"ip": `#!/bin/sh
echo "ip-stub $@" >> "$AVAHI_STUB_LOG"
if [ "$1" = "-o" ]; then echo "9: eth0    inet 10.99.0.5/24 brd x scope global eth0"; fi
exit 0
`,
	}
	for name, body := range stubs {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

func TestPrepareAndStartDaemonFullStack(t *testing.T) {
	bin := t.TempDir()
	stubBinaries(t, bin)
	log := filepath.Join(bin, "calls.log")
	t.Setenv("AVAHI_STUB_LOG", log)
	m := NewManagerInDir(zap.NewNop(), t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dir, err := m.Prepare(ctx, "app1")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := m.StartDaemon(ctx, "app1", "/proc/1/ns/net", func(context.Context) (bool, error) {
		return true, nil
	}); err != nil {
		t.Fatalf("StartDaemon: %v", err)
	}
	if dir != filepath.Join(m.baseDir, "app1", "proxy") {
		t.Fatalf("proxy dir %q", dir)
	}
	calls, _ := os.ReadFile(log)
	s := string(calls)
	for _, want := range []string{"dbus-stub", "proxy-stub"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s in:\n%s", want, s)
		}
	}
	// The supervise goroutine spawns avahi just after Ensure returns.
	deadline := time.Now().Add(5 * time.Second)
	for {
		calls, _ = os.ReadFile(log)
		s = string(calls)
		if strings.Contains(s, "nsenter-stub") && strings.Contains(s, "avahi-stub") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("avahi never spawned in:\n%s", s)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(s, "--talk=org.freedesktop.Avahi") || !strings.Contains(s, "--see=org.freedesktop.Avahi") {
		t.Fatalf("proxy not scoped to Avahi:\n%s", s)
	}
	if err := m.Stop("app1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.baseDir, "app1")); !os.IsNotExist(err) {
		t.Fatal("state dir not cleaned")
	}
}

func TestPrepareIsIdempotent(t *testing.T) {
	bin := t.TempDir()
	stubBinaries(t, bin)
	t.Setenv("AVAHI_STUB_LOG", filepath.Join(bin, "calls.log"))
	m := NewManagerInDir(zap.NewNop(), t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := m.Prepare(ctx, "app1"); err != nil {
		t.Fatalf("first Prepare: %v", err)
	}
	defer m.Stop("app1")
	dir2, err := m.Prepare(ctx, "app1")
	if err != nil {
		t.Fatalf("repeat Prepare: %v", err)
	}
	if dir2 == "" {
		t.Fatal("repeat Prepare returned empty dir")
	}
}

func TestStartDaemonWaitsForAddress(t *testing.T) {
	bin := t.TempDir()
	stubBinaries(t, bin)
	t.Setenv("AVAHI_STUB_LOG", filepath.Join(bin, "calls.log"))
	m := NewManagerInDir(zap.NewNop(), t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	calls := 0
	if _, err := m.Prepare(ctx, "app1"); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer m.Stop("app1")
	err := m.StartDaemon(ctx, "app1", "/proc/1/ns/net", func(context.Context) (bool, error) {
		calls++
		return false, nil
	})
	if err == nil || !strings.Contains(err.Error(), "no IPv4") {
		t.Fatalf("expected address timeout, got %v (checks=%d)", err, calls)
	}
}
