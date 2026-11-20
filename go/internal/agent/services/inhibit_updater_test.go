package services

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// TestInhibitAutoUpdater asserts inhibit probes for the updater, stops both
// units, and restore re-starts the timer. Why this matters: see inhibitAutoUpdater.
func TestInhibitAutoUpdater(t *testing.T) {
	orig := systemctlFn
	t.Cleanup(func() { systemctlFn = orig })

	var calls [][]string
	systemctlFn = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		return nil, nil
	}

	inhibitAutoUpdater(zap.NewNop())()

	if len(calls) != 4 {
		t.Fatalf("expected cat, is-enabled, stop, start (4 calls), got %d: %v", len(calls), calls)
	}
	if calls[0][0] != "cat" || !strings.Contains(strings.Join(calls[0], " "), "wendyos-agent-updater.timer") {
		t.Fatalf("first call must probe the updater timer, got %v", calls[0])
	}
	calls = calls[1:]
	stop := strings.Join(calls[1], " ")
	if calls[1][0] != "stop" ||
		!strings.Contains(stop, "wendyos-agent-updater.timer") ||
		!strings.Contains(stop, "wendyos-agent-updater.service") {
		t.Fatalf("stop must target the updater timer and service, got %v", calls[1])
	}
	if calls[2][0] != "start" ||
		!strings.Contains(strings.Join(calls[2], " "), "wendyos-agent-updater.timer") {
		t.Fatalf("restore must re-start the updater timer, got %v", calls[2])
	}
}

// Absent updater (Jetson/QEMU): the cat probe fails, so inhibit is a silent
// no-op and restore touches nothing.
func TestInhibitAutoUpdater_NoUpdaterUnit(t *testing.T) {
	orig := systemctlFn
	t.Cleanup(func() { systemctlFn = orig })

	var calls [][]string
	systemctlFn = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		if args[0] == "cat" {
			return []byte("No files found for wendyos-agent-updater.timer."), errors.New("exit status 1")
		}
		return nil, nil
	}

	inhibitAutoUpdater(zap.NewNop())()

	if len(calls) != 1 || calls[0][0] != "cat" {
		t.Fatalf("absent updater must probe once and do nothing else, got %v", calls)
	}
}

// Best-effort: a failing stop must not panic and must still return a working
// restore that attempts to re-start the timer.
func TestInhibitAutoUpdater_StopFailureIsBestEffort(t *testing.T) {
	orig := systemctlFn
	t.Cleanup(func() { systemctlFn = orig })

	var calls [][]string
	systemctlFn = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		if args[0] == "stop" {
			return []byte("boom"), errors.New("exit status 1")
		}
		return nil, nil
	}

	inhibitAutoUpdater(zap.NewNop())() // must not panic

	if len(calls) != 4 || calls[3][0] != "start" {
		t.Fatalf("stop failure must still attempt restore, got %v", calls)
	}
}

// Local agent builds mask the updater: nothing to stop, and re-starting a
// masked timer would only log a failure on every OTA.
func TestInhibitAutoUpdater_MaskedUpdater(t *testing.T) {
	orig := systemctlFn
	t.Cleanup(func() { systemctlFn = orig })

	var calls [][]string
	systemctlFn = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		if args[0] == "is-enabled" {
			return []byte("masked\n"), errors.New("exit status 1")
		}
		return nil, nil
	}

	inhibitAutoUpdater(zap.NewNop())()

	for _, call := range calls {
		if call[0] == "stop" || call[0] == "start" {
			t.Fatalf("a masked updater must not be stopped or started, got %v", calls)
		}
	}
}
