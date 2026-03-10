//go:build linux

package bleserver

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"go.uber.org/zap"
)

const (
	// wendyBLEServiceUUID is the BLE service UUID that identifies WendyOS agents.
	wendyBLEServiceUUID = "7565e9eb-4c20-4b67-9272-d708b397b631"
)

// bluetoothctlPaths are common absolute paths for bluetoothctl on Linux.
// Checked in order before falling back to $PATH lookup.
var bluetoothctlPaths = []string{
	"/usr/bin/bluetoothctl",
	"/usr/local/bin/bluetoothctl",
}

// Advertiser manages BLE advertising via a long-running bluetoothctl process.
// The advertisement is registered on bluetoothctl's D-Bus connection and remains
// active only while the process is alive, so we keep it running.
type Advertiser struct {
	logger       *zap.Logger
	bluetoothctl string // resolved absolute path

	mu    sync.Mutex
	stdin io.WriteCloser
	cmd   *exec.Cmd
}

// NewAdvertiser creates a new BLE advertiser.
func NewAdvertiser(logger *zap.Logger) *Advertiser {
	return &Advertiser{logger: logger}
}

// findBluetoothctl resolves the bluetoothctl binary path by checking well-known
// locations first (systemd services often have a minimal $PATH), then falling
// back to $PATH lookup.
func findBluetoothctl() (string, error) {
	for _, p := range bluetoothctlPaths {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return exec.LookPath("bluetoothctl")
}

// Start begins BLE advertising with the WendyOS service UUID.
// It launches a long-running bluetoothctl process that keeps the advertisement
// registered for as long as the process is alive.
// Returns an error if bluetoothctl is not available.
func (a *Advertiser) Start() error {
	path, err := findBluetoothctl()
	if err != nil {
		return fmt.Errorf("bluetoothctl not found: %w", err)
	}
	a.bluetoothctl = path
	a.logger.Debug("Found bluetoothctl", zap.String("path", path))

	// Start a long-running bluetoothctl process.
	cmd := exec.Command(a.bluetoothctl)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("creating stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return fmt.Errorf("creating stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		return fmt.Errorf("starting bluetoothctl: %w", err)
	}

	a.mu.Lock()
	a.cmd = cmd
	a.stdin = stdin
	a.mu.Unlock()

	// Drain stdout in the background to log output and prevent pipe blocking.
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			a.logger.Debug("bluetoothctl", zap.String("output", scanner.Text()))
		}
	}()

	// Resolve hostname for the BLE adapter name. Without this, the adapter
	// uses a default name that gets truncated in the advertising packet
	// (only ~8 bytes fit alongside the 128-bit service UUID).
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "WendyOS"
	}

	// Send the setup commands. Each command needs time for bluetoothctl to
	// process it, so we add small delays between them.
	//
	// Main menu commands:
	//   power on              — ensure adapter is powered
	//   system-alias <name>   — set adapter name to hostname (shown in scan results)
	//
	// NOTE: We intentionally do NOT use "discoverable on" or "pairable on" in
	// the main menu. Those enable BR/EDR (classic Bluetooth) discoverable mode,
	// which causes BlueZ to register a competing advertisement that alternates
	// with ours — making the device flicker in/out of BLE scan results.
	//
	// Advertise submenu:
	//   uuids <uuid>          — include WendyOS service UUID in advertisement
	//   name on               — include local name in scan response data
	//   discoverable on       — set LE discoverable flag in AD data
	//   timeout 0             — keep the advertisement registered forever
	//
	// Finally: register the advertisement.
	commands := []string{
		"power on",
		"system-alias " + hostname,
		"menu advertise",
		"uuids " + wendyBLEServiceUUID,
		"name on",
		"discoverable on",
		"timeout 0",
		"back",
		"advertise on",
	}

	for _, c := range commands {
		if _, err := fmt.Fprintf(stdin, "%s\n", c); err != nil {
			a.logger.Warn("Failed to send bluetoothctl command", zap.String("cmd", c), zap.Error(err))
			a.kill()
			return fmt.Errorf("sending command %q: %w", c, err)
		}
		// Small delay to let bluetoothctl process each command before the next.
		time.Sleep(200 * time.Millisecond)
	}

	a.logger.Info("BLE advertising started", zap.String("uuid", wendyBLEServiceUUID))
	return nil
}

// Stop tears down the BLE advertisement by sending "advertise off" and then
// terminating the long-running bluetoothctl process.
func (a *Advertiser) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.stdin == nil {
		return
	}

	// Best-effort: tell bluetoothctl to stop advertising before exiting.
	fmt.Fprintf(a.stdin, "advertise off\n")
	time.Sleep(200 * time.Millisecond)
	fmt.Fprintf(a.stdin, "quit\n")

	// Wait for process to exit, with a timeout.
	done := make(chan struct{})
	go func() {
		a.cmd.Wait()
		close(done)
	}()

	select {
	case <-done:
		a.logger.Info("BLE advertisement stopped")
	case <-time.After(3 * time.Second):
		a.logger.Warn("bluetoothctl did not exit in time, killing")
		a.cmd.Process.Kill()
	}

	a.stdin = nil
	a.cmd = nil
}

// kill forcefully stops the bluetoothctl process (used on setup failure).
func (a *Advertiser) kill() {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.cmd != nil && a.cmd.Process != nil {
		a.cmd.Process.Kill()
		a.cmd.Wait()
	}
	if a.stdin != nil {
		a.stdin.Close()
	}
	a.stdin = nil
	a.cmd = nil
}
