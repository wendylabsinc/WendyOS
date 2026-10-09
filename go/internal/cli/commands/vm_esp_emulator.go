package commands

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

const espEmulatorInstallerURL = "https://raw.githubusercontent.com/espressif/esp-emulator/main/install.sh"
const espEmulatorVersion = "0.48.0"

var espEmulatorLookPath = exec.LookPath
var installESPEmulatorFn = installESPEmulator
var espEmulatorDir = func() (string, error) {
	dir, err := config.ConfigDir()
	return filepath.Join(dir, "tools", "esp-emulator"), err
}

func ensureESPEmulator(ctx context.Context) (string, error) {
	return ensureESPEmulatorForHost(ctx, runtime.GOOS, runtime.GOARCH)
}

func ensureESPEmulatorForHost(ctx context.Context, hostOS, arch string) (string, error) {
	if path, err := espEmulatorLookPath("esp-emu"); err == nil {
		return path, nil
	}
	dir, err := espEmulatorDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "esp-emu")
	if resolved, err := espEmulatorLookPath(path); err == nil {
		return resolved, nil
	}
	if !((hostOS == "darwin" && arch == "arm64") || (hostOS == "linux" && (arch == "amd64" || arch == "arm64"))) {
		if hostOS == "windows" {
			return "", fmt.Errorf("ESP32 Emulator has no native Windows release; install Wendy in WSL2 (Ubuntu x86_64 or ARM64), then start the simulator there to install the emulator")
		}
		return "", fmt.Errorf("ESP32 Emulator has no prebuilt release for %s/%s; use Apple Silicon macOS or x86_64/ARM64 Linux (https://github.com/espressif/esp-emulator)", hostOS, arch)
	}
	manual := fmt.Errorf("ESP32 Emulator is required; run interactively to install it, or install manually:\n  curl -fsSL %s | sh -s -- --version %s\nThen add $HOME/.local/bin to PATH and retry", espEmulatorInstallerURL, espEmulatorVersion)
	if jsonOutput || (!isInteractiveTerminalFn() && !vmAssumeYes) {
		return "", manual
	}
	if !vmAssumeYes && !confirmFn("Do you want to install the ESP32 Emulator?") {
		return "", ErrUserCancelled
	}
	if err := installESPEmulatorFn(ctx, dir); err != nil {
		return "", fmt.Errorf("installing ESP32 Emulator: %w", err)
	}
	resolved, err := espEmulatorLookPath(path)
	if err != nil {
		return "", fmt.Errorf("ESP32 Emulator installer finished but %s is not executable: %w", path, err)
	}
	return resolved, nil
}

// Use Espressif's platform-aware installer, installing for this user only.
// The absolute binary path lets the current command continue without a shell
// restart or a process-wide PATH change.
func installESPEmulator(ctx context.Context, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	for _, binary := range []string{"sh", "curl", "tar"} {
		if _, err := exec.LookPath(binary); err != nil {
			return fmt.Errorf("install %s first: %w", binary, err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, espEmulatorInstallerURL, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading Espressif installer: HTTP %d", resp.StatusCode)
	}
	script, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(script) > 1<<20 {
		return fmt.Errorf("Espressif installer exceeds 1 MiB")
	}
	f, err := os.CreateTemp("", "wendy-esp-install-*.sh")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(script); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Installing ESP32 Emulator from Espressif...")
	cmd := exec.CommandContext(ctx, "sh", f.Name(), "--version", espEmulatorVersion, "--bin-dir", dir)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	return cmd.Run()
}
