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
//     mount namespace with private runtime/service mounts, so avahi's own
//     chroot/user drop still apply. It talks to the private bus only.
//   - The caller configures the namespace before spawning the daemon,
//     then releases the application entrypoint only after daemon readiness.
//   - Fail-closed: any setup failure is an error and the caller must refuse
//     container start. A daemon that dies mid-run is restarted with
//     backoff (up to maxRestarts); then the container keeps running
//     degraded and the failure stays logged.
package avahibridge

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
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
	daemonReadyTimeout  = 15 * time.Second
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
	for _, bin := range []string{"dbus-daemon", "avahi-daemon", "xdg-dbus-proxy", "nsenter", "ip", "unshare", "mount", "sh"} {
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
    <allow user="*"/>
    <allow own="*"/>
    <allow send_destination="*" eavesdrop="true"/>
    <allow receive_sender="*"/>
  </policy>
</busconfig>
`
}

// avahiConfig renders a per-instance daemon config. disallow-other-stacks
// must stay off: the agent bridge and app libraries share :5353.
func avahiConfig(containerName string) string {
	host, _ := os.Hostname()
	id := sha256.Sum256([]byte(host + "/" + containerName))
	return fmt.Sprintf(`[server]
host-name=wendy-%x
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
`, id[:8])
}

// instance tracks one container's avahi stack.
type instance struct {
	cancel        context.CancelFunc
	runCtx        context.Context
	wg            sync.WaitGroup
	mu            sync.Mutex
	procs         []*exec.Cmd
	daemonStarted bool
}

// Manager tracks one avahi stack per container name.
type Manager struct {
	logger    *zap.Logger
	mu        sync.Mutex
	baseDir   string
	instances map[string]*instance
	ready     func(context.Context, string) error
}

// NewManager creates an avahi bridge manager.
func NewManager(logger *zap.Logger) *Manager {
	return &Manager{logger: logger, baseDir: defaultBaseDir, instances: map[string]*instance{}, ready: waitForDaemon}
}

// NewManagerInDir creates a manager rooted at dir (tests).
func NewManagerInDir(logger *zap.Logger, dir string) *Manager {
	return &Manager{logger: logger, baseDir: dir, instances: map[string]*instance{}, ready: waitForDaemon}
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
	defer m.mu.Unlock()
	return m.prepareLocked(ctx, containerName)
}

// PrepareForTask retires a previous task's daemon and bus before NewTask
// consumes its mounts. Keeping the directory inode also supports a surviving
// task during agent recovery. A fresh namespace must never reuse the daemon
// that was launched in an exited task's namespace.
func (m *Manager) PrepareForTask(ctx context.Context, containerName string) (string, error) {
	if missing := missingBinaries(); len(missing) > 0 {
		return "", fmt.Errorf("avahi bridge unavailable, missing: %v", missing)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if prior, exists := m.instances[containerName]; exists {
		m.stopInstance(containerName, prior, true)
		delete(m.instances, containerName)
	}
	return m.prepareLocked(ctx, containerName)
}

func (m *Manager) prepareLocked(ctx context.Context, containerName string) (string, error) {
	if prior, exists := m.instances[containerName]; exists && prior.runCtx.Err() != nil {
		m.stopInstance(containerName, prior, true)
		delete(m.instances, containerName)
	}
	if _, exists := m.instances[containerName]; exists {
		dir := m.proxyDir(containerName)
		return dir, nil
	}
	runCtx, cancel := context.WithCancel(context.Background())
	inst := &instance{cancel: cancel, runCtx: runCtx}
	m.instances[containerName] = inst
	failed := true
	defer func() {
		if failed {
			m.stopInstance(containerName, inst, true)
			delete(m.instances, containerName)
		}
	}()

	stateDir := m.stateDir(containerName)
	proxyDir := m.proxyDir(containerName)
	if err := os.MkdirAll(proxyDir, 0755); err != nil {
		return "", fmt.Errorf("avahi state dir: %w", err)
	}
	for _, dir := range []string{"daemon-run", "services"} {
		if err := os.MkdirAll(filepath.Join(stateDir, dir), 0755); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(filepath.Join(stateDir, "hosts"), nil, 0644); err != nil {
		return "", err
	}
	// Retain the mounted proxy directory inode across agent restart.
	_ = os.Remove(filepath.Join(proxyDir, proxySocketName))
	busSocket := filepath.Join(stateDir, busSocketName)
	_ = os.Remove(busSocket)
	_ = os.Remove(filepath.Join(stateDir, "daemon-run", "pid"))
	if err := os.WriteFile(filepath.Join(stateDir, busConfigName), []byte(busConfig(busSocket)), 0644); err != nil {
		return "", fmt.Errorf("avahi bus config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, avahiConfigName), []byte(avahiConfig(containerName)), 0644); err != nil {
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
	inst.wg.Add(1)
	go func() {
		defer inst.wg.Done()
		_ = busCmd.Wait()
		if runCtx.Err() == nil {
			inst.cancel()
		}
	}()
	if err := waitForSocket(ctx, busSocket, proxyStartupTimeout); err != nil {
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
	inst.wg.Add(1)
	go func() {
		defer inst.wg.Done()
		_ = proxyCmd.Wait()
		if runCtx.Err() == nil {
			inst.cancel()
		}
	}()
	if err := waitForSocket(ctx, filepath.Join(proxyDir, proxySocketName), proxyStartupTimeout); err != nil {
		return "", fmt.Errorf("avahi proxy socket: %w", err)
	}

	failed = false
	return proxyDir, nil
}

// StartDaemon requires an IPv4 address inside netnsPath and then launches
// the supervised per-container avahi-daemon. Call after the container
// network is configured; fail-closed like Prepare.
func (m *Manager) StartDaemon(ctx context.Context, containerName, netnsPath string, addrOK func(context.Context) (bool, error)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst, ok := m.instances[containerName]
	if !ok {
		return fmt.Errorf("avahi bridge not prepared for %q", containerName)
	}
	if inst.daemonStarted {
		return nil
	}
	if inst.runCtx.Err() != nil {
		return fmt.Errorf("avahi stack stopped for %q", containerName)
	}
	stateDir := m.stateDir(containerName)
	// Address-before-daemon: the daemon publishes interface addresses, so it
	// must only start once the netns actually has an IPv4.
	addrCtx, addrCancel := context.WithTimeout(ctx, daemonReadyTimeout)
	defer addrCancel()
	ok, err := addrOK(addrCtx)
	if err != nil {
		return fmt.Errorf("avahi netns address check: %w", err)
	}
	if !ok {
		return fmt.Errorf("avahi netns has no IPv4 address after network setup")
	}
	inst.wg.Add(1)
	ready := make(chan error, 1)
	inst.daemonStarted = true
	go m.supervise(inst.runCtx, inst, containerName, netnsPath, stateDir, ready)
	select {
	case err := <-ready:
		if err != nil {
			inst.cancel()
			inst.daemonStarted = false
		}
		return err
	case <-ctx.Done():
		inst.cancel()
		return ctx.Err()
	}
}

// supervise runs avahi-daemon in the container netns, restarting it with
// backoff up to maxRestarts. The daemon self-confines (chroot + user drop);
// it reaches only the private bus via DBUS_SYSTEM_BUS_ADDRESS.
func (m *Manager) supervise(ctx context.Context, inst *instance, containerName, netnsPath, stateDir string, ready chan<- error) {
	defer inst.wg.Done()
	defer inst.cancel()
	defer func() {
		select {
		case ready <- fmt.Errorf("avahi supervisor stopped before readiness"):
		default:
		}
	}()
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
		// Each daemon needs its own runtime PID file and static service tree.
		// The host binaries remain available; Avahi still drops privileges and
		// chroots normally. All mounts are private to this child process.
		cmd := exec.CommandContext(ctx, "nsenter", "--net="+netnsPath,
			"unshare", "--mount", "--propagation", "private", "sh", "-c",
			`mkdir -p /run/avahi-daemon /etc/avahi/services &&
    mount --bind "$1/daemon-run" /run/avahi-daemon &&
    mount --bind "$1/services" /etc/avahi/services &&
    { test ! -e /etc/avahi/hosts || mount --bind "$1/hosts" /etc/avahi/hosts; } &&
    exec env DBUS_SYSTEM_BUS_ADDRESS="unix:path=$1/bus.sock" avahi-daemon -f "$1/avahi-daemon.conf"`,
			"avahi-instance", stateDir)
		var stderr daemonOutput
		cmd.Stderr = &stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
		cmd.WaitDelay = 2 * time.Second
		m.logger.Info("avahi daemon starting", zap.String("container", containerName), zap.Int("attempt", restarts+1))
		err := cmd.Start()
		if err == nil {
			m.track(inst, cmd)
			exited := make(chan error, 1)
			go func() { exited <- cmd.Wait() }()
			probeCtx, stopProbe := context.WithTimeout(ctx, daemonReadyTimeout)
			probe := make(chan error, 1)
			go func() { probe <- m.ready(probeCtx, bus) }()
			select {
			case err = <-probe:
				if err != nil {
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
					<-exited
				}
				if restarts == 0 {
					if err != nil {
						ready <- fmt.Errorf("avahi startup: %w: %s", err, stderr.String())
					} else {
						ready <- nil
					}
				}
				stopProbe()
				if err != nil {
					return
				}
				err = <-exited
			case err = <-exited:
				stopProbe()
				<-probe
				if err == nil {
					err = errors.New("avahi exited before readiness")
				}
				if restarts == 0 {
					ready <- fmt.Errorf("avahi startup: %w: %s", err, stderr.String())
					return
				}
			}
		} else if restarts == 0 {
			ready <- err
			return
		}
		m.logger.Warn("avahi daemon output", zap.String("container", containerName), zap.String("stderr_tail", stderr.String()))
		restarts++
		if ctx.Err() != nil {
			return
		}
		m.logger.Warn("avahi daemon exited unexpectedly; restarting",
			zap.String("container", containerName), zap.Error(err))
	}
}

// stopInstance cancels, joins, kills leftovers, and removes state.
func (m *Manager) stopInstance(containerName string, inst *instance, preserveDirs bool) {
	inst.cancel()
	inst.wg.Wait()
	inst.mu.Lock()
	procs := inst.procs
	inst.procs = nil
	inst.mu.Unlock()
	for _, cmd := range procs {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			// Child waits are owned by the launch goroutines above.
		}
	}
	if !preserveDirs {
		_ = os.RemoveAll(m.stateDir(containerName))
	}
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
	m.stopInstance(containerName, inst, false)
	m.mu.Unlock()
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
		if st, err := os.Stat(path); err == nil && st.Mode()&os.ModeSocket != 0 {
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

// Close leaves directories mounted by surviving tasks in place. A replacement
// agent recreates socket files inside those same directories.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, inst := range m.instances {
		m.stopInstance(name, inst, true)
		delete(m.instances, name)
	}
}

type daemonOutput struct {
	mu   sync.Mutex
	tail string
}

func (d *daemonOutput) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tail += string(p)
	if len(d.tail) > 8192 {
		d.tail = d.tail[len(d.tail)-8192:]
	}
	return len(p), nil
}
func (d *daemonOutput) String() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return strings.TrimSpace(d.tail)
}

func waitForDaemon(ctx context.Context, bus string) error {
	conn, err := dbus.Connect("unix:path="+bus, dbus.WithContext(ctx))
	if err != nil {
		return err
	}
	defer conn.Close()
	var lastState int32 = -1
	for {
		var state int32
		err = conn.Object(avahiBusName, "/").CallWithContext(ctx, avahiBusName+".Server.GetState", 0).Store(&state)
		if err == nil {
			lastState = state
		}
		if err == nil && state == 2 {
			return nil
		} // AVAHI_SERVER_RUNNING
		select {
		case <-ctx.Done():
			return fmt.Errorf("avahi readiness: %w (last state: %d, last D-Bus error: %v)", ctx.Err(), lastState, err)
		case <-time.After(50 * time.Millisecond):
		}
	}
}
