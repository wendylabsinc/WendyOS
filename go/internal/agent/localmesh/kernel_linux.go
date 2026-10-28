//go:build linux

package localmesh

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"golang.org/x/sys/unix"
)

const interfaceOwner = "wendy:local-mesh:2"
const RouteTable = 18865
const routeProtocol = 186

type kernelPeer struct {
	link   *netlink.Tuntap
	file   *os.File
	rule   *netlink.Rule
	device meshid.Device
}

// Kernel owns its links, ULA routes and scoped mesh transit grant. IPv6
// forwarding is shared with Internet sharing through hostnetwork ownership.
// Session policy must quiesce transport around Apply.
//
// The mesh is IPv6-only: loopback carries the device's own ULA address, each
// point-to-point TUN carries the complementary pair derived from the two device
// UUIDs, and Babel routes are installed as ULA destinations with link-local next
// hops on the interface they arrived on.
type Kernel struct {
	link             netlink.Link
	addressInstalled bool
	peers            map[babel.LinkID]kernelPeer
	routes           []netlink.Route
	rule             *netlink.Rule
	device           meshid.Device
	address          net.IP
	forwarding       bool
	forwardRule      bool
}

// ReconcileBase restores only this kernel's tagged loopback address, policy
// rule and fall-through guard. Never modify the shared loopback interface,
// adopt an untagged address or replace an unfamiliar rule/route.
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
			return fmt.Errorf("restore wendy net guard route: %w", err)
		}
	}
	if !state.address {
		if err := addLoopbackAddress(state.link, k.device.MeshAddress()); err != nil && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("restore wendy net address: %w", err)
		}
	}
	if !state.rule {
		if err := netlink.RuleAdd(k.rule); err != nil && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("restore wendy net policy rule: %w", err)
		}
	}
	// EEXIST can mean a competing writer installed different state. Confirm
	// all invariants after repair rather than treating that error as success.
	state, err = k.inspectBase()
	if err != nil {
		return err
	}
	if !state.address || !state.guard || !state.rule {
		return errors.New("wendy net base state changed during reconciliation")
	}
	return nil
}

type baseState struct {
	link    netlink.Link
	address bool
	rule    bool
	guard   bool
}

func (k *Kernel) inspectBase() (baseState, error) {
	var state baseState
	if k.link == nil || k.rule == nil || len(k.routes) == 0 {
		return state, errors.New("wendy net kernel is not initialized")
	}
	live, err := meshLoopback()
	if err != nil {
		return state, fmt.Errorf("wendy net interface missing: %w", err)
	}
	if live.Attrs().Index != k.link.Attrs().Index {
		return state, errors.New("wendy net loopback interface changed")
	}
	state.link = live
	state.address, err = inspectLoopbackAddress(live, k.device.MeshAddress())
	if err != nil {
		return state, fmt.Errorf("inspect wendy net address: %w", err)
	}
	rules, err := netlink.RuleList(netlink.FAMILY_V6)
	if err != nil {
		return state, fmt.Errorf("inspect wendy net policy rule: %w", err)
	}
	for _, rule := range rules {
		if rule.Priority == RouteTable {
			if !baseRuleMatches(rule) {
				return state, errors.New("wendy net policy rule ownership conflict")
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
				return state, errors.New("wendy net route table rule ownership conflict")
			}
		}
	}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V6, &netlink.Route{Table: RouteTable}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return state, fmt.Errorf("inspect wendy net guard route: %w", err)
	}
	for _, route := range routes {
		if !defaultIPv6Route(route.Dst) {
			continue
		}
		// A signed peer's Babel default is also installed in this table. It is
		// an owned route, tracked by Apply, even when this host is not roaming.
		if k.ownsLearnedDefault(route) {
			continue
		}
		// The kernel binds device-less error routes to the loopback interface,
		// so a reclaimed guard reports RTA_OIF=lo rather than no device. The
		// protocol, metric, type and absent gateway are the ownership signature.
		if route.Priority != 32767 || route.Protocol != routeProtocol || route.Type != unix.RTN_UNREACHABLE || route.Gw != nil {
			return state, errors.New("wendy net guard route ownership conflict")
		}
		state.guard = true
	}
	return state, nil
}

func (k *Kernel) ownsLearnedDefault(route netlink.Route) bool {
	for _, expected := range k.routes {
		if expected.Priority != 100 || !defaultIPv6Route(expected.Dst) ||
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

// Unspecified selectors must remain unspecified: an extra suppression, mark or
// input constraint changes the rule even if it selects our reserved table.
func plainRuleSelectors(rule netlink.Rule) bool {
	return rule.Family == netlink.FAMILY_V6 && rule.Table == RouteTable &&
		rule.Src == nil && rule.OifName == "" && rule.Mark == 0 && rule.Mask == nil &&
		rule.Tos == 0 && !rule.Invert && rule.Dport == nil && rule.Sport == nil &&
		rule.UIDRange == nil && rule.IPProto == 0 && rule.TunID == 0 &&
		rule.Goto == -1 && rule.Flow == -1 &&
		rule.SuppressIfgroup == -1 && rule.SuppressPrefixlen == -1
}

func baseRuleMatches(rule netlink.Rule) bool {
	return plainRuleSelectors(rule) && rule.Priority == RouteTable &&
		rule.Dst != nil && rule.Dst.String() == meshid.MeshPrefix.String() && rule.IifName == ""
}

func peerRuleMatches(rule netlink.Rule, peer *netlink.Rule) bool {
	return plainRuleSelectors(rule) && rule.Priority == peer.Priority &&
		rule.IifName == peer.IifName && rule.Dst == nil
}

func ownedPeerInterfaceName(name string) bool {
	if !strings.HasPrefix(name, peerInterfacePrefix) {
		return false
	}
	suffix := strings.TrimPrefix(name, peerInterfacePrefix)
	id, err := strconv.ParseUint(suffix, 16, 32)
	return err == nil && id != 0 && strconv.FormatUint(id, 16) == suffix
}

func defaultIPv6Route(dst *net.IPNet) bool {
	if dst == nil {
		return true
	}
	return dst.String() == "::/0"
}

func NewKernel(device meshid.Device) (*Kernel, error) {
	if !device.IsValid() {
		return nil, errors.New("wendy net kernel needs a device principal")
	}
	// Fail on missing IPv6 policy routing or TUN support before touching any
	// owned forwarding, interfaces, rules or routes from a previous instance.
	if err := preflightKernel(); err != nil {
		return nil, err
	}
	loopback, err := meshLoopback()
	if err != nil {
		return nil, err
	}
	addresses, err := readLoopbackAddresses(loopback)
	if err != nil {
		return nil, fmt.Errorf("inspect loopback address capabilities: %w", err)
	}
	if err := requireLoopbackAddressProtocol(addresses); err != nil {
		return nil, err
	}
	if _, err := inspectLoopbackAddress(loopback, device.MeshAddress()); err != nil {
		return nil, err
	}
	if err := reclaimMeshForwarding(); err != nil {
		return nil, kernelOperationError("reclaim net forwarding", "", err)
	}
	address := device.MeshAddress()
	// An abrupt exit leaves the tagged address, its TUNs, their rules and the mesh
	// routes behind. Reclaim exactly the state this package owns first, so a
	// restart recovers instead of refusing to start forever; anything else in
	// the reserved table or rule slots belongs to someone else and stays a
	// hard conflict.
	if err = reclaimReservedState(); err != nil {
		return nil, err
	}
	k := &Kernel{link: loopback, peers: map[babel.LinkID]kernelPeer{}, device: device, address: net.IP(address.AsSlice())}
	ok := false
	defer func() {
		if !ok {
			_ = k.Close()
		}
	}()
	if err = addLoopbackAddress(k.link, address); err != nil {
		return nil, kernelOperationError("assign loopback mesh address", kernelIPv6Requirement, err)
	}
	k.addressInstalled = true
	if present, err := inspectLoopbackAddress(k.link, address); err != nil {
		return nil, fmt.Errorf("verify loopback mesh address ownership (requires Linux IFA_PROTO support): %w", err)
	} else if !present {
		return nil, errors.New("loopback mesh address disappeared during setup")
	}
	// A permanent unreachable default prevents this table from falling through
	// to the host's ordinary uplink. A learnt route has a lower priority.
	guard := netlink.Route{Table: RouteTable, Protocol: routeProtocol, Family: netlink.FAMILY_V6, Type: unix.RTN_UNREACHABLE, Priority: 32767, Dst: ipNet(netip.MustParsePrefix("::/0"))}
	if err = netlink.RouteAdd(&guard); err != nil {
		return nil, kernelOperationError("install IPv6 guard route in net table", kernelPolicyRequirement, err)
	}
	k.routes = append(k.routes, guard)
	// Mesh traffic is looked up in this table only. Everything else keeps the
	// host's ordinary routing.
	rule := netlink.NewRule()
	rule.Family = netlink.FAMILY_V6
	rule.Priority = RouteTable
	rule.Table = RouteTable
	rule.Dst = ipNet(meshid.MeshPrefix)
	if err = netlink.RuleAdd(rule); err != nil {
		return nil, kernelOperationError("install IPv6 net policy rule", kernelPolicyRequirement, err)
	}
	k.rule = rule
	if err = k.enableForwarding(); err != nil {
		return nil, kernelOperationError("enable net forwarding", "", err)
	}
	ok = true
	return k, nil
}

// CleanupStaleKernel removes only the state reserved and marked by local-mesh.
// It does not require credentials or create interfaces, so reset recovery can
// use it after the enrollment has already been erased. No mesh node may be live.
func CleanupStaleKernel() error {
	// Inspect all reserved objects before deleting any. Reset cleanup must not
	// remove an administrator's conflicting rule merely because it selects our
	// usual table number.
	if err := validateStaleKernelOwnership(); err != nil {
		return err
	}
	if err := reclaimMeshForwarding(); err != nil {
		return err
	}
	return reclaimReservedStateWith(netlinkKernelCleanup(), true)
}

func validateStaleKernelOwnership() error {
	return validateStaleKernelOwnershipWith(netlink.LinkList, netlink.RuleList, netlink.RouteListFiltered)
}

func validateStaleKernelOwnershipWith(listLinks func() ([]netlink.Link, error), listRules func(int) ([]netlink.Rule, error), listRoutes func(int, *netlink.Route, uint64) ([]netlink.Route, error)) error {
	links, err := listLinks()
	if err != nil {
		return err
	}
	ownedLinks := false
	for _, link := range links {
		name := link.Attrs().Name
		if name != legacyInterfaceName && !strings.HasPrefix(name, peerInterfacePrefix) {
			continue
		}
		if link.Attrs().Alias != interfaceOwner || (name == legacyInterfaceName && link.Type() != "dummy") || (name != legacyInterfaceName && (!ownedPeerInterfaceName(name) || link.Type() != "tuntap")) {
			return fmt.Errorf("interface %q is not owned by wendy-net", name)
		}
		ownedLinks = true
	}
	rules, err := listRules(netlink.FAMILY_ALL)
	if err != nil && (ownedLinks || (!errors.Is(err, unix.EAFNOSUPPORT) && !errors.Is(err, unix.EOPNOTSUPP))) {
		return err
	}
	for _, rule := range rules {
		if rule.Table != RouteTable && rule.Priority != RouteTable && rule.Priority != RouteTable+1 {
			continue
		}
		if !ownedKernelRule(rule) {
			return errors.New("wendy-net policy rule ownership conflict during reset cleanup")
		}
	}
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		routes, err := listRoutes(family, &netlink.Route{Table: RouteTable}, netlink.RT_FILTER_TABLE)
		if errors.Is(err, unix.EAFNOSUPPORT) || errors.Is(err, unix.EOPNOTSUPP) {
			if ownedLinks {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		for _, route := range routes {
			if route.Protocol != routeProtocol {
				return errors.New("wendy-net route table holds a foreign route during reset cleanup")
			}
		}
	}
	return nil
}

func ownedKernelRule(rule netlink.Rule) bool {
	if rule.Family != netlink.FAMILY_V6 || rule.Table != RouteTable {
		return false
	}
	base := rule.Priority == RouteTable && baseRuleMatches(rule)
	peer := rule.Priority == RouteTable+1 && ownedPeerInterfaceName(rule.IifName) && peerRuleMatches(rule, &netlink.Rule{Priority: RouteTable + 1, IifName: rule.IifName})
	return base || peer
}

func ipNet(p netip.Prefix) *net.IPNet {
	return &net.IPNet{IP: net.IP(p.Addr().AsSlice()), Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen())}
}

func (k *Kernel) AddPeer(id babel.LinkID, peer meshid.Device) (*os.File, error) {
	if id == 0 || uint64(id) > 0xffffffff {
		return nil, errors.New("kernel link incarnation out of range")
	}
	if _, ok := k.peers[id]; ok {
		return nil, errors.New("duplicate kernel link")
	}
	if peer.Principal() == k.device.Principal() {
		return nil, errors.New("self link")
	}
	local, _, err := meshid.LinkLocalAddresses(k.device, peer)
	if err != nil {
		return nil, err
	}
	attrs := netlink.NewLinkAttrs()
	attrs.Name = fmt.Sprintf("wlmp%x", uint64(id))
	attrs.MTU = TunnelMTU
	attrs.Alias = interfaceOwner
	tun := &netlink.Tuntap{LinkAttrs: attrs, Mode: netlink.TUNTAP_MODE_TUN, Flags: netlink.TUNTAP_NO_PI | netlink.TUNTAP_TUN_EXCL, Queues: 1, NonPersist: true}
	if err = netlink.LinkAdd(tun); err != nil {
		return nil, kernelOperationError("create peer TUN "+attrs.Name, kernelTUNRequirement, err)
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
		return nil, kernelOperationError("mark peer TUN ownership", "", err)
	}
	if err = netlink.LinkSetMTU(tun, TunnelMTU); err != nil {
		return nil, kernelOperationError("configure peer TUN MTU", "", err)
	}
	// Babel owns addressing and routing on this point-to-point link. Never
	// accept peer router advertisements or manufacture SLAAC, prefix or
	// default routes, and never let the kernel add its own MAC-derived
	// link-local beside the UUID-derived pair.
	// accept_ra_rt_info_max_plen only bounds routes carried inside router
	// information options, and some kernel builds do not expose it at all;
	// accept_ra=0 already blocks every advertisement. The knobs that actually
	// stop SLAAC, prefix assignment and solicitations stay mandatory.
	if err = sysctls(attrs.Name); err != nil {
		return nil, kernelOperationError("configure peer TUN IPv6", kernelIPv6Requirement, err)
	}
	if err = netlink.AddrAdd(tun, &netlink.Addr{IPNet: ipNet(netip.PrefixFrom(local, 64)), Flags: unix.IFA_F_NODAD}); err != nil {
		return nil, kernelOperationError("assign peer TUN IPv6 link-local address", kernelIPv6Requirement, err)
	}
	// The device's own mesh address is the source for payload leaving this link.
	if err = netlink.AddrAdd(tun, &netlink.Addr{IPNet: ipNet(netip.PrefixFrom(k.device.MeshAddress(), 128)), Flags: unix.IFA_F_NOPREFIXROUTE | unix.IFA_F_NODAD}); err != nil {
		return nil, kernelOperationError("assign peer TUN IPv6 net address", kernelIPv6Requirement, err)
	}
	if err = netlink.LinkSetUp(tun); err != nil {
		return nil, kernelOperationError("bring up peer TUN", kernelTUNRequirement, err)
	}
	rule := netlink.NewRule()
	rule.Family = netlink.FAMILY_V6
	rule.Priority = RouteTable + 1
	rule.Table = RouteTable
	rule.IifName = attrs.Name
	if err = netlink.RuleAdd(rule); err != nil {
		return nil, kernelOperationError("install IPv6 peer policy rule", kernelPolicyRequirement, err)
	}
	k.peers[id] = kernelPeer{tun, tun.Fds[0], rule, peer}
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

// Apply installs ULA routes with IPv6 link-local next hops. The caller MUST hold
// the packet forwarding gate until all routes and durable engine state are
// committed. On any error it must call Stop, not resume a partial FIB.
func (k *Kernel) Apply(routes []babel.Route) error {
	// Re-assert the unreachable fallback and the table rule before mutating
	// anything. If either was removed out from under this instance, a pool
	// address with no mesh route would fall through to the host's ordinary
	// uplink instead of failing closed.
	if len(k.routes) == 0 || k.routes[0].Priority != 32767 {
		return errors.New("wendy net fallback guard is missing")
	}
	if err := netlink.RouteReplace(&k.routes[0]); err != nil {
		return err
	}
	if k.rule != nil {
		if err := netlink.RuleAdd(k.rule); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
	}
	var desired []netlink.Route
	for _, r := range routes {
		if r.Local {
			continue
		}
		if !r.Prefix.Addr().Is6() || r.Prefix.Addr().Is4In6() {
			return errors.New("wendy net payload routing is IPv6-only")
		}
		n := netlink.Route{Table: RouteTable, Protocol: routeProtocol, Family: netlink.FAMILY_V6, Dst: ipNet(r.Prefix), Priority: 100, Type: unix.RTN_UNICAST}
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
			// A native IPv6 route names its next hop with RTA_GATEWAY; RTA_VIA
			// (netlink.Via) is the cross-family form and kernels reject it here.
			n.Gw = net.IP(r.NextHop.AsSlice())
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

func (k *Kernel) Stop() error {
	var errs []error
	for _, p := range k.peers {
		errs = append(errs, netlink.LinkSetDown(p.link))
	}
	return errors.Join(errs...)
}

func (k *Kernel) Close() error {
	var errs []error
	// Withdraw transit before removing its routes and interfaces.
	errs = append(errs, k.closeForwarding())
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
	if k.addressInstalled {
		err := removeLoopbackAddress(k.link, loopbackAddress{
			prefix: netip.PrefixFrom(k.device.MeshAddress(), 128), protocol: meshAddressProtocol,
		})
		errs = append(errs, err)
		if err == nil {
			k.addressInstalled = false
		}
	}
	return errors.Join(errs...)
}

// sysctls confines one local-mesh interface to Babel-managed addressing and
// routing. addr_gen_mode=1 (none) suppresses the kernel's MAC-derived
// link-local, so the only link-local on a TUN is the one both ends derived
// from the device UUIDs.
func sysctls(name string) error {
	for _, setting := range []struct {
		name     string
		value    string
		optional bool
	}{
		{"disable_ipv6", "0", false},
		{"accept_ra", "0", false},
		{"accept_ra_rt_info_max_plen", "0", true},
		{"autoconf", "0", false},
		{"router_solicitations", "0", false},
		{"addr_gen_mode", "1", false},
	} {
		path := "/proc/sys/net/ipv6/conf/" + name + "/" + setting.name
		// A namespace may preconfigure these defaults and mount sysctls
		// read-only. Existing correct state needs no write; a differing or
		// unreadable value still requires the normal successful update.
		if current, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(current)) == setting.value {
			continue
		}
		if err := os.WriteFile(path, []byte(setting.value+"\n"), 0o600); err != nil {
			if setting.optional && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("configure %s for %s: %w", setting.name, name, err)
		}
	}
	return nil
}

// reclaimReservedState checks the complete reserved state before deleting any
// of it. Links carry our alias, routes carry our protocol, and policy rules must
// match the exact base or peer signature. A table number alone is not ownership.
func reclaimReservedState() error {
	return reclaimReservedStateWith(netlinkKernelCleanup(), false)
}

type kernelCleanupOps struct {
	links         func() ([]netlink.Link, error)
	deleteLink    func(netlink.Link) error
	addresses     func(netlink.Link) ([]loopbackAddress, error)
	deleteAddress func(netlink.Link, loopbackAddress) error
	rules         func(int) ([]netlink.Rule, error)
	deleteRule    func(*netlink.Rule) error
	routes        func(int, *netlink.Route, uint64) ([]netlink.Route, error)
	deleteRoute   func(*netlink.Route) error
}

func netlinkKernelCleanup() kernelCleanupOps {
	return kernelCleanupOps{
		links: netlink.LinkList, deleteLink: netlink.LinkDel,
		addresses: readLoopbackAddresses, deleteAddress: removeLoopbackAddress,
		rules: netlink.RuleList, deleteRule: netlink.RuleDel,
		routes: netlink.RouteListFiltered, deleteRoute: netlink.RouteDel,
	}
}

func reclaimReservedStateWith(ops kernelCleanupOps, allowUnavailableQueries bool) error {
	links, err := ops.links()
	if err != nil {
		return kernelOperationError("inspect existing interfaces", "", err)
	}
	var ownedLinks []netlink.Link
	var loopback netlink.Link
	var ownedAddresses []loopbackAddress
	for _, l := range links {
		name := l.Attrs().Name
		if name == loopbackInterfaceName {
			if l.Attrs().Flags&net.FlagLoopback == 0 {
				return errors.New("lo is not a loopback interface")
			}
			loopback = l
			addresses, err := ops.addresses(l)
			if err != nil && !(allowUnavailableQueries && (errors.Is(err, unix.EAFNOSUPPORT) || errors.Is(err, unix.EOPNOTSUPP))) {
				return fmt.Errorf("inspect loopback mesh addresses: %w", err)
			}
			for _, address := range addresses {
				if address.owned() {
					ownedAddresses = append(ownedAddresses, address)
				}
			}
			continue
		}
		if name != legacyInterfaceName && !strings.HasPrefix(name, peerInterfacePrefix) {
			continue
		}
		if l.Attrs().Alias != interfaceOwner ||
			(name == legacyInterfaceName && l.Type() != "dummy") ||
			(name != legacyInterfaceName && (!ownedPeerInterfaceName(name) || l.Type() != "tuntap")) {
			return fmt.Errorf("interface %q exists and is not owned by wendy net", name)
		}
		ownedLinks = append(ownedLinks, l)
	}
	rules, err := ops.rules(netlink.FAMILY_ALL)
	if err != nil && !(allowUnavailableQueries && (errors.Is(err, unix.EAFNOSUPPORT) || errors.Is(err, unix.EOPNOTSUPP))) {
		return kernelOperationError("inspect existing IP policy rules", "", err)
	}
	var ownedRules []netlink.Rule
	for _, r := range rules {
		if r.Table == RouteTable {
			if !ownedKernelRule(r) {
				return fmt.Errorf("wendy net route table %d holds a foreign policy rule", RouteTable)
			}
			ownedRules = append(ownedRules, r)
		} else if r.Priority == RouteTable || r.Priority == RouteTable+1 {
			return fmt.Errorf("wendy net rule priority %d is already in use", r.Priority)
		}
	}
	var ownedRoutes []netlink.Route
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		routes, err := ops.routes(family, &netlink.Route{Table: RouteTable}, netlink.RT_FILTER_TABLE)
		if allowUnavailableQueries && (errors.Is(err, unix.EAFNOSUPPORT) || errors.Is(err, unix.EOPNOTSUPP)) {
			continue
		}
		if err != nil {
			return kernelOperationError(fmt.Sprintf("inspect route table %d for address family %d", RouteTable, family), "", err)
		}
		for _, r := range routes {
			if r.Protocol != routeProtocol {
				return fmt.Errorf("wendy net route table %d holds a foreign route", RouteTable)
			}
			ownedRoutes = append(ownedRoutes, r)
		}
	}
	for _, r := range ownedRules {
		if err := ops.deleteRule(&r); err != nil && !errors.Is(err, unix.ENOENT) {
			return kernelOperationError("reclaim owned IP policy rule", "", err)
		}
	}
	for _, r := range ownedRoutes {
		if err := ops.deleteRoute(&r); err != nil && !errors.Is(err, unix.ESRCH) {
			return kernelOperationError("reclaim owned wendy net route", "", err)
		}
	}
	for _, l := range ownedLinks {
		if err := ops.deleteLink(l); err != nil && !errors.Is(err, unix.ENODEV) {
			return kernelOperationError("reclaim owned interface "+l.Attrs().Name, "", err)
		}
	}
	for _, address := range ownedAddresses {
		if err := ops.deleteAddress(loopback, address); err != nil {
			return fmt.Errorf("reclaim loopback mesh address: %w", err)
		}
	}
	return nil
}
