//go:build unix

package services

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func withHelperScript(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcu-flash")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := mcuHelperPath
	t.Cleanup(func() { mcuHelperPath = orig })
	mcuHelperPath = path
}

func TestRunMCUHelperStreamsOutputAndExitCode(t *testing.T) {
	withHelperScript(t, "echo out; echo err >&2; exit 4\n")
	r, wait, err := runMCUHelper(context.Background(), "zephyr-1")
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(r)
	var exitErr *exec.ExitError
	if err := wait(); !errors.As(err, &exitErr) || exitErr.ExitCode() != 4 {
		t.Fatalf("wait() = %v, want exit status 4", err)
	}
	if got := string(out); !strings.Contains(got, "out") || !strings.Contains(got, "err") {
		t.Fatalf("output = %q, want stdout and stderr", got)
	}
}

func TestRunMCUHelperTimeoutKillsChildren(t *testing.T) {
	withHelperScript(t, "sleep 30 &\nwait\n")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	r, wait, err := runMCUHelper(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.ReadAll(r)
		_ = wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("helper output stayed open after the timeout")
	}
}

func resetMCULayouts(t *testing.T) {
	t.Helper()
	mcuLayoutsKnown, mcuLayoutsCached = false, nil
	t.Cleanup(func() { mcuLayoutsKnown, mcuLayoutsCached = false, nil })
}

func TestLoadMCULayoutsCachesAnUnparsableAnswer(t *testing.T) {
	withHelperScript(t, "echo garbage\n")
	resetMCULayouts(t)
	if got := loadMCULayouts(); got != nil {
		t.Fatalf("layouts from garbage = %v, want none", got)
	}
	withHelperScript(t, `[ "$1" = --layouts ] && echo "`+strings.TrimSpace(testLayoutsLine)+`"`+"\n")
	if got := loadMCULayouts(); got != nil {
		t.Fatalf("layouts = %v after a final answer, want it cached", got)
	}
}

func TestLoadMCULayoutsRetriesAFailedProbe(t *testing.T) {
	withHelperScript(t, "exit 1\n")
	resetMCULayouts(t)
	if got := loadMCULayouts(); got != nil {
		t.Fatalf("layouts after a failed probe = %v, want none", got)
	}
	withHelperScript(t, `[ "$1" = --layouts ] && echo "`+strings.TrimSpace(testLayoutsLine)+`"`+"\n")
	if got := loadMCULayouts(); len(got) != 1 || got[0].Name != "zephyr-1" {
		t.Fatalf("layouts after the helper recovered = %v, want zephyr-1", got)
	}
}

func TestDetectFeaturesetAdvertisesMCULayouts(t *testing.T) {
	withHelperScript(t, `[ "$1" = --layouts ] && echo "`+strings.TrimSpace(testLayoutsLine)+`"`+"\n")
	resetMCULayouts(t)
	if got := detectFeatureset(); !slices.Contains(got, "mcu-flash:zephyr-1") {
		t.Fatalf("featureset %v lacks mcu-flash:zephyr-1", got)
	}
}

func TestDetectFeaturesetWithoutMCUHelper(t *testing.T) {
	orig := mcuHelperPath
	t.Cleanup(func() { mcuHelperPath = orig })
	mcuHelperPath = filepath.Join(t.TempDir(), "absent")
	resetMCULayouts(t)
	for _, f := range detectFeatureset() {
		if strings.HasPrefix(f, "mcu-flash:") {
			t.Fatalf("featureset advertises %s without a helper", f)
		}
	}
}
