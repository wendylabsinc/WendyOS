package devices

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// MaxDialCandidates bounds the number of resolved endpoints attempted.
const MaxDialCandidates = 3

// HostPort brackets IPv6 literals, including scoped link-local addresses.
func HostPort(host string, port int) string {
	if addr, err := netip.ParseAddr(host); err == nil && addr.Is6() {
		return fmt.Sprintf("[%s]:%d", host, port)
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// StripZone removes an IPv6 zone for address-family classification only.
func StripZone(host string) string {
	if i := strings.IndexByte(host, '%'); i >= 0 {
		return host[:i]
	}
	return host
}

// OrderDialCandidates de-duplicates ips and orders them by how likely a dial to
// each is to reach a device on an ordinary LAN: IPv4 first, then routable IPv6,
// then link-local and ULA IPv6 last. Connect paths add host-interface preference
// with orderRoutedDialCandidates below.
//
// The ordering is a preference *within an exhaustive walk*, not a filter. The
// single-address resolvers this replaced also preferred IPv4, but they THREW
// THE REST AWAY, which is what let one stale record make a reachable device
// look unreachable. Anything unparseable is dropped rather than passed through:
// a non-address in this list can only produce a dial that cannot succeed.
// OrderDialCandidates orders addresses and applies the connection candidate cap.
func OrderDialCandidates(ips []string) []string {
	ordered := OrderDialCandidatesUnbounded(ips)
	if len(ordered) > MaxDialCandidates {
		ordered = ordered[:MaxDialCandidates]
	}
	return ordered
}

// OrderDialCandidatesUnbounded de-duplicates literal addresses, preferring IPv4,
// then global IPv6, then private/link-local IPv6; zones remain intact.
func OrderDialCandidatesUnbounded(ips []string) []string {
	var v4, v6Global, v6Local []string
	seen := map[string]bool{}
	for _, raw := range ips {
		ip := strings.TrimSpace(raw)
		if ip == "" || seen[ip] {
			continue
		}
		parsed := net.ParseIP(StripZone(ip))
		if parsed == nil {
			continue
		}
		seen[ip] = true
		switch {
		case parsed.To4() != nil:
			v4 = append(v4, ip)
		case parsed.IsLinkLocalUnicast() || parsed.IsPrivate():
			v6Local = append(v6Local, ip)
		default:
			v6Global = append(v6Global, ip)
		}
	}
	ordered := make([]string, 0, len(v4)+len(v6Global)+len(v6Local))
	ordered = append(ordered, v4...)
	ordered = append(ordered, v6Global...)
	ordered = append(ordered, v6Local...)
	return ordered
}

// DefaultAgentPort is the plaintext Agent port; TLS conventionally uses port+1.
const DefaultAgentPort = 50051

// AgentMTLSPortOffset is the advertised mTLS port offset.
const AgentMTLSPortOffset = 1

// LANDevice is host discovery evidence used to select addresses, never identity.
type LANDevice struct {
	Port      int
	IsMTLS    bool
	IPAddress string
	Hostname  string
	USB       string
}

// LANAgentPort restores the plaintext base port from an mTLS advertisement.
func LANAgentPort(dev LANDevice) int {
	port := dev.Port
	if port == 0 {
		port = DefaultAgentPort
	}
	if dev.IsMTLS && dev.Port != 0 && port > AgentMTLSPortOffset {
		port -= AgentMTLSPortOffset // advertised port is mTLS; connectWithAutoTLS will add the offset back
	}
	return port
}

// LANAgentAddresses preserves USB hostname and zoned literal preferences.
func LANAgentAddresses(dev LANDevice) []string {
	port := LANAgentPort(dev)

	ip, hostname := strings.TrimSpace(dev.IPAddress), strings.TrimSpace(dev.Hostname)
	hosts := []string{ip, hostname}
	// A zoned IPv6 literal (fe80::5741:1%enx0) can only come from the USB
	// well-known-address probe, which just proved the agent answers there. Such
	// a device is typically one whose mDNS is broken — that is why the probe
	// found it — so its .local name costs a full resolver timeout plus a dial
	// timeout before the address that works is even tried. It stays first.
	if strings.TrimSpace(dev.USB) != "" && !strings.Contains(ip, "%") {
		// A USB-NCM path exists. The routed Wi-Fi IP (dev.IPAddress) may be
		// black-holed by AP isolation on residential routers, so try the
		// link-local .local hostname (reachable over USB) first.
		hosts = []string{hostname, ip}
	}

	var addresses []string
	seen := make(map[string]bool)
	for _, host := range hosts {
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		addresses = append(addresses, HostPort(host, port))
	}

	return addresses
}

// PreferredLANAddress returns the first discovered endpoint, or empty.
func PreferredLANAddress(dev LANDevice) string {
	addresses := LANAgentAddresses(dev)
	if len(addresses) == 0 {
		return ""
	}
	return addresses[0]
}
