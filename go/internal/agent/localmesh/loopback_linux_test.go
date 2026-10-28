//go:build linux

package localmesh

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

func TestLoopbackAddressOwnership(t *testing.T) {
	for _, tc := range []struct {
		prefix          string
		protocol, scope uint8
		owned           bool
	}{
		{"fd45:b891:567f::1234/128", meshAddressProtocol, 0, true},
		{"fd45:b891:567f::1234/128", 0, 0, false},
		{"fd45:b891:567f::1234/64", meshAddressProtocol, 0, false},
		{"::1/128", meshAddressProtocol, unix.RT_SCOPE_HOST, false},
		{"2001:db8::1/128", meshAddressProtocol, 0, false},
		{"fd45:b891:567f::1234/128", meshAddressProtocol, unix.RT_SCOPE_HOST, false},
	} {
		a := loopbackAddress{prefix: netip.MustParsePrefix(tc.prefix), protocol: tc.protocol, scope: tc.scope}
		if a.owned() != tc.owned {
			t.Fatalf("ownership of %+v: want %v", a, tc.owned)
		}
	}
}

func TestParseLoopbackAddressProtocol(t *testing.T) {
	msg := nl.NewIfAddrmsg(unix.AF_INET6)
	msg.Index, msg.Prefixlen = 1, 128
	data := append([]byte{}, msg.Serialize()...)
	data = append(data, nl.NewRtAttr(unix.IFA_ADDRESS, netip.MustParseAddr("fd45:b891:567f::1234").AsSlice()).Serialize()...)
	untagged := append([]byte{}, data...)
	data = append(data, nl.NewRtAttr(ifaProto, []byte{meshAddressProtocol}).Serialize()...)
	addresses, err := parseLoopbackAddresses([][]byte{data}, 1)
	if err != nil || len(addresses) != 1 || !addresses[0].owned() {
		t.Fatalf("lost address owner: %v, %v", addresses, err)
	}
	if err := requireLoopbackAddressProtocol(addresses); err != nil {
		t.Fatal(err)
	}
	addresses, err = parseLoopbackAddresses([][]byte{untagged}, 1)
	if err != nil || len(addresses) != 1 || addresses[0].owned() {
		t.Fatalf("adopted untagged address: %v, %v", addresses, err)
	}
	if err := requireLoopbackAddressProtocol(addresses); err == nil {
		t.Fatal("accepted a kernel without address protocol metadata")
	}
	zeroProtocol := append(append([]byte{}, untagged...), nl.NewRtAttr(ifaProto, []byte{0}).Serialize()...)
	zeroAddresses, err := parseLoopbackAddresses([][]byte{zeroProtocol}, 1)
	if err != nil || requireLoopbackAddressProtocol(zeroAddresses) != nil {
		t.Fatalf("zero-valued protocol still indicates kernel support: %v", err)
	}
	if err := requireLoopbackAddressProtocol(nil); err == nil {
		t.Fatal("accepted missing kernel capability evidence")
	}
	addresses, err = parseLoopbackAddresses([][]byte{data}, 2)
	if err != nil || len(addresses) != 0 {
		t.Fatalf("accepted another interface's address: %v, %v", addresses, err)
	}
	for _, malformed := range [][]byte{{1}, append(untagged, nl.NewRtAttr(ifaProto, nil).Serialize()...)} {
		if _, err := parseLoopbackAddresses([][]byte{malformed}, 1); err == nil {
			t.Fatal("accepted malformed address metadata")
		}
	}
}

func TestResetKernelCleanupPreservesSharedLoopback(t *testing.T) {
	ops := emptyKernelCleanupOps(t)
	lo := &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: loopbackInterfaceName, Index: 1, Flags: net.FlagLoopback}}
	owned := loopbackAddress{prefix: netip.MustParsePrefix("fd45:b891:567f::1234/128"), protocol: meshAddressProtocol}
	ops.links = func() ([]netlink.Link, error) { return []netlink.Link{lo}, nil }
	ops.addresses = func(netlink.Link) ([]loopbackAddress, error) {
		return []loopbackAddress{
			{prefix: netip.MustParsePrefix("::1/128"), protocol: 1, scope: unix.RT_SCOPE_HOST},
			{prefix: netip.MustParsePrefix("fd45:b891:567f::4321/128")},
			{prefix: netip.MustParsePrefix("2001:db8::1/128"), protocol: meshAddressProtocol},
			owned,
		}, nil
	}
	deleted := 0
	ops.deleteAddress = func(link netlink.Link, a loopbackAddress) error {
		if link != lo || a != owned {
			t.Fatalf("removed another owner's address: %+v", a)
		}
		deleted++
		return nil
	}
	if err := reclaimReservedStateWith(ops, true); err != nil || deleted != 1 {
		t.Fatalf("cleanup: deleted=%d, err=%v", deleted, err)
	}
	// Do not remove addresses before refusing foreign routing state.
	deleted = 0
	ops.rules = func(int) ([]netlink.Rule, error) { return []netlink.Rule{{Table: RouteTable, Priority: 99}}, nil }
	if err := reclaimReservedStateWith(ops, true); err == nil || deleted != 0 {
		t.Fatalf("foreign rule caused partial cleanup: deleted=%d, err=%v", deleted, err)
	}
	ops.rules = func(int) ([]netlink.Rule, error) { return nil, nil }
	ops.deleteAddress = func(netlink.Link, loopbackAddress) error { return unix.EPERM }
	if err := reclaimReservedStateWith(ops, true); !errors.Is(err, unix.EPERM) {
		t.Fatalf("address deletion failure was hidden: %v", err)
	}
}

func isolatedLoopback(t *testing.T) netlink.Link {
	t.Helper()
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("requires disposable Linux network namespace with NET_ADMIN")
	}
	lo, err := meshLoopback()
	if err != nil {
		t.Fatal(err)
	}
	return lo
}

func assertNoOwnedLoopbackAddresses(t *testing.T) {
	t.Helper()
	lo, err := meshLoopback()
	if err != nil {
		t.Fatal(err)
	}
	addresses, err := readLoopbackAddresses(lo)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addresses {
		if a.owned() {
			t.Fatalf("shutdown retained Wendy address %s on lo", a.prefix)
		}
	}
}

func TestKernelLoopbackPreservesHostIsolatedNamespace(t *testing.T) {
	lo := isolatedLoopback(t)
	for _, prefix := range []string{"2001:db8::1234/128", "fd45:b891:567f::abcd/128"} {
		a := &netlink.Addr{IPNet: ipNet(netip.MustParsePrefix(prefix)), Flags: unix.IFA_F_NODAD}
		if err := netlink.AddrAdd(lo, a); err != nil {
			t.Fatal(err)
		}
		defer netlink.AddrDel(lo, a)
	}
	before, err := readLoopbackAddresses(lo)
	if err != nil {
		t.Fatal(err)
	}
	sysctl := func() []byte {
		t.Helper()
		var values []byte
		for _, name := range []string{"disable_ipv6", "accept_ra", "autoconf", "addr_gen_mode"} {
			value, err := os.ReadFile("/proc/sys/net/ipv6/conf/lo/" + name)
			if err != nil {
				t.Fatal(err)
			}
			values = append(values, value...)
		}
		return values
	}
	settings := sysctl()
	self, _ := testDevices(t)
	k, err := NewKernel(self)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	if _, err := netlink.LinkByName(legacyInterfaceName); err == nil {
		t.Fatal("created a legacy dummy interface")
	}
	if present, err := inspectLoopbackAddress(lo, self.MeshAddress()); err != nil || !present {
		t.Fatalf("missing tagged /128: %v", err)
	}
	listener, err := net.ListenPacket("udp6", net.JoinHostPort(self.MeshAddress().String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := listener.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := listener.WriteTo([]byte("mesh-on-loopback"), listener.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if n, _, err := listener.ReadFrom(buf); err != nil || string(buf[:n]) != "mesh-on-loopback" {
		t.Fatalf("local mesh address is not usable: %v", err)
	}
	if err := k.Close(); err != nil {
		t.Fatal(err)
	}
	// Reset is also idempotent and must preserve untagged mesh addresses.
	if err := CleanupStaleKernel(); err != nil {
		t.Fatal(err)
	}
	after, err := readLoopbackAddresses(lo)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("host addresses changed: before=%v after=%v err=%v", before, after, err)
	}
	live, err := meshLoopback()
	if err != nil {
		t.Fatal(err)
	}
	if live.Attrs().MTU != lo.Attrs().MTU || live.Attrs().Alias != lo.Attrs().Alias || live.Attrs().Flags != lo.Attrs().Flags || !reflect.DeepEqual(settings, sysctl()) {
		t.Fatal("mesh changed shared loopback settings")
	}
	assertNoOwnedLoopbackAddresses(t)
}

func TestKernelRefusesForeignLoopbackAddressIsolatedNamespace(t *testing.T) {
	lo := isolatedLoopback(t)
	self, _ := testDevices(t)
	a := &netlink.Addr{IPNet: ipNet(netip.PrefixFrom(self.MeshAddress(), 128)), Flags: unix.IFA_F_NODAD}
	if err := netlink.AddrAdd(lo, a); err != nil {
		t.Fatal(err)
	}
	defer netlink.AddrDel(lo, a)
	if k, err := NewKernel(self); err == nil {
		k.Close()
		t.Fatal("adopted untagged loopback address")
	}
	if err := CleanupStaleKernel(); err != nil {
		t.Fatal(err)
	}
	if _, err := netlink.RouteGet(self.MeshAddress().AsSlice()); err != nil {
		t.Fatalf("removed foreign local address: %v", err)
	}
}

func TestKernelRefusesReplacedLoopbackAddressIsolatedNamespace(t *testing.T) {
	lo := isolatedLoopback(t)
	self, _ := testDevices(t)
	k, err := NewKernel(self)
	if err != nil {
		t.Fatal(err)
	}
	a := &netlink.Addr{IPNet: ipNet(netip.PrefixFrom(self.MeshAddress(), 128)), Flags: unix.IFA_F_NODAD}
	if err := netlink.AddrDel(lo, a); err != nil {
		t.Fatal(err)
	}
	if err := netlink.AddrAdd(lo, a); err != nil {
		t.Fatal(err)
	}
	defer netlink.AddrDel(lo, a)
	if err := k.ReconcileBase(); err == nil {
		t.Fatal("adopted replacement address")
	}
	if err := k.Close(); err == nil {
		t.Fatal("removed replacement address")
	}
	if _, err := netlink.RouteGet(self.MeshAddress().AsSlice()); err != nil {
		t.Fatalf("replacement address lost: %v", err)
	}
}

func TestKernelMigratesLegacyHostIsolatedNamespace(t *testing.T) {
	isolatedLoopback(t)
	self, _ := testDevices(t)
	legacy := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: legacyInterfaceName, Alias: interfaceOwner}}
	if err := netlink.LinkAdd(legacy); err != nil {
		t.Fatal(err)
	}
	defer netlink.LinkDel(legacy)
	if err := netlink.LinkSetAlias(legacy, interfaceOwner); err != nil {
		t.Fatal(err)
	}
	if err := netlink.AddrAdd(legacy, &netlink.Addr{IPNet: ipNet(netip.PrefixFrom(self.MeshAddress(), 128))}); err != nil {
		t.Fatal(err)
	}
	k, err := NewKernel(self)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	if _, err := netlink.LinkByName(legacyInterfaceName); err == nil {
		t.Fatal("legacy link retained")
	}
	if present, err := inspectLoopbackAddress(k.link, self.MeshAddress()); err != nil || !present {
		t.Fatalf("migration: %v", err)
	}
}
