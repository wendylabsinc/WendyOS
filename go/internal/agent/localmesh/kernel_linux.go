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
	lock    net.Listener
	link    *netlink.Dummy
	peers   map[babel.LinkID]kernelPeer
	routes  []netlink.Route
	rule    *netlink.Rule
	self    int32
	address net.IP
	exit    *netlink.Route
	roam    *netlink.Route
}

// ReconcileBase restores only this kernel's invariant host address, policy
// rule and fall-through guard. NetworkManager may remove these after link
// creation even when it is subsequently told to leave wlmesh0 unmanaged.
// Never adopt a replacement interface or replace an unfamiliar rule/route.
func (k *Kernel) ReconcileBase() error {
	state, err := k.inspectBase()
	if err != nil {
		return err
	}
	// Restore the guard first so a surviving policy rule cannot fall through
	// to the ordinary uplink while the other base state is being repaired.
	if !state.guard {
		guard := k.routes[0]
		if err := netlink.RouteAdd(&guard); err != nil && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("restore local-mesh guard route: %w", err)
		}
	}
	if !state.address {
		addr := &netlink.Addr{IPNet: &net.IPNet{IP: k.address, Mask: net.CIDRMask(32, 32)}}
		if err := netlink.AddrAdd(state.link, addr); err != nil && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("restore local-mesh address: %w", err)
		}
	}
	if !state.up {
		if err := netlink.LinkSetUp(state.link); err != nil {
			return fmt.Errorf("restore local-mesh link state: %w", err)
		}
	}
	if !state.rule {
		if err := netlink.RuleAdd(k.rule); err != nil && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("restore local-mesh policy rule: %w", err)
		}
	}
	// EEXIST can mean a competing writer installed different state. Confirm
	// all invariants after repair rather than treating that error as success.
	state, err = k.inspectBase()
	if err != nil {
		return err
	}
	if !state.address || !state.up || !state.guard || !state.rule {
		return errors.New("local-mesh base state changed during reconciliation")
	}
	return nil
}

type baseState struct {
	link    netlink.Link
	address bool
	up      bool
	rule    bool
	guard   bool
}

func (k *Kernel) inspectBase() (baseState, error) {
	var state baseState
	if k.link == nil || k.rule == nil || len(k.routes) == 0 {
		return state, errors.New("local-mesh kernel is not initialized")
	}
	live, err := netlink.LinkByName(InterfaceName)
	if err != nil {
		return state, fmt.Errorf("local-mesh interface missing: %w", err)
	}
	if live.Attrs().Index != k.link.Index || live.Attrs().Alias != interfaceOwner || live.Type() != "dummy" {
		return state, errors.New("local-mesh interface ownership changed")
	}
	state.link = live
	state.up = live.Attrs().Flags&net.FlagUp != 0
	addresses, err := netlink.AddrList(live, netlink.FAMILY_V4)
	if err != nil {
		return state, fmt.Errorf("inspect local-mesh address: %w", err)
	}
	for _, address := range addresses {
		if !address.IP.Equal(k.address) {
			continue
		}
		if address.IPNet == nil || address.IPNet.String() != (&net.IPNet{IP: k.address, Mask: net.CIDRMask(32, 32)}).String() {
			return state, errors.New("local-mesh address has conflicting prefix")
		}
		state.address = true
	}
	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		return state, fmt.Errorf("inspect local-mesh policy rule: %w", err)
	}
	for _, rule := range rules {
		if rule.Priority == RouteTable {
			if !baseRuleMatches(rule) {
				return state, errors.New("local-mesh policy rule ownership conflict")
			}
			state.rule = true
		} else if rule.Table == RouteTable {
			ownedPeer := false
			for _, peer := range k.peers {
				if peerRuleMatches(rule, peer.rule) {
					ownedPeer = true
					break
				}
			}
			if !ownedPeer {
				return state, errors.New("local-mesh route table rule ownership conflict")
			}
		}
	}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: RouteTable}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return state, fmt.Errorf("inspect local-mesh guard route: %w", err)
	}
	for _, route := range routes {
		if !defaultIPv4Route(route.Dst) {
			continue
		}
		// A signed peer's Babel default is also installed in this table. It is
		// an owned route, tracked by Apply, even when this host is not roaming.
		if k.ownsLearnedDefault(route) {
			continue
		}
		// Sharing an independent uplink installs our own lower-metric default
		// in this table. It must coexist with the unreachable fall-through guard.
		if k.exit != nil && route.Priority == k.exit.Priority &&
			route.Protocol == routeProtocol && route.Type == unix.RTN_UNICAST &&
			route.LinkIndex == k.exit.LinkIndex && route.Gw.Equal(k.exit.Gw) {
			continue
		}
		if route.Priority != 32767 || route.Protocol != routeProtocol || route.Type != unix.RTN_UNREACHABLE || route.LinkIndex != 0 || route.Gw != nil {
			return state, errors.New("local-mesh guard route ownership conflict")
		}
		state.guard = true
	}
	return state, nil
}

func (k *Kernel) ownsLearnedDefault(route netlink.Route) bool {
	for _, expected := range k.routes {
		if expected.Priority != 100 || !defaultIPv4Route(expected.Dst) ||
			route.Priority != expected.Priority || route.Protocol != expected.Protocol ||
			route.Type != expected.Type || route.LinkIndex != expected.LinkIndex ||
			!route.Src.Equal(expected.Src) || !route.Gw.Equal(expected.Gw) {
			continue
		}
		if route.Via == nil || expected.Via == nil {
			return route.Via == nil && expected.Via == nil
		}
		actual, ok := route.Via.(*netlink.Via)
		want, expectedOK := expected.Via.(*netlink.Via)
		if ok && expectedOK && actual.AddrFamily == want.AddrFamily && actual.Addr.Equal(want.Addr) {
			return true
		}
	}
	return false
}

func baseRuleMatches(rule netlink.Rule) bool {
	return rule.Table == RouteTable && rule.Dst != nil && rule.Dst.String() == "10.88.0.0/16" &&
		rule.Src == nil && rule.IifName == "" && rule.OifName == "" &&
		rule.Mark == 0 && rule.Mask == nil && rule.Tos == 0 && !rule.Invert &&
		rule.Dport == nil && rule.Sport == nil && rule.UIDRange == nil && rule.IPProto == 0
}

func peerRuleMatches(rule netlink.Rule, peer *netlink.Rule) bool {
	return rule.Priority == peer.Priority && rule.IifName == peer.IifName &&
		rule.OifName == "" && rule.Src == nil && rule.Dst == nil &&
		rule.Mark == 0 && rule.Mask == nil && rule.Tos == 0 && !rule.Invert &&
		rule.Dport == nil && rule.Sport == nil && rule.UIDRange == nil && rule.IPProto == 0
}

func defaultIPv4Route(dst *net.IPNet) bool {
	if dst == nil {
		return true
	}
	return dst.String() == "0.0.0.0/0"
}

func NewKernel(org, asset int32) (*Kernel, error) {
	v4, _, err := Addresses(org, asset)
	if err != nil {
		return nil, err
	}
	// Abstract Unix socket names are scoped to the network namespace and are
	// released by the kernel on process death. An active agent must never have
	// its links reclaimed by a second agent in the same namespace.
	lock, err := net.Listen("unix", "@wendy-localmesh-kernel")
	if err != nil {
		return nil, fmt.Errorf("local-mesh kernel already owned: %w", err)
	}
	owned := false
	defer func() {
		if !owned {
			_ = lock.Close()
		}
	}()
	if err := recoverStaleKernel(); err != nil {
		return nil, err
	}
	attrs := netlink.NewLinkAttrs()
	attrs.Name = InterfaceName
	attrs.Alias = interfaceOwner
	attrs.MTU = TunnelMTU
	k := &Kernel{lock: lock, link: &netlink.Dummy{LinkAttrs: attrs}, peers: map[babel.LinkID]kernelPeer{}, self: asset, address: net.IP(v4.AsSlice())}
	if err = netlink.LinkAdd(k.link); err != nil {
		return nil, err
	}
	owned = true
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
	if k.lock != nil {
		defer func() { _ = k.lock.Close(); k.lock = nil }()
	}
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
