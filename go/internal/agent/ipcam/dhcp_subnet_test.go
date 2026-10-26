package ipcam

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestDHCPAllocationAvoidsOtherInterfaceNetworks(t *testing.T) {
	for _, tc := range []struct {
		name      string
		addresses []string
		firstFree int
	}{
		{"retained-with-uplink", []string{"10.98.0.1/24", "192.168.1.10/24"}, 1},
		{"other-host-in-subnet", []string{"10.98.0.200/24"}, 1},
		{"broader-subnet", []string{"10.98.1.200/23"}, 2},
		{"narrower-subnet", []string{"10.98.0.200/25"}, 1},
		{"single-host", []string{"10.98.0.200/32"}, 1},
		{"entire-camera-range", []string{"10.98.200.1/16"}, -1},
		{"unrelated-subnet", []string{"192.168.1.10/24"}, 0},
		{"adjacent-subnet", []string{"10.98.1.1/24"}, 0},
	} {
		for _, restore := range []bool{false, true} {
			mode := "discover"
			if restore {
				mode = "registry-restore"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				h := newManagerHarness(t, []Interface{cabledEth("eth0"), cabledEth("eth1")})
				// The overlapping network reserves space even without DHCP permission.
				configureDHCPControl(t, h, map[string]bool{"eth1": true})
				for _, address := range tc.addresses {
					h.addForeignAddress("eth0", address)
				}
				if restore {
					if _, err := h.reg.Upsert(Camera{MAC: "02:00:00:00:00:01", Link: "eth1", Address: "10.98.0.50"}); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				h.mgr.scanOnce(ctx)
				waitFor(t, "new port watcher", func() bool { return len(h.watchedLinks()) == 1 })
				if !restore {
					h.feed("eth1", &Packet{Type: Discover})
				}
				h.advance(unansweredWindow)
				h.mgr.scanOnce(ctx)
				seg, claimed := h.mgr.claimedSegment("eth1")
				if tc.firstFree < 0 {
					if claimed || len(h.addedAddresses()) != 0 {
						t.Fatal("allocated an overlapping subnet when all camera subnets are occupied")
					}
					return
				}
				if !claimed || !seg.ServerIP.Equal(SegmentFor(tc.firstFree).ServerIP) {
					t.Fatalf("allocated %s (claimed=%v), want segment %d", seg.ServerIP, claimed, tc.firstFree)
				}
			})
		}
	}
}

func TestDHCPRetainedSegmentDoesNotOverrideOverlappingNetwork(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0"), cabledEth("eth1")})
	configureDHCPControl(t, h, map[string]bool{"eth1": true})
	h.addForeignAddress("eth0", "10.98.0.200/25")
	h.addForeignAddress("eth1", "10.98.0.1/24")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.mgr.scanOnce(ctx)
	waitFor(t, "retained port watcher", func() bool { return len(h.watchedLinks()) == 1 })
	h.advance(unansweredWindow)
	h.mgr.scanOnce(ctx)
	if h.mgr.isClaimed("eth1") || len(h.addedAddresses()) != 0 {
		t.Fatal("resumed an overlapping retained segment or added another address alongside it")
	}
}

func TestDHCPActivePrefixChangeStopsServing(t *testing.T) {
	for _, prefix := range []string{"16", "25", "32"} {
		t.Run(prefix, func(t *testing.T) {
			h := newManagerHarness(t, []Interface{cabledEth("eth0")})
			configureDHCPControl(t, h, map[string]bool{"eth0": true})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stopped := make(chan struct{})
			serve := h.mgr.serve
			h.mgr.serve = func(ctx context.Context, link string, seg CameraSegment, pool *LeasePool, lease func(net.HardwareAddr, net.IP, string)) error {
				defer close(stopped)
				return serve(ctx, link, seg, pool, lease)
			}
			claimEth0(t, h, ctx)
			h.addForeignAddress("eth0", "10.98.0.1/"+prefix)
			if s := h.mgr.DHCPStatus().Interfaces[0]; s.State != "degraded" || !strings.Contains(s.Detail, "Conflicting IPv4") {
				t.Fatalf("prefix change reported healthy before scan: %+v", s)
			}
			if _, err := h.mgr.SetDHCPEnabled("eth0", true); !errors.Is(err, ErrDHCPInterfaceBlocked) {
				t.Fatalf("accepted enable with conflicting prefix: %v", err)
			}
			h.mgr.scanOnce(ctx)
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("server not stopped after prefix change")
			}
			if s := h.mgr.DHCPStatus().Interfaces[0]; s.State != "blocked" || s.Serving || !s.Enabled || !strings.Contains(s.Detail, "conflicting IPv4") {
				t.Fatalf("prefix change status: %+v", s)
			}
			h.mgr.scanOnce(ctx)
			if h.mgr.isClaimed("eth0") || len(h.addedAddresses()) != 1 {
				t.Fatal("conflicting configuration was reclaimed or overwritten")
			}
			h.mu.Lock()
			remaining := h.ifaces[0].IPv4Networks
			h.mu.Unlock()
			if len(remaining) != 1 || remaining[0].String() != "10.98.0.1/"+prefix {
				t.Fatalf("removed another configuration's address: %v", remaining)
			}
		})
	}
}
