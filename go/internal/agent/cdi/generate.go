package cdi

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"
)

var cdiSpecPaths = []string{
	"/etc/cdi/nvidia.yaml",
	"/var/run/cdi/nvidia.yaml",
}

const cdiOutputDir = "/var/run/cdi"
const cdiOutputPath = "/var/run/cdi/nvidia.yaml"

// EnsureNVIDIACDISpec checks if the NVIDIA CDI spec exists, and if not,
// attempts to generate it by running `nvidia-ctk cdi generate`.
// It is idempotent and non-fatal: errors are logged but do not stop the agent.
func EnsureNVIDIACDISpec(logger *zap.Logger) {
	ensureNVIDIACDISpecInternal(logger, cdiSpecPaths, cdiOutputDir, cdiOutputPath, hasNVIDIAHardware, exec.LookPath, execCommandContext)
}

// execCommandContext is the default implementation that runs a real command.
func execCommandContext(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// ensureNVIDIACDISpecInternal is the testable implementation.
func ensureNVIDIACDISpecInternal(
	logger *zap.Logger,
	specPaths []string,
	outputDir string,
	outputPath string,
	hasHardware func() bool,
	lookPath func(string) (string, error),
	runCommand func(ctx context.Context, name string, args ...string) ([]byte, error),
) {
	// Check if a CDI spec already exists at any known path.
	for _, p := range specPaths {
		if _, err := os.Stat(p); err == nil {
			logger.Debug("NVIDIA CDI spec already exists, skipping generation", zap.String("path", p))
			return
		}
	}

	// Detect NVIDIA hardware.
	if !hasHardware() {
		logger.Debug("No NVIDIA hardware detected, skipping CDI spec generation")
		return
	}

	// Check if nvidia-ctk is available.
	ctkPath, err := lookPath("nvidia-ctk")
	if err != nil {
		logger.Warn("nvidia-ctk not found in PATH, cannot generate CDI spec (GPU containers will use minimal device mounting)",
			zap.Error(err))
		return
	}

	// Create output directory.
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		logger.Warn("Failed to create CDI spec directory", zap.String("dir", outputDir), zap.Error(err))
		return
	}

	// Run nvidia-ctk cdi generate with a 30-second timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	output, err := runCommand(ctx, ctkPath, "cdi", "generate", "--output="+outputPath)
	if err != nil {
		logger.Warn("Failed to generate NVIDIA CDI spec",
			zap.Error(err),
			zap.String("output", string(output)))
		return
	}

	logger.Info("Generated NVIDIA CDI spec", zap.String("path", outputPath))
}

// hasNVIDIAHardware checks for the presence of NVIDIA hardware by looking for
// /dev/nvidia* device nodes or Tegra SoC identification.
func hasNVIDIAHardware() bool {
	// Check for discrete GPU device nodes.
	matches, _ := filepath.Glob("/dev/nvidia*")
	if len(matches) > 0 {
		return true
	}

	// Check for Jetson/Tegra platforms.
	data, err := os.ReadFile("/sys/devices/soc0/family")
	if err == nil && strings.Contains(strings.ToLower(string(data)), "tegra") {
		return true
	}

	return false
}
