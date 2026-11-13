//go:build linux

package localmesh

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"golang.org/x/sys/unix"
)

// TestKernelIsolatedNamespace exercises the real FIB: a device's own ULA address
// on the host link, a peer TUN addressed with the complementary link-local pair
// derived from the two device UUIDs, and a Babel route for the peer's ULA /128
// with a link-local next hop.
func TestKernelIsolatedNamespace(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("requires disposable privileged container network namespace")
	}
	self, peer := testDevices(t)
	k, err := NewKernel(self)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := k.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err = k.AddPeer(1, self); err == nil {
		t.Fatal("kernel accepted a link to our own device principal")
	}
	tun, err := k.AddPeer(1, peer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = k.AddPeer(1, peer); err == nil {
		t.Fatal("kernel accepted a duplicate link incarnation")
	}
	selfLink, peerLink, err := meshid.LinkLocalAddresses(self, peer)
	if err != nil {
		t.Fatal(err)
	}
	if selfLink.String() != "fe80::1" || peerLink.String() != "fe80::2" {
		t.Fatalf("TUN addressing is not the UUID-ordered pair: %s / %s", selfLink, peerLink)
	}
	dst := netip.PrefixFrom(peer.MeshAddress(), 128)
	route := babel.Route{Prefix: dst, RouterID: babel.RouterIDFromRandom([6]byte{0x2b, 0x1c, 0, 0, 0x46, 0}), Link: 1, NextHop: peerLink, Metric: 256}
	if err = k.Apply([]babel.Route{route}); err != nil {
		t.Fatal(err)
	}
	got, err := netlink.RouteGet(peer.MeshAddress().AsSlice())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].LinkIndex != k.peers[1].link.Index {
		t.Fatalf("wrong FIB: %v", got)
	}
	conn, err := net.DialUDP("udp6", nil, &net.UDPAddr{IP: peer.MeshAddress().AsSlice(), Port: 8080})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	done := make(chan []byte, 1)
	go func() {
		buf := make([]byte, TunnelMTU)
		for {
			n, err := tun.Read(buf)
			if err != nil {
				done <- nil
				return
			}
			// Interface setup also emits IPv6 multicast control traffic.
			// Wait for the injected UDP packet before checking its addresses.
			if n >= 40 && buf[0]>>4 == 6 && buf[6] == 17 {
				done <- buf[:n]
				return
			}
		}
	}()
	if _, err = conn.Write([]byte("local-mesh-kernel-test")); err != nil {
		t.Fatal(err)
	}
	select {
	case packet := <-done:
		if !validIP(packet) || packet[0]>>4 != 6 {
			t.Fatalf("invalid TUN output: %x", packet)
		}
		src, dstAddr, err := ipv6Addresses(packet)
		if err != nil {
			t.Fatal(err)
		}
		if src != self.MeshAddress() || dstAddr != peer.MeshAddress() {
			t.Fatalf("payload left with %s -> %s, want %s -> %s", src, dstAddr, self.MeshAddress(), peer.MeshAddress())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no IP packet delivered through TUN")
	}
	if err = k.Apply([]babel.Route{{Prefix: dst, Unreachable: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err = netlink.RouteGet(peer.MeshAddress().AsSlice()); err == nil {
		t.Fatal("unreachable FIB fell through")
	}
	if err = k.Apply(nil); err != nil {
		t.Fatal(err)
	}
	if _, err = netlink.RouteGet(peer.MeshAddress().AsSlice()); err == nil {
		t.Fatal("empty table fell through to default")
	}
}

// TestKernelRefusesForeignTableOwnership runs in the same disposable namespace:
// a route in the reserved table that does not carry this package's protocol is
// never adopted. A route that does carry it is this package's own leftover from
// an abrupt exit, which NewKernel reclaims instead (see
// TestKernelCrashRecoveryIsolatedNamespace).
func TestKernelRefusesForeignTableOwnership(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("requires disposable privileged container network namespace")
	}
	self, _ := testDevices(t)
	owned := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: InterfaceName, Alias: interfaceOwner}}
	if err := netlink.LinkAdd(owned); err != nil {
		t.Fatal(err)
	}
	defer netlink.LinkDel(owned)
	foreign := netlink.Route{Table: RouteTable, Protocol: 187, Family: netlink.FAMILY_V6, Dst: ipNet(netip.MustParsePrefix("fd00::445:1/128")), LinkIndex: 1}
	if err := netlink.RouteReplace(&foreign); err != nil {
		t.Skipf("cannot plant a foreign route: %v", err)
	}
	defer netlink.RouteDel(&foreign)
	rule := netlink.NewRule()
	rule.Family = netlink.FAMILY_V6
	rule.Priority = RouteTable + 10
	rule.Table = RouteTable
	rule.Dst = foreign.Dst
	if err := netlink.RuleAdd(rule); err != nil {
		t.Fatal(err)
	}
	defer netlink.RuleDel(rule)
	if _, err := NewKernel(self); err == nil {
		t.Fatal("kernel adopted a foreign route in the reserved table")
	}
	if live, err := netlink.LinkByName(InterfaceName); err != nil || live.Attrs().Index != owned.Index {
		t.Fatalf("ownership refusal removed the pre-existing owned link: %v", err)
	}
	rules, err := netlink.RuleList(netlink.FAMILY_V6)
	if err != nil {
		t.Fatal(err)
	}
	for _, live := range rules {
		if live.Priority == rule.Priority && live.Table == rule.Table && live.Dst != nil && live.Dst.String() == rule.Dst.String() {
			return
		}
	}
	t.Fatal("refusing a foreign route still deleted its unrelated policy rule")
}

func TestKernelRefusesForeignRuleInReservedTable(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("requires disposable privileged container network namespace")
	}
	self, _ := testDevices(t)
	rule := netlink.NewRule()
	rule.Family = netlink.FAMILY_V6
	rule.Priority = RouteTable + 10
	rule.Table = RouteTable
	rule.Dst = ipNet(netip.MustParsePrefix("fd00::1234/128"))
	if err := netlink.RuleAdd(rule); err != nil {
		t.Fatal(err)
	}
	defer netlink.RuleDel(rule)
	if kernel, err := NewKernel(self); err == nil {
		_ = kernel.Close()
		t.Fatal("kernel removed and adopted an unrelated rule selecting the reserved table")
	}
	rules, err := netlink.RuleList(netlink.FAMILY_V6)
	if err != nil {
		t.Fatal(err)
	}
	for _, live := range rules {
		if live.Priority == rule.Priority && live.Table == rule.Table && live.Dst != nil && live.Dst.String() == rule.Dst.String() {
			return
		}
	}
	t.Fatal("foreign rule was removed")
}

// ipv6Addresses returns the source and destination of an IPv6 packet.
func ipv6Addresses(p []byte) (netip.Addr, netip.Addr, error) {
	if len(p) < 40 || p[0]>>4 != 6 {
		return netip.Addr{}, netip.Addr{}, errors.New("not an IPv6 packet")
	}
	src, ok := netip.AddrFromSlice(p[8:24])
	if !ok {
		return netip.Addr{}, netip.Addr{}, errors.New("invalid IPv6 source")
	}
	dst, ok := netip.AddrFromSlice(p[24:40])
	if !ok {
		return netip.Addr{}, netip.Addr{}, errors.New("invalid IPv6 destination")
	}
	return src.Unmap(), dst.Unmap(), nil
}

// TestKernelReconcileBaseIsolatedNamespace runs in the same disposable
// namespace: a peer's advertised default coexists with the guard, base state
// stripped behind the instance's back is restored, and foreign owners of the
// guard, the link or the reserved rule slot are refused.
func TestKernelReconcileBaseIsolatedNamespace(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("requires disposable privileged container network namespace")
	}
	self, peer := testDevices(t)
	k, err := NewKernel(self)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := k.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := k.AddPeer(1, peer); err != nil {
		t.Fatal(err)
	}
	_, nextHop, err := meshid.LinkLocalAddresses(self, peer)
	if err != nil {
		t.Fatal(err)
	}
	// A signed peer advertises a default into the reserved table. Periodic
	// reconciliation treats it as an owned route, not a foreign default.
	if err := k.Apply([]babel.Route{{Prefix: netip.MustParsePrefix("::/0"), Link: 1, NextHop: nextHop, Metric: 256}}); err != nil {
		t.Fatal(err)
	}
	if err := k.ReconcileBase(); err != nil {
		t.Fatalf("reconcile with learned Babel default: %v", err)
	}
	if err := k.Apply(nil); err != nil {
		t.Fatal(err)
	}
	own := ipNet(netip.PrefixFrom(self.MeshAddress(), 128))
	live, err := netlink.LinkByName(InterfaceName)
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.AddrDel(live, &netlink.Addr{IPNet: own}); err != nil {
		t.Fatal(err)
	}
	if err := netlink.RuleDel(k.rule); err != nil {
		t.Fatal(err)
	}
	guard := k.routes[0]
	if err := netlink.RouteDel(&guard); err != nil {
		t.Fatal(err)
	}
	foreign := netlink.Route{Table: RouteTable, Protocol: 99, Family: netlink.FAMILY_V6,
		Type: unix.RTN_UNICAST, Priority: 200, LinkIndex: 1,
		Dst: ipNet(netip.MustParsePrefix("2001:db8::/64"))}
	if err := netlink.RouteAdd(&foreign); err != nil {
		t.Fatal(err)
	}
	// A conflicting default must fail before any missing state is repaired.
	foreignDefault := netlink.Route{Table: RouteTable, Protocol: 99, Family: netlink.FAMILY_V6,
		Type: unix.RTN_UNREACHABLE, Priority: 200, Dst: ipNet(netip.MustParsePrefix("::/0"))}
	if err := netlink.RouteAdd(&foreignDefault); err != nil {
		t.Fatal(err)
	}
	if err := k.ReconcileBase(); err == nil {
		t.Fatal("foreign default route accepted")
	}
	addresses, err := netlink.AddrList(live, netlink.FAMILY_V6)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addresses {
		if a.IP.Equal(k.address) {
			t.Fatal("repaired address before checking route conflict")
		}
	}
	if err := netlink.RouteDel(&foreignDefault); err != nil {
		t.Fatal(err)
	}
	if err := k.ReconcileBase(); err != nil {
		t.Fatalf("restore missing base state: %v", err)
	}
	if err := k.ReconcileBase(); err != nil {
		t.Fatalf("idempotent reconciliation: %v", err)
	}
	if err := netlink.LinkSetDown(live); err != nil {
		t.Fatal(err)
	}
	if err := k.ReconcileBase(); err != nil {
		t.Fatalf("restore link state: %v", err)
	}
	live, err = netlink.LinkByName(InterfaceName)
	if err != nil {
		t.Fatal(err)
	}
	if live.Attrs().Flags&net.FlagUp == 0 {
		t.Fatal("wendy net link was not brought up")
	}
	addresses, err = netlink.AddrList(live, netlink.FAMILY_V6)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range addresses {
		if a.IP.Equal(k.address) && a.IPNet != nil && a.IPNet.String() == own.String() {
			found = true
		}
	}
	if !found {
		t.Fatal("own /128 address not restored")
	}
	rules, err := netlink.RuleList(netlink.FAMILY_V6)
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, r := range rules {
		if r.Priority == RouteTable && r.Table == RouteTable && r.Dst != nil && r.Dst.String() == meshid.MeshPrefix.String() {
			found = true
		}
	}
	if !found {
		t.Fatal("own policy rule not restored")
	}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V6, &netlink.Route{Table: RouteTable}, netlink.RT_FILTER_TABLE)
	if err != nil {
		t.Fatal(err)
	}
	guardFound, foreignFound := false, false
	for _, r := range routes {
		if defaultIPv6Route(r.Dst) && r.Priority == 32767 && r.Type == unix.RTN_UNREACHABLE && r.Protocol == routeProtocol {
			guardFound = true
		}
		if r.Dst != nil && r.Dst.String() == "2001:db8::/64" && r.Protocol == 99 {
			foreignFound = true
		}
	}
	if !guardFound || !foreignFound {
		t.Fatalf("guard or foreign route lost: guard=%v foreign=%v", guardFound, foreignFound)
	}
	if err := netlink.RouteDel(&foreign); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetAlias(live, "another-owner"); err != nil {
		t.Fatal(err)
	}
	if err := k.ReconcileBase(); err == nil {
		t.Fatal("foreign replacement link accepted")
	}
	if err := netlink.LinkSetAlias(live, interfaceOwner); err != nil {
		t.Fatal(err)
	}
	if err := netlink.RuleDel(k.rule); err != nil {
		t.Fatal(err)
	}
	conflictRule := netlink.NewRule()
	conflictRule.Family = netlink.FAMILY_V6
	conflictRule.Priority = RouteTable
	conflictRule.Table = 254
	if err := netlink.RuleAdd(conflictRule); err != nil {
		t.Fatal(err)
	}
	if err := k.ReconcileBase(); err == nil {
		t.Fatal("foreign rule at reserved priority accepted")
	}
	if err := netlink.RuleDel(conflictRule); err != nil {
		t.Fatal(err)
	}
	if err := k.ReconcileBase(); err != nil {
		t.Fatal(err)
	}
	if err := netlink.RouteDel(&guard); err != nil {
		t.Fatal(err)
	}
	foreignGuard := guard
	foreignGuard.Protocol = 99
	if err := netlink.RouteAdd(&foreignGuard); err != nil {
		t.Fatal(err)
	}
	if err := k.ReconcileBase(); err == nil {
		t.Fatal("foreign guard route accepted")
	}
	if err := netlink.RouteDel(&foreignGuard); err != nil {
		t.Fatal(err)
	}
	if err := k.ReconcileBase(); err != nil {
		t.Fatal(err)
	}
}

// TestKernelRoamingIsolatedNamespace exercises the borrowed IPv4 default:
// installed in the main table at the reserved priority with the host's
// transport address as source and the peer's link-local as next hop, refused
// when a foreign writer holds that priority, withdrawn with the link, and
// swept from a crashed predecessor.
