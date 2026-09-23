//go:build linux

package nanprovider

import (
	"testing"

	"github.com/vishvananda/netlink"
)

func TestOwnedNDIAddressRequiresExactPrivateUnderlay(t *testing.T) {
	owned, err := netlink.ParseAddr("10.89.1.189/16")
	if err != nil {
		t.Fatal(err)
	}
	other, err := netlink.ParseAddr("10.89.1.190/16")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		addrs []netlink.Addr
		want  bool
	}{
		{"abandoned own interface", []netlink.Addr{*owned}, true},
		{"unconfigured interface", nil, false},
		{"another asset", []netlink.Addr{*other}, false},
		{"shared interface", []netlink.Addr{*owned, *other}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ownedNDIAddress(tc.addrs, 445); got != tc.want {
				t.Fatalf("ownedNDIAddress = %v, want %v", got, tc.want)
			}
		})
	}
}
