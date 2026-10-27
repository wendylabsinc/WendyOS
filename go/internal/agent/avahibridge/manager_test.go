package avahibridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestStatePaths(t *testing.T) {
	name := strings.Repeat("a", 253) + "_" + strings.Repeat("b", 57)
	path := filepath.Join(ProxySocketDir(name), proxySocketName)
	if len(path) >= 104 {
		t.Fatalf("valid ID exceeds socket path bound: %q", path)
	}
	if path == filepath.Join(ProxySocketDir(name+"x"), proxySocketName) {
		t.Fatal("different IDs shared a socket path")
	}
	if !strings.HasPrefix(path, defaultBaseDir+"/") {
		t.Fatal("state path escaped its root")
	}
}

func TestBusConfigListenPath(t *testing.T) {
	cfg := busConfig("/run/wendy/avahi/x/bus.sock", os.Getuid())
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
if name == 'avahi-daemon' and os.environ.get('AVAHI_STUB_IGNORE_TERM'):
 import signal
 signal.signal(signal.SIGTERM, signal.SIG_IGN)
 print('private-daemon-detail', file=sys.stderr, flush=True)
with open(os.environ['AVAHI_STUB_LOG'], 'a') as f: f.write(name+'-stub '+' '.join(sys.argv[1:])+'\n')
if name == 'nsenter':
 with open(os.environ['AVAHI_STUB_LOG'], 'a') as f: f.write('nsenter-fd '+str(os.fstat(3).st_ino)+'\n')
 os.execvp('avahi-daemon', ['avahi-daemon'])
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
	m := newStubManager(t)
	m.ready = func(context.Context, string) error { return nil }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dir, err := m.Prepare(ctx, "app1")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := m.StartDaemon(ctx, "app1", "/dev/null", func(context.Context) (bool, error) {
		return true, nil
	}); err != nil {
		t.Fatalf("StartDaemon: %v", err)
	}
	if dir != m.proxyDir("app1") {
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
	if _, err := os.Stat(m.stateDir("app1")); !os.IsNotExist(err) {
		t.Fatal("state dir not cleaned")
	}
}

func TestPrepareIsIdempotent(t *testing.T) {
	bin := t.TempDir()
	stubBinaries(t, bin)
	t.Setenv("AVAHI_STUB_LOG", filepath.Join(bin, "calls.log"))
	m := newStubManager(t)
	m.ready = func(context.Context, string) error { return nil }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	planned := m.ProxySocketDir("app1")
	if _, err := os.Stat(planned); !os.IsNotExist(err) {
		t.Fatalf("planning the proxy mount changed filesystem state: %v", err)
	}
	if _, err := m.Prepare(ctx, "app1"); err != nil {
		t.Fatalf("first Prepare: %v", err)
	}
	defer m.Stop("app1")
	dir2, err := m.Prepare(ctx, "app1")
	if err != nil {
		t.Fatalf("repeat Prepare: %v", err)
	}
	if dir2 == "" || dir2 != planned {
		t.Fatalf("prepared proxy mount %q differs from validated path %q", dir2, planned)
	}
}

func TestStartDaemonRequiresPreparedAddress(t *testing.T) {
	bin := t.TempDir()
	stubBinaries(t, bin)
	t.Setenv("AVAHI_STUB_LOG", filepath.Join(bin, "calls.log"))
	m := newStubManager(t)
	m.ready = func(context.Context, string) error { return nil }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	calls := 0
	if _, err := m.Prepare(ctx, "app1"); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer m.Stop("app1")
	err := m.StartDaemon(ctx, "app1", "/dev/null", func(context.Context) (bool, error) {
		calls++
		return false, nil
	})
	if err == nil || !strings.Contains(err.Error(), "no IPv4") || calls != 1 {
		t.Fatalf("expected missing-address error, got %v (checks=%d)", err, calls)
	}
}

// A task restart gets a new daemon without replacing the mounted directory.
func TestPrepareForTaskRetiresPreviousStack(t *testing.T) {
	bin := t.TempDir()
	stubBinaries(t, bin)
	t.Setenv("AVAHI_STUB_LOG", filepath.Join(bin, "calls.log"))
	m := newStubManager(t)
	m.ready = func(context.Context, string) error { return nil }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dir, err := m.Prepare(ctx, "app1")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop("app1")
	before, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.StartDaemon(ctx, "app1", "/dev/null", func(context.Context) (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	old := m.instances["app1"]
	if _, err := m.PrepareForTask(ctx, "app1"); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || old.runCtx.Err() == nil || m.instances["app1"] == old || m.instances["app1"].daemonStarted {
		t.Fatal("new task retained the previous stack or replaced its mounted directory")
	}
}

// macOS's default test temp path can exceed sockaddr_un's path limit.
func newStubManager(t *testing.T) *Manager {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "avahi-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	m := NewManagerInDir(zap.NewNop(), dir)
	m.identity = func() (int, int, error) { return os.Getuid(), os.Getgid(), nil }
	t.Cleanup(m.StopAll)
	return m
}

func waitStubDaemon(log string) error {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(log)
		if strings.Contains(string(b), "avahi-daemon-stub") {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("daemon never entered stub")
}

func TestFailedReadinessAndStopBoundStubbornDaemon(t *testing.T) {
	for _, startupFailure := range []bool{true, false} {
		t.Run(fmt.Sprint(startupFailure), func(t *testing.T) {
			bin := t.TempDir()
			stubBinaries(t, bin)
			log := filepath.Join(bin, "calls.log")
			t.Setenv("AVAHI_STUB_LOG", log)
			t.Setenv("AVAHI_STUB_IGNORE_TERM", "1")
			m := newStubManager(t)
			m.ready = func(context.Context, string) error {
				if err := waitStubDaemon(log); err != nil {
					return err
				}
				if startupFailure {
					return errors.New("injected readiness failure")
				}
				return nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := m.Prepare(ctx, "app1"); err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			err := m.StartDaemon(ctx, "app1", "/dev/null", func(context.Context) (bool, error) { return true, nil })
			if startupFailure {
				if err == nil || !strings.Contains(err.Error(), "injected readiness failure") || strings.Contains(err.Error(), "private-daemon-detail") {
					t.Fatalf("readiness failure: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if err := m.Stop("app1"); err != nil {
				t.Fatal(err)
			}
			if time.Since(started) > 5*time.Second {
				t.Fatal("daemon cleanup exceeded its shutdown bound")
			}
		})
	}
}

func TestDaemonCrashRestartsAndDeadStackFailsClosed(t *testing.T) {
	bin := t.TempDir()
	stubBinaries(t, bin)
	log := filepath.Join(bin, "calls.log")
	t.Setenv("AVAHI_STUB_LOG", log)
	m := newStubManager(t)
	var probes atomic.Int32
	m.ready = func(ctx context.Context, _ string) error {
		if probes.Add(1) != 2 {
			return nil
		}
		// The first recovery attempt fails readiness after its process starts;
		// supervision must retry rather than abandon the remaining budget.
		for {
			b, _ := os.ReadFile(log)
			if strings.Count(string(b), "avahi-daemon-stub") >= 2 {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
		}
		return errors.New("transient recovery readiness failure")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := m.Prepare(ctx, "app1"); err != nil {
		t.Fatal(err)
	}
	namespace := filepath.Join(bin, "task-namespace")
	if err := os.WriteFile(namespace, nil, 0600); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(namespace)
	if err != nil {
		t.Fatal(err)
	}
	wantFD := fmt.Sprintf("nsenter-fd %d", stat.Sys().(*syscall.Stat_t).Ino)
	if err := m.StartDaemon(ctx, "app1", namespace, func(context.Context) (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	if err := waitStubDaemon(log); err != nil {
		t.Fatal(err)
	}
	// Simulate /proc/<pid>/ns/net now naming a different task. A restarted
	// daemon must keep receiving the original anchored namespace descriptor.
	if err := os.Remove(namespace); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(namespace, nil, 0600); err != nil {
		t.Fatal(err)
	}
	inst := m.instances["app1"]
	inst.mu.Lock()
	cmd := inst.procs[len(inst.procs)-1]
	inst.mu.Unlock()
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(13 * time.Second)
	for {
		b, _ := os.ReadFile(log)
		if strings.Count(string(b), "avahi-daemon-stub") >= 3 {
			if strings.Count(string(b), wantFD) != 3 {
				t.Fatalf("restart changed namespace fd: %s", b)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon did not restart")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A bus/proxy failure cancels the instance. Repeated StartDaemon must
	// reject that stopped stack even though daemonStarted was already true.
	inst.mu.Lock()
	bus := inst.procs[0]
	inst.mu.Unlock()
	if err := bus.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for inst.runCtx.Err() == nil {
		if time.Now().After(deadline) {
			t.Fatal("bus death did not cancel stack")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := m.StartDaemon(ctx, "app1", "/dev/null", nil); err == nil {
		t.Fatal("dead instance returned ready")
	}
	before, _ := os.Stat(m.proxyDir("app1"))
	if _, err := m.Prepare(ctx, "app1"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(m.proxyDir("app1"))
	if !os.SameFile(before, after) {
		t.Fatal("stack recovery replaced mounted directory")
	}
	if err := m.StartDaemon(ctx, "app1", "/dev/null", func(context.Context) (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareLongContainerIDAndOwnerMismatch(t *testing.T) {
	bin := t.TempDir()
	stubBinaries(t, bin)
	t.Setenv("AVAHI_STUB_LOG", filepath.Join(bin, "calls.log"))
	m := newStubManager(t)
	name := strings.Repeat("a", 253) + "_" + strings.Repeat("b", 57)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := m.Prepare(ctx, name); err != nil {
		t.Fatalf("valid long name: %v", err)
	}
	ownerPath := filepath.Join(m.stateDir(name), "container-id")
	owner, err := os.ReadFile(ownerPath)
	if err != nil || string(owner) != name {
		t.Fatalf("state owner: %q %v", owner, err)
	}
	m.Close()
	if err := os.WriteFile(ownerPath, []byte("another-container"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Prepare(ctx, name); err == nil {
		t.Fatal("foreign runtime state was reused")
	}
	owner, _ = os.ReadFile(ownerPath)
	if string(owner) != "another-container" {
		t.Fatal("foreign state was overwritten")
	}
}

func TestFailedRunningRecoveryPreservesMountedDirectory(t *testing.T) {
	bin := t.TempDir()
	stubBinaries(t, bin)
	t.Setenv("AVAHI_STUB_LOG", filepath.Join(bin, "calls.log"))
	m := newStubManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dir, err := m.Prepare(ctx, "app1")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.StartDaemon(ctx, "app1", "/missing-namespace", nil); err == nil {
		t.Fatal("missing namespace accepted")
	}
	if err := m.StopPreservingMounts("app1"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Prepare(ctx, "app1"); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(dir)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("failed running recovery replaced mounted directory")
	}
	m.ready = func(context.Context, string) error { return nil }
	if err := m.StartDaemon(ctx, "app1", "/dev/null", func(context.Context) (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
}

func TestNilManagerCleanup(t *testing.T) {
	var m *Manager
	if err := m.Stop("plain-app"); err != nil {
		t.Fatal(err)
	}
	if err := m.StopPreservingMounts("plain-app"); err != nil {
		t.Fatal(err)
	}
	m.StopAll()
	m.Close()
}

func TestPrepareRunningStackKeepsDaemonAndClients(t *testing.T) {
	bin := t.TempDir()
	stubBinaries(t, bin)
	t.Setenv("AVAHI_STUB_LOG", filepath.Join(bin, "calls.log"))
	m := newStubManager(t)
	m.ready = func(context.Context, string) error { return nil }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dir, err := m.Prepare(ctx, "app1")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.StartDaemon(ctx, "app1", "/dev/null", func(context.Context) (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	old := m.instances["app1"]
	if got, err := m.Prepare(ctx, "app1"); err != nil || got != dir {
		t.Fatalf("running preparation: %s %v", got, err)
	}
	if m.instances["app1"] != old || old.runCtx.Err() != nil || !old.daemonStarted {
		t.Fatal("running recovery retired a healthy stack")
	}
}
