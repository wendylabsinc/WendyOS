//go:build linux

package nanprovider

import (
	"fmt"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func underlayDevice(t *testing.T, last uint16) meshid.Device {
	t.Helper()
	id, err := meshid.ParseDevice(testTenant(t), "018b8657-2f1a-4d3e-9c1a-"+fmt.Sprintf("%012x", last))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestOwnedNDIAddressRequiresExactPrivateUnderlay(t *testing.T) {
	owned, err := netlink.ParseAddr("10.89.1.189/16")
	if err != nil {
		t.Fatal(err)
	}
	other, err := netlink.ParseAddr("10.89.1.190/16")
	if err != nil {
		t.Fatal(err)
	}
	self := underlayDevice(t, 0x01bd) // 10.89.1.189
	for _, tc := range []struct {
		name  string
		addrs []netlink.Addr
		id    meshid.Device
		want  bool
	}{
		{"abandoned own interface", []netlink.Addr{*owned}, self, true},
		{"unconfigured interface", nil, self, false},
		{"another device", []netlink.Addr{*other}, self, false},
		{"shared interface", []netlink.Addr{*owned, *other}, self, false},
		{"other device owns it", []netlink.Addr{*other}, underlayDevice(t, 0x01be), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ownedNDIAddress(tc.addrs, tc.id); got != tc.want {
				t.Fatalf("ownedNDIAddress = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUnderlayAddressReservesNetworkAndBroadcast(t *testing.T) {
	for _, last := range []uint16{0x0000, 0xffff} {
		if _, err := underlayAddr(underlayDevice(t, last)); err == nil {
			t.Errorf("accepted reserved underlay for last UUID bytes %04x", last)
		}
	}
	addr, err := underlayAddr(underlayDevice(t, 0x01bd))
	if err != nil || addr.String() != "10.89.1.189" {
		t.Fatalf("underlayAddr = %s, %v", addr, err)
	}
}
