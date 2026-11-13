package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
)

// stubWendyOSPreflightBinary records every subprocess invocation without
// accessing firmware or installing an artifact.
func stubWendyOSPreflightBinary(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	trace := filepath.Join(dir, "calls")
	script := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$1\" >> \"$WENDY_TEST_UPDATE_TRACE\"\n" + body
	if err := os.WriteFile(filepath.Join(dir, "wendyos-update"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("WENDY_TEST_UPDATE_TRACE", trace)
	return trace
}

func recordWendyOSPreflightCall(t *testing.T, trace, call string) {
	t.Helper()
	f, err := os.OpenFile(trace, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteString(call + "\n")
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		t.Fatal(err)
	}
}

func assertWendyOSPreflightCalls(t *testing.T, trace, want string) {
	t.Helper()
	got, err := os.ReadFile(trace)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("update calls = %q, want %q", got, want)
	}
}

func TestWendyOSInstallRejectsFailedRootfsPreflight(t *testing.T) {
	trace := stubWendyOSPreflightBinary(t, "exit 0\n")
	w := newWendyOSUpdater(zap.NewNop())
	failure := errors.New("firmware rejected RootfsRedundancyLevel")
	w.prepareRootfs = func(context.Context) error { return failure }

	err := w.install(context.Background(), "https://example.invalid/update.wendy", func(string, int32) {
		recordWendyOSPreflightCall(t, trace, "progress")
	})
	if !errors.Is(err, failure) {
		t.Fatalf("install error = %v, want the rootfs setup failure", err)
	}
	assertWendyOSPreflightCalls(t, trace, "")
}

func TestWendyOSInstallPreparesRootfsBeforeInstall(t *testing.T) {
	trace := stubWendyOSPreflightBinary(t, "exit 0\n")
	w := newWendyOSUpdater(zap.NewNop())
	w.prepareRootfs = func(context.Context) error {
		recordWendyOSPreflightCall(t, trace, "prepare")
		return nil
	}

	if err := w.install(context.Background(), "https://example.invalid/update.wendy", func(string, int32) {
		recordWendyOSPreflightCall(t, trace, "progress")
	}); err != nil {
		t.Fatal(err)
	}
	assertWendyOSPreflightCalls(t, trace, "prepare\nprogress\ninstall\n")
}

func TestWendyOSInstallCancelledBeforeRootfsPreflight(t *testing.T) {
	trace := stubWendyOSPreflightBinary(t, "exit 0\n")
	w := newWendyOSUpdater(zap.NewNop())
	w.prepareRootfs = func(context.Context) error {
		recordWendyOSPreflightCall(t, trace, "prepare")
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := w.install(ctx, "https://example.invalid/update.wendy", func(string, int32) {
		recordWendyOSPreflightCall(t, trace, "progress")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("install error = %v, want context cancellation", err)
	}
	assertWendyOSPreflightCalls(t, trace, "")
}

func TestWendyOSInstallCancelledDuringRootfsPreflight(t *testing.T) {
	trace := stubWendyOSPreflightBinary(t, "exit 0\n")
	w := newWendyOSUpdater(zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.prepareRootfs = func(context.Context) error {
		recordWendyOSPreflightCall(t, trace, "prepare")
		cancel()
		return nil
	}

	err := w.install(ctx, "https://example.invalid/update.wendy", func(string, int32) {
		recordWendyOSPreflightCall(t, trace, "progress")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("install error = %v, want context cancellation", err)
	}
	assertWendyOSPreflightCalls(t, trace, "prepare\n")
}

func TestWendyOSInstallPreparesRootfsOnceAcrossStaleRetry(t *testing.T) {
	trace := stubWendyOSPreflightBinary(t, `
if [ "$1" = install ] && [ ! -f "$WENDY_TEST_UPDATE_TRACE.first-install" ]; then
    : > "$WENDY_TEST_UPDATE_TRACE.first-install"
    printf '%s\n' 'an update is already in flight; run rollback or mark-good first' >&2
    exit 1
fi
exit 0
`)
	w := newWendyOSUpdater(zap.NewNop())
	w.prepareRootfs = func(context.Context) error {
		recordWendyOSPreflightCall(t, trace, "prepare")
		return nil
	}

	if err := w.install(context.Background(), "https://example.invalid/update.wendy", func(string, int32) {}); err != nil {
		t.Fatal(err)
	}
	assertWendyOSPreflightCalls(t, trace, "prepare\ninstall\nrollback\ninstall\n")
}
