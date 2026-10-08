package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// On macOS without a TTY, ensureDockerDaemon used to open Docker Desktop (a
// GUI app) on its own and then wait up to 60 s for the daemon.
func TestEnsureDockerDaemon_DarwinNonInteractiveDoesNotLaunchRuntime(t *testing.T) {
	oldRuntimes := darwinDockerRuntimes
	oldLookPath := dockerLookPathFn
	oldVersionOK := dockerVersionOKFn
	oldOpenRuntime := dockerOpenRuntimeFn
	oldInstallRuntime := dockerInstallRuntimeFn
	oldInteractive := isInteractiveTerminalFn
	t.Cleanup(func() {
		darwinDockerRuntimes = oldRuntimes
		dockerLookPathFn = oldLookPath
		dockerVersionOKFn = oldVersionOK
		dockerOpenRuntimeFn = oldOpenRuntime
		dockerInstallRuntimeFn = oldInstallRuntime
		isInteractiveTerminalFn = oldInteractive
	})

	appPath := filepath.Join(t.TempDir(), "Docker.app")
	if err := os.MkdirAll(appPath, 0o755); err != nil {
		t.Fatal(err)
	}
	darwinDockerRuntimes = []dockerRuntime{{name: "Docker Desktop", app: appPath, cliLinkHint: "link Docker Desktop CLI tools"}}
	// The docker CLI is on PATH, but the daemon is down.
	dockerLookPathFn = func(string) (string, error) { return "/usr/local/bin/docker", nil }
	dockerVersionOKFn = func(context.Context) bool { return false }
	dockerOpenRuntimeFn = func(context.Context, string) error {
		t.Fatal("must not open Docker Desktop without an interactive terminal")
		return nil
	}
	dockerInstallRuntimeFn = func(context.Context) error {
		t.Fatal("must not install anything")
		return nil
	}
	isInteractiveTerminalFn = func() bool { return false }
	stubHumanPresent(t, false)

	start := time.Now()
	err := ensureDockerDaemonForHostOS(context.Background(), dockerHostOSDarwin)
	if err == nil {
		t.Fatal("want an error telling the user to start Docker Desktop")
	}
	if !strings.Contains(err.Error(), "start Docker Desktop") {
		t.Fatalf("error = %q, want it to tell the user to start Docker Desktop", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("returned after %s; want an immediate error, not a readiness wait", elapsed)
	}
}

// `wendy run | tee build.log` typed in a terminal: stdout is a pipe, so no
// prompt can be drawn, but a person is there. As before this change, the
// runtime is opened without a prompt and waited for.
func TestEnsureDockerDaemon_DarwinPersonWithPipedOutputOpensRuntime(t *testing.T) {
	stubDockerDaemonSeams(t, time.Second)

	appPath := filepath.Join(t.TempDir(), "Docker.app")
	if err := os.MkdirAll(appPath, 0o755); err != nil {
		t.Fatal(err)
	}
	darwinDockerRuntimes = []dockerRuntime{{name: "Docker Desktop", app: appPath}}
	dockerLookPathFn = func(string) (string, error) { return "/usr/local/bin/docker", nil }
	opened := false
	dockerVersionOKFn = func(context.Context) bool { return opened }
	dockerOpenRuntimeFn = func(context.Context, string) error { opened = true; return nil }
	dockerInstallRuntimeFn = func(context.Context) error {
		t.Fatal("must not install anything")
		return nil
	}
	isInteractiveTerminalFn = func() bool { return false }
	stubHumanPresent(t, true)
	stubConfirmFn(t, func(q string) bool {
		t.Fatalf("must not prompt (%q) when stdout is not a terminal", q)
		return false
	})

	if err := ensureDockerDaemonForHostOS(context.Background(), dockerHostOSDarwin); err != nil {
		t.Fatalf("ensureDockerDaemonForHostOS: %v", err)
	}
	if !opened {
		t.Fatal("want Docker Desktop opened for the person at the terminal")
	}
}
