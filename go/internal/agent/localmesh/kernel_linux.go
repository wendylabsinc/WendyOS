//go:build linux

package localmesh

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"

	"github.com/vishvananda/netlink"
	"github.com/wendylabsinc/WendyOS/babel"
	"golang.org/x/sys/unix"
)

const InterfaceName = "wlmesh0"
const interfaceOwner = "wendy:local-mesh:1"
const RouteTable = 18865
const routeProtocol = 186

type kernelPeer struct {
	link *netlink.Tuntap
	file *os.File
	rule *netlink.Rule
}

// Kernel owns only links it creates. It does not enable forwarding, change
// default routes, or touch the old NAN implementation. Session policy owns those
// operations and must quiesce transport around Apply. IPv4 internet sharing is
// the initial host policy; IPv6 Babel next hops work without IPv6 policy routing.
type Kernel struct {
	link    *netlink.Dummy
	peers   map[babel.LinkID]kernelPeer
	routes  []netlink.Route
	rule    *netlink.Rule
	self    int32
	address net.IP
	exit    *netlink.Route
	roam    *netlink.Route
}

func NewKernel(org, asset int32) (*Kernel, error) {
	v4, _, err := Addresses(org, asset)
	if err != nil {
		return nil, err
	}
	// Reserved route table must be empty; never adopt foreign or stale state.
	existing, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: RouteTable}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return nil, err
	}
	if len(existing) != 0 {
		return nil, errors.New("local-mesh route table already occupied")
	}
	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		return nil, err
	}
	for _, r := range rules {
		if r.Table == RouteTable || r.Priority == RouteTable {
			return nil, errors.New("local-mesh rule ownership conflict")
		}
	}
	attrs := netlink.NewLinkAttrs()
	attrs.Name = InterfaceName
	attrs.Alias = interfaceOwner
	attrs.MTU = TunnelMTU
	k := &Kernel{link: &netlink.Dummy{LinkAttrs: attrs}, peers: map[babel.LinkID]kernelPeer{}, self: asset, address: net.IP(v4.AsSlice())}
	if err = netlink.LinkAdd(k.link); err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = k.Close()
		}
	}()
	if err = netlink.LinkSetAlias(k.link, interfaceOwner); err != nil {
		return nil, err
	}
	if err = netlink.AddrAdd(k.link, &netlink.Addr{IPNet: ipNet(netip.PrefixFrom(v4, 32))}); err != nil {
		return nil, err
	}
	if err = netlink.LinkSetUp(k.link); err != nil {
		return nil, err
	}
	// A permanent unreachable default prevents this table from falling through
	// to the host's ordinary uplink. A learnt default has a lower priority.
	guard := netlink.Route{Table: RouteTable, Protocol: routeProtocol, Family: netlink.FAMILY_V4, Type: unix.RTN_UNREACHABLE, Priority: 32767, Dst: ipNet(netip.MustParsePrefix("0.0.0.0/0"))}
	if err = netlink.RouteAdd(&guard); err != nil {
		return nil, err
	}
	k.routes = append(k.routes, guard)
	rule := netlink.NewRule()
	rule.Family = netlink.FAMILY_V4
	rule.Priority = RouteTable
	rule.Table = RouteTable
	rule.Dst = ipNet(netip.MustParsePrefix("10.88.0.0/16"))
	if err = netlink.RuleAdd(rule); err != nil {
		return nil, err
	}
	k.rule = rule
	ok = true
	return k, nil
}

func ipNet(p netip.Prefix) *net.IPNet {
	return &net.IPNet{IP: net.IP(p.Addr().AsSlice()), Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen())}
}

func (k *Kernel) AddPeer(id babel.LinkID, asset int32) (*os.File, error) {
	if id == 0 || uint64(id) > 0xffffffff {
		return nil, errors.New("kernel link incarnation out of range")
	}
	if _, ok := k.peers[id]; ok {
		return nil, errors.New("duplicate kernel link")
	}
	local, _, err := LinkAddresses(k.self, asset)
	if err != nil {
		return nil, err
	}
	attrs := netlink.NewLinkAttrs()
	attrs.Name = fmt.Sprintf("wlmp%x", uint64(id))
	attrs.MTU = TunnelMTU
	attrs.Alias = interfaceOwner
	tun := &netlink.Tuntap{LinkAttrs: attrs, Mode: netlink.TUNTAP_MODE_TUN, Flags: netlink.TUNTAP_NO_PI | netlink.TUNTAP_TUN_EXCL, Queues: 1, NonPersist: true}
	if err = netlink.LinkAdd(tun); err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			for _, f := range tun.Fds {
				_ = f.Close()
			}
		}
	}()
	if len(tun.Fds) != 1 {
		return nil, errors.New("TUN did not return one queue")
	}
	if err = netlink.LinkSetAlias(tun, interfaceOwner); err != nil {
		return nil, err
	}
	if err = netlink.LinkSetMTU(tun, TunnelMTU); err != nil {
		return nil, err
	}
	// Babel owns addressing and routing on this point-to-point link. Never
	// accept peer router advertisements or manufacture SLAAC/default routes.
	for _, setting := range []string{"accept_ra", "autoconf", "router_solicitations"} {
		if err = os.WriteFile("/proc/sys/net/ipv6/conf/"+attrs.Name+"/"+setting, []byte("0\n"), 0600); err != nil {
			return nil, err
		}
	}
	if err = netlink.AddrAdd(tun, &netlink.Addr{IPNet: ipNet(netip.PrefixFrom(local, 64)), Flags: unix.IFA_F_NODAD}); err != nil {
		return nil, err
	}
	if err = netlink.AddrAdd(tun, &netlink.Addr{IPNet: &net.IPNet{IP: k.address, Mask: net.CIDRMask(32, 32)}, Flags: unix.IFA_F_NOPREFIXROUTE}); err != nil {
		return nil, err
	}
	if err = netlink.LinkSetUp(tun); err != nil {
		return nil, err
	}
	rule := netlink.NewRule()
	rule.Family = netlink.FAMILY_V4
	rule.Priority = RouteTable + 1
	rule.Table = RouteTable
	rule.IifName = attrs.Name
	if err = netlink.RuleAdd(rule); err != nil {
		return nil, err
	}
	k.peers[id] = kernelPeer{tun, tun.Fds[0], rule}
	ok = true
	return tun.Fds[0], nil
}

func (k *Kernel) RemovePeer(id babel.LinkID) error {
	p, ok := k.peers[id]
	if !ok {
		return nil
	}
	delete(k.peers, id)
	return errors.Join(netlink.RuleDel(p.rule), p.file.Close())
}

// Apply installs IPv4 routes with RFC 9229 IPv6 link-local next hops. The caller
// MUST hold the packet forwarding gate until all routes and durable engine state
// are committed. On any error it must call Stop, not resume a partial FIB.
func (k *Kernel) Apply(routes []babel.Route) error {
	var desired []netlink.Route
	for _, r := range routes {
		if r.Local {
			continue
		}
		if !r.Prefix.Addr().Is4() {
			return errors.New("IPv6 payload routing not enabled in initial host policy")
		}
		n := netlink.Route{Table: RouteTable, Protocol: routeProtocol, Family: netlink.FAMILY_V4, Dst: ipNet(r.Prefix), Priority: 100, Type: unix.RTN_UNICAST}
		if r.Unreachable {
			n.Type = unix.RTN_UNREACHABLE
		} else {
			n.Src = k.address
			p, ok := k.peers[r.Link]
			if !ok {
				return errors.New("route references missing kernel link")
			}
			if !r.NextHop.Is6() || !r.NextHop.IsLinkLocalUnicast() {
				return errors.New("route has invalid scoped next hop")
			}
			n.LinkIndex = p.link.Index
			n.Via = &netlink.Via{AddrFamily: unix.AF_INET6, Addr: net.IP(r.NextHop.AsSlice())}
		}
		desired = append(desired, n)
	}
	// Keep the permanent guard. Add/replace before deleting old prefixes, so
	// neither a withdrawal nor a route change creates a default fallback gap.
	for i := range desired {
		if err := netlink.RouteReplace(&desired[i]); err != nil {
			return err
		}
		// Track each successful mutation immediately for cleanup if a later
		// netlink operation fails. Replace the prior entry for this prefix.
		found := false
		for j := range k.routes {
			if k.routes[j].Priority != 32767 && k.routes[j].Dst.String() == desired[i].Dst.String() {
				k.routes[j] = desired[i]
				found = true
				break
			}
		}
		if !found {
			k.routes = append(k.routes, desired[i])
		}
	}
	for _, old := range k.routes {
		if old.Priority == 32767 {
			continue
		}
		keep := false
		for _, n := range desired {
			if n.Dst.String() == old.Dst.String() {
				keep = true
				break
			}
		}
		if !keep {
			if err := netlink.RouteDel(&old); err != nil && !errors.Is(err, unix.ESRCH) {
				return err
			}
		}
	}
	k.routes = append(k.routes[:1], desired...)
	return nil
}

// SetExit is called with transport paused, after NAT/DNS preparation.
func (k *Kernel) SetExit(iface string) error {
	if iface == "" {
		if k.exit == nil {
			return nil
		}
		err := netlink.RouteDel(k.exit)
		if err == nil || errors.Is(err, unix.ESRCH) {
			k.exit = nil
			return nil
		}
		return err
	}
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return err
	}
	routes, err := netlink.RouteList(link, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	var best *netlink.Route
	for i := range routes {
		r := &routes[i]
		if r.Table != 254 || r.Gw == nil {
			continue
		}
		if r.Dst != nil {
			bits, _ := r.Dst.Mask.Size()
			if bits != 0 {
				continue
			}
		}
		if best == nil || r.Priority < best.Priority {
			best = r
		}
	}
	if best == nil {
		return errors.New("selected exit has no independent IPv4 gateway")
	}
	route := netlink.Route{Family: netlink.FAMILY_V4, Table: RouteTable, Protocol: routeProtocol, Priority: 50, LinkIndex: link.Attrs().Index, Gw: best.Gw, Flags: unix.RTNH_F_ONLINK}
	if err = netlink.RouteReplace(&route); err != nil {
		return err
	}
	k.exit = &route
	return nil
}

func (k *Kernel) SetRoam(route *babel.Route) error {
	if route == nil {
		if k.roam == nil {
			return nil
		}
		err := netlink.RouteDel(k.roam)
		if err == nil || errors.Is(err, unix.ESRCH) {
			k.roam = nil
			return nil
		}
		return err
	}
	peer, ok := k.peers[route.Link]
	if !ok || route.Unreachable || route.Local || route.Prefix.Bits() != 0 {
		return errors.New("invalid roaming route")
	}
	if k.roam == nil {
		routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
		if err != nil {
			return err
		}
		for _, r := range routes {
			if r.Table == 254 && r.Priority == 5 && (r.Dst == nil || r.Dst.String() == "0.0.0.0/0") {
				return errors.New("roaming default priority already occupied")
			}
		}
	}
	next := &netlink.Route{Family: netlink.FAMILY_V4, Table: 254, Protocol: routeProtocol, Priority: 5, LinkIndex: peer.link.Index, Src: k.address, Via: &netlink.Via{AddrFamily: unix.AF_INET6, Addr: net.IP(route.NextHop.AsSlice())}}
	if err := netlink.RouteReplace(next); err != nil {
		return err
	}
	k.roam = next
	return nil
}

func (k *Kernel) Stop() error {
	var errs []error
	for _, p := range k.peers {
		errs = append(errs, netlink.LinkSetDown(p.link))
	}
	return errors.Join(errs...)
}

func (k *Kernel) Close() error {
	var errs []error
	errs = append(errs, k.SetExit(""))
	errs = append(errs, k.SetRoam(nil))
	for id := range k.peers {
		errs = append(errs, k.RemovePeer(id))
	}
	if k.rule != nil {
		errs = append(errs, netlink.RuleDel(k.rule))
		k.rule = nil
	}
	for _, r := range k.routes {
		if err := netlink.RouteDel(&r); err != nil && !errors.Is(err, unix.ESRCH) {
			errs = append(errs, err)
		}
	}
	k.routes = nil
	live, err := netlink.LinkByName(InterfaceName)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	if live.Attrs().Index != k.link.Index || live.Attrs().Alias != interfaceOwner || live.Type() != "dummy" {
		return errors.Join(append(errs, errors.New("local-mesh interface ownership changed"))...)
	}
	errs = append(errs, netlink.LinkDel(live))
	return errors.Join(errs...)
}
