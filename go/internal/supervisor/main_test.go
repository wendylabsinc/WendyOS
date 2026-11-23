//go:build linux

package supervisor

import (
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

// childBehaviorEnv names the env var that tells a fake agent (this test binary
// started by the supervisor) what to do; the key is the basename it runs as.
func childBehaviorEnv(exe string) string {
	return "SUPERVISOR_TEST_CHILD_" + strings.ToUpper(strings.ReplaceAll(filepath.Base(exe), "-", "_"))
}

func TestMain(m *testing.M) {
	if os.Getenv("SUPERVISOR_TEST_CHILD") == "1" {
		runFakeAgent()
		return
	}
	os.Exit(m.Run())
}

func runFakeAgent() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		os.Stdout.WriteString("2026.11.02-101010\n")
		os.Exit(0)
	}
	exe, _ := os.Executable()
	switch os.Getenv(childBehaviorEnv(exe)) {
	case "crash":
		os.Exit(1)
	case "late-crash":
		// Passes its checks first, then dies.
		time.Sleep(400 * time.Millisecond)
		os.Exit(1)
	case "confirm-then-clean":
		// Passes its checks once, then every restart exits 0 at once.
		marker := os.Getenv("SUPERVISOR_TEST_MARKER")
		if _, err := os.Stat(marker); err != nil {
			os.WriteFile(marker, nil, 0o644)
			time.Sleep(400 * time.Millisecond)
		}
		os.Exit(0)
	case "restarts-cleanly":
		// A healthy agent that restarts itself, as after a settings change.
		time.Sleep(300 * time.Millisecond)
		os.Exit(0)
	case "crash-once":
		// Dies after its checks the first time, then runs (and, to the probe, may hang).
		marker := os.Getenv("SUPERVISOR_TEST_MARKER")
		if _, err := os.Stat(marker); err != nil {
			os.WriteFile(marker, nil, 0o644)
			time.Sleep(400 * time.Millisecond)
			os.Exit(1)
		}
		waitForTerm()
	case "clean":
		time.Sleep(20 * time.Millisecond)
		os.Exit(0)
	case "setsid-orphan":
		// Leaves a helper in a session of its own, out of reach of its group.
		helper := exec.Command("sleep", "61")
		helper.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := helper.Start(); err == nil {
			os.WriteFile(os.Getenv("SUPERVISOR_TEST_ORPHAN_PID"), []byte(strconv.Itoa(helper.Process.Pid)), 0o644)
		}
		os.Exit(1)
	case "orphan":
		// Leaves a helper behind, as a crashing agent might.
		helper := exec.Command("sleep", "60")
		if err := helper.Start(); err == nil {
			os.WriteFile(os.Getenv("SUPERVISOR_TEST_ORPHAN_PID"), []byte(strconv.Itoa(helper.Process.Pid)), 0o644)
		}
		os.Exit(1)
	default:
		waitForTerm()
	}
}

func waitForTerm() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	select {
	case <-sig:
		os.Exit(0)
	case <-time.After(time.Minute):
		os.Exit(3)
	}
}
