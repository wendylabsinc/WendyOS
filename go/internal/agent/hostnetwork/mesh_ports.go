package hostnetwork

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// EnableRouteLocalnet turns on net.ipv4.conf.<bridge>.route_localnet for the
// given bridge interface. Without it, the kernel's own martian-source check
// can drop a packet whose source is 127.0.0.1 once it's routed out a
// non-loopback interface — which is exactly what happens to the DNAT'd
// MeshDial hairpin (see meshPortForwardArgs/meshPortMasqueradeArgs): the
// destination is rewritten from 127.0.0.1 to the container's IP, forcing a
// re-route out the bridge, but the source is still 127.0.0.1 until
// POSTROUTING's MASQUERADE rule runs — and on some kernels that martian
// check happens before NAT gets a chance to fix up the source. Idempotent
// (repeated writes of the same value are harmless); best-effort, since a
// non-Linux dev host or a missing /proc/sys path must not block a container
// from starting.
func EnableRouteLocalnet(bridge string) error {
	path := fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/route_localnet", bridge)
	if err := os.WriteFile(path, []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("enabling route_localnet on %s: %w", bridge, err)
	}
	return nil
}

// MeshPortsChainName is the nat-table chain that forwards a loopback
// connection the agent itself dials into a meshed, isolated container's own
// IP. MeshService.MeshDial (the serving side of a peer-initiated MeshDial)
// always dials 127.0.0.1:<port> from the host network namespace — the agent
// process is not inside any container's netns, so without this forward
// nothing is ever listening on that loopback port and the dial fails with
// "connection refused" for every isolated mesh app that publishes a port.
const MeshPortsChainName = "WENDY-MESH-PORTS"

// The agent reconciles ingress while containerd and other startup services
// may also update iptables. Wait a bounded five seconds for xtables.lock;
// callers still fail closed if the lock cannot be acquired.
func meshPortsIPTables(args ...string) ([]byte, error) {
	return exec.Command("iptables", append([]string{"-w", "5"}, args...)...).CombinedOutput()
}

// InitMeshPortsChain ensures the WENDY-MESH-PORTS chain exists in the nat
// table and that OUTPUT jumps into it. Idempotent and safe on every agent
// startup, mirroring InitMeshNATChain — but hooked to OUTPUT (locally
// generated traffic, i.e. the agent's own MeshDial dial) rather than
// PREROUTING (traffic arriving from elsewhere).
func InitMeshPortsChain() error {
	if err := ensureNATChain(MeshPortsChainName); err != nil {
		return fmt.Errorf("hostnetwork: ensure nat chain %s: %w", MeshPortsChainName, err)
	}
	if err := ensureOutputJump(MeshPortsChainName); err != nil {
		return fmt.Errorf("hostnetwork: ensure OUTPUT jump to %s: %w", MeshPortsChainName, err)
	}
	return nil
}

// ensureOutputJump appends an `OUTPUT -j <chain>` rule only if one is not
// already present, so repeated calls never create duplicate jump rules.
// Mirrors ensurePreroutingJump in mesh_redirect.go.
func ensureOutputJump(chain string) error {
	out, err := meshPortsIPTables("-t", "nat", "-C", "OUTPUT", "-j", chain)
	if err == nil {
		return nil
	}
	if exitCode(err) != 1 {
		return fmt.Errorf("iptables -t nat -C OUTPUT -j %s: %w (%s)", chain, err, strings.TrimSpace(string(out)))
	}
	out, err = meshPortsIPTables("-t", "nat", "-A", "OUTPUT", "-j", chain)
	if err != nil {
		return fmt.Errorf("iptables -t nat -A OUTPUT -j %s: %w (%s)", chain, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// meshPortForwardArgs returns the nat rule (sans verb) that DNATs a
// loopback-destined connection on hostPort to containerIP:containerPort.
// Shared by add/remove/check so the three can never drift.
func meshPortForwardArgs(hostPort uint16, containerIP string, containerPort uint16) []string {
	return []string{
		"-t", "nat",
		"-d", "127.0.0.1",
		"-p", "tcp",
		"--dport", strconv.Itoa(int(hostPort)),
		"-j", "DNAT",
		"--to-destination", net.JoinHostPort(containerIP, strconv.Itoa(int(containerPort))),
	}
}

// meshPortMasqueradeArgs returns the POSTROUTING nat rule (sans verb) that
// rewrites the source of a DNAT'd loopback dial to the host's own
// bridge-facing address. Without this, the packet still carries source
// 127.0.0.1 after meshPortForwardArgs' DNAT rewrites its destination —  and
// 127.0.0.1 is the CONTAINER's own loopback from inside its netns, so its
// SYN-ACK reply routes nowhere and the connection hangs until it times out.
// This is the standard NAT-hairpin fix (the same thing Docker's own
// port-publish masquerading does): MASQUERADE picks the correct outgoing
// address for whatever interface the now-rerouted packet actually leaves on.
// Scoped to exactly this (127.0.0.1 -> containerIP:containerPort) flow so it
// cannot affect any other traffic reaching the container (e.g. real LAN or
// cross-container mesh traffic, which already carries a valid, routable
// source and must not be masqueraded).
func meshPortMasqueradeArgs(containerIP string, containerPort uint16) []string {
	return []string{
		"-t", "nat",
		"-s", "127.0.0.1",
		"-d", containerIP,
		"-p", "tcp",
		"--dport", strconv.Itoa(int(containerPort)),
		"-j", "MASQUERADE",
	}
}

// flushStaleRulesForPort removes every existing rule in chain (nat table)
// whose args mention "--dport <port>", regardless of what destination/target
// it otherwise carries, then returns the CLEANED args unchanged (callers add
// their own fresh rule afterward). Without this, a container that gets
// redeployed with a new IP (the common case — every `wendy run`/`apps
// remove`+redeploy cycle allocates a fresh CNI IP) leaves its OLD DNAT/
// MASQUERADE rule in place forever if the old container's teardown was ever
// skipped (e.g. removed after its own CNI ADD had failed, so no forward was
// ever recorded against it to clean up) or simply raced with a fresh ADD.
// iptables then has two rules matching the same hostPort/containerPort, and
// -C/-A only ever check for exact-match presence — they never notice or
// replace a DIFFERENT stale rule for the same port, so the OLDEST match wins
// every time traffic actually arrives, silently DNATing to a containerIP
// that no longer has a route (found via RemoteCam demo debugging: repeated
// "dial tcp 127.0.0.1:9090: connect: no route to host" pointing at a
// long-gone bridge IP even though the CURRENT container's own forward had
// just been installed correctly).
// requireSubstr, when non-empty, must also appear in the rule line — used so
// a flush of the shared, not-fully-owned POSTROUTING chain only ever touches
// rules carrying our own exact MASQUERADE signature (127.0.0.1 + MASQUERADE),
// never an unrelated rule some other app happens to have installed against
// the same port number. The fully-owned MeshPortsChainName flush passes ""
// since every rule in that chain is already known to be ours.
func flushStaleRulesForPort(chain string, port uint16, requireSubstr, protocol string) error {
	out, err := meshPortsIPTables("-t", "nat", "-S", chain)
	if err != nil {
		return fmt.Errorf("iptables -t nat -S %s: %w (%s)", chain, err, strings.TrimSpace(string(out)))
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		fields := strings.Fields(line)
		if len(fields) == 0 || iptablesOption(fields, "--dport") != strconv.Itoa(int(port)) {
			continue
		}
		if !strings.Contains(line, "-p "+protocol+" ") {
			continue
		}
		if requireSubstr != "" && !strings.Contains(line, requireSubstr) {
			continue
		}
		if chain == "POSTROUTING" && (!strings.Contains(line, "-s 127.0.0.1/32 ") || !strings.HasSuffix(line, "-j MASQUERADE")) {
			continue
		}
		if len(fields) == 0 || fields[0] != "-A" {
			continue
		}
		fields[0] = "-D"
		delArgs := append([]string{"-t", "nat"}, fields...)
		if delOut, delErr := meshPortsIPTables(delArgs...); delErr != nil {
			return fmt.Errorf("iptables -t nat -D %s (stale rule cleanup): %w (%s)", chain, delErr, strings.TrimSpace(string(delOut)))
		}
	}
	return nil
}

func iptablesOption(fields []string, option string) string {
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == option {
			return fields[i+1]
		}
	}
	return ""
}

// FlushOrphanMeshPort removes a forward for the opposite protocol only when
// the ingress registry has already proved that no live app owns that protocol
// and host port. This handles a redeploy that changes TCP to UDP (or back)
// after a stale task left its previous rule behind. The caller serializes the
// registry check and this cleanup with other app ingress setup.
func FlushOrphanMeshPort(hostPort uint16, protocol string) error {
	if hostPort == 0 || protocol != "tcp" && protocol != "udp" {
		return fmt.Errorf("invalid orphan mesh port cleanup")
	}
	out, err := meshPortsIPTables("-t", "nat", "-S", MeshPortsChainName)
	if err != nil {
		return fmt.Errorf("listing mesh ingress forwards: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "-A" || fields[1] != MeshPortsChainName ||
			iptablesOption(fields, "-p") != protocol ||
			iptablesOption(fields, "--dport") != strconv.Itoa(int(hostPort)) ||
			iptablesOption(fields, "-d") != "127.0.0.1/32" ||
			iptablesOption(fields, "-j") != "DNAT" {
			continue
		}
		target := iptablesOption(fields, "--to-destination")
		ip, portText, err := net.SplitHostPort(target)
		if err != nil || net.ParseIP(ip).To4() == nil {
			return fmt.Errorf("invalid Wendy mesh ingress target %q", target)
		}
		portValue, err := strconv.ParseUint(portText, 10, 16)
		if err != nil || portValue == 0 {
			return fmt.Errorf("invalid Wendy mesh ingress target port %q", target)
		}
		if delOut, err := meshPortsIPTables(append([]string{"-t", "nat", "-D", MeshPortsChainName}, fields[2:]...)...); err != nil {
			return fmt.Errorf("removing orphan mesh ingress: %w (%s)", err, strings.TrimSpace(string(delOut)))
		}
		if err := flushStaleRulesForPort("POSTROUTING", uint16(portValue), "-d "+ip+"/32", protocol); err != nil {
			return err
		}
	}
	return nil
}

// AddIngressPortForward idempotently installs the DNAT rule that lets
// MeshService.MeshDial's 127.0.0.1:hostPort dial reach containerIP:containerPort
// instead, plus the companion POSTROUTING MASQUERADE rule the hairpinned
// reply needs (see meshPortMasqueradeArgs). Any stale rule left over from a
// previous container that published the same hostPort/containerPort is
// flushed first (see flushStaleRulesForPort) so exactly one forward for this
// port is ever active.
func AddIngressPortForward(hostPort uint16, containerIP string, containerPort uint16) error {
	if err := flushStaleRulesForPort(MeshPortsChainName, hostPort, "", "tcp"); err != nil {
		return fmt.Errorf("flushing stale ingress forwards for port %d: %w", hostPort, err)
	}
	// A different app may publish the same container port behind a different
	// host port. Its hairpin rule must remain in place. Only clear a stale
	// rule for this exact container IP and port.
	if err := flushStaleRulesForPort("POSTROUTING", containerPort, "-d "+containerIP+"/32", "tcp"); err != nil {
		return fmt.Errorf("flushing stale ingress masquerades for port %d: %w", containerPort, err)
	}

	exists, err := meshPortForwardExists(hostPort, containerIP, containerPort)
	if err != nil {
		return err
	}
	if !exists {
		args := append([]string{"-A", MeshPortsChainName}, meshPortForwardArgs(hostPort, containerIP, containerPort)...)
		out, err := meshPortsIPTables(args...)
		if err != nil {
			return fmt.Errorf("iptables -t nat -A %s: %w (%s)", MeshPortsChainName, err, strings.TrimSpace(string(out)))
		}
	}

	masqExists, err := meshPortMasqueradeExists(containerIP, containerPort)
	if err != nil {
		return err
	}
	if !masqExists {
		args := append([]string{"-A", "POSTROUTING"}, meshPortMasqueradeArgs(containerIP, containerPort)...)
		out, err := meshPortsIPTables(args...)
		if err != nil {
			return fmt.Errorf("iptables -t nat -A POSTROUTING: %w (%s)", err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// RemoveIngressPortForward idempotently removes the DNAT and MASQUERADE
// rules installed by AddIngressPortForward.
func RemoveIngressPortForward(hostPort uint16, containerIP string, containerPort uint16) error {
	exists, err := meshPortForwardExists(hostPort, containerIP, containerPort)
	if err != nil {
		return err
	}
	if exists {
		args := append([]string{"-D", MeshPortsChainName}, meshPortForwardArgs(hostPort, containerIP, containerPort)...)
		out, err := meshPortsIPTables(args...)
		if err != nil {
			return fmt.Errorf("iptables -t nat -D %s: %w (%s)", MeshPortsChainName, err, strings.TrimSpace(string(out)))
		}
	}

	masqExists, err := meshPortMasqueradeExists(containerIP, containerPort)
	if err != nil {
		return err
	}
	if masqExists {
		args := append([]string{"-D", "POSTROUTING"}, meshPortMasqueradeArgs(containerIP, containerPort)...)
		out, err := meshPortsIPTables(args...)
		if err != nil {
			return fmt.Errorf("iptables -t nat -D POSTROUTING: %w (%s)", err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func meshPortForwardExists(hostPort uint16, containerIP string, containerPort uint16) (bool, error) {
	args := append([]string{"-C", MeshPortsChainName}, meshPortForwardArgs(hostPort, containerIP, containerPort)...)
	out, err := meshPortsIPTables(args...)
	if err == nil {
		return true, nil
	}
	if exitCode(err) == 1 {
		return false, nil
	}
	return false, fmt.Errorf("iptables -t nat -C %s: %w (%s)", MeshPortsChainName, err, strings.TrimSpace(string(out)))
}

func meshPortMasqueradeExists(containerIP string, containerPort uint16) (bool, error) {
	args := append([]string{"-C", "POSTROUTING"}, meshPortMasqueradeArgs(containerIP, containerPort)...)
	out, err := meshPortsIPTables(args...)
	if err == nil {
		return true, nil
	}
	if exitCode(err) == 1 {
		return false, nil
	}
	return false, fmt.Errorf("iptables -t nat -C POSTROUTING: %w (%s)", err, strings.TrimSpace(string(out)))
}
