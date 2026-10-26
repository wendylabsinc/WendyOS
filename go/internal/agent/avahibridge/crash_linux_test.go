//go:build linux

package avahibridge

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// This helper intentionally skips Close: the parent kills it like an agent
// crash, then proves that its bus, proxy and daemon children stop and that a
// replacement agent can recreate the stack in the surviving mounted directory.
func TestAgentCrashHelper(t *testing.T) {
	dir := os.Getenv("AVAHI_CRASH_HELPER_DIR")
	if dir == "" {
		return
	}
	m := NewManagerInDir(zap.NewNop(), dir)
	namespace := os.Getenv("AVAHI_CRASH_REAL_NETNS")
	if namespace == "" {
		namespace = "/dev/null"
		m.identity = func() (int, int, error) { return os.Getuid(), os.Getgid(), nil }
		m.ready = func(context.Context, string) error { return nil }
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := m.Prepare(ctx, "app1"); err != nil {
		t.Fatal(err)
	}
	if err := m.StartDaemon(ctx, "app1", namespace, func(context.Context) (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	inst := m.instances["app1"]
	inst.mu.Lock()
	var pids []int
	for _, cmd := range inst.procs {
		pids = append(pids, cmd.Process.Pid)
	}
	inst.mu.Unlock()
	b, _ := json.Marshal(pids)
	if err := os.WriteFile(filepath.Join(dir, "children.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	select {}
}

func TestAgentCrashRetiresChildrenAndReplacementRecovers(t *testing.T) {
	bin := t.TempDir()
	stubBinaries(t, bin)
	log := filepath.Join(bin, "calls.log")
	t.Setenv("AVAHI_STUB_LOG", log)
	m := newStubManager(t)
	m.ready = func(context.Context, string) error { return nil }
	testAgentCrashRecovery(t, m, "/dev/null", log)
}

func testAgentCrashRecovery(t *testing.T, m *Manager, namespace, log string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestAgentCrashHelper$")
	cmd.Env = append(os.Environ(), "AVAHI_CRASH_HELPER_DIR="+m.baseDir)
	if namespace != "/dev/null" {
		cmd.Env = append(cmd.Env, "AVAHI_CRASH_REAL_NETNS="+namespace)
	}
	var output daemonOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	deadline := time.Now().Add(8 * time.Second)
	var pids []int
	for {
		b, err := os.ReadFile(filepath.Join(m.baseDir, "children.json"))
		if err == nil && json.Unmarshal(b, &pids) == nil && len(pids) == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper failed: %s", output.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if log != "" {
		if err := waitStubDaemon(log); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.Stat(m.proxyDir("app1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	for _, pid := range pids {
		deadline := time.Now().Add(3 * time.Second)
		for {
			stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
			// Container PID 1 may leave dead children as zombies briefly.
			if os.IsNotExist(err) || strings.Contains(string(stat), ") Z ") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("child %d survived agent crash: %s", pid, stat)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := m.Prepare(ctx, "app1"); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(m.proxyDir("app1"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("crash recovery replaced mounted directory")
	}
	if err := m.StartDaemon(ctx, "app1", namespace, func(context.Context) (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
}
