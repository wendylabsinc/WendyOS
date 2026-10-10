//go:build linux

package localmesh

import (
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/wendylabsinc/WendyOS/babel"
	"golang.org/x/sys/unix"
)

func TestKernelIsolatedNamespace(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("requires disposable privileged container network namespace")
	}
	k, err := NewKernel(64, 445)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := k.Close(); err != nil {
			t.Error(err)
		}
	})
	tun, err := k.AddPeer(1, 460)
	if err != nil {
		t.Fatal(err)
	}
	_, peer, _ := LinkAddresses(445, 460)
	dst := netip.MustParsePrefix("10.88.1.204/32")
	if err = k.Apply([]babel.Route{{Prefix: dst, RouterID: babel.RouterID(64<<32 | 460), Link: 1, NextHop: peer, Metric: 256}}); err != nil {
		t.Fatal(err)
	}
	got, err := netlink.RouteGet(net.ParseIP("10.88.1.204"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].LinkIndex != k.peers[1].link.Index {
		t.Fatalf("wrong FIB: %v", got)
	}
	conn, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.ParseIP("10.88.1.204"), Port: 8080})
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
			// Bringing up IPv6 link-local may emit a kernel router solicitation
			// before the test's IPv4 packet; it is not routed application data.
			if n > 0 && buf[0]>>4 == 4 {
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
		if !validIP(packet) || packet[0]>>4 != 4 {
			t.Fatalf("invalid TUN output: %x", packet)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no IP packet delivered through TUN")
	}
	if err = k.Apply([]babel.Route{{Prefix: dst, Unreachable: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err = netlink.RouteGet(net.ParseIP("10.88.1.204")); err == nil {
		t.Fatal("unreachable FIB fell through")
	}
	if err = k.Apply(nil); err != nil {
		t.Fatal(err)
	}
	if _, err = netlink.RouteGet(net.ParseIP("10.88.1.204")); err == nil {
		t.Fatal("empty table fell through to default")
	}
}

func TestKernelReconcileBaseIsolatedNamespace(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("requires disposable privileged container network namespace")
	}
	k, err := NewKernel(64, 445)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := k.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := k.AddPeer(1, 460); err != nil {
		t.Fatal(err)
	}
	if err := k.ReconcileBase(); err != nil {
		t.Fatalf("initial reconciliation: %v", err)
	}
	_, nextHop, _ := LinkAddresses(445, 460)
	if err := k.Apply([]babel.Route{{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Link: 1, NextHop: nextHop}}); err != nil {
		t.Fatal(err)
	}
	if err := k.ReconcileBase(); err != nil {
		t.Fatalf("reconcile with learned Babel default: %v", err)
	}
	if err := k.Apply(nil); err != nil {
		t.Fatal(err)
	}
	// The internet-sharing exit is a second owned default in the mesh table.
	// Periodic reconciliation must leave it in place alongside the guard.
	exit := netlink.Route{Table: RouteTable, Protocol: routeProtocol, Family: netlink.FAMILY_V4,
		Type: unix.RTN_UNICAST, Priority: 50, LinkIndex: k.link.Index,
		Dst: ipNet(netip.MustParsePrefix("0.0.0.0/0"))}
	if err := netlink.RouteAdd(&exit); err != nil {
		t.Fatal(err)
	}
	k.exit = &exit
	if err := k.ReconcileBase(); err != nil {
		t.Fatalf("reconcile with owned sharing exit: %v", err)
	}
	if err := netlink.RouteDel(&exit); err != nil {
		t.Fatal(err)
	}
	k.exit = nil
	live, err := netlink.LinkByName(InterfaceName)
	if err != nil {
		t.Fatal(err)
	}
	addr := &netlink.Addr{IPNet: &net.IPNet{IP: k.address, Mask: net.CIDRMask(32, 32)}}
	if err := netlink.AddrDel(live, addr); err != nil {
		t.Fatal(err)
	}
	if err := netlink.RuleDel(k.rule); err != nil {
		t.Fatal(err)
	}
	guard := k.routes[0]
	if err := netlink.RouteDel(&guard); err != nil {
		t.Fatal(err)
	}
	foreign := netlink.Route{Table: RouteTable, Protocol: 99, Family: netlink.FAMILY_V4,
		Type: unix.RTN_UNREACHABLE, Priority: 200, Dst: ipNet(netip.MustParsePrefix("192.0.2.0/24"))}
	if err := netlink.RouteAdd(&foreign); err != nil {
		t.Fatal(err)
	}
	// A conflicting default must fail before any missing state is repaired.
	foreignGuard := guard
	foreignGuard.Protocol = 99
	if err := netlink.RouteAdd(&foreignGuard); err != nil {
		t.Fatal(err)
	}
	if err := k.ReconcileBase(); err == nil {
		t.Fatal("foreign guard route accepted")
	}
	addresses, err := netlink.AddrList(live, netlink.FAMILY_V4)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addresses {
		if a.IP.Equal(k.address) {
			t.Fatal("repaired address before checking route conflict")
		}
	}
	if err := netlink.RouteDel(&foreignGuard); err != nil {
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
		t.Fatal("local-mesh link was not brought up")
	}
	addresses, err = netlink.AddrList(live, netlink.FAMILY_V4)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range addresses {
		if a.IP.Equal(k.address) && a.IPNet.String() == addr.IPNet.String() {
			found = true
		}
	}
	if !found {
		t.Fatal("own /32 address not restored")
	}
	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, r := range rules {
		if r.Priority == RouteTable && r.Table == RouteTable && r.Dst != nil && r.Dst.String() == "10.88.0.0/16" {
			found = true
		}
	}
	if !found {
		t.Fatal("own policy rule not restored")
	}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: RouteTable}, netlink.RT_FILTER_TABLE)
	if err != nil {
		t.Fatal(err)
	}
	guardFound, foreignFound := false, false
	for _, r := range routes {
		if defaultIPv4Route(r.Dst) && r.Priority == 32767 && r.Type == unix.RTN_UNREACHABLE && r.Protocol == routeProtocol {
			guardFound = true
		}
		if r.Dst != nil && r.Dst.String() == "192.0.2.0/24" && r.Protocol == 99 {
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
	conflict := netlink.NewRule()
	conflict.Family = netlink.FAMILY_V4
	conflict.Priority = RouteTable
	conflict.Table = 254
	if err := netlink.RuleAdd(conflict); err != nil {
		t.Fatal(err)
	}
	if err := k.ReconcileBase(); err == nil {
		t.Fatal("foreign rule at reserved priority accepted")
	}
	if err := netlink.RuleDel(conflict); err != nil {
		t.Fatal(err)
	}
	if err := k.ReconcileBase(); err != nil {
		t.Fatal(err)
	}
	if err := netlink.RouteDel(&guard); err != nil {
		t.Fatal(err)
	}
	foreignGuard = guard
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
