package discovery

import (
	"bufio"
	"net"
	"strings"
)

// hasIPv6LinkLocalFn and bridgeMasterFn are package vars so tests can inject
// fixtures without depending on the host's interfaces.
var (
	hasIPv6LinkLocalFn = hasIPv6LinkLocal
	bridgeMasterFn     = bridgeMaster
)

// LinkLocalDialZone returns the zone to dial an IPv6 link-local peer seen on
// iface. Usually that is iface itself, but a port enslaved to a bridge carries
// no IPv6 address of its own — the bridge does — so the kernel has no route
// for fe80::…%port and dials fail with "no route to host". macOS Internet
// Sharing does exactly this to a WendyOS USB gadget link (en10 becomes a
// member of bridge100), while mDNS still reports the answer as arriving on the
// member port. Dialing through the bridge reaches the same wire.
func LinkLocalDialZone(iface string) string {
	if iface == "" || hasIPv6LinkLocalFn(iface) {
		return iface
	}
	if master := bridgeMasterFn(iface); master != "" {
		return master
	}
	return iface
}

// hasIPv6LinkLocal reports whether iface holds an fe80::/10 address. An
// interface that cannot be looked up is treated as having one, so the zone is
// left untouched rather than guessed.
func hasIPv6LinkLocal(iface string) bool {
	ifc, err := net.InterfaceByName(iface)
	if err != nil {
		return true
	}
	addrs, err := ifc.Addrs()
	if err != nil {
		return true
	}
	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok && ipNet.IP.To4() == nil && ipNet.IP.IsLinkLocalUnicast() {
			return true
		}
	}
	return false
}

// parseBridgeMembers maps each bridge member to its bridge from BSD
// `ifconfig` output, where a bridge's stanza lists its ports as
// "\tmember: en10 flags=3<LEARNING,DISCOVER>".
func parseBridgeMembers(ifconfigOutput string) map[string]string {
	members := make(map[string]string)
	current := ""
	scanner := bufio.NewScanner(strings.NewReader(ifconfigOutput))
	for scanner.Scan() {
		line := scanner.Text()
		if line != "" && line[0] != ' ' && line[0] != '\t' {
			name, _, _ := strings.Cut(line, ":")
			current = name
			continue
		}
		fields := strings.Fields(line)
		if current != "" && len(fields) >= 2 && fields[0] == "member:" {
			members[fields[1]] = current
		}
	}
	return members
}
