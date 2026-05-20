package swifttoolchain

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// testFileInfo is a minimal os.FileInfo implementation for statFile mocks.
type testFileInfo struct{ mode os.FileMode }

func (f testFileInfo) Name() string       { return "brew" }
func (f testFileInfo) Size() int64        { return 0 }
func (f testFileInfo) Mode() os.FileMode  { return f.mode }
func (f testFileInfo) ModTime() time.Time { return time.Time{} }
func (f testFileInfo) IsDir() bool        { return false }
func (f testFileInfo) Sys() any           { return nil }

// brewExists returns a statFile that reports the given paths as non-world-writable (mode 0755).
func brewExists(paths ...string) func(string) (os.FileInfo, error) {
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		set[p] = true
	}
	return func(name string) (os.FileInfo, error) {
		if set[name] {
			return testFileInfo{mode: 0755}, nil
		}
		return nil, fmt.Errorf("not found")
	}
}

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
	origExec := execCommandContext
	origStat := statFile
	t.Cleanup(func() {
		execCommandContext = origExec
		statFile = origStat
	})

	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "nonexistent-binary-that-does-not-exist")
	}
	statFile = func(name string) (os.FileInfo, error) {
		return nil, fmt.Errorf("not found")
	}

	err := EnsureSwiftVersion(context.Background(), io.Discard, io.Discard)
	if err == nil {
		t.Fatal("EnsureSwiftVersion() expected error when swiftly not found, got nil")
	}
	if !strings.Contains(err.Error(), "swiftly is required but not installed") {
		t.Errorf("expected actionable error message, got: %v", err)
	}
}

func TestEnsureSwiftVersion_SwiftlyNotFound_BrewConfirmed(t *testing.T) {
	origExec := execCommandContext
	origStat := statFile
	origConfirm := confirmFunc
	origOS := currentOS
	t.Cleanup(func() {
		execCommandContext = origExec
		statFile = origStat
		confirmFunc = origConfirm
		currentOS = origOS
	})

	currentOS = "darwin"
	statFile = brewExists("/opt/homebrew/bin/brew")
	confirmFunc = func(question string) (bool, error) { return true, nil }

	const fakeBrew = "/opt/homebrew/bin/brew"
	var calls [][]string
	whichCallCount := 0
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		call := append([]string{name}, args...)
		calls = append(calls, call)
		switch {
		case name == "swiftly" && len(args) > 0 && args[0] == "which":
			whichCallCount++
			if whichCallCount == 1 {
				// first call: swiftly binary not in PATH at all
				return exec.CommandContext(ctx, "nonexistent-binary-that-does-not-exist")
			}
			// second call (post-brew): binary exists but version not installed yet
			return exec.CommandContext(ctx, "false")
		case name == fakeBrew:
			return exec.CommandContext(ctx, "true") // brew install succeeds
		default:
			return exec.CommandContext(ctx, "true")
		}
	}

	if err := EnsureSwiftVersion(context.Background(), io.Discard, io.Discard); err != nil {
		t.Fatalf("EnsureSwiftVersion() unexpected error: %v", err)
	}

	// Expect: swiftly which, /opt/homebrew/bin/brew install swiftly, swiftly which (retry), swiftly install
	brewCall := false
	installCall := false
	for _, c := range calls {
		if c[0] == fakeBrew && len(c) >= 3 && c[1] == "install" && c[2] == brewFormula {
			brewCall = true
		}
		if c[0] == "swiftly" && len(c) >= 2 && c[1] == "install" {
			installCall = true
		}
	}
	if !brewCall {
		t.Errorf("expected %s install swiftly call, calls: %v", fakeBrew, calls)
	}
	if !installCall {
		t.Errorf("expected swiftly install call after brew, calls: %v", calls)
	}
}

func TestEnsureSwiftVersion_SwiftlyNotFound_BrewDeclined(t *testing.T) {
	origExec := execCommandContext
	origStat := statFile
	origConfirm := confirmFunc
	origOS := currentOS
	t.Cleanup(func() {
		execCommandContext = origExec
		statFile = origStat
		confirmFunc = origConfirm
		currentOS = origOS
	})

	currentOS = "darwin"
	statFile = brewExists("/opt/homebrew/bin/brew")
	confirmFunc = func(question string) (bool, error) { return false, nil }

	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "nonexistent-binary-that-does-not-exist")
	}

	err := EnsureSwiftVersion(context.Background(), io.Discard, io.Discard)
	if err == nil {
		t.Fatal("EnsureSwiftVersion() expected error when brew declined, got nil")
	}
	if !strings.Contains(err.Error(), "swiftly is required but not installed") {
		t.Errorf("expected actionable error message, got: %v", err)
	}
}

func TestEnsureSwiftVersion_SwiftlyNotFound_BrewFails(t *testing.T) {
	origExec := execCommandContext
	origStat := statFile
	origConfirm := confirmFunc
	origOS := currentOS
	t.Cleanup(func() {
		execCommandContext = origExec
		statFile = origStat
		confirmFunc = origConfirm
		currentOS = origOS
	})

	currentOS = "darwin"
	statFile = brewExists("/opt/homebrew/bin/brew")
	confirmFunc = func(question string) (bool, error) { return true, nil }

	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name == "/opt/homebrew/bin/brew" {
			return exec.CommandContext(ctx, "false") // brew install fails
		}
		return exec.CommandContext(ctx, "nonexistent-binary-that-does-not-exist")
	}

	err := EnsureSwiftVersion(context.Background(), io.Discard, io.Discard)
	if err == nil {
		t.Fatal("EnsureSwiftVersion() expected error when brew install fails, got nil")
	}
	if !strings.Contains(err.Error(), "brew install") {
		t.Errorf("expected brew error message, got: %v", err)
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

	installedSDK := fmt.Sprintf("%s-RELEASE_wendyos_aarch64", DefaultVersion)
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
