package containerd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

const (
	cniStateDir = "/run/wendy/cni"

	// CNIBinDir is the directory the agent maintains with "bridge" and
	// "host-local" symlinks that point back at its own running executable
	// (see ensureCNIBinDir in cmd/wendy-agent, invoked at daemon startup).
	// CNIAdd/CNIDel set CNI_PATH to this directory so the vendored bridge
	// plugin's IPAM delegation (it execs "host-local" via CNI_PATH) also
	// resolves back into the agent binary rather than a third-party
	// /opt/cni/bin binary.
	CNIBinDir = "/run/wendy/cni/bin"

	// cniSystemPathDirs are appended to PATH (after CNIBinDir) for the bridge
	// exec below. The vendored bridge plugin shells out to the real
	// "iptables" for NAT/masquerade rules, which lives here, not in
	// CNIBinDir. These are standard root-owned system directories — not
	// writable by an unrelated/unprivileged process — so including them
	// doesn't reopen the PATH-hijack risk that restricting PATH to CNIBinDir
	// alone was guarding against (SOC2-CC6, NIST-SC-7, ISO27001-A.8).
	cniSystemPathDirs = ":/usr/sbin:/usr/bin:/sbin:/bin"

	// selfExePath execs the exact inode of the currently-running process.
	// Unlike a path such as /opt/cni/bin/bridge, this is TOCTOU-safe by
	// construction — the kernel resolves it to the already-running agent
	// binary, so there is no window in which a third party could swap the
	// target between a check and the exec, and no third-party binary to pin
	// a digest for (SOC2-CC6, ISO27001-A.8, NIST-SI-3).
	selfExePath = "/proc/self/exe"
)

// serviceNamePattern is the allowlist for service names written into
// /etc/hosts. Only ASCII alphanumeric, hyphen, and underscore are permitted to
// prevent tab/newline/space injection that would corrupt the hosts file
// (SOC2-CC6, ISO27001-A.8, NIST-SI-10).
var serviceNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$`)

// cniResult is a minimal subset of the CNI ADD result.
type cniResult struct {
	IPs []struct {
		Address string `json:"address"` // CIDR notation, e.g. "10.x.y.z/28"
	} `json:"ips"`
}

// netnsPathPattern accepts the three netns path forms used in this package:
//   - /proc/{pid}/ns/net          — direct procfs reference
//   - /proc/self/fd/{n}           — fd-anchored reference (prevents PID-reuse races)
//   - /run/wendy/netns/{ctrID}    — bind-mounted path (spec-compliant for CNI plugins)
var netnsPathPattern = regexp.MustCompile(`^(/proc/\d+/ns/net|/proc/self/fd/\d+|/run/wendy/netns/[a-zA-Z0-9][a-zA-Z0-9._@-]{0,319})$`)

// containerIDPattern is an allowlist for CNI containerID values. Wendy
// container IDs are either a bare appID or "{appID}@{serviceName}", so valid
// characters are letters, digits, dot, underscore, hyphen, and '@'.
// This allowlist replaces a previous denylist that blocked only a few
// characters; an allowlist is more robust against unforeseen CNI plugin
// behaviour (SOC2-CC6, ISO27001-A.8, NIST-SI-10).
var containerIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._@-]{0,319}$`)

// cniSubnetRegistryPath is the persistent file mapping appID → subnet.
// Declared as a var (not const) so tests can redirect it to a temp directory.
var cniSubnetRegistryPath = cniStateDir + "/subnets.json"

// allocateSubnet returns the /28 subnet for an appID. It maintains a
// persistent registry so that:
//   - The same appID always gets the same subnet (stable CNI config).
//   - A collision with an existing entry for a different appID is detected
//     and rejected immediately rather than silently routing cross-app traffic
//     (SOC2-CC6, ISO27001-A.8, NIST-SC-7).
//
// The read-modify-write is serialised with an exclusive flock on a companion
// lock file to prevent two concurrent app starts from both reading the same
// unallocated subnet and writing conflicting entries (SOC2-CC6, NIST-SC-7).
//
// Four bytes of SHA-256 are used as the initial candidate. If a collision is
// detected the candidate is rejected and an error is returned.
func allocateSubnet(appID string) (string, error) {
	registryPath := cniSubnetRegistryPath
	stateDir := filepath.Dir(registryPath)
	// 0o700: owner-only. subnets.json maps every appID to its subnet, which is
	// internal routing topology. Group-0 access would expose this inventory to
	// any setgid binary or GID-0 daemon on the host (SOC2-CC6, NIST-SC-7,
	// ISO27001-A.8).
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", fmt.Errorf("creating CNI state dir: %w", err)
	}
	// Explicit chmod covers pre-existing directories (MkdirAll is a no-op for
	// them and would leave a previously-wider mode unchanged).
	if err := os.Chmod(stateDir, 0o700); err != nil {
		return "", fmt.Errorf("setting permissions on CNI state dir: %w", err)
	}

	// Serialise concurrent read-modify-writes with an exclusive file lock.
	// A companion lock file (not the registry itself) is used so the lock
	// remains valid across the atomic rename that replaces subnets.json
	// (SOC2-CC6, NIST-SC-7, ISO27001-A.8).
	lockF, err := os.OpenFile(registryPath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", fmt.Errorf("opening CNI registry lock: %w", err)
	}
	defer func() {
		if closeErr := lockF.Close(); closeErr != nil {
			zap.L().Warn("failed to close CNI registry lock file", zap.Error(closeErr))
		}
	}()
	if err := syscall.Flock(int(lockF.Fd()), syscall.LOCK_EX); err != nil {
		return "", fmt.Errorf("locking CNI registry: %w", err)
	}
	defer syscall.Flock(int(lockF.Fd()), syscall.LOCK_UN) //nolint:errcheck

	// Load the existing registry; a corrupted file is a hard error rather than
	// a silent reset to avoid masking filesystem or write failures.
	registry := map[string]string{}
	if data, err := os.ReadFile(registryPath); err == nil {
		if jsonErr := json.Unmarshal(data, &registry); jsonErr != nil {
			return "", fmt.Errorf("CNI subnet registry corrupted (%w): delete %s to reset", jsonErr, registryPath)
		}
	}

	// Return already-assigned subnet for this appID.
	if existing, ok := registry[appID]; ok {
		return existing, nil
	}

	// Compute a /28 candidate from 4 SHA-256 bytes.  When the derived /24 is
	// full, extend probing into adjacent /24s by incrementing b3 (then b2) to
	// prevent a targeted DoS where an attacker registers 16 apps that all hash
	// to the same b2.b3 prefix and exhausts allocation for a victim appID.
	// Each outer step covers 16 /28 blocks in one /24; up to 256 /24s are tried
	// within the same b2 octet before failing (SOC2-CC6, NIST-SC-7, ISO27001-A.8).
	h := sha256.Sum256([]byte(appID))
	b2 := h[0]
	b3Start := h[1]
	b4base := (h[2] ^ h[3]) & 0xF0

	allocated := make(map[string]struct{}, len(registry))
	for _, s := range registry {
		allocated[s] = struct{}{}
	}

	var candidate string
outer:
	for b3Offset := 0; b3Offset < 256; b3Offset++ {
		b3 := byte((int(b3Start) + b3Offset) & 0xFF)
		for probe := 0; probe < 16; probe++ {
			b4 := byte((int(b4base)+probe*0x10)&0xFF) & 0xF0
			c := fmt.Sprintf("10.%d.%d.%d/28", b2, b3, b4)
			if _, taken := allocated[c]; !taken {
				candidate = c
				break outer
			}
		}
	}
	if candidate == "" {
		return "", fmt.Errorf("all /28 subnets in 10.%d.x.0/8 are allocated — consider releasing unused apps (SOC2-CC6, NIST-SC-7)", b2)
	}

	// Persist the new assignment atomically via temp-file + rename so the
	// registry is never partially written (SOC2-CC6, NIST-SC-7).
	registry[appID] = candidate
	data, err := json.Marshal(registry)
	if err != nil {
		return "", fmt.Errorf("marshalling CNI subnet registry: %w", err)
	}
	// LockOSThread + Umask(0) ensures os.CreateTemp creates the file with the
	// kernel-assigned 0600 mode (before umask), closing the window between file
	// creation and the subsequent Chmod call (SOC2-CC6, NIST-SI-10).
	runtime.LockOSThread()
	oldUmask := syscall.Umask(0)
	tmp, err := os.CreateTemp(stateDir, ".subnets-*.json.tmp")
	syscall.Umask(oldUmask)
	runtime.UnlockOSThread()
	if err != nil {
		return "", fmt.Errorf("creating temp CNI registry: %w", err)
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", fmt.Errorf("chmod temp CNI registry: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", fmt.Errorf("writing temp CNI registry: %w", err)
	}
	tmp.Close()
	if err := os.Rename(tmpName, registryPath); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("renaming temp CNI registry: %w", err)
	}
	return candidate, nil
}

// bridgeName returns a Linux network interface name for the app's CNI bridge.
// The kernel limit is 15 chars (IFNAMSIZ-1). Short appIDs that fit are embedded
// directly; longer ones fall back to an 8-hex-digit SHA-256 prefix for uniform
// distribution (birthday collision probability ~0.12% at 100 apps).
// cniDuplicateAllocation reports whether a CNI plugin's stdout indicates the
// host-local IPAM refused to re-allocate an IP the container already holds.
// host-local emits code 999 with "duplicate allocation is not allowed" / "has
// been allocated" in this case.
func cniDuplicateAllocation(stdout string) bool {
	return strings.Contains(stdout, "duplicate allocation is not allowed") ||
		strings.Contains(stdout, "has been allocated")
}

func bridgeName(appID string) string {
	const prefix = "wendy-br-"
	if len(prefix)+len(appID) <= 15 {
		return prefix + appID
	}
	h := sha256.Sum256([]byte(appID))
	return fmt.Sprintf("wendy-%08x", binary.BigEndian.Uint32(h[:4]))
}

// validateCNIInputs provides defence-in-depth validation of the values that
// reach the CNI exec environment, guarding against any future caller that
// bypasses the RPC-layer ValidateAppID check (SOC2-CC6, NIST-SI-10).
func validateCNIInputs(appID, containerID, netnsPath string) error {
	if err := appconfig.ValidateAppID(appID); err != nil {
		return fmt.Errorf("CNI: %w", err)
	}
	if !containerIDPattern.MatchString(containerID) {
		return fmt.Errorf("CNI: containerID %q does not match allowed pattern (letters, digits, '.', '_', '@', '-'; 1–320 chars)", containerID)
	}
	if !netnsPathPattern.MatchString(netnsPath) {
		return fmt.Errorf("CNI: netnsPath %q does not match expected pattern", netnsPath)
	}
	return nil
}

// buildBridgeCNIConfig returns the JSON config string for the CNI bridge plugin.
func buildBridgeCNIConfig(appID, subnet string, defaultGateway bool) string {
	cfg := map[string]interface{}{
		"cniVersion": "0.4.0",
		"name":       "wendy-" + appID,
		"type":       "bridge",
		"bridge":     bridgeName(appID), // capped at 15 chars (IFNAMSIZ-1)
		// Merely assigning the bridge gateway does not install a default
		// route in the app namespace. The vendored bridge plugin adds and
		// verifies that route only with isDefaultGateway. Only Internet-enabled
		// bridge apps receive it; mesh apps retain their scoped service routes.
		"isGateway":        true,
		"isDefaultGateway": defaultGateway,
		"ipMasq":           true,
		"ipam": map[string]interface{}{
			"type":    "host-local",
			"subnet":  subnet,
			"dataDir": cniStateDir + "/" + appID,
		},
	}
	b, _ := json.Marshal(cfg)
	return string(b)
}

func buildBridgeCNICheckConfig(appID, subnet, result string, defaultGateway bool) (string, error) {
	var prevResult any
	if len(result) == 0 || len(result) > cniStdoutLimit || json.Unmarshal([]byte(result), &prevResult) != nil {
		return "", fmt.Errorf("invalid CNI previous result")
	}
	if defaultGateway {
		var proof struct {
			Routes []struct{ Dst, GW string }
		}
		_, network, err := net.ParseCIDR(subnet)
		if err != nil || network.IP.To4() == nil {
			return "", fmt.Errorf("invalid CNI subnet")
		}
		gateway := append(net.IP(nil), network.IP.To4()...)
		gateway[3]++ // allocated /28: first host is the bridge gateway
		if json.Unmarshal([]byte(result), &proof) != nil {
			return "", fmt.Errorf("invalid CNI route proof")
		}
		found := false
		for _, route := range proof.Routes {
			found = found || (route.Dst == "0.0.0.0/0" && route.GW == gateway.String())
		}
		if !found {
			return "", fmt.Errorf("CNI proof lacks the required bridge default route")
		}
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(buildBridgeCNIConfig(appID, subnet, defaultGateway)), &cfg); err != nil {
		return "", err
	}
	cfg["prevResult"] = prevResult
	b, err := json.Marshal(cfg)
	return string(b), err
}

// cniStdoutLimit is the maximum bytes read from the CNI plugin's stdout.
// A valid CNI ADD response is well under 1 KB; this cap prevents memory
// exhaustion if the vendored plugin logic ever emits unexpectedly large
// output (SOC2-CC6, NIST-SI-10: input bounds enforcement).
const cniStdoutLimit = 64 << 10 // 64 KB

// warnSubnetCollision checks whether the allocated /28 subnet overlaps with
// any address already assigned to a network interface on this host. A collision
// means two apps would share the same bridge subnet, causing routing conflicts.
// The allocation is deterministic so we cannot change it here; we log a warning
// so operators can detect and resolve the conflict (SOC2-CC6, NIST-SC-7).
func warnSubnetCollision(logger *zap.Logger, appID, subnet string) {
	_, allocNet, err := net.ParseCIDR(subnet)
	if err != nil {
		return // malformed subnet is caught elsewhere
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return // best-effort; failure to list interfaces is not fatal here
	}
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip != nil && allocNet.Contains(ip) {
				logger.Warn("CNI subnet collision detected",
					zap.String("app_id", appID),
					zap.String("allocated_subnet", subnet),
					zap.String("conflicting_interface", iface.Name),
					zap.String("conflicting_ip", ip.String()))
			}
		}
	}
}

// CNIAdd calls the vendored CNI bridge plugin ADD for a container, returning
// its assigned IP address. netnsPath is the container's network namespace
// path (e.g. /proc/self/fd/{n} for fd-anchored references, or
// /proc/{pid}/ns/net).
//
// The agent execs itself (selfExePath, i.e. /proc/self/exe) with argv0
// overridden to "bridge" rather than shelling out to a third-party
// /opt/cni/bin/bridge binary. /proc/self/exe always resolves to the exact
// inode of the currently-running agent process, so there is no TOCTOU
// window and — because the exec path never leaves the agent's own already-
// running, already-trusted code — no third-party binary to pin a digest for
// (SOC2-CC6, ISO27001-A.8, NIST-SI-3). CNI_PATH points at CNIBinDir, where
// the agent maintains "bridge"/"host-local" symlinks back at itself (see
// ensureCNIBinDir in cmd/wendy-agent), so the vendored bridge plugin's IPAM
// delegation (it execs "host-local" via CNI_PATH) also resolves back into
// this same binary instead of a third-party binary.
func (c *Client) CNIAdd(ctx context.Context, appID, containerID, netnsPath string, defaultGateway bool) (string, string, error) {
	if err := validateCNIInputs(appID, containerID, netnsPath); err != nil {
		return "", "", err
	}

	subnet, err := allocateSubnet(appID)
	if err != nil {
		return "", "", err
	}
	c.reconcileLANReplyGuards(ctx)
	warnSubnetCollision(c.logger, appID, subnet)
	cfgJSON := buildBridgeCNIConfig(appID, subnet, defaultGateway)

	// Defence-in-depth NUL guard: validateCNIInputs uses allowlist regexes
	// that already exclude NUL, but an explicit check at the exec boundary
	// prevents a kernel-level env truncation if a NUL ever reaches here via a
	// future bypass (SOC2-CC6, NIST-SC-7, ISO27001-A.8).
	if strings.ContainsRune(containerID, '\x00') || strings.ContainsRune(netnsPath, '\x00') {
		return "", "", fmt.Errorf("CNI ADD: NUL byte in containerID or netnsPath rejected")
	}

	// runAddOnce execs the vendored bridge plugin (via self-exec) for one ADD
	// attempt and returns its stdout, stderr, and run error.
	runAddOnce := func() (stdoutStr, stderrStr string, runErr error) {
		cmd := exec.CommandContext(ctx, selfExePath)
		// argv0 "bridge" is what cniPluginName (cmd/wendy-agent) matches on to
		// dispatch this re-exec of the agent into the vendored bridge plugin
		// logic instead of starting the daemon.
		cmd.Args = []string{"bridge"}
		cmd.Dir = "/" // prevent relative-path resolution from an attacker-controlled cwd
		cmd.Stdin = strings.NewReader(cfgJSON)
		cmd.Env = []string{
			// Explicit minimal environment — never inherit the agent's environment,
			// which may contain credentials or Wendy-internal tokens (SOC2-CC6, NIST-SC-7).
			// CNI_PATH is restricted to CNIBinDir, which contains only symlinks
			// back to this agent binary, so bridge/host-local delegation can
			// never resolve to a third-party binary. PATH additionally includes
			// the standard system sbin/bin directories (root-owned, not writable
			// by unrelated processes, so this doesn't reopen the PATH-hijack risk
			// CNIBinDir-only was guarding against) because the vendored bridge
			// plugin shells out to the real "iptables" for NAT/masquerade rules.
			"PATH=" + CNIBinDir + cniSystemPathDirs,
			"CNI_COMMAND=ADD",
			"CNI_CONTAINERID=" + containerID,
			"CNI_NETNS=" + netnsPath,
			"CNI_IFNAME=eth0",
			"CNI_PATH=" + CNIBinDir,
		}
		// Bound stdout to cniStdoutLimit to guard against unbounded output
		// exhausting agent memory.
		var stdoutBuf, stderrBuf bytes.Buffer
		cmd.Stdout = &limitedWriter{w: &stdoutBuf, remaining: cniStdoutLimit, cap: cniStdoutLimit}
		cmd.Stderr = &stderrBuf
		runErr = cmd.Run()
		return stdoutBuf.String(), stderrBuf.String(), runErr
	}

	stdoutStr, stderrStr, runErr := runAddOnce()
	// host-local refuses to re-allocate an IP a container already holds
	// ("duplicate allocation is not allowed"). This happens when a prior ADD
	// allocated an IP but the container later crashed/restarted and the
	// monitor re-runs ADD without an intervening DEL. Release the stale lease
	// with a DEL and retry once so container restarts are idempotent.
	if runErr != nil && cniDuplicateAllocation(stdoutStr) {
		c.logger.Warn("CNI ADD hit a stale IPAM lease; releasing and retrying",
			zap.String("app_id", appID), zap.String("container_id", containerID))
		_ = c.CNIDel(ctx, appID, containerID, netnsPath)
		stdoutStr, stderrStr, runErr = runAddOnce()
	}

	if runErr != nil {
		// Sanitize before logging to prevent log injection from a rogue CNI
		// binary (newlines, ANSI codes, JSON-alike content) (SOC2-CC6, NIST-SI-10).
		// CNI plugins report errors as a JSON body on stdout (not stderr), so
		// surface both — otherwise a plugin failure looks like a bare
		// "exit status 1" with empty stderr and no cause.
		c.logger.Warn("CNI ADD failed",
			zap.String("app_id", appID),
			zap.String("container_id", containerID),
			zap.String("stdout", sanitizeForLog(stdoutStr, 1024)),
			zap.String("stderr", sanitizeForLog(stderrStr, 512)),
			zap.Error(runErr))
		return "", "", fmt.Errorf("CNI ADD failed for %s/%s; see agent logs for details", appID, containerID)
	}

	var result cniResult
	if err := json.Unmarshal([]byte(stdoutStr), &result); err != nil {
		return "", "", fmt.Errorf("parsing CNI ADD result for %s/%s: %w", appID, containerID, err)
	}
	if len(result.IPs) == 0 {
		return "", "", fmt.Errorf("CNI ADD returned no IPs for %s/%s", appID, containerID)
	}
	rawIP, _, _ := strings.Cut(result.IPs[0].Address, "/")
	// Validate and normalise the IP before using it in /etc/hosts bind-mounts.
	// An unvalidated string from CNI stdout could contain newlines or tabs that
	// poison the hosts file injected into sibling containers (SOC2-CC6, NIST-SC-7).
	parsed := net.ParseIP(rawIP)
	if parsed == nil {
		return "", "", fmt.Errorf("CNI ADD returned invalid IP %q for %s/%s", rawIP, appID, containerID)
	}
	ip := parsed.String()
	c.logger.Info("CNI ADD: assigned IP",
		zap.String("app_id", appID),
		zap.String("container_id", containerID),
		zap.String("ip", ip))
	return ip, stdoutStr, nil
}

// CNICheck asks the same vendored bridge and host-local plugins used for ADD
// to verify the complete persisted setup before it is reused.
func (c *Client) CNICheck(ctx context.Context, appID, containerID, netnsPath, result string, defaultGateway bool) error {
	if err := validateCNIInputs(appID, containerID, netnsPath); err != nil {
		return err
	}
	subnet, err := allocateSubnet(appID)
	if err != nil {
		return err
	}
	cfgJSON, err := buildBridgeCNICheckConfig(appID, subnet, result, defaultGateway)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, selfExePath)
	cmd.Args = []string{"bridge"}
	cmd.Dir = "/"
	cmd.Stdin = strings.NewReader(cfgJSON)
	cmd.Env = []string{
		"PATH=" + CNIBinDir + cniSystemPathDirs,
		"CNI_COMMAND=CHECK",
		"CNI_CONTAINERID=" + containerID,
		"CNI_NETNS=" + netnsPath,
		"CNI_IFNAME=eth0",
		"CNI_PATH=" + CNIBinDir,
	}
	var output bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &output, remaining: cniStdoutLimit, cap: cniStdoutLimit}
	cmd.Stderr = &limitedWriter{w: &output, remaining: cniStdoutLimit, cap: cniStdoutLimit}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("CNI CHECK failed for %s/%s: %w (%s)", appID, containerID, err, sanitizeForLog(output.String(), 512))
	}
	return nil
}

// limitedWriter is an io.Writer that returns an error if more than `cap`
// bytes are written. Unlike silent truncation, an error causes cmd.Run() to
// fail, preventing a truncated (potentially garbage) buffer from being parsed
// as a valid CNI result (SOC2-CC6, NIST-SI-10: fail-safe over silent discard).
type limitedWriter struct {
	w         io.Writer
	remaining int
	cap       int
}

func (lw *limitedWriter) Write(p []byte) (int, error) {
	if lw.remaining <= 0 || len(p) > lw.remaining {
		return 0, fmt.Errorf("CNI plugin output exceeded %d-byte safety limit", lw.cap)
	}
	n, err := lw.w.Write(p)
	lw.remaining -= n
	return n, err
}

// writeHostsFile atomically replaces path with a hosts-format file containing
// entries for each service name → IP mapping plus 127.0.0.1 localhost.
// Entries are written in sorted order for determinism.
// Atomicity is achieved via a temp-file write + os.Rename, so a container
// reading /etc/hosts never sees a truncated or zero-byte file during the
// update (SOC2-CC6, NIST-SC-7, ISO27001-A.8).
func writeHostsFile(path string, serviceIPs map[string]string) error {
	var sb strings.Builder
	sb.WriteString("127.0.0.1\tlocalhost\n")
	names := make([]string, 0, len(serviceIPs))
	for n := range serviceIPs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		// Allowlist: reject any service name that does not match the hostname
		// character set. This prevents space, tab, newline, Unicode tricks, or
		// any other character from injecting extra fields or lines into the file
		// (SOC2-CC6, ISO27001-A.8, NIST-SI-10).
		if !serviceNamePattern.MatchString(name) {
			continue
		}
		// Re-validate the IP at the write site as defence-in-depth; the primary
		// validation happens in CNIAdd but a future code path could bypass it.
		ip := serviceIPs[name]
		if net.ParseIP(ip) == nil {
			continue
		}
		fmt.Fprintf(&sb, "%s\t%s\n", ip, name)
	}

	dir := filepath.Dir(path)
	// LockOSThread + Umask(0) closes the window between os.CreateTemp and
	// the subsequent fd-based Chmod (SOC2-CC6, NIST-SI-10).
	runtime.LockOSThread()
	oldUmask := syscall.Umask(0)
	tmp, err := os.CreateTemp(dir, ".hosts-*.tmp")
	syscall.Umask(oldUmask)
	runtime.UnlockOSThread()
	if err != nil {
		return fmt.Errorf("creating temp hosts file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(sb.String()); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("writing temp hosts file: %w", err)
	}
	// Chmod via the open fd before Close to eliminate the TOCTOU window between
	// tmp.Close() and os.Chmod(path) — an attacker cannot swap the file between
	// the fd-based chmod and the subsequent rename (SOC2-CC6, NIST-SI-10).
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("chmod temp hosts file: %w", err)
	}
	tmp.Close()
	return os.Rename(tmpName, path)
}

// CNIDel calls the vendored CNI bridge plugin DEL to release a container's
// IP. Errors are logged as warnings but not returned — DEL is best-effort.
// See CNIAdd for the self-exec rationale (no third-party binary, no digest
// pin needed).
func (c *Client) CNIDel(ctx context.Context, appID, containerID, netnsPath string) error {
	if err := validateCNIInputs(appID, containerID, netnsPath); err != nil {
		c.logger.Warn("CNI DEL skipped: invalid inputs", zap.Error(err))
		return nil
	}
	// NUL guard: defence-in-depth at the exec boundary (SOC2-CC6, NIST-SC-7).
	if strings.ContainsRune(containerID, '\x00') || strings.ContainsRune(netnsPath, '\x00') {
		c.logger.Warn("CNI DEL skipped: NUL byte in input", zap.String("container_id", containerID))
		return nil
	}
	subnet, err := allocateSubnet(appID)
	if err != nil {
		c.logger.Warn("CNI DEL skipped: subnet allocation failed", zap.Error(err))
		return nil
	}
	cfgJSON := buildBridgeCNIConfig(appID, subnet, false)

	cmd := exec.CommandContext(ctx, selfExePath)
	cmd.Args = []string{"bridge"} // argv0 the multiplexer dispatches on; see CNIAdd
	cmd.Dir = "/"                 // prevent relative-path resolution from an attacker-controlled cwd
	cmd.Stdin = strings.NewReader(cfgJSON)
	cmd.Env = []string{
		// Explicit minimal environment — never inherit the agent's environment
		// (SOC2-CC6, NIST-SC-7). CNI_PATH restricted to CNIBinDir (symlinks back
		// to this agent binary only). PATH additionally includes the standard
		// system sbin/bin directories — see the matching comment in CNIAdd for
		// why (the vendored bridge plugin shells out to "iptables").
		"PATH=" + CNIBinDir + cniSystemPathDirs,
		"CNI_COMMAND=DEL",
		"CNI_CONTAINERID=" + containerID,
		"CNI_NETNS=" + netnsPath,
		"CNI_IFNAME=eth0",
		"CNI_PATH=" + CNIBinDir,
	}
	if runErr := cmd.Run(); runErr != nil {
		c.logger.Warn("CNI DEL failed (non-fatal)",
			zap.String("app_id", appID),
			zap.String("container_id", containerID),
			zap.Error(runErr))
	}
	return nil
}
