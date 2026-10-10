//go:build linux

package localmesh

import (
	"net"
	"net/netip"
	"os"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/wendylabsinc/WendyOS/babel"
	"golang.org/x/sys/unix"
)

func TestKernelRecoversCrashStateIsolatedNamespace(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("requires disposable privileged container network namespace")
	}
	old, err := NewKernel(64, 445)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, peer := range old.peers {
			_ = peer.file.Close()
		}
	})
	if _, err := old.AddPeer(1, 460); err != nil {
		t.Fatal(err)
	}
	_, nextHop, _ := LinkAddresses(445, 460)
	if err := old.Apply([]babel.Route{{Prefix: netip.MustParsePrefix("10.88.1.204/32"), Link: 1, NextHop: nextHop}}); err != nil {
		t.Fatal(err)
	}
	if err := old.SetRoam(&babel.Route{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Link: 1, NextHop: nextHop}); err != nil {
		t.Fatal(err)
	}
	uplink := &netlink.Dummy{LinkAttrs: netlink.NewLinkAttrs()}
	uplink.Name = "wtestup0"
	if err := netlink.LinkAdd(uplink); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(uplink) })
	if err := netlink.AddrAdd(uplink, &netlink.Addr{IPNet: ipNet(netip.MustParsePrefix("192.0.2.2/24"))}); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(uplink); err != nil {
		t.Fatal(err)
	}
	sharing := netlink.Route{Table: RouteTable, Protocol: routeProtocol, Family: netlink.FAMILY_V4,
		Type: unix.RTN_UNICAST, Priority: 50, LinkIndex: uplink.Index,
		Gw: net.ParseIP("192.0.2.1"), Flags: unix.RTNH_F_ONLINK}
	if err := netlink.RouteAdd(&sharing); err != nil {
		t.Fatal(err)
	}
	// The old process dies without Close. Its abstract ownership socket is
	// released, but the dummy, routes and rules still occupy the namespace.
	if err := old.lock.Close(); err != nil {
		t.Fatal(err)
	}
	old.lock = nil
	foreign := netlink.Route{Table: RouteTable, Protocol: 99, Family: netlink.FAMILY_V4,
		Type: unix.RTN_UNREACHABLE, Priority: 200, Dst: ipNet(netip.MustParsePrefix("198.51.100.0/24"))}
	if err := netlink.RouteAdd(&foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := NewKernel(64, 445); err == nil {
		t.Fatal("recovered through a foreign route")
	}
	if _, err := netlink.LinkByName(InterfaceName); err != nil {
		t.Fatalf("ownership conflict removed stale dummy: %v", err)
	}
	if err := netlink.RouteDel(&foreign); err != nil {
		t.Fatal(err)
	}
	newKernel, err := NewKernel(64, 445)
	if err != nil {
		t.Fatalf("recover owned crash state: %v", err)
	}
	t.Cleanup(func() { _ = newKernel.Close() })
	if _, err := NewKernel(64, 445); err == nil {
		t.Fatal("second live agent acquired kernel")
	}
	if _, err := netlink.LinkByName("wlmp1"); err == nil {
		t.Fatal("stale peer TUN survived recovery")
	}
	if _, err := netlink.LinkByName(uplink.Name); err != nil {
		t.Fatalf("unrelated uplink was removed: %v", err)
	}
	meshRoutes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: RouteTable}, netlink.RT_FILTER_TABLE)
	if err != nil {
		t.Fatal(err)
	}
	if len(meshRoutes) != 1 || meshRoutes[0].Priority != 32767 || meshRoutes[0].Type != unix.RTN_UNREACHABLE {
		t.Fatalf("stale learned/sharing routes survived recovery: %v", meshRoutes)
	}
	mainRoutes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_TABLE)
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range mainRoutes {
		if defaultIPv4Route(route.Dst) && route.Priority == 5 {
			t.Fatal("stale roaming default survived recovery")
		}
	}
	if err := newKernel.ReconcileBase(); err != nil {
		t.Fatalf("new kernel base state: %v", err)
	}
}

func TestKernelRefusesForeignNamedLinkIsolatedNamespace(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("requires disposable privileged container network namespace")
	}
	foreign := &netlink.Dummy{LinkAttrs: netlink.NewLinkAttrs()}
	foreign.Name = InterfaceName
	foreign.Alias = "another-owner"
	if err := netlink.LinkAdd(foreign); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(foreign) })
	if _, err := NewKernel(64, 445); err == nil {
		t.Fatal("foreign wlmesh0 was reclaimed")
	}
	if _, err := netlink.LinkByName(InterfaceName); err != nil {
		t.Fatalf("foreign wlmesh0 was removed: %v", err)
	}
}

func TestKernelRecoversStrippedDummyIsolatedNamespace(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("requires disposable privileged container network namespace")
	}
	old, err := NewKernel(64, 445)
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.AddrDel(old.link, &netlink.Addr{IPNet: &net.IPNet{IP: old.address, Mask: net.CIDRMask(32, 32)}}); err != nil {
		t.Fatal(err)
	}
	if err := netlink.RuleDel(old.rule); err != nil {
		t.Fatal(err)
	}
	guard := old.routes[0]
	if err := netlink.RouteDel(&guard); err != nil {
		t.Fatal(err)
	}
	if err := old.lock.Close(); err != nil {
		t.Fatal(err)
	}
	old.lock = nil
	newKernel, err := NewKernel(64, 445)
	if err != nil {
		t.Fatalf("recover stripped wlmesh0: %v", err)
	}
	t.Cleanup(func() { _ = newKernel.Close() })
	if old.link.Index == newKernel.link.Index {
		t.Fatal("stale dummy was adopted rather than recreated")
	}
	if err := newKernel.ReconcileBase(); err != nil {
		t.Fatalf("new kernel base state: %v", err)
	}
}
