package ipcam

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

func TestDHCPInterfaceInspectionIncludesIPv6AndErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		ip   string
		err  error
		want bool
	}{
		{name: "IPv6 link-local is normal on a camera port", ip: "fe80::1", want: true},
		{name: "private IPv6 configuration", ip: "fdab:1234::1"},
		{name: "global IPv6 configuration", ip: "2001:db8::1"},
		{name: "address inspection failure", err: errors.New("address lookup failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ifaces := interfacesFrom([]net.Interface{{Name: "eth0", Index: 1, Flags: net.FlagUp}},
				func(string) bool { return true }, func(net.Interface) ([]net.Addr, error) {
					return []net.Addr{&net.IPNet{IP: net.ParseIP(tc.ip), Mask: net.CIDRMask(64, 128)}}, tc.err
				})
			if len(ifaces) != 1 || ifaces[0].Eligible() != tc.want {
				t.Fatalf("eligibility after address inspection: %+v", ifaces)
			}
			h := newManagerHarness(t, ifaces)
			configureDHCPControl(t, h, map[string]bool{})
			if _, err := h.mgr.SetDHCPEnabled("eth0", true); (err == nil) != tc.want {
				t.Fatalf("enable error = %v, want allowed=%v", err, tc.want)
			}
			if tc.err != nil {
				s := h.mgr.DHCPStatus()
				if s.InterfaceError == "" || s.Interfaces[0].State != "unknown" {
					t.Fatalf("address inspection failure was hidden: %+v", s)
				}
			}
		})
	}
}

func TestDHCPUnknownAddressesPreventNewClaimsUntilRecovery(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0"), cabledEth("eth1")})
	configureDHCPControl(t, h, map[string]bool{"eth0": true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.mgr.scanOnce(ctx)
	waitFor(t, "camera watcher", func() bool { return len(h.watchedLinks()) == 2 })
	h.feed("eth0", &Packet{Type: Discover})
	h.advance(unansweredWindow)
	h.mu.Lock()
	h.ifaces[1].AddressError = "cannot inspect possible overlapping subnet"
	h.mu.Unlock()
	h.mgr.scanOnce(ctx)
	if h.mgr.isClaimed("eth0") || len(h.addedAddresses()) != 0 {
		t.Fatal("allocated a subnet while another port's addresses were unknown")
	}
	h.mu.Lock()
	h.ifaces[1].AddressError = ""
	h.mu.Unlock()
	h.mgr.scanOnce(ctx)
	if !h.mgr.isClaimed("eth0") {
		t.Fatal("did not recover after interface inspection succeeded")
	}
}

func TestDHCPIPv6ConfigurationAppearingStopsExistingClaim(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	claimEth0(t, h, ctx)
	h.mu.Lock()
	h.ifaces[0].HasIPv6Config = true
	h.mu.Unlock()
	h.mgr.scanOnce(ctx)
	if h.mgr.isClaimed("eth0") || len(h.removedAddresses()) != 1 {
		t.Fatal("kept serving after non-link-local IPv6 configuration appeared")
	}
	s := h.mgr.DHCPStatus()
	if s.Interfaces[0].State != "blocked" || !strings.Contains(s.Interfaces[0].Detail, "IPv6") {
		t.Fatalf("IPv6 conflict not reported: %+v", s)
	}
}
