package hostnetwork

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"
)

// Keep the check-and-update pair atomic within the agent. -w in meshIPTables
// handles other processes; this mutex prevents two app workers from both
// observing an absent rule and appending duplicates for the same container.
var meshRuleMu sync.Mutex

// meshRuleArgs returns the iptables rule (sans -A/-C/-D verb) permitting
// exactly this container's IP to egress toward the mesh service CIDR. Shared
// by AddMeshRule and RemoveMeshRule so the two can never drift.
func meshRuleArgs(containerIP, serviceCIDR string) []string {
	return []string{
		"-t", "filter",
		"-s", containerIP + "/32",
		"-d", serviceCIDR,
		"-j", "ACCEPT",
	}
}

// AddMeshRule idempotently installs a host iptables ACCEPT rule in the
// WENDY-MESH chain scoping one meshed container's egress to the mesh service
// CIDR. It checks for the rule's presence first (`iptables -C`) and only
// appends (`-A`) if it is absent, so repeated calls (e.g. a retried container
// create) never produce duplicate rules.
func AddMeshRule(containerIP, serviceCIDR string) error {
	meshRuleMu.Lock()
	defer meshRuleMu.Unlock()
	exists, err := meshRuleExists(containerIP, serviceCIDR)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	args := append([]string{"-A", MeshChainName}, meshRuleArgs(containerIP, serviceCIDR)...)
	out, err := meshIPTables(args...)
	if err != nil {
		return fmt.Errorf("iptables -A %s: %w (%s)", MeshChainName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RemoveMeshRule idempotently removes the host iptables ACCEPT rule for one
// container's egress to the mesh service CIDR. Removing a rule (or from a
// chain) that is already absent is treated as success.
func RemoveMeshRule(containerIP, serviceCIDR string) error {
	meshRuleMu.Lock()
	defer meshRuleMu.Unlock()
	exists, err := meshRuleExists(containerIP, serviceCIDR)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	args := append([]string{"-D", MeshChainName}, meshRuleArgs(containerIP, serviceCIDR)...)
	out, err := meshIPTables(args...)
	if err != nil {
		return fmt.Errorf("iptables -D %s: %w (%s)", MeshChainName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// meshRuleExists checks for an existing per-container ACCEPT rule via
// `iptables -C`, whose exit code directly encodes presence: 0 means the rule
// exists, 1 means it doesn't (including when the chain itself is missing),
// and anything else is a real error.
func meshRuleExists(containerIP, serviceCIDR string) (bool, error) {
	args := append([]string{"-C", MeshChainName}, meshRuleArgs(containerIP, serviceCIDR)...)
	out, err := meshIPTables(args...)
	if err == nil {
		return true, nil
	}
	if exitCode(err) == 1 {
		return false, nil
	}
	return false, fmt.Errorf("iptables -C %s: %w (%s)", MeshChainName, err, strings.TrimSpace(string(out)))
}

// SetMeshRoute installs a route inside a container's network namespace
// directing the mesh service CIDR via the mesh gateway. It shells out to
// nsenter + ip route, mirroring the agent's exec-based approach to host
// networking (see InitMeshChain) rather than adding a netlink dependency.
//
// `ip route replace` (not `ip route add`) is used so a retried call against
// the same netns is idempotent instead of failing with "File exists".
//
// serviceCIDR and gateway are validated before being placed on the exec
// argument list. They are internal-derived (not user input), but validating
// defensively costs little and catches programming errors early; since args
// are passed as separate exec arguments (never a shell string), there is no
// injection risk regardless.
func SetMeshRoute(netnsPath, serviceCIDR, gateway string) error {
	if _, _, err := net.ParseCIDR(serviceCIDR); err != nil {
		return fmt.Errorf("parsing serviceCIDR %q: %w", serviceCIDR, err)
	}
	if net.ParseIP(gateway) == nil {
		return fmt.Errorf("parsing gateway %q: invalid IP", gateway)
	}

	out, err := exec.Command("nsenter", "--net="+netnsPath, "--",
		"ip", "route", "replace", serviceCIDR, "via", gateway).CombinedOutput()
	if err != nil {
		return fmt.Errorf("nsenter --net=%s -- ip route replace %s via %s: %w (%s)",
			netnsPath, serviceCIDR, gateway, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// SetMeshMDNSRoute lets an isolated mesh app use ordinary DNS-SD multicast on
// its own CNI link. The /32 stays inside that app's network namespace; it does
// not grant general multicast or Internet egress.
func SetMeshMDNSRoute(netnsPath, iface string) error {
	if netnsPath == "" || iface == "" {
		return fmt.Errorf("mesh mDNS route needs a namespace and interface")
	}
	out, err := exec.Command("nsenter", "--net="+netnsPath, "--",
		"ip", "route", "replace", "224.0.0.251/32", "dev", iface, "scope", "link").CombinedOutput()
	if err != nil {
		return fmt.Errorf("nsenter --net=%s -- ip route replace mDNS dev %s: %w (%s)",
			netnsPath, iface, err, strings.TrimSpace(string(out)))
	}
	return nil
}
