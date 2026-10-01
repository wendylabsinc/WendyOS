package commands

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubDockerDaemonSeams saves and restores every seam ensureDockerDaemon uses,
// plus the probe bound, and shrinks the bound to d.
func stubDockerDaemonSeams(t *testing.T, d time.Duration) {
	t.Helper()
	oldRuntimes, oldWinRuntimes := darwinDockerRuntimes, windowsDockerRuntimes
	oldLookPath := dockerLookPathFn
	oldVersionOK := dockerVersionOKFn
	oldOpenRuntime := dockerOpenRuntimeFn
	oldInstallRuntime := dockerInstallRuntimeFn
	oldInteractive := isInteractiveTerminalFn
	oldTimeout := dockerVersionProbeTimeout
	oldPoll, oldWait := dockerDaemonPollInterval, dockerDaemonReadyWait
	t.Cleanup(func() {
		darwinDockerRuntimes, windowsDockerRuntimes = oldRuntimes, oldWinRuntimes
		dockerLookPathFn = oldLookPath
		dockerVersionOKFn = oldVersionOK
		dockerOpenRuntimeFn = oldOpenRuntime
		dockerInstallRuntimeFn = oldInstallRuntime
		isInteractiveTerminalFn = oldInteractive
		dockerVersionProbeTimeout = oldTimeout
		dockerDaemonPollInterval, dockerDaemonReadyWait = oldPoll, oldWait
	})
	dockerVersionProbeTimeout = d
	dockerDaemonPollInterval = 10 * time.Millisecond
	dockerDaemonReadyWait = 3 * d
}

// stubHungDaemonHost sets up a host whose docker CLI and runtime app are
// installed and whose daemon never answers until answerAfter probes (never
// when answerAfter < 0). Launching, installing and prompting fail the test:
// the runtime is already running, just slow or wedged.
func stubHungDaemonHost(t *testing.T, answerAfter int) {
	t.Helper()
	appPath := filepath.Join(t.TempDir(), "Docker.app")
	if err := os.MkdirAll(appPath, 0o755); err != nil {
		t.Fatal(err)
	}
	darwinDockerRuntimes = []dockerRuntime{{name: "Docker Desktop", app: appPath}}
	windowsDockerRuntimes = []dockerRuntime{{name: "Docker Desktop", app: appPath}}
	dockerLookPathFn = func(string) (string, error) { return "/usr/local/bin/docker", nil }
	var mu sync.Mutex
	probes := 0
	dockerVersionOKFn = func(ctx context.Context) bool {
		mu.Lock()
		probes++
		n := probes
		mu.Unlock()
		if answerAfter >= 0 && n > answerAfter {
			return true
		}
		<-ctx.Done() // no answer
		return false
	}
	dockerOpenRuntimeFn = func(context.Context, string) error {
		t.Fatal("must not open a runtime whose daemon is running but slow or hung")
		return nil
	}
	dockerInstallRuntimeFn = func(context.Context) error {
		t.Fatal("must not install anything")
		return nil
	}
	isInteractiveTerminalFn = func() bool { return true }
	stubConfirmFn(t, func(q string) bool {
		t.Fatalf("must not prompt (%q) for a daemon that is running but not answering", q)
		return false
	})
}

// A wedged Docker daemon (the socket accepts the connection but never
// answers) used to stall `wendy run`/`build` forever: the `docker version`
// probe ran under the command's context, which has no deadline. With no one
// at the terminal it must now fail fast with an error that says Docker isn't
// responding, and never offer to open or install a runtime that is running.
func TestEnsureDockerDaemon_HungDaemonWithNoOneThereFailsWithClearError(t *testing.T) {
	for _, hostOS := range []dockerHostOS{dockerHostOSDarwin, dockerHostOSWindows, "linux"} {
		t.Run(string(hostOS), func(t *testing.T) {
			stubDockerDaemonSeams(t, 100*time.Millisecond)
			stubHungDaemonHost(t, -1)
			stubHumanPresent(t, false)

			start := time.Now()
			err := ensureDockerDaemonForHostOS(context.Background(), hostOS)
			if err == nil || !strings.Contains(err.Error(), "not responding") {
				t.Fatalf("err = %v, want an error saying the Docker daemon is not responding", err)
			}
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Fatalf("took %s; want the probe bounded by dockerVersionProbeTimeout", elapsed)
			}
		})
	}
}

// With a person at the terminal, a daemon that does not answer the first
// probe is usually one that is starting (Docker Desktop just launched,
// resuming from sleep, Resource Saver). As before the probe was bounded, it
// is waited for — without launching anything.
func TestEnsureDockerDaemon_StartingDaemonIsWaitedForWithAPersonThere(t *testing.T) {
	for _, hostOS := range []dockerHostOS{dockerHostOSDarwin, dockerHostOSWindows, "linux"} {
		t.Run(string(hostOS), func(t *testing.T) {
			stubDockerDaemonSeams(t, 100*time.Millisecond)
			stubHungDaemonHost(t, 2) // answers from the third probe on
			stubHumanPresent(t, true)

			if err := ensureDockerDaemonForHostOS(context.Background(), hostOS); err != nil {
				t.Fatalf("ensureDockerDaemonForHostOS: %v, want it to wait for the starting daemon", err)
			}
		})
	}
}

// ...but not forever: a daemon that never answers still ends in a clear error.
func TestEnsureDockerDaemon_HungDaemonWithAPersonThereGivesUp(t *testing.T) {
	stubDockerDaemonSeams(t, 100*time.Millisecond)
	stubHungDaemonHost(t, -1)
	stubHumanPresent(t, true)

	start := time.Now()
	err := ensureDockerDaemonForHostOS(context.Background(), "linux")
	if err == nil || !strings.Contains(err.Error(), "not responding") {
		t.Fatalf("err = %v, want an error saying the Docker daemon is not responding", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %s; want the wait bounded by dockerDaemonReadyWait", elapsed)
	}
}

func TestDockerDaemonReady(t *testing.T) {
	cases := []struct {
		name      string
		probe     func(ctx context.Context) bool
		cancelCtx bool
		wantReady bool
		wantHung  bool
		wantErr   error
	}{
		{name: "answers", probe: func(context.Context) bool { return true }, wantReady: true},
		{name: "slow but healthy", probe: func(context.Context) bool { time.Sleep(20 * time.Millisecond); return true }, wantReady: true},
		{name: "not running fails fast", probe: func(context.Context) bool { return false }},
		{name: "hung", probe: func(ctx context.Context) bool { <-ctx.Done(); return false }, wantHung: true},
		// Ctrl-C while probing is the user stopping, not a hung daemon.
		{name: "caller cancelled", probe: func(ctx context.Context) bool { <-ctx.Done(); return false }, cancelCtx: true, wantErr: context.Canceled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubDockerDaemonSeams(t, 200*time.Millisecond)
			dockerVersionOKFn = tc.probe
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelCtx {
				time.AfterFunc(20*time.Millisecond, cancel)
			}
			ready, err := dockerDaemonReady(ctx)
			if ready != tc.wantReady {
				t.Errorf("ready = %v, want %v", ready, tc.wantReady)
			}
			if gotHung := err != nil && strings.Contains(err.Error(), "not responding"); gotHung != tc.wantHung {
				t.Errorf("err = %v, want not-responding error: %v", err, tc.wantHung)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
			if !tc.wantHung && tc.wantErr == nil && err != nil {
				t.Errorf("err = %v, want nil", err)
			}
		})
	}
}

// The real probe (`docker version` via exec) is bounded too: a docker CLI
// that never returns is killed at the deadline.
func TestDockerDaemonReady_HungCLIIsKilled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake docker is a shell script")
	}
	stubDockerDaemonSeams(t, 200*time.Millisecond)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nexec /bin/sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	start := time.Now()
	ready, err := dockerDaemonReady(context.Background())
	if ready || err == nil || !strings.Contains(err.Error(), "not responding") {
		t.Fatalf("dockerDaemonReady = %v, %v; want a not-responding error", ready, err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %s; want the hung docker CLI killed at the probe deadline", elapsed)
	}
}
