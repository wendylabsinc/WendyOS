package swifttoolchain

import (
	"context"
	"io"
	"os/exec"
	"strings"
	"testing"
)

func TestEnsureSwiftVersion_AlreadyInstalled(t *testing.T) {
	original := execCommandContext
	t.Cleanup(func() { execCommandContext = original })

	var calls [][]string
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls = append(calls, append([]string{name}, args...))
		return exec.CommandContext(ctx, "true")
	}

	if err := EnsureSwiftVersion(context.Background(), io.Discard, io.Discard); err != nil {
		t.Fatalf("EnsureSwiftVersion() unexpected error: %v", err)
	}

	if len(calls) != 1 {
		t.Fatalf("expected 1 call (which), got %d: %v", len(calls), calls)
	}
	if calls[0][0] != "swiftly" || calls[0][1] != "which" || calls[0][2] != DefaultVersion {
		t.Errorf("expected [swiftly which %s], got %v", DefaultVersion, calls[0])
	}
}

func TestEnsureSwiftVersion_InstallsWhenMissing(t *testing.T) {
	original := execCommandContext
	t.Cleanup(func() { execCommandContext = original })

	var calls [][]string
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		call := append([]string{name}, args...)
		calls = append(calls, call)
		if len(args) > 0 && args[0] == "which" {
			return exec.CommandContext(ctx, "false")
		}
		return exec.CommandContext(ctx, "true")
	}

	if err := EnsureSwiftVersion(context.Background(), io.Discard, io.Discard); err != nil {
		t.Fatalf("EnsureSwiftVersion() unexpected error: %v", err)
	}

	if len(calls) != 2 {
		t.Fatalf("expected 2 calls (which + install), got %d: %v", len(calls), calls)
	}
	if calls[0][1] != "which" {
		t.Errorf("first call should be which, got %v", calls[0])
	}
	if calls[1][1] != "install" || calls[1][2] != DefaultVersion {
		t.Errorf("expected [swiftly install %s], got %v", DefaultVersion, calls[1])
	}
}

func TestEnsureSwiftVersion_SwiftlyNotFound(t *testing.T) {
	original := execCommandContext
	t.Cleanup(func() { execCommandContext = original })

	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "nonexistent-binary-that-does-not-exist")
	}

	err := EnsureSwiftVersion(context.Background(), io.Discard, io.Discard)
	if err == nil {
		t.Fatal("EnsureSwiftVersion() expected error when swiftly not found, got nil")
	}
	if !strings.Contains(err.Error(), "swiftly is required but not installed") {
		t.Errorf("expected actionable error message, got: %v", err)
	}
}

func TestEnsureSwiftVersion_InstallFails(t *testing.T) {
	original := execCommandContext
	t.Cleanup(func() { execCommandContext = original })

	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "false")
	}

	err := EnsureSwiftVersion(context.Background(), io.Discard, io.Discard)
	if err == nil {
		t.Fatal("EnsureSwiftVersion() expected error on install failure, got nil")
	}
	if !strings.Contains(err.Error(), "installing Swift") {
		t.Errorf("expected install error message, got: %v", err)
	}
}

func TestEnsureSwiftVersion_Cancellation(t *testing.T) {
	original := execCommandContext
	t.Cleanup(func() { execCommandContext = original })

	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "true")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := EnsureSwiftVersion(ctx, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("EnsureSwiftVersion() expected error on cancelled context, got nil")
	}
}
