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
	cfg := avahiConfig("app1")
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
	body := `#!/usr/bin/env python3
import os, sys, socket, time
from pathlib import Path
name=Path(sys.argv[0]).name
with open(os.environ['AVAHI_STUB_LOG'], 'a') as f: f.write(name+'-stub '+' '.join(sys.argv[1:])+'\n')
if name == 'nsenter': os.execvp('avahi-daemon', ['avahi-daemon'])
path=None
if name=='dbus-daemon':
 import re
 cfg=next(a.split('=',1)[1] for a in sys.argv[1:] if a.startswith('--config-file='))
 path=re.search(r'unix:path=([^<]+)',Path(cfg).read_text()).group(1)
if name=='xdg-dbus-proxy': path=sys.argv[2]
if path:
 s=socket.socket(socket.AF_UNIX); s.bind(path); s.listen(1)
while True: time.sleep(1)
`
	stubs := map[string]string{}
	for _, name := range []string{"dbus-daemon", "avahi-daemon", "xdg-dbus-proxy", "nsenter", "ip", "unshare", "mount", "sh"} {
		stubs[name] = body
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
	m.ready = func(context.Context, string) error { return nil }
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
	for _, want := range []string{"dbus-daemon-stub", "xdg-dbus-proxy-stub"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s in:\n%s", want, s)
		}
	}
	// The supervise goroutine spawns avahi just after Ensure returns.
	deadline := time.Now().Add(5 * time.Second)
	for {
		calls, _ = os.ReadFile(log)
		s = string(calls)
		if strings.Contains(s, "nsenter-stub") && strings.Contains(s, "avahi-daemon-stub") {
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
	m.ready = func(context.Context, string) error { return nil }
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
	m.ready = func(context.Context, string) error { return nil }
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
