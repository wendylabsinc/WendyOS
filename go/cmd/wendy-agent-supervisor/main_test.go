//go:build linux

package main

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/supervisor"
)

func TestRefusesToRunUnderAnotherSupervisor(t *testing.T) {
	t.Setenv(supervisor.EnvSupervisor, "/opt/wendyos/bin/wendy-agent-supervisor")
	if code := run(); code == 0 {
		t.Fatal("a supervisor uploaded as an agent must exit at once")
	}
}

func TestHangupWakesTheSupervisorOnce(t *testing.T) {
	wake := wakeOnHangup()
	defer signal.Reset(syscall.SIGHUP)
	for range 3 {
		if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-wake:
	case <-time.After(5 * time.Second):
		t.Fatal("SIGHUP must wake the supervisor")
	}
	time.Sleep(100 * time.Millisecond)
	select {
	case <-wake:
		// A second wake may follow if a signal landed after the first was taken.
	default:
	}
	select {
	case <-wake:
		t.Fatal("hangups must not queue up restarts")
	default:
	}
}

func TestATimedOutGateLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "helper.pid")
	gate := filepath.Join(dir, "agent")
	script := "#!/bin/sh\nsleep 600 &\necho $! > " + pidFile + "\nsleep 600\n"
	if err := os.WriteFile(gate, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runGate(context.Background(), gate, 300*time.Millisecond); err == nil {
		t.Fatal("a gate that runs past its timeout must fail")
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	// What the gate started must not keep running into the image agent's own gate.
	for deadline := time.Now().Add(5 * time.Second); alive(pid); {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("the gate's helper %d outlived it", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// alive reports whether pid runs; a zombie waiting for init does not count.
func alive(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	_, after, _ := strings.Cut(string(stat), ") ")
	return !strings.HasPrefix(after, "Z")
}
