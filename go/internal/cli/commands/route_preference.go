package commands

import (
	"net"
	"net/netip"
	"sort"
	"strings"
)

// routePreference ranks the host-side path an address would use. It is kept
// deliberately small: route discovery is a performance hint, never a reason to
// discard an address or weaken the authenticated dial ladder.
type routePreference uint8

const (
	routeUnknown routePreference = iota
	routeWireless
	routeWired
)

// dialCandidateRoutePreferenceFn is a seam for deterministic candidate-order
// and cached-address tests.
var dialCandidateRoutePreferenceFn = dialCandidateRoutePreference

var networkInterfaceRoutePreferenceFn = networkInterfaceRoutePreference

var routeInterfaceForIPFn = routeInterfaceForIP

func dialCandidateRoutePreference(rawIP string) routePreference {
	return networkInterfaceRoutePreferenceFn(routeInterfaceForIPFn(rawIP))
}

// routeInterfaceForIP asks the kernel which source address it would use for a
// UDP route to rawIP, then maps that source back to an interface. DialUDP does
// not send a packet; this avoids interface-name assumptions.
func routeInterfaceForIP(rawIP string) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(rawIP))
	if err != nil {
		return ""
	}
	if addr.Zone() != "" {
		return addr.Zone()
	}
	remote := &net.UDPAddr{IP: net.ParseIP(addr.String()), Port: 9}
	conn, err := net.DialUDP("udp", nil, remote)
	if err != nil {
		return ""
	}
	localIP := conn.LocalAddr().(*net.UDPAddr).IP
	_ = conn.Close()

	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for i := range ifaces {
		addrs, err := ifaces[i].Addrs()
		if err != nil {
			continue
		}
		for _, candidate := range addrs {
			ip, _, err := net.ParseCIDR(candidate.String())
			if err == nil && ip.Equal(localIP) {
				return ifaces[i].Name
			}
		}
	}
	return ""
}

func cachedRoutePreference(interfaceName, ip string) routePreference {
	// A legacy/connect-minted cache row may not know which interface observed
	// it. Preserve that fast path; only positive discovery metadata that names a
	// Wi-Fi interface is enough to pay for fresh multi-address resolution.
	if routedInterface := routeInterfaceForIPFn(ip); routedInterface != "" && interfaceName != "" && routedInterface != interfaceName {
		// Discovery metadata and the kernel route must describe the same path.
		// Old Darwin discovery could label an answer en7 while globally resolving
		// its IP to en0; trusting that row forever pinned deploys to Wi-Fi.
		if networkInterfaceRoutePreferenceFn(interfaceName) != routeWired || networkInterfaceRoutePreferenceFn(routedInterface) != routeWired {
			return routeWireless
		}
	}
	return networkInterfaceRoutePreferenceFn(interfaceName)
}

func shouldUseCachedDeviceAddress(interfaceName, ip string) bool {
	return strings.TrimSpace(ip) != "" && cachedRoutePreference(interfaceName, ip) != routeWireless
}

// preferWiredDialCandidates stably promotes addresses routed over a wired
// interface. Unknown routes retain resolver order, and every address remains
// in the exhaustive authenticated walk.
func preferWiredDialCandidates(candidates []string) []string {
	ordered := append([]string(nil), candidates...)
	preferences := make(map[string]routePreference, len(ordered))
	for _, candidate := range ordered {
		preferences[candidate] = dialCandidateRoutePreferenceFn(candidate)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		return preferences[ordered[i]] > preferences[ordered[j]]
	})
	return ordered
}
