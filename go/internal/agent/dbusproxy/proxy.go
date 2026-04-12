// Package dbusproxy manages xdg-dbus-proxy processes to provide filtered
// D-Bus access for containers. Each container gets its own proxy socket
// that only allows communication with specified D-Bus services (e.g. org.bluez).
package dbusproxy

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"
)

const (
	baseDir         = "/run/wendy/dbus-proxy"
	hostBusSocket   = "/var/run/dbus/system_bus_socket"
	socketFileName  = "system_bus_socket"
	startupTimeout  = 5 * time.Second
	startupPollWait = 50 * time.Millisecond
)

// proxyProcess tracks a running xdg-dbus-proxy instance.
type proxyProcess struct {
	cmd       *exec.Cmd
	socketDir string
}

// Manager manages xdg-dbus-proxy processes, one per container.
type Manager struct {
	logger    *zap.Logger
	mu        sync.Mutex
	processes map[string]*proxyProcess // keyed by appID
}

// NewManager creates a new proxy manager.
func NewManager(logger *zap.Logger) *Manager {
	return &Manager{
		logger:    logger,
		processes: make(map[string]*proxyProcess),
	}
}

// IsAvailable returns true if xdg-dbus-proxy is installed on the system.
func IsAvailable() bool {
	_, err := exec.LookPath("xdg-dbus-proxy")
	return err == nil
}

// SocketDir returns the proxy socket directory path for a given appID.
// This can be used by callers to determine mount paths without starting a proxy.
func SocketDir(appID string) string {
	return filepath.Join(baseDir, appID)
}

// Start launches an xdg-dbus-proxy process for the given appID. It creates a
// filtered proxy socket at /run/wendy/dbus-proxy/<appID>/system_bus_socket
// that only allows communication with org.bluez.
// Returns the socket directory path to be mounted into the container.
func (m *Manager) Start(ctx context.Context, appID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Stop any existing proxy for this app.
	if existing, ok := m.processes[appID]; ok {
		m.stopLocked(appID, existing)
	}

	socketDir := SocketDir(appID)
	socketPath := filepath.Join(socketDir, socketFileName)

	// Create the socket directory.
	if err := os.MkdirAll(socketDir, 0755); err != nil {
		return "", fmt.Errorf("creating proxy socket directory: %w", err)
	}

	// Launch xdg-dbus-proxy with a filter that only allows org.bluez.
	// Use exec.Command (not CommandContext) so the proxy outlives the
	// request context that started it. Pdeathsig ensures the proxy is
	// cleaned up if the agent process crashes.
	cmd := exec.Command(
		"xdg-dbus-proxy",
		"unix:path="+hostBusSocket,
		socketPath,
		"--filter",
		"--talk=org.bluez",
	)
	setPdeathsig(cmd)

	if err := cmd.Start(); err != nil {
		os.RemoveAll(socketDir)
		return "", fmt.Errorf("starting xdg-dbus-proxy: %w", err)
	}

	// Poll for the socket file to appear.
	deadline := time.Now().Add(startupTimeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		time.Sleep(startupPollWait)
	}

	// Final check.
	if _, err := os.Stat(socketPath); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		os.RemoveAll(socketDir)
		return "", fmt.Errorf("xdg-dbus-proxy socket did not appear within %s", startupTimeout)
	}

	proc := &proxyProcess{
		cmd:       cmd,
		socketDir: socketDir,
	}
	m.processes[appID] = proc

	// Monitor the process in background.
	go func() {
		err := cmd.Wait()
		m.mu.Lock()
		defer m.mu.Unlock()
		// Only log if this process is still the active one for this appID.
		if current, ok := m.processes[appID]; ok && current == proc {
			if err != nil {
				m.logger.Warn("xdg-dbus-proxy exited unexpectedly",
					zap.String("app_id", appID),
					zap.Error(err),
				)
			}
			delete(m.processes, appID)
		}
	}()

	m.logger.Info("Started D-Bus proxy",
		zap.String("app_id", appID),
		zap.String("socket_dir", socketDir),
	)

	return socketDir, nil
}

// Stop terminates the proxy process for the given appID and cleans up its socket directory.
func (m *Manager) Stop(appID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	proc, ok := m.processes[appID]
	if !ok {
		return nil
	}

	m.stopLocked(appID, proc)
	return nil
}

// StopAll terminates all running proxy processes.
func (m *Manager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for appID, proc := range m.processes {
		m.stopLocked(appID, proc)
	}
}

// stopLocked kills the proxy process and removes its socket directory.
// Must be called with m.mu held.
func (m *Manager) stopLocked(appID string, proc *proxyProcess) {
	if proc.cmd.Process != nil {
		_ = proc.cmd.Process.Kill()
		_ = proc.cmd.Wait()
	}
	os.RemoveAll(proc.socketDir)
	delete(m.processes, appID)

	m.logger.Info("Stopped D-Bus proxy", zap.String("app_id", appID))
}
