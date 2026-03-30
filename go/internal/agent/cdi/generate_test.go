package cdi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestEnsureNVIDIACDISpec_SkipsWhenSpecExists(t *testing.T) {
	// Create a temp dir with an existing spec file.
	tmpDir := t.TempDir()
	existingSpec := filepath.Join(tmpDir, "nvidia.yaml")
	if err := os.WriteFile(existingSpec, []byte("cdiVersion: 0.5.0"), 0644); err != nil {
		t.Fatal(err)
	}

	core, logs := observer.New(zap.DebugLevel)
	logger := zap.New(core)

	commandCalled := false
	mockRun := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		commandCalled = true
		return nil, nil
	}

	ensureNVIDIACDISpecInternal(logger, []string{existingSpec}, tmpDir, filepath.Join(tmpDir, "out.yaml"),
		func() bool { return true },
		func(string) (string, error) { return "/usr/bin/nvidia-ctk", nil },
		mockRun,
	)

	if commandCalled {
		t.Error("expected nvidia-ctk to NOT be called when spec already exists")
	}

	if logs.Len() == 0 {
		t.Error("expected at least one log entry")
	}
	if logs.All()[0].Message != "NVIDIA CDI spec already exists, skipping generation" {
		t.Errorf("unexpected log message: %s", logs.All()[0].Message)
	}
}

func TestEnsureNVIDIACDISpec_SkipsWhenNoHardware(t *testing.T) {
	tmpDir := t.TempDir()
	outputPath := filepath.Join(tmpDir, "nvidia.yaml")

	core, logs := observer.New(zap.DebugLevel)
	logger := zap.New(core)

	commandCalled := false
	mockRun := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		commandCalled = true
		return nil, nil
	}

	ensureNVIDIACDISpecInternal(logger,
		[]string{filepath.Join(tmpDir, "nonexistent.yaml")},
		tmpDir, outputPath,
		func() bool { return false },
		func(string) (string, error) { return "/usr/bin/nvidia-ctk", nil },
		mockRun,
	)

	if commandCalled {
		t.Error("expected nvidia-ctk to NOT be called when no hardware detected")
	}

	found := false
	for _, entry := range logs.All() {
		if entry.Message == "No NVIDIA hardware detected, skipping CDI spec generation" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected debug log about no NVIDIA hardware")
	}
}

func TestEnsureNVIDIACDISpec_SkipsWhenNoCTK(t *testing.T) {
	tmpDir := t.TempDir()
	outputPath := filepath.Join(tmpDir, "nvidia.yaml")

	core, logs := observer.New(zap.DebugLevel)
	logger := zap.New(core)

	commandCalled := false
	mockRun := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		commandCalled = true
		return nil, nil
	}

	ensureNVIDIACDISpecInternal(logger,
		[]string{filepath.Join(tmpDir, "nonexistent.yaml")},
		tmpDir, outputPath,
		func() bool { return true },
		func(string) (string, error) { return "", errors.New("not found") },
		mockRun,
	)

	if commandCalled {
		t.Error("expected nvidia-ctk to NOT be called when it's not in PATH")
	}

	found := false
	for _, entry := range logs.All() {
		if entry.Message == "nvidia-ctk not found in PATH, cannot generate CDI spec (GPU containers will use minimal device mounting)" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected warning log about missing nvidia-ctk")
	}
}

func TestEnsureNVIDIACDISpec_CallsCTKWithCorrectArgs(t *testing.T) {
	tmpDir := t.TempDir()
	outputPath := filepath.Join(tmpDir, "nvidia.yaml")

	core, _ := observer.New(zap.DebugLevel)
	logger := zap.New(core)

	var capturedName string
	var capturedArgs []string
	mockRun := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		capturedName = name
		capturedArgs = args
		return []byte("generated"), nil
	}

	ensureNVIDIACDISpecInternal(logger,
		[]string{filepath.Join(tmpDir, "nonexistent.yaml")},
		tmpDir, outputPath,
		func() bool { return true },
		func(string) (string, error) { return "/usr/bin/nvidia-ctk", nil },
		mockRun,
	)

	if capturedName != "/usr/bin/nvidia-ctk" {
		t.Errorf("expected command /usr/bin/nvidia-ctk, got %s", capturedName)
	}

	expectedArgs := []string{"cdi", "generate", "--output=" + outputPath}
	if len(capturedArgs) != len(expectedArgs) {
		t.Fatalf("expected %d args, got %d: %v", len(expectedArgs), len(capturedArgs), capturedArgs)
	}
	for i, arg := range expectedArgs {
		if capturedArgs[i] != arg {
			t.Errorf("arg[%d]: expected %q, got %q", i, arg, capturedArgs[i])
		}
	}
}

func TestEnsureNVIDIACDISpec_HandlesCommandFailure(t *testing.T) {
	tmpDir := t.TempDir()
	outputPath := filepath.Join(tmpDir, "nvidia.yaml")

	core, logs := observer.New(zap.DebugLevel)
	logger := zap.New(core)

	mockRun := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte("error: no GPU found"), errors.New("exit status 1")
	}

	ensureNVIDIACDISpecInternal(logger,
		[]string{filepath.Join(tmpDir, "nonexistent.yaml")},
		tmpDir, outputPath,
		func() bool { return true },
		func(string) (string, error) { return "/usr/bin/nvidia-ctk", nil },
		mockRun,
	)

	// Should log a warning, not panic.
	found := false
	for _, entry := range logs.All() {
		if entry.Message == "Failed to generate NVIDIA CDI spec" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected warning log about failed CDI spec generation")
	}
}

func TestHasNVIDIAHardware_NoHardware(t *testing.T) {
	// On macOS dev machines, this should return false.
	// We just verify it doesn't panic.
	_ = hasNVIDIAHardware()
}
