//go:build darwin && cgo

package central

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"
)

func TestL2CAPNativeWriteBackpressure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "l2cap-write-test")
	cmd := exec.CommandContext(ctx, "xcrun", "clang", "-g", "-fno-omit-frame-pointer", "-fobjc-arc", "-fblocks", "-UNDEBUG", "-framework", "Foundation", "-framework", "CoreBluetooth",
		"testdata/l2cap_write_test.m", "-o", binary)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile native BLE regression: %v\n%s", err, output)
	}
	nativeCmd := exec.CommandContext(ctx, binary)
	started := time.Now()
	if output, err := nativeCmd.CombinedOutput(); err != nil {
		// Keep the original failure even if a diagnostic replay succeeds. A
		// debugger can expose macOS/libdispatch traps that otherwise produce no
		// stderr; its separate budget cannot extend the actual write budgets.
		t.Logf("original native BLE failure: %v\n%s", err, output)
		if nativeCmd.Process != nil {
			logNativeBLECrashReport(t, nativeCmd.Process.Pid, started)
		}
		if lldb, lookupErr := exec.LookPath("lldb"); lookupErr == nil {
			diagnosticCtx, diagnosticCancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer diagnosticCancel()
			diagnosticCmd := exec.CommandContext(diagnosticCtx, lldb, "--no-lldbinit", "--batch",
				"-o", "run", "-k", "thread backtrace all", "-k", "register read", "-k", "image list",
				"--", binary)
			diagnosticCmd.WaitDelay = time.Second // bound inherited debugger/child output pipes
			diagnostics, diagnosticErr := diagnosticCmd.CombinedOutput()
			t.Logf("native BLE diagnostic replay (original failure retained): %v\n%s", diagnosticErr, diagnostics)
		} else {
			t.Logf("native BLE diagnostic debugger unavailable: %v", lookupErr)
		}
		t.Fatalf("native BLE regression: %v\n%s", err, output)
	}
}

func logNativeBLECrashReport(t *testing.T, pid int, started time.Time) {
	t.Helper()
	home, _ := os.UserHomeDir()
	directories := []string{filepath.Join(home, "Library/Logs/DiagnosticReports"), "/Library/Logs/DiagnosticReports"}
	process := regexp.MustCompile(`"pid"\s*:\s*` + strconv.Itoa(pid) + `\b`)
	// ReportCrash publishes asynchronously. Inspect only this invocation's
	// reports; other native tests may run concurrently on the same runner.
	deadline := time.Now().Add(3 * time.Second)
	for {
		for _, directory := range directories {
			paths, _ := filepath.Glob(filepath.Join(directory, "l2cap-write-test*"))
			for _, path := range paths {
				info, err := os.Stat(path)
				if err != nil || !info.Mode().IsRegular() || info.ModTime().Before(started) || info.Size() > 2<<20 {
					continue
				}
				report, err := os.ReadFile(path)
				if err != nil || !process.Match(report) {
					continue
				}
				if len(report) > 200<<10 {
					report = append(report[:200<<10], []byte("\n[crash report truncated]\n")...)
				}
				t.Logf("original native BLE crash report %s (pid %d):\n%s", path, pid, report)
				return
			}
		}
		if time.Now().After(deadline) {
			t.Logf("no native BLE DiagnosticReports record available for pid %d", pid)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
