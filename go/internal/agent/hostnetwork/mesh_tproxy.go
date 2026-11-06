//go:build linux

package hostnetwork

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

const MeshUDPChainName = "WENDY-MESH-UDP"
const meshUDPMark = "0x40000000/0x40000000"
const meshUDPTable = "18867"
const meshUDPPriority = "18864"

type meshUDPInterceptOwner struct {
	ip, cidr  string
	proxyPort int
}

var meshUDPInterceptMu sync.Mutex
var meshUDPInterceptOwners = make(map[string]meshUDPInterceptOwner)

func meshUDPCommand(program string, args ...string) (string, error) {
	if program == "iptables" {
		args = append([]string{"-w", "5"}, args...)
	}
	out, err := exec.Command(program, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w (%s)", program, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func meshUDPRuleExists(chain string, args []string) (bool, error) {
	out, err := exec.Command("iptables", append([]string{"-w", "5", "-t", "mangle", "-C", chain}, args...)...).CombinedOutput()
	if err == nil {
		return true, nil
	}
	if exitCode(err) == 1 {
		return false, nil
	}
	return false, fmt.Errorf("checking UDP TPROXY rule: %w (%s)", err, strings.TrimSpace(string(out)))
}

// InitMeshUDPTProxy owns one mangle chain, one mark bit, and one policy table.
// It refuses a foreign rule at our policy priority/table and probes the kernel
// TPROXY target before reporting readiness to the app lifecycle.
func InitMeshUDPTProxy(proxyPort int) error {
	if proxyPort <= 0 || proxyPort > 65535 {
		return errors.New("invalid net UDP proxy port")
	}
	// br_netfilter invokes IPv4 PREROUTING while the packet is still being
	// bridged. TPROXY can mark it there, but the bridge then consumes the
	// packet before the local UDP socket sees it. The Wendy CNI bridge sends
	// routed app traffic through the ordinary IPv4 hook when this sysctl is
	// zero (or br_netfilter is not loaded, as on the target images).
	if value, err := os.ReadFile("/proc/sys/net/bridge/bridge-nf-call-iptables"); err == nil && strings.TrimSpace(string(value)) != "0" {
		return errors.New("net UDP TPROXY requires net.bridge.bridge-nf-call-iptables=0")
	}
	if out, err := exec.Command("iptables", "-w", "5", "-t", "mangle", "-N", MeshUDPChainName).CombinedOutput(); err != nil && !strings.Contains(string(out), "already exists") {
		return fmt.Errorf("creating net UDP chain: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	// An impossible tuple checks the actual xt_TPROXY kernel target without
	// catching host or app traffic while the probe rule exists.
	probe := []string{"-s", "0.0.0.0/32", "-d", "0.0.0.0/32", "-p", "udp", "-j", "TPROXY", "--on-port", strconv.Itoa(proxyPort), "--tproxy-mark", meshUDPMark}
	if _, err := meshUDPCommand("iptables", append([]string{"-t", "mangle", "-A", MeshUDPChainName}, probe...)...); err != nil {
		return fmt.Errorf("UDP VIP interception requires kernel xt_TPROXY/NF_TPROXY_IPV4: %w", err)
	}
	if _, err := meshUDPCommand("iptables", append([]string{"-t", "mangle", "-D", MeshUDPChainName}, probe...)...); err != nil {
		return err
	}
	if err := EnsureMeshUDPTProxyPolicy(); err != nil {
		return err
	}
	jump := []string{"-j", MeshUDPChainName}
	if exists, err := meshUDPRuleExists("PREROUTING", jump); err != nil {
		return err
	} else if !exists {
		if _, err := meshUDPCommand("iptables", "-t", "mangle", "-A", "PREROUTING", "-j", MeshUDPChainName); err != nil {
			return err
		}
	}
	// A previous agent may have stopped mid-recovery. Only this dedicated
	// chain is flushed; live app rules are then rebuilt from verified tasks.
	meshUDPInterceptMu.Lock()
	defer meshUDPInterceptMu.Unlock()
	_, err := meshUDPCommand("iptables", "-t", "mangle", "-F", MeshUDPChainName)
	if err == nil {
		clear(meshUDPInterceptOwners)
	}
	return err
}

// EnsureMeshUDPTProxyPolicy repairs the marked packet route if another
// network manager removes it after agent startup. It leaves app rules intact.
func EnsureMeshUDPTProxyPolicy() error {
	rule, route, err := meshUDPPolicyState()
	if err != nil {
		return err
	}
	if !route {
		if _, err := meshUDPCommand("ip", "-4", "route", "add", "local", "0.0.0.0/0", "dev", "lo", "table", meshUDPTable); err != nil {
			return err
		}
	}
	if !rule {
		if _, err := meshUDPCommand("ip", "-4", "rule", "add", "pref", meshUDPPriority, "fwmark", meshUDPMark, "lookup", meshUDPTable); err != nil {
			return err
		}
	}
	return nil
}

func ownedMeshUDPPolicyRule(line string) bool {
	return strings.Join(strings.Fields(line), " ") == meshUDPPriority+": from all fwmark "+meshUDPMark+" lookup "+meshUDPTable
}
func ownedMeshUDPPolicyRoute(line string) bool {
	return strings.Join(strings.Fields(line), " ") == "local default dev lo scope host"
}

// Validate the complete installed skeleton, including implicit source scope.
// Matching only the mark/table could adopt a foreign source-specific rule.
func meshUDPPolicyState() (rule, route bool, err error) {
	rules, err := meshUDPCommand("ip", "-4", "rule", "show", "pref", meshUDPPriority)
	if err != nil {
		return false, false, err
	}
	ruleLines := nonemptyLines(rules)
	if len(ruleLines) > 1 || len(ruleLines) == 1 && !ownedMeshUDPPolicyRule(ruleLines[0]) {
		return false, false, errors.New("foreign IPv4 policy rule owns net UDP priority 18864")
	}
	routes, routeErr := meshUDPCommand("ip", "-4", "route", "show", "table", meshUDPTable)
	if routeErr != nil && !strings.Contains(routes, "FIB table does not exist") {
		return false, false, routeErr
	}
	routeLines := nonemptyLines(routes)
	if routeErr != nil {
		routeLines = nil
	}
	if len(routeLines) > 1 || len(routeLines) == 1 && !ownedMeshUDPPolicyRoute(routeLines[0]) {
		return false, false, errors.New("foreign route owns net UDP table 18867")
	}
	return len(ruleLines) == 1, len(routeLines) == 1, nil
}

func nonemptyLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func meshUDPInterceptArgs(ip, cidr string, proxyPort int) []string {
	return []string{"-s", ip + "/32", "-d", cidr, "-p", "udp", "-j", "TPROXY", "--on-port", strconv.Itoa(proxyPort), "--tproxy-mark", meshUDPMark}
}

// AddMeshUDPIntercept tracks one service container, so sibling services on the
// same app bridge retain independent rules while a redeploy replaces its IP.
func AddMeshUDPIntercept(ip, cidr, ownerID string, proxyPort int) error {
	meshUDPInterceptMu.Lock()
	defer meshUDPInterceptMu.Unlock()
	owner := meshUDPInterceptOwner{ip, cidr, proxyPort}
	if old, ok := meshUDPInterceptOwners[ownerID]; ok && old != owner {
		if err := removeMeshUDPInterceptRule(meshUDPInterceptArgs(old.ip, old.cidr, old.proxyPort)); err != nil {
			return fmt.Errorf("replacing stale net UDP interception for %s: %w", ownerID, err)
		}
		delete(meshUDPInterceptOwners, ownerID)
	}
	args := meshUDPInterceptArgs(ip, cidr, proxyPort)
	if exists, err := meshUDPRuleExists(MeshUDPChainName, args); err != nil {
		return err
	} else if exists {
		meshUDPInterceptOwners[ownerID] = owner
		return nil
	}
	_, err := meshUDPCommand("iptables", append([]string{"-t", "mangle", "-A", MeshUDPChainName}, args...)...)
	if err == nil {
		meshUDPInterceptOwners[ownerID] = owner
	}
	return err
}

func RemoveMeshUDPIntercept(ip, cidr, ownerID string, proxyPort int) error {
	meshUDPInterceptMu.Lock()
	defer meshUDPInterceptMu.Unlock()
	owner := meshUDPInterceptOwner{ip, cidr, proxyPort}
	if err := removeMeshUDPInterceptRule(meshUDPInterceptArgs(ip, cidr, proxyPort)); err != nil {
		return err
	}
	if meshUDPInterceptOwners[ownerID] == owner {
		delete(meshUDPInterceptOwners, ownerID)
	}
	return nil
}

func removeMeshUDPInterceptRule(args []string) error {
	if exists, err := meshUDPRuleExists(MeshUDPChainName, args); err != nil {
		return err
	} else if !exists {
		return nil
	}
	_, err := meshUDPCommand("iptables", append([]string{"-t", "mangle", "-D", MeshUDPChainName}, args...)...)
	return err
}

func CloseMeshUDPTProxy() error {
	meshUDPInterceptMu.Lock()
	defer meshUDPInterceptMu.Unlock()
	rule, route, err := meshUDPPolicyState()
	if err != nil {
		return err
	}
	// A retry may start after the chain was deleted but a policy deletion
	// failed. Missing owned state is already retired, not a cleanup failure.
	_, chainErr := meshUDPCommand("iptables", "-t", "mangle", "-S", MeshUDPChainName)
	var exitErr *exec.ExitError
	if chainErr != nil && (!errors.As(chainErr, &exitErr) || exitErr.ExitCode() != 1) {
		return chainErr
	}
	if chainErr == nil {
		if _, err := meshUDPCommand("iptables", "-t", "mangle", "-F", MeshUDPChainName); err != nil {
			return err
		}
		if exists, err := meshUDPRuleExists("PREROUTING", []string{"-j", MeshUDPChainName}); err != nil {
			return err
		} else if exists {
			if _, err := meshUDPCommand("iptables", "-t", "mangle", "-D", "PREROUTING", "-j", MeshUDPChainName); err != nil {
				return err
			}
		}
		if _, err := meshUDPCommand("iptables", "-t", "mangle", "-X", MeshUDPChainName); err != nil {
			return err
		}
	}
	clear(meshUDPInterceptOwners)
	if rule {
		if _, err := meshUDPCommand("ip", "-4", "rule", "del", "pref", meshUDPPriority, "fwmark", meshUDPMark, "lookup", meshUDPTable); err != nil {
			return err
		}
	}
	if route {
		_, err = meshUDPCommand("ip", "-4", "route", "del", "local", "0.0.0.0/0", "dev", "lo", "table", meshUDPTable)
	}
	return err
}
