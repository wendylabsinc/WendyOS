//go:build !windows

package mcp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRunProcessHelper is the `wendy run` stand-in for the process tests
// below. Like the CLI starting docker or swift, it starts a descendant that
// stays in the run's process group, and records the descendant's PID.
func TestRunProcessHelper(t *testing.T) {
	mode := os.Getenv("WENDY_MCP_RUN_PROCESS_HELPER")
	if mode == "" {
		t.Skip("helper process for the run process tests")
	}
	// "interrupt": the descendant ignores SIGINT and keeps no pipe, like a
	// build that outlives the CLI. "orphan": the descendant holds stdout open
	// after the CLI succeeds. "stubborn": the CLI and its descendant survive
	// SIGINT and SIGTERM, so only SIGKILL stops them.
	script := "trap '' INT; exec sleep 60"
	switch mode {
	case "stubborn":
		script = "trap '' INT TERM; exec sleep 60"
	}
	child := exec.Command("/bin/sh", "-c", script)
	if mode == "orphan" {
		child = exec.Command("/bin/sh", "-c", "exec sleep 60")
		child.Stdout = os.Stdout
	}
	interrupted := make(chan os.Signal, 2)
	signal.Notify(interrupted, os.Interrupt, syscall.SIGTERM)
	if err := child.Start(); err != nil {
		fmt.Println("helper:", err)
		os.Exit(2)
	}
	if err := os.WriteFile(os.Getenv("WENDY_MCP_RUN_PROCESS_PIDFILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
		os.Exit(2)
	}
	if mode == "orphan" {
		fmt.Println("deployed")
		os.Exit(0)
	}
	fmt.Println("building")
	if mode == "stubborn" {
		for sig := range interrupted {
			fmt.Println("ignored", sig)
		}
	}
	<-interrupted
	fmt.Println("interrupted: stopping application")
	os.Exit(3)
}

func startRunProcessHelper(t *testing.T, ctx context.Context, mode string) (<-chan runProcessResult, int) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	t.Setenv("WENDY_MCP_RUN_PROCESS_HELPER", mode)
	t.Setenv("WENDY_MCP_RUN_PROCESS_PIDFILE", pidFile)
	done := make(chan runProcessResult, 1)
	go func() {
		out, truncated, err := executeRunCommand(ctx, []string{"-test.run=^TestRunProcessHelper$"}, commandTarget{}, 16384)
		done <- runProcessResult{out, truncated, err}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil && len(data) > 0 {
			pid, err := strconv.Atoi(string(data))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			return done, pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("helper did not start its descendant")
	return nil, 0
}

type runProcessResult struct {
	output    string
	truncated bool
	err       error
}

func runProcessAlive(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	state := strings.TrimSpace(string(out))
	return err == nil && state != "" && !strings.HasPrefix(state, "Z")
}

// Review: a cancelled or timed-out run must interrupt the whole process group
// (so the CLI can stop what it started, as on Ctrl-C) and must not leave build
// descendants running afterwards.
func TestExecuteRunCommandInterruptsProcessGroupAndReapsDescendants(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done, descendant := startRunProcessHelper(t, ctx, "interrupt")
	cancel()
	var result runProcessResult
	select {
	case result = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled run did not return")
	}
	if result.err == nil || !strings.Contains(result.output, "interrupted: stopping application") {
		t.Fatalf("CLI did not receive SIGINT: output=%q err=%v", result.output, result.err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for runProcessAlive(descendant) {
		if time.Now().After(deadline) {
			t.Fatalf("descendant %d outlived the cancelled run", descendant)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Review: a CLI that exits 0 while a descendant still holds its stdout is a
// successful deploy, not ErrWaitDelay/INTERNAL.
func TestExecuteRunCommandSucceedsWhenDescendantHoldsOutput(t *testing.T) {
	done, descendant := startRunProcessHelper(t, context.Background(), "orphan")
	select {
	case result := <-done:
		if result.err != nil || !strings.Contains(result.output, "deployed") {
			t.Fatalf("successful run reported failure: output=%q err=%v", result.output, result.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after the CLI exited")
	}
	// Only a cancelled run is reaped; a successful CLI's descendants are its own.
	if !runProcessAlive(descendant) {
		t.Fatal("a successful run's descendant was killed")
	}
}

// A CLI that survives SIGINT gets SIGTERM after the first grace period and
// SIGKILL after the second, and its descendants go with it.
func TestExecuteRunCommandEscalatesToSIGTERMThenSIGKILL(t *testing.T) {
	old := runStopGrace
	t.Cleanup(func() { runStopGrace = old })
	runStopGrace = [...]time.Duration{300 * time.Millisecond, 300 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done, descendant := startRunProcessHelper(t, ctx, "stubborn")
	start := time.Now()
	cancel()
	var result runProcessResult
	select {
	case result = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stubborn run was never killed")
	}
	interrupt, terminate := strings.Index(result.output, "ignored interrupt"), strings.Index(result.output, "ignored terminated")
	if interrupt < 0 || terminate < interrupt || result.err == nil || result.err.Error() != "signal: killed" {
		t.Fatalf("want SIGINT, then SIGTERM, then SIGKILL: output=%q err=%v", result.output, result.err)
	}
	if elapsed := time.Since(start); elapsed < 600*time.Millisecond {
		t.Fatalf("killed after %v, before both grace periods passed", elapsed)
	}
	deadline := time.Now().Add(3 * time.Second)
	for runProcessAlive(descendant) {
		if time.Now().After(deadline) {
			t.Fatalf("descendant %d survived the kill", descendant)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
