package commands

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/discoverycache"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
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

// addrOnLinkFn and cableAnswersFn are seams for tests.
var (
	addrOnLinkFn   = discovery.OnLink
	cableAnswersFn = discovery.CableAnswers
)

// routesAtLeastAsWell reports whether this host's path to a ranks no worse
// than its path to b, so a device's cable never displaces a better route.
func routesAtLeastAsWell(a, b string) bool {
	return dialCandidateRoutePreferenceFn(a) >= dialCandidateRoutePreferenceFn(b)
}

// liveCable returns the first of a device's advertised cable addresses that is
// on one of this host's subnets, routes at least as well as current, and
// answers mDNS as host; "" when none does. A subnet match proves neither a path
// nor the device, so a cable never goes first without that answer.
func liveCable(ctx context.Context, wired []string, current, host string) string {
	for _, cable := range wired {
		if cable != current && addrOnLinkFn(cable) && routesAtLeastAsWell(cable, current) && askCable(ctx, cable, host) {
			return cable
		}
	}
	return ""
}

// answeringCables returns the cables in wired that resolved does not hold but
// that answer mDNS as host.
func answeringCables(ctx context.Context, host string, wired, resolved []string) []string {
	var asked, out []string
	for _, cable := range wired {
		if slices.Contains(resolved, cable) || slices.Contains(asked, cable) {
			continue
		}
		asked = append(asked, cable)
		if addrOnLinkFn(cable) && askCable(ctx, cable, host) {
			out = append(out, cable)
		}
	}
	return out
}

// askCable reports whether cable answers mDNS as host. A silent one leaves
// host's cached record, so later connects stop waiting on it; discovery adds
// it back once it answers.
func askCable(ctx context.Context, cable, host string) bool {
	if cableAnswersFn(ctx, cable, host) {
		return true
	}
	if ctx.Err() == nil {
		forgetCable(host, cable)
	}
	return false
}

// forgetCable drops cable from host's cached record, keeping the entry's age.
func forgetCable(host, cable string) {
	cache, err := deviceCacheLoadFn()
	if err != nil || cache == nil {
		return
	}
	e, ok := cachedDeviceEntry(cache, host)
	if !ok || !slices.Contains(e.WiredAddresses, cable) {
		return
	}
	e.WiredAddresses = slices.DeleteFunc(slices.Clone(e.WiredAddresses), func(a string) bool { return a == cable })
	cache.Replace(e, e.LastSeen)
	_ = cache.Flush(time.Now())
}

// preferLiveCable points a cached entry at a live cable. A cached link-local
// address is a direct USB link and stays.
func preferLiveCable(ctx context.Context, e discoverycache.Entry) discoverycache.Entry {
	ip := net.ParseIP(stripZone(e.IP))
	if ip == nil || ip.IsLinkLocalUnicast() || slices.Contains(e.WiredAddresses, e.IP) {
		return e
	}
	if cable := liveCable(ctx, e.WiredAddresses, e.IP, e.Hostname); cable != "" {
		e.IP = cable
	}
	return e
}

// preferCableCandidate makes a discovered device's live cable its primary
// address; the address it was found at stays in the walk. A USB link keeps its
// own ordering.
func preferCableCandidate(ctx context.Context, dev models.LANDevice) models.LANDevice {
	if dev.USB != "" || slices.Contains(dev.WiredAddresses, dev.IPAddress) {
		return dev
	}
	if cable := liveCable(ctx, dev.WiredAddresses, dev.IPAddress, dev.Hostname); cable != "" {
		dev.Addresses = append([]string{dev.IPAddress}, dev.Addresses...)
		dev.IPAddress = cable
	}
	return dev
}

func shouldUseCachedDeviceAddress(interfaceName, ip string) bool {
	return strings.TrimSpace(ip) != "" && cachedRoutePreference(interfaceName, ip) != routeWireless
}

// preferWiredDialCandidates stably promotes addresses routed over a wired
// interface of this host, then, among equally routed ones, the device's own
// cable addresses (deviceWired). Otherwise resolver order is kept, and every
// address remains in the exhaustive authenticated walk.
func preferWiredDialCandidates(candidates, deviceWired []string) []string {
	ordered := append([]string(nil), candidates...)
	preferences := make(map[string]routePreference, len(ordered))
	for _, candidate := range ordered {
		preferences[candidate] = dialCandidateRoutePreferenceFn(candidate)
	}
	cable := func(candidate string) bool { return slices.Contains(deviceWired, stripZone(candidate)) }
	sort.SliceStable(ordered, func(i, j int) bool {
		if pi, pj := preferences[ordered[i]], preferences[ordered[j]]; pi != pj {
			return pi > pj
		}
		return cable(ordered[i]) && !cable(ordered[j])
	})
	return ordered
}
