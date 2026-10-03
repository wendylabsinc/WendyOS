package hostnetwork

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

func meshUDPForwardArgs(host uint16, ip string, container uint16) []string {
	return []string{"-t", "nat", "-d", "127.0.0.1", "-p", "udp", "--dport", strconv.Itoa(int(host)),
		"-j", "DNAT", "--to-destination", net.JoinHostPort(ip, strconv.Itoa(int(container)))}
}

func meshUDPMasqueradeArgs(ip string, container uint16) []string {
	return []string{"-t", "nat", "-s", "127.0.0.1", "-d", ip, "-p", "udp", "--dport", strconv.Itoa(int(container)), "-j", "MASQUERADE"}
}

func meshUDPPortRule(verb, chain string, args []string) (bool, error) {
	cmd := append([]string{verb, chain}, args...)
	out, err := meshPortsIPTables(cmd...)
	if err == nil {
		return true, nil
	}
	if verb == "-C" && exitCode(err) == 1 {
		return false, nil
	}
	return false, fmt.Errorf("iptables -t nat %s %s: %w (%s)", verb, chain, err, strings.TrimSpace(string(out)))
}

// AddIngressUDPPortForward publishes an explicitly entitled UDP port to the
// host agent. TCP rules of the same port remain independent.
func AddIngressUDPPortForward(host uint16, ip string, container uint16) error {
	if err := flushStaleRulesForPort(MeshPortsChainName, host, "", "udp"); err != nil {
		return err
	}
	if err := flushStaleUDPMasquerades(ip, container); err != nil {
		return err
	}
	for _, rule := range []struct {
		chain string
		args  []string
	}{
		{MeshPortsChainName, meshUDPForwardArgs(host, ip, container)},
		{"POSTROUTING", meshUDPMasqueradeArgs(ip, container)},
	} {
		if ok, err := meshUDPPortRule("-C", rule.chain, rule.args); err != nil {
			return err
		} else if !ok {
			if _, err := meshUDPPortRule("-A", rule.chain, rule.args); err != nil {
				return err
			}
		}
	}
	return nil
}

// Only the exact Wendy loopback hairpin signature is ours to remove.
func flushStaleUDPMasquerades(ip string, container uint16) error {
	out, err := meshPortsIPTables("-t", "nat", "-S", "POSTROUTING")
	if err != nil {
		return fmt.Errorf("listing UDP hairpin rules: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "-A POSTROUTING ") ||
			!strings.Contains(line, "-s 127.0.0.1/32 ") ||
			!strings.Contains(line, "-d "+ip+"/32 ") ||
			!strings.Contains(line, "-p udp ") ||
			!strings.Contains(line, "--dport "+strconv.Itoa(int(container))+" ") ||
			!strings.HasSuffix(line, "-j MASQUERADE") {
			continue
		}
		fields := strings.Fields(line)
		if delOut, err := meshPortsIPTables(append([]string{"-t", "nat", "-D", "POSTROUTING"}, fields[2:]...)...); err != nil {
			return fmt.Errorf("removing stale Wendy UDP hairpin: %w (%s)", err, strings.TrimSpace(string(delOut)))
		}
	}
	return nil
}

func RemoveIngressUDPPortForward(host uint16, ip string, container uint16) error {
	for _, rule := range []struct {
		chain string
		args  []string
	}{
		{MeshPortsChainName, meshUDPForwardArgs(host, ip, container)},
		{"POSTROUTING", meshUDPMasqueradeArgs(ip, container)},
	} {
		if ok, err := meshUDPPortRule("-C", rule.chain, rule.args); err != nil {
			return err
		} else if ok {
			if _, err := meshUDPPortRule("-D", rule.chain, rule.args); err != nil {
				return err
			}
		}
	}
	return nil
}
