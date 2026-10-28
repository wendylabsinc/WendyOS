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
	"os/user"
	"path/filepath"
	"strconv"
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
	if dir, ok := m.restoredDirs.Load(containerName); ok {
		return dir.(string)
	}
	return filepath.Join(m.baseDir, stateKey(containerName))
}

// StateDir returns the production host state directory for a container.
func StateDir(containerName string) string {
	return filepath.Join(defaultBaseDir, stateKey(containerName))
}

// Socket paths have a much smaller limit than valid application IDs. Hash the
// whole container ID, including its service suffix, into a bounded component.
func stateKey(containerName string) string {
	id := sha256.Sum256([]byte(containerName))
	return fmt.Sprintf("%x", id[:20])
}

// ProxySocketDir returns the host directory to bind-mount at /var/run/dbus.
func (m *Manager) proxyDir(containerName string) string {
	return filepath.Join(m.stateDir(containerName), "proxy")
}

// ProxySocketDir returns the directory this manager's Prepare will provide.
// It does not create the directory or start a private bus. Call Prepare before
// using the directory as a container mount.
func (m *Manager) ProxySocketDir(containerName string) string {
	return m.proxyDir(containerName)
}

// ProxySocketDir returns the production proxy directory to mount at /var/run/dbus.
func ProxySocketDir(containerName string) string {
	return filepath.Join(StateDir(containerName), "proxy")
}

// IsProxySocketDir recognizes this container's current or pre-hash persisted
// mount. Existing tasks keep their directory inode across agent upgrades.
func IsProxySocketDir(containerName, dir string) bool {
	return isProxySocketDir(defaultBaseDir, containerName, dir)
}

func isProxySocketDir(base, containerName, dir string) bool {
	dir = filepath.Clean(dir)
	if dir == filepath.Join(base, stateKey(containerName), "proxy") {
		return true
	}
	if containerName == "." || containerName == ".." || filepath.Base(containerName) != containerName {
		return false
	}
	legacy := filepath.Join(base, containerName, "proxy")
	return len(filepath.Join(legacy, proxySocketName)) < 104 && dir == legacy
}

// RestoreProxySocketDir adopts the exact persisted OCI mount before Prepare.
// Newly created containers continue to use the bounded hashed path.
func (m *Manager) RestoreProxySocketDir(containerName, dir string) error {
	dir = filepath.Clean(dir)
	if !isProxySocketDir(m.baseDir, containerName, dir) {
		return fmt.Errorf("avahi persisted proxy does not match container %q", containerName)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.instances[containerName]; exists && m.proxyDir(containerName) != dir {
		return fmt.Errorf("avahi proxy already active at another path for %q", containerName)
	}
	m.restoredDirs.Store(containerName, filepath.Dir(dir))
	return nil
}

// busConfig admits the root agent/proxy and Avahi's service identity. Apps
// reach this bus through the filtered proxy; only Avahi may own its well-known
// name. Method replies and directed client callbacks still need unrestricted
// destinations between these peers. Like the root-owned xdg proxy, this bridge
// supports the root client identity used by Wendy's OCI spec.
func busConfig(socketPath string, uid int) string {
	return fmt.Sprintf(`<busconfig>
  <type>session</type>
  <listen>unix:path=%s</listen>
  <policy context="default">
    <deny user="*"/>
    <allow user="root"/>
    <allow user="%d"/>
    <deny own="*"/>
    <allow send_destination="*"/>
    <allow receive_sender="*"/>
  </policy>
  <policy user="%d">
    <allow own="org.freedesktop.Avahi"/>
  </policy>
</busconfig>
`, socketPath, uid, uid)
}

func avahiIdentity() (int, int, error) {
	u, err := user.Lookup("avahi")
	if err != nil {
		return 0, 0, fmt.Errorf("looking up avahi service user: %w", err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, err
	}
	gid, err := strconv.Atoi(u.Gid)
	return uid, gid, err
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
	logger       *zap.Logger
	mu           sync.Mutex
	baseDir      string
	instances    map[string]*instance
	ready        func(context.Context, string) error
	identity     func() (int, int, error)
	restoredDirs sync.Map // container name -> exact persisted state directory
}

// NewManager creates an avahi bridge manager.
func NewManager(logger *zap.Logger) *Manager {
	return &Manager{logger: logger, baseDir: defaultBaseDir, instances: map[string]*instance{}, ready: waitForDaemon, identity: avahiIdentity}
}

// NewManagerInDir creates a manager rooted at dir (tests).
func NewManagerInDir(logger *zap.Logger, dir string) *Manager {
	return &Manager{logger: logger, baseDir: dir, instances: map[string]*instance{}, ready: waitForDaemon, identity: avahiIdentity}
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

	uid, gid, err := m.identity()
	if err != nil {
		return "", err
	}
	stateDir := m.stateDir(containerName)
	// Keep the proxy directory accessible through its app bind mount while
	// its parent hides both sockets from unrelated host users. Avahi needs
	// group traversal after dropping privileges. Reapply permissions on recovery.
	if err := os.MkdirAll(m.baseDir, 0755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(stateDir, 0750); err != nil {
		return "", err
	}
	if err := os.Chown(stateDir, os.Getuid(), gid); err != nil {
		return "", fmt.Errorf("avahi state ownership: %w", err)
	}
	if err := os.Chmod(stateDir, 0750); err != nil {
		return "", fmt.Errorf("avahi state permissions: %w", err)
	}
	// Preserve the full owner ID alongside the bounded hash and fail closed
	// if a pre-existing runtime directory belongs to another container.
	ownerPath := filepath.Join(stateDir, "container-id")
	if owner, err := os.ReadFile(ownerPath); err == nil {
		if string(owner) != containerName {
			return "", fmt.Errorf("avahi state belongs to a different container")
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("reading avahi state owner: %w", err)
	}
	if err := os.WriteFile(ownerPath, []byte(containerName), 0644); err != nil {
		return "", fmt.Errorf("recording avahi state owner: %w", err)
	}
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
	if err := os.WriteFile(filepath.Join(stateDir, busConfigName), []byte(busConfig(busSocket, uid)), 0644); err != nil {
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
	setParentDeathSignal(busCmd)
	busCmd.Stdout, busCmd.Stderr = nil, nil
	if err := busCmd.Start(); err != nil {
		return "", fmt.Errorf("avahi private bus start: %w", err)
	}
	m.track(inst, busCmd)
	inst.wg.Add(1)
	go func() {
		defer inst.wg.Done()
		err := busCmd.Wait()
		if runCtx.Err() == nil {
			m.logger.Warn("avahi private bus exited unexpectedly", zap.String("container", containerName), zap.Error(err))
			inst.cancel()
		}
	}()
	if err := waitForSocket(ctx, busSocket, proxyStartupTimeout); err != nil {
		return "", fmt.Errorf("avahi private bus socket: %w", err)
	}
	// Root hosts the proxy; only the Avahi service group may use the raw bus.
	if err := os.Chown(busSocket, os.Getuid(), gid); err != nil {
		return "", fmt.Errorf("avahi bus ownership: %w", err)
	}
	if err := os.Chmod(busSocket, 0660); err != nil {
		return "", fmt.Errorf("avahi bus permissions: %w", err)
	}

	proxyCmd := exec.CommandContext(runCtx, "xdg-dbus-proxy",
		"unix:path="+busSocket,
		filepath.Join(proxyDir, proxySocketName),
		"--filter",
		"--talk="+avahiBusName,
		"--see="+avahiBusName,
	)
	setParentDeathSignal(proxyCmd)
	proxyCmd.Stdout, proxyCmd.Stderr = nil, nil
	if err := proxyCmd.Start(); err != nil {
		return "", fmt.Errorf("avahi dbus proxy start: %w", err)
	}
	m.track(inst, proxyCmd)
	inst.wg.Add(1)
	go func() {
		defer inst.wg.Done()
		err := proxyCmd.Wait()
		if runCtx.Err() == nil {
			m.logger.Warn("avahi proxy exited unexpectedly", zap.String("container", containerName), zap.Error(err))
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
	if inst.runCtx.Err() != nil {
		return fmt.Errorf("avahi stack stopped for %q", containerName)
	}
	if inst.daemonStarted {
		return nil
	}
	// Keep the namespace alive across daemon retries. The task PID can exit
	// and be reused while a delayed restart is pending.
	netns, err := os.Open(netnsPath)
	if err != nil {
		return fmt.Errorf("avahi namespace anchor: %w", err)
	}
	stateDir := m.stateDir(containerName)
	// Address-before-daemon: the daemon publishes interface addresses, so it
	// must only start once the netns actually has an IPv4.
	addrCtx, addrCancel := context.WithTimeout(ctx, daemonReadyTimeout)
	defer addrCancel()
	ok, err = addrOK(addrCtx)
	if err != nil {
		_ = netns.Close()
		return fmt.Errorf("avahi netns address check: %w", err)
	}
	if !ok {
		_ = netns.Close()
		return fmt.Errorf("avahi netns has no IPv4 address after network setup")
	}
	inst.wg.Add(1)
	ready := make(chan error, 1)
	inst.daemonStarted = true
	go m.supervise(inst.runCtx, inst, containerName, netns, stateDir, ready)
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
func (m *Manager) supervise(ctx context.Context, inst *instance, containerName string, netns *os.File, stateDir string, ready chan<- error) {
	defer inst.wg.Done()
	defer inst.cancel()
	defer netns.Close()
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
		cmd := exec.Command("nsenter", "--net=/proc/self/fd/3",
			"unshare", "--mount", "--propagation", "private", "sh", "-c",
			`mkdir -p /run/avahi-daemon /etc/avahi/services &&
    mount --bind "$1/daemon-run" /run/avahi-daemon &&
    mount --bind "$1/services" /etc/avahi/services &&
    { test ! -e /etc/avahi/hosts || mount --bind "$1/hosts" /etc/avahi/hosts; } &&
    exec env DBUS_SYSTEM_BUS_ADDRESS="unix:path=$1/bus.sock" avahi-daemon -f "$1/avahi-daemon.conf"`,
			"avahi-instance", stateDir)
		cmd.ExtraFiles = []*os.File{netns}
		var stderr daemonOutput
		cmd.Stderr = &stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		setParentDeathSignal(cmd)
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
			case <-ctx.Done():
				stopProbe()
				terminateDaemon(cmd, exited)
				<-probe
				return
			case err = <-probe:
				stopProbe()
				if err != nil {
					terminateDaemon(cmd, exited)
					m.logger.Warn("avahi readiness failed", zap.String("container", containerName), zap.Error(err), zap.String("stderr_tail", stderr.String()))
					if restarts == 0 {
						ready <- fmt.Errorf("avahi startup: %w", err)
						return
					}
				} else {
					if restarts == 0 {
						ready <- nil
					}
					select {
					case err = <-exited:
					case <-ctx.Done():
						terminateDaemon(cmd, exited)
						return
					}
				}
			case err = <-exited:
				stopProbe()
				<-probe
				if err == nil {
					err = errors.New("avahi exited before readiness")
				}
				if restarts == 0 {
					m.logger.Warn("avahi startup failed", zap.String("container", containerName), zap.Error(err), zap.String("stderr_tail", stderr.String()))
					ready <- fmt.Errorf("avahi startup: %w", err)
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

// terminateDaemon bounds readiness-failure cleanup even if the daemon ignores
// SIGTERM. Killing the process group also retires Avahi's chroot helper.
func terminateDaemon(cmd *exec.Cmd, exited <-chan error) {
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-exited:
		return
	case <-timer.C:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-exited
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
		m.restoredDirs.Delete(containerName)
	}
}

// Stop kills a container's avahi stack and removes its state.
func (m *Manager) Stop(containerName string) error {
	return m.stop(containerName, false)
}

// StopPreservingMounts releases a failed recovery stack without detaching the
// proxy directory inode that a surviving task may still have bind-mounted.
func (m *Manager) StopPreservingMounts(containerName string) error {
	return m.stop(containerName, true)
}

func (m *Manager) stop(containerName string, preserveDirs bool) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	inst, ok := m.instances[containerName]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	delete(m.instances, containerName)
	m.stopInstance(containerName, inst, preserveDirs)
	m.mu.Unlock()
	return nil
}

// StopAll kills every tracked stack (agent shutdown).
func (m *Manager) StopAll() {
	if m == nil {
		return
	}
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
	if m == nil {
		return
	}
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
