package camerastream

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

func TestEmulatedSourceRequiresRunBeforeStarting(t *testing.T) {
	var commands []*exec.Cmd
	cfg := helperConfig("stream", &commands)
	cfg.EmulatedSource = "emulator-observer"
	if err := Run(context.Background(), cfg, func([]byte, string, uint64) error { return nil }, nil); err == nil || len(commands) != 0 {
		t.Fatal("emulated source started without simulation provenance")
	}
}

func TestEmulatedSourceHasNoPhysicalCLIArgumentsAndIsReaped(t *testing.T) {
	var commands []*exec.Cmd
	cfg := helperConfig("stream", &commands)
	cfg.EmulatedSource, cfg.SimulationRun = "emulator-observer", "run-a"
	command := cfg.command
	cfg.command = func(ctx context.Context, binary string, args ...string) *exec.Cmd {
		if binary != cfg.EmulatedSource || len(args) != 0 {
			t.Errorf("unexpected emulator invocation: %q %q", binary, args)
		}
		return command(ctx, binary, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stop := errors.New("received complete frame")
	err := Run(ctx, cfg, func([]byte, string, uint64) error { return stop }, nil)
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	assertReaped(t, commands)
}
