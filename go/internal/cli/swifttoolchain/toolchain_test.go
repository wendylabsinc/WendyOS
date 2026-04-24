package swifttoolchain

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

func TestFindSwiftSDK_AlreadyInstalled(t *testing.T) {
	original := execCommandContext
	t.Cleanup(func() { execCommandContext = original })

	home := t.TempDir()
	originalHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatalf("Setenv HOME: %v", err)
	}
	t.Cleanup(func() { _ = os.Setenv("HOME", originalHome) })

	installedSDK := fmt.Sprintf("%s-RELEASE_wendyos_aarch64", DefaultVersion)
	infoPath := filepath.Join(home, "Library", "org.swift.swiftpm", "swift-sdks", installedSDK+".artifactbundle", "info.json")
	if err := os.MkdirAll(filepath.Dir(infoPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	info := fmt.Sprintf(`{"artifacts":{"%s":{"type":"swiftSDK","variants":[{"path":"%s/aarch64-unknown-linux-gnu"}],"version":"0.0.1"}},"schemaVersion":"1.0"}`,
		installedSDK, installedSDK)
	if err := os.WriteFile(infoPath, []byte(info), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var calls [][]string
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls = append(calls, append([]string{name}, args...))
		return exec.CommandContext(ctx, "echo", installedSDK)
	}

	got, err := FindSwiftSDK(context.Background(), "arm64", io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("FindSwiftSDK() unexpected error: %v", err)
	}
	if got != installedSDK {
		t.Fatalf("expected SDK %q, got %q", installedSDK, got)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 command (sdk list), got %d: %v", len(calls), calls)
	}
	if calls[0][0] != "swiftly" || calls[0][1] != "run" || calls[0][2] != "+"+DefaultVersion || calls[0][3] != "swift" || calls[0][4] != "sdk" || calls[0][5] != "list" {
		t.Fatalf("unexpected command: %v", calls[0])
	}
}

func TestFindSwiftSDK_RejectsWendySDKWithWrongVariant(t *testing.T) {
	original := execCommandContext
	t.Cleanup(func() { execCommandContext = original })

	home := t.TempDir()
	originalHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatalf("Setenv HOME: %v", err)
	}
	t.Cleanup(func() { _ = os.Setenv("HOME", originalHome) })

	installedSDK := fmt.Sprintf("%s-RELEASE_wendyos_aarch64", DefaultVersion)
	infoPath := filepath.Join(home, "Library", "org.swift.swiftpm", "swift-sdks", installedSDK+".artifactbundle", "info.json")
	if err := os.MkdirAll(filepath.Dir(infoPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	info := fmt.Sprintf(`{"artifacts":{"%s":{"type":"swiftSDK","variants":[{"path":"%s/x86_64-unknown-linux-gnu"}],"version":"0.0.1"}},"schemaVersion":"1.0"}`,
		installedSDK, installedSDK)
	if err := os.WriteFile(infoPath, []byte(info), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "echo", installedSDK)
	}

	_, err := FindSwiftSDK(context.Background(), "arm64", io.Discard, io.Discard)
	if err == nil {
		t.Fatal("FindSwiftSDK() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "provides x86_64-unknown-linux-gnu, not aarch64-unknown-linux-gnu") {
		t.Fatalf("unexpected error: %v", err)
	}
}
