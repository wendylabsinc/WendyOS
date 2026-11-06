//go:build linux

package meshsharing

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// The table is shared by this donor's leases, but only an admitted source on
// its exact tunnel may look it up. Never steer host traffic or mesh transit.
// 18863–18867 belong to CNI replies, UDP VIPs and the Babel host policy.
const donorRoutingTable = 18868
const donorRoutingPriority = 18868
const donorUnreachablePriority = 18869

type donorRouting struct{ routes []netlink.Route }

// Read the chosen interface's native defaults on every reconciliation. DHCP/RA
// can change a gateway without changing either the interface or DNS address.
func selectedDonorRoutes(iface string, ipv6 bool) ([]netlink.Route, error) {
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return nil, err
	}
	if link.Attrs().Flags&net.FlagUp == 0 {
		return nil, errors.New("selected Internet uplink is down")
	}
	var result []netlink.Route
	families := []int{netlink.FAMILY_V4}
	if ipv6 {
		families = append(families, netlink.FAMILY_V6)
	}
	for _, family := range families {
		routes, err := netlink.RouteListFiltered(family, &netlink.Route{Table: unix.RT_TABLE_MAIN, LinkIndex: link.Attrs().Index}, netlink.RT_FILTER_TABLE|netlink.RT_FILTER_OIF)
		if err != nil {
			return nil, err
		}
		var best *netlink.Route
		for i := range routes {
			r := &routes[i]
			if r.Flags&(unix.RTNH_F_DEAD|unix.RTNH_F_LINKDOWN) != 0 || r.Type != unix.RTN_UNICAST || len(r.MultiPath) != 0 || (r.Dst != nil && r.Dst.String() != "0.0.0.0/0" && r.Dst.String() != "::/0") {
				continue
			}
			if best == nil || r.Priority < best.Priority {
				best = r
			}
		}
		if best == nil {
			return nil, fmt.Errorf("selected Internet uplink %s has no native IPv%d default", iface, map[int]int{netlink.FAMILY_V4: 4, netlink.FAMILY_V6: 6}[family])
		}
		// The terminal route has no interface: deleting the selected interface must
		// never turn a table miss into fallback through another native default.
		zero := net.IPv4zero
		bits := 32
		if family == netlink.FAMILY_V6 {
			zero, bits = net.IPv6zero, 128
		}
		result = append(result, netlink.Route{Table: donorRoutingTable, Protocol: tunnelProtocol, Family: family, Type: unix.RTN_UNREACHABLE, Priority: 32767, Dst: &net.IPNet{IP: zero, Mask: net.CIDRMask(0, bits)}})
		if len(best.Gw) != 0 {
			address, ok := netip.AddrFromSlice(best.Gw)
			if !ok {
				return nil, errors.New("invalid selected Internet gateway")
			}
			result = append(result, netlink.Route{Table: donorRoutingTable, Protocol: tunnelProtocol, Family: family, Type: unix.RTN_UNICAST, LinkIndex: best.LinkIndex, Dst: tunnelIPNet(address.Unmap()), Scope: netlink.SCOPE_LINK})
		}
		result = append(result, netlink.Route{Table: donorRoutingTable, Protocol: tunnelProtocol, Family: family, Type: unix.RTN_UNICAST, LinkIndex: best.LinkIndex, Gw: best.Gw, Src: best.Src, Priority: 100, MTU: best.MTU, Dst: &net.IPNet{IP: zero, Mask: net.CIDRMask(0, bits)}})
	}
	return result, nil
}

func sameDonorRoutes(a, b []netlink.Route) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].Equal(b[i]) || a[i].MTU != b[i].MTU {
			return false
		}
	}
	return true
}

func donorLeaseRules(tunnel string, lease tunnelLease) []netlink.Rule {
	var rules []netlink.Rule
	addresses := []netip.Addr{lease.address4()}
	if lease.IPv6 {
		addresses = append(addresses, lease.address6())
	}
	for _, address := range addresses {
		r := netlink.NewRule()
		r.Family = netlink.FAMILY_V4
		if address.Is6() {
			r.Family = netlink.FAMILY_V6
		}
		r.Src, r.IifName, r.Protocol = tunnelIPNet(address), tunnel, tunnelProtocol
		// Install the terminal rule first. It also closes a table-miss window if an
		// administrator removes routes while a live lease is being reconciled.
		r.Priority, r.Type = donorUnreachablePriority, unix.FR_ACT_UNREACHABLE
		rules = append(rules, *r)
		r.Priority, r.Type, r.Table = donorRoutingPriority, unix.FR_ACT_TO_TBL, donorRoutingTable
		rules = append(rules, *r)
	}
	return rules
}

func addDonorLeaseRules(tunnel string, lease tunnelLease, installed *[]netlink.Rule) error {
	for _, rule := range donorLeaseRules(tunnel, lease) {
		if err := netlink.RuleAdd(&rule); err != nil {
			return err
		}
		*installed = append(*installed, rule)
	}
	return nil
}

func removeDonorLeaseRules(installed *[]netlink.Rule) error {
	var remaining []netlink.Rule
	var errs []error
	for i := len(*installed) - 1; i >= 0; i-- {
		rule := (*installed)[i]
		if err := netlink.RuleDel(&rule); err != nil && !errors.Is(err, unix.ENOENT) {
			remaining = append(remaining, rule)
			errs = append(errs, err)
		}
	}
	*installed = remaining
	return errors.Join(errs...)
}

func (d *donorRouting) install(routes []netlink.Route) error {
	if err := checkDonorRoutingAvailable(); err != nil {
		return err
	}
	for _, route := range routes {
		if err := netlink.RouteAdd(&route); err != nil {
			return err
		}
		d.routes = append(d.routes, route)
	}
	return nil
}

func (d *donorRouting) close() error {
	var remaining []netlink.Route
	var errs []error
	for i := len(d.routes) - 1; i >= 0; i-- {
		route := d.routes[i]
		if err := netlink.RouteDel(&route); err != nil && !errors.Is(err, unix.ESRCH) && !errors.Is(err, unix.ENODEV) {
			remaining = append(remaining, route)
			errs = append(errs, err)
		}
	}
	d.routes = remaining
	return errors.Join(errs...)
}

// Match our complete rule shape, including lease/address and tunnel slot, not
// just a priority which a user could already be using for another purpose.
func ownedDonorRule(rule netlink.Rule) bool {
	if rule.Protocol != tunnelProtocol || rule.Src == nil || !strings.HasPrefix(rule.IifName, tunnelPrefix) {
		return false
	}
	slot, err := strconv.ParseUint(strings.TrimPrefix(rule.IifName, tunnelPrefix), 16, 16)
	if err != nil || slot == 0 {
		return false
	}
	lease := tunnelLease{Slot: uint16(slot), IPv6: rule.Family == netlink.FAMILY_V6}
	for _, expected := range donorLeaseRules(rule.IifName, lease) {
		// netlink v1.3.1 does not decode the dumped rule action. The
		// protocol, exact priority/table and complete source+iif selectors
		// still identify the lease; use zero only for dump comparison.
		expected.Type = 0
		if reflect.DeepEqual(rule, expected) {
			return true
		}
	}
	return false
}

func ownedDonorRoute(route netlink.Route) bool {
	if route.Table != donorRoutingTable || route.Protocol != tunnelProtocol {
		return false
	}
	if route.Dst == nil || route.Dst.String() == "0.0.0.0/0" || route.Dst.String() == "::/0" {
		// IPv6 dumps assign unreachable routes to loopback despite no OIF
		// in the creation request; the type provides the fail-closed action.
		return (route.Type == unix.RTN_UNREACHABLE && route.Priority == 32767) || (route.Type == unix.RTN_UNICAST && route.LinkIndex > 0 && route.Priority == 100)
	}
	ones, bits := route.Dst.Mask.Size()
	// IPv6 normalizes on-link host routes to universe scope in dumps.
	return route.Type == unix.RTN_UNICAST && route.LinkIndex > 0 && ones == bits && (route.Scope == netlink.SCOPE_LINK || (route.Family == netlink.FAMILY_V6 && route.Scope == netlink.SCOPE_UNIVERSE))
}

func checkDonorRoutingAvailable() error {
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		rules, err := netlink.RuleList(family)
		if err != nil {
			return err
		}
		for _, rule := range rules {
			if rule.Table == donorRoutingTable || rule.Priority == donorRoutingPriority || rule.Priority == donorUnreachablePriority {
				return errors.New("Internet donor routing table or priority is already in use")
			}
		}
		routes, err := netlink.RouteListFiltered(family, &netlink.Route{Table: donorRoutingTable}, netlink.RT_FILTER_TABLE)
		if err != nil {
			return err
		}
		if len(routes) != 0 {
			return fmt.Errorf("Internet donor routing table is already in use: %v", routes)
		}
	}
	return nil
}

func reclaimDonorRouting() error {
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		rules, err := netlink.RuleList(family)
		if err != nil && !errors.Is(err, unix.EAFNOSUPPORT) && !errors.Is(err, unix.EOPNOTSUPP) {
			return err
		}
		for _, rule := range rules {
			if ownedDonorRule(rule) {
				if err := netlink.RuleDel(&rule); err != nil {
					return err
				}
			}
		}
		routes, err := netlink.RouteListFiltered(family, &netlink.Route{Table: donorRoutingTable}, netlink.RT_FILTER_TABLE)
		if errors.Is(err, unix.EAFNOSUPPORT) || errors.Is(err, unix.EOPNOTSUPP) {
			continue // This kernel cannot retain policy routes for this family.
		}
		if err != nil {
			return err
		}
		for _, route := range routes {
			if ownedDonorRoute(route) {
				if err := netlink.RouteDel(&route); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
