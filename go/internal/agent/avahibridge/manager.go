// Package avahibridge gives each opted-in app container its own Avahi mDNS
// stack: a private D-Bus daemon, an xdg-dbus-proxy filtered to
// org.freedesktop.Avahi.*, and an avahi-daemon running inside the
// container's network namespace.
//
// Design notes:
//   - Apps use stock avahi-compat (dns_sd.h) or native Avahi clients
//     against the proxied bus. No special socket handling is required;
//     avahi-daemon multicasts on the app bridge exactly like any other
//     speaker, so the existing catalog bridge picks publications up
//     transparently and answers browses from mesh/LAN records.
//   - The daemon runs in the container netns (nsenter --net) but the host
//     mount namespace, so state paths stay on the host and avahi's own
//     chroot/user drop still apply. It talks to the private bus only.
//   - Spawning waits for an IPv4 address inside the netns first
//     (address-before-daemon); the app itself must still tolerate late
//     addresses per the platform contract.
//   - Fail-closed: any setup failure is an error and the caller must refuse
//     container start. A daemon that dies mid-run is restarted with
//     backoff (up to maxRestarts); then the container keeps running
//     degraded and the failure stays logged.
package avahibridge

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
	// defaultBaseDir holds per-container state: bus socket, proxy socket, configs.
	defaultBaseDir = "/run/wendy/avahi"
	// busSocketName is the private bus endpoint inside each state dir.
	busSocketName = "bus.sock"
	// proxySocketName mirrors the bluetooth convention for the filtered socket.
	proxySocketName = "system_bus_socket"
	// busConfigName is the generated dbus-daemon config.
	busConfigName = "bus.conf"
	// avahiConfigName is the generated avahi-daemon config.
	avahiConfigName = "avahi-daemon.conf"

	proxyStartupTimeout = 5 * time.Second
	proxyPollWait       = 50 * time.Millisecond
	addressWaitTimeout  = 15 * time.Second
	addressPollWait     = 500 * time.Millisecond
	maxRestarts         = 3
	restartBackoff      = 5 * time.Second
)

// avahiBusName is the only D-Bus name the proxy exposes to the app.
const avahiBusName = "org.freedesktop.Avahi"

// IsAvailable reports whether all host binaries the bridge needs exist.
func IsAvailable() bool {
	return len(missingBinaries()) == 0
}

// missingBinaries names which required host binaries are absent, for logs.
func missingBinaries() []string {
	var out []string
	for _, bin := range []string{"dbus-daemon", "avahi-daemon", "xdg-dbus-proxy", "nsenter", "ip"} {
		if _, err := exec.LookPath(bin); err != nil {
			out = append(out, bin)
		}
	}
	return out
}

// StateDir returns the host state directory for a container.
func (m *Manager) stateDir(containerName string) string {
	return filepath.Join(m.baseDir, containerName)
}

// StateDir returns the production host state directory for a container.
func StateDir(containerName string) string {
	return filepath.Join(defaultBaseDir, containerName)
}

// ProxySocketDir returns the host directory to bind-mount at /var/run/dbus.
func (m *Manager) proxyDir(containerName string) string {
	return filepath.Join(m.stateDir(containerName), "proxy")
}

// ProxySocketDir returns the production proxy directory to mount at /var/run/dbus.
func ProxySocketDir(containerName string) string {
	return filepath.Join(StateDir(containerName), "proxy")
}

// busConfig renders a permissive private-bus config: filtering happens at
// the xdg-dbus-proxy, not here.
func busConfig(socketPath string) string {
	return `<busconfig>
  <type>session</type>
  <listen>unix:path=` + socketPath + `</listen>
  <policy context="default">
    <allow own="*"/>
    <allow send_destination="*" eavesdrop="true"/>
    <allow receive_sender="*"/>
  </policy>
</busconfig>
`
}

// avahiConfig renders a per-instance daemon config. disallow-other-stacks
// must stay off: the agent bridge and app libraries share :5353.
func avahiConfig() string {
	return `[server]
disallow-other-stacks=no
ratelimit-interval-usec=1000000
ratelimit-burst=1000
[wide-area]
enable-wide-area=no
[publish]
publish-addresses=yes
publish-hinfo=no
publish-workstation=no
[reflector]
enable-reflector=no
`
}

type childProc struct {
	cmd *exec.Cmd
}

// instance tracks one container's avahi stack.
type instance struct {
	cancel context.CancelFunc
	runCtx context.Context
	wg     sync.WaitGroup
	mu     sync.Mutex
	procs  []*exec.Cmd
}

// Manager tracks one avahi stack per container name.
type Manager struct {
	logger    *zap.Logger
	mu        sync.Mutex
	baseDir   string
	instances map[string]*instance
}

// NewManager creates an avahi bridge manager.
func NewManager(logger *zap.Logger) *Manager {
	return &Manager{logger: logger, baseDir: defaultBaseDir, instances: map[string]*instance{}}
}

// NewManagerInDir creates a manager rooted at dir (tests).
func NewManagerInDir(logger *zap.Logger, dir string) *Manager {
	return &Manager{logger: logger, baseDir: dir, instances: map[string]*instance{}}
}

// track registers a child process for teardown with the instance.
func (m *Manager) track(inst *instance, cmd *exec.Cmd) {
	inst.mu.Lock()
	inst.procs = append(inst.procs, cmd)
	inst.mu.Unlock()
}

// Prepare starts the private bus and filtered proxy for a container. It
// returns the proxy socket directory to mount at /var/run/dbus, before the
// container spec is created (mounts are spec-time). The daemon itself starts
// later via StartDaemon once the network namespace exists. Fail-closed: any
// error means the caller must refuse container start.
func (m *Manager) Prepare(ctx context.Context, containerName string) (string, error) {
	if missing := missingBinaries(); len(missing) > 0 {
		return "", fmt.Errorf("avahi bridge unavailable, missing: %v", missing)
	}
	m.mu.Lock()
	if _, exists := m.instances[containerName]; exists {
		dir := m.proxyDir(containerName)
		m.mu.Unlock()
		return dir, nil
	}
	runCtx, cancel := context.WithCancel(context.Background())
	inst := &instance{cancel: cancel, runCtx: runCtx}
	m.instances[containerName] = inst
	m.mu.Unlock()
	failed := true
	defer func() {
		if failed {
			m.stopInstance(containerName, inst)
		}
	}()

	stateDir := m.stateDir(containerName)
	proxyDir := m.proxyDir(containerName)
	if err := os.MkdirAll(proxyDir, 0755); err != nil {
		return "", fmt.Errorf("avahi state dir: %w", err)
	}
	busSocket := filepath.Join(stateDir, busSocketName)
	if err := os.WriteFile(filepath.Join(stateDir, busConfigName), []byte(busConfig(busSocket)), 0644); err != nil {
		return "", fmt.Errorf("avahi bus config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, avahiConfigName), []byte(avahiConfig()), 0644); err != nil {
		return "", fmt.Errorf("avahi daemon config: %w", err)
	}

	// Private bus, foreground so the agent holds the handle. polkit-style
	// system-bus defaults are avoided: this bus is throwaway and filtered.
	busCmd := exec.CommandContext(runCtx, "dbus-daemon",
		"--config-file="+filepath.Join(stateDir, busConfigName),
		"--nopidfile")
	busCmd.Stdout, busCmd.Stderr = nil, nil
	if err := busCmd.Start(); err != nil {
		return "", fmt.Errorf("avahi private bus start: %w", err)
	}
	m.track(inst, busCmd)
	if err := waitForSocket(runCtx, busSocket, proxyStartupTimeout); err != nil {
		return "", fmt.Errorf("avahi private bus socket: %w", err)
	}
	// The avahi user (which the daemon drops to) must traverse and use it.
	_ = os.Chmod(busSocket, 0777)

	proxyCmd := exec.CommandContext(runCtx, "xdg-dbus-proxy",
		"unix:path="+busSocket,
		filepath.Join(proxyDir, proxySocketName),
		"--filter",
		"--talk="+avahiBusName,
		"--see="+avahiBusName,
	)
	proxyCmd.Stdout, proxyCmd.Stderr = nil, nil
	if err := proxyCmd.Start(); err != nil {
		return "", fmt.Errorf("avahi dbus proxy start: %w", err)
	}
	m.track(inst, proxyCmd)
	if err := waitForSocket(runCtx, filepath.Join(proxyDir, proxySocketName), proxyStartupTimeout); err != nil {
		return "", fmt.Errorf("avahi proxy socket: %w", err)
	}

	failed = false
	return proxyDir, nil
}

// StartDaemon waits for an IPv4 address inside netnsPath and then launches
// the supervised per-container avahi-daemon. Call after the container
// network is configured; fail-closed like Prepare.
func (m *Manager) StartDaemon(ctx context.Context, containerName, netnsPath string, addrOK func(context.Context) (bool, error)) error {
	m.mu.Lock()
	inst, ok := m.instances[containerName]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("avahi bridge not prepared for %q", containerName)
	}
	stateDir := m.stateDir(containerName)
	// Address-before-daemon: the daemon publishes interface addresses, so it
	// must only start once the netns actually has an IPv4.
	addrCtx, addrCancel := context.WithTimeout(ctx, addressWaitTimeout)
	defer addrCancel()
	for {
		ok, err := addrOK(addrCtx)
		if err != nil {
			return fmt.Errorf("avahi netns address check: %w", err)
		}
		if ok {
			break
		}
		select {
		case <-addrCtx.Done():
			return fmt.Errorf("avahi netns has no IPv4 address: %w", addrCtx.Err())
		case <-time.After(addressPollWait):
		}
	}
	inst.wg.Add(1)
	go m.supervise(inst.runCtx, inst, containerName, netnsPath, stateDir)
	return nil
}

// supervise runs avahi-daemon in the container netns, restarting it with
// backoff up to maxRestarts. The daemon self-confines (chroot + user drop);
// it reaches only the private bus via DBUS_SYSTEM_BUS_ADDRESS.
func (m *Manager) supervise(ctx context.Context, inst *instance, containerName, netnsPath, stateDir string) {
	defer inst.wg.Done()
	conf := filepath.Join(stateDir, avahiConfigName)
	bus := filepath.Join(stateDir, busSocketName)
	restarts := 0
	for {
		if ctx.Err() != nil {
			return
		}
		if restarts > maxRestarts {
			m.logger.Error("avahi daemon restart budget exhausted; container keeps running degraded",
				zap.String("container", containerName))
			return
		}
		if restarts > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(restartBackoff):
			}
		}
		cmd := exec.CommandContext(ctx, "nsenter", "--net="+netnsPath,
			"env", "DBUS_SYSTEM_BUS_ADDRESS=unix:path="+bus,
			"avahi-daemon", "-f", conf)
		cmd.Stdout, cmd.Stderr = nil, nil
		m.track(inst, cmd)
		m.logger.Info("avahi daemon starting", zap.String("container", containerName), zap.Int("attempt", restarts+1))
		err := cmd.Run()
		restarts++
		if ctx.Err() != nil {
			return
		}
		m.logger.Warn("avahi daemon exited unexpectedly; restarting",
			zap.String("container", containerName), zap.Error(err))
	}
}

// stopInstance cancels, joins, kills leftovers, and removes state.
func (m *Manager) stopInstance(containerName string, inst *instance) {
	inst.cancel()
	inst.wg.Wait()
	inst.mu.Lock()
	procs := inst.procs
	inst.procs = nil
	inst.mu.Unlock()
	for _, cmd := range procs {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}
	_ = os.RemoveAll(m.stateDir(containerName))
}

// Stop kills a container's avahi stack and removes its state.
func (m *Manager) Stop(containerName string) error {
	m.mu.Lock()
	inst, ok := m.instances[containerName]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	delete(m.instances, containerName)
	m.mu.Unlock()
	m.stopInstance(containerName, inst)
	return nil
}

// StopAll kills every tracked stack (agent shutdown).
func (m *Manager) StopAll() {
	m.mu.Lock()
	names := make([]string, 0, len(m.instances))
	for name := range m.instances {
		names = append(names, name)
	}
	m.mu.Unlock()
	for _, name := range names {
		_ = m.Stop(name)
	}
}

func waitForSocket(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(proxyPollWait):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("socket %s never appeared", path)
		}
	}
}
