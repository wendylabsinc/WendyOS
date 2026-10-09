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

func TestNANRecoveryEscalatesOnlyOwnedSessionAndOnlyOnce(t *testing.T) {
	owned := nanRecovery{ownsSession: true}
	if !owned.canReset() || owned.attempted() {
		t.Fatal("owned session should permit its first data-path recovery")
	}
	if got := owned.next(false); got != resetNDI || !owned.canReset() {
		t.Fatalf("first failure: action=%v, canReset=%v", got, owned.canReset())
	}
	if got := owned.next(true); got != resetOwnedSession || owned.canReset() {
		t.Fatalf("persistent no-RX after NDI reset: action=%v, canReset=%v", got, owned.canReset())
	}
	if got := owned.next(true); got != resetNone {
		t.Fatalf("third failure restarted NAN again: %v", got)
	}
	warm := nanRecovery{ownsSession: true}
	if warm.next(false) != resetNDI || warm.next(false) != resetNone {
		t.Fatal("authentication failure with RX must not restart NAN interface")
	}

	external := nanRecovery{}
	if external.canReset() || external.attempted() || external.next(true) != resetNone {
		t.Fatal("external NAN session must not be reset")
	}
}
