//go:build linux

package localmesh

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// recoverStaleKernel removes state left by a dead agent. Every object is
// inspected before any mutation; an unfamiliar object in a reserved name,
// rule priority, or route table fails startup instead of being overwritten.
func recoverStaleKernel() error {
	links, err := netlink.LinkList()
	if err != nil {
		return fmt.Errorf("inspect local-mesh links: %w", err)
	}
	var dummy netlink.Link
	peers := make(map[string]netlink.Link)
	peerIndices := make(map[int]bool)
	for _, link := range links {
		name := link.Attrs().Name
		switch {
		case name == InterfaceName:
			if link.Type() != "dummy" || link.Attrs().Alias != interfaceOwner {
				return errors.New("local-mesh interface ownership conflict")
			}
			dummy = link
		case peerLinkName(name):
			if link.Type() != "tuntap" || link.Attrs().Alias != interfaceOwner {
				return fmt.Errorf("local-mesh peer interface %s ownership conflict", name)
			}
			peers[name] = link
			peerIndices[link.Attrs().Index] = true
		}
	}
	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("inspect local-mesh rules: %w", err)
	}
	var ownedRules []netlink.Rule
	for _, rule := range rules {
		if rule.Table != RouteTable && rule.Priority != RouteTable && rule.Priority != RouteTable+1 {
			continue
		}
		if rule.Priority == RouteTable && baseRuleMatches(rule) {
			ownedRules = append(ownedRules, rule)
			continue
		}
		if rule.Table == RouteTable && peerLinkName(rule.IifName) &&
			peerRuleMatches(rule, &netlink.Rule{Priority: RouteTable + 1, IifName: rule.IifName}) {
			// A crashed nonpersistent TUN may already be gone. If a link with
			// this name still exists, the link audit above verified its owner.
			ownedRules = append(ownedRules, rule)
			continue
		}
		return fmt.Errorf("local-mesh rule ownership conflict at priority %d", rule.Priority)
	}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: RouteTable}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return fmt.Errorf("inspect local-mesh routes: %w", err)
	}
	var ownedRoutes []netlink.Route
	for _, route := range routes {
		if !staleMeshRoute(route, peerIndices) {
			return fmt.Errorf("local-mesh route table ownership conflict: %s", route.String())
		}
		ownedRoutes = append(ownedRoutes, route)
	}
	// A roaming default lives in main, outside the reserved mesh table.
	// Inspect it only when other stale local-mesh state proves this is a
	// recovery, leaving unrelated ordinary main-table routes untouched.
	var roam *netlink.Route
	if dummy != nil || len(peers) != 0 || len(ownedRules) != 0 || len(ownedRoutes) != 0 {
		mainRoutes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_TABLE)
		if err != nil {
			return fmt.Errorf("inspect local-mesh roaming route: %w", err)
		}
		for i := range mainRoutes {
			route := mainRoutes[i]
			if route.Priority != 5 || !defaultIPv4Route(route.Dst) {
				continue
			}
			if roam != nil || !staleRoamRoute(route, peerIndices) {
				return errors.New("local-mesh roaming default ownership conflict")
			}
			roam = &route
		}
	}
	// Remove rules first, so removing routes cannot make mesh traffic fall
	// through to the host's unrelated default route.
	for _, rule := range ownedRules {
		if err := netlink.RuleDel(&rule); err != nil && !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("remove stale local-mesh rule: %w", err)
		}
	}
	if roam != nil {
		if err := netlink.RouteDel(roam); err != nil && !errors.Is(err, unix.ESRCH) {
			return fmt.Errorf("remove stale local-mesh roaming route: %w", err)
		}
	}
	for _, route := range ownedRoutes {
		if err := netlink.RouteDel(&route); err != nil && !errors.Is(err, unix.ESRCH) {
			return fmt.Errorf("remove stale local-mesh route: %w", err)
		}
	}
	for name, link := range peers {
		if err := deleteOwnedLink(name, link.Attrs().Index, "tuntap"); err != nil {
			return err
		}
	}
	if dummy != nil {
		if err := deleteOwnedLink(InterfaceName, dummy.Attrs().Index, "dummy"); err != nil {
			return err
		}
	}
	return nil
}

func peerLinkName(name string) bool {
	if !strings.HasPrefix(name, "wlmp") || len(name) <= len("wlmp") {
		return false
	}
	id, err := strconv.ParseUint(strings.TrimPrefix(name, "wlmp"), 16, 32)
	return err == nil && id != 0 && name == fmt.Sprintf("wlmp%x", id)
}

func staleMeshRoute(route netlink.Route, peers map[int]bool) bool {
	if route.Protocol != routeProtocol {
		return false
	}
	if defaultIPv4Route(route.Dst) {
		if route.Priority == 32767 && route.Type == unix.RTN_UNREACHABLE &&
			route.LinkIndex == 0 && route.Gw == nil && route.Via == nil {
			return true
		}
		if route.Priority == 50 && route.Type == unix.RTN_UNICAST &&
			route.LinkIndex != 0 && route.Gw != nil && route.Via == nil {
			return true
		}
	}
	if route.Priority != 100 {
		return false
	}
	if route.Type == unix.RTN_UNREACHABLE {
		return route.LinkIndex == 0 && route.Gw == nil && route.Via == nil
	}
	if route.Type != unix.RTN_UNICAST || !peers[route.LinkIndex] || !meshSource(route.Src) {
		return false
	}
	return linkLocalVia(route.Via)
}

func staleRoamRoute(route netlink.Route, peers map[int]bool) bool {
	return route.Protocol == routeProtocol && route.Type == unix.RTN_UNICAST &&
		peers[route.LinkIndex] && meshSource(route.Src) && linkLocalVia(route.Via)
}

func meshSource(ip net.IP) bool {
	return ip4MeshRange.Contains(ip)
}

var ip4MeshRange = &net.IPNet{IP: net.IPv4(10, 88, 0, 0), Mask: net.CIDRMask(16, 32)}

func linkLocalVia(value netlink.Destination) bool {
	via, ok := value.(*netlink.Via)
	return ok && via.AddrFamily == unix.AF_INET6 && via.Addr.IsLinkLocalUnicast()
}

func deleteOwnedLink(name string, index int, kind string) error {
	live, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("inspect stale local-mesh link %s: %w", name, err)
	}
	if live.Attrs().Index != index || live.Attrs().Alias != interfaceOwner || live.Type() != kind {
		return fmt.Errorf("stale local-mesh link %s ownership changed", name)
	}
	if err := netlink.LinkDel(live); err != nil {
		return fmt.Errorf("remove stale local-mesh link %s: %w", name, err)
	}
	return nil
}
