package ipcam

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestDHCPRestartReusesRetainedSegment(t *testing.T) {
	for _, registry := range []bool{false, true} {
		name := "without-camera-record"
		if registry {
			name = "with-camera-record"
		}
		t.Run(name, func(t *testing.T) {
			h := newManagerHarness(t, []Interface{cabledEth("eth0")})
			configureDHCPControl(t, h, map[string]bool{"eth0": true})
			h.addForeignAddress("eth0", "10.98.7.1/24")
			if registry {
				if _, err := h.reg.Upsert(Camera{MAC: "02:00:00:00:00:01", Link: "eth0", Address: "10.98.7.50"}); err != nil {
					t.Fatal(err)
				}
			}
			// Repeating enable is idempotent even before the first scan recovers.
			if status, err := h.mgr.SetDHCPEnabled("eth0", true); err != nil || status.Interfaces[0].State != "waiting" {
				t.Fatalf("enable retained segment: %+v, %v", status, err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h.mgr.scanOnce(ctx)
			waitFor(t, "retained port watcher", func() bool { return len(h.watchedLinks()) == 1 })
			if h.mgr.isClaimed("eth0") {
				t.Fatal("resumed before observing for competing servers")
			}
			h.advance(unansweredWindow)
			h.mgr.scanOnce(ctx)
			waitFor(t, "resumed server", func() bool { return len(h.servedLinks()) == 1 })
			seg, ok := h.mgr.claimedSegment("eth0")
			if !ok || seg.CIDR() != "10.98.7.1/24" {
				t.Fatalf("recovered segment: %+v, %v", seg, ok)
			}
			if got := h.mgr.DHCPStatus().Interfaces[0]; got.State != "serving" {
				t.Fatalf("recovered status: %+v", got)
			}
			if _, err := h.mgr.SetDHCPEnabled("eth0", false); err != nil {
				t.Fatal(err)
			}
			if h.mgr.isClaimed("eth0") || len(h.removedAddresses()) != 1 {
				t.Fatal("disable did not stop and remove the recovered segment")
			}
		})
	}
}

func TestDHCPRetainedSegmentStillRequiresPermissionAndGuard(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "competing-server"}[enabled], func(t *testing.T) {
			h := newManagerHarness(t, []Interface{cabledEth("eth0")})
			configureDHCPControl(t, h, map[string]bool{"eth0": enabled})
			h.addForeignAddress("eth0", "10.98.7.1/24")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h.mgr.scanOnce(ctx)
			if enabled {
				waitFor(t, "watcher", func() bool { return len(h.watchedLinks()) == 1 })
				h.feed("eth0", &Packet{Type: Offer, ServerID: net.ParseIP("10.98.7.2")})
			}
			h.advance(unansweredWindow)
			h.mgr.scanOnce(ctx)
			if h.mgr.isClaimed("eth0") || len(h.addedAddresses()) != 0 {
				t.Fatal("retained address bypassed permission or competing-server guard")
			}
		})
	}
}

func TestDHCPRetainedAddressMustMatchExactCameraConfiguration(t *testing.T) {
	for _, addresses := range [][]string{
		{"10.98.7.1/16"}, {"10.98.7.50/24"}, {"192.168.1.1/24"},
		{"10.98.7.1/24", "192.168.1.1/24"},
	} {
		t.Run(strings.Join(addresses, "+"), func(t *testing.T) {
			h := newManagerHarness(t, []Interface{cabledEth("eth0")})
			configureDHCPControl(t, h, map[string]bool{"eth0": true})
			for _, addr := range addresses {
				h.addForeignAddress("eth0", addr)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h.mgr.scanOnce(ctx)
			h.advance(unansweredWindow)
			h.mgr.scanOnce(ctx)
			if h.mgr.isClaimed("eth0") || len(h.addedAddresses()) != 0 || len(h.removedAddresses()) != 0 {
				t.Fatal("conflicting configuration was claimed or modified")
			}
			if got := h.mgr.DHCPStatus().Interfaces[0]; got.State != "blocked" {
				t.Fatalf("conflicting configuration status: %+v", got)
			}
			if _, err := h.mgr.SetDHCPEnabled("eth0", true); !errors.Is(err, ErrDHCPInterfaceBlocked) {
				t.Fatalf("enable conflicting configuration: %v", err)
			}
		})
	}
}

func TestDHCPNewPortDoesNotTakeRetainedSegment(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0"), cabledEth("eth1")})
	h.addForeignAddress("eth1", "10.98.0.1/24")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.mgr.scanOnce(ctx)
	waitFor(t, "both watchers", func() bool { return len(h.watchedLinks()) == 2 })
	h.feed("eth0", &Packet{Type: Discover})
	h.advance(unansweredWindow)
	h.mgr.scanOnce(ctx)
	first, firstOK := h.mgr.claimedSegment("eth0")
	second, secondOK := h.mgr.claimedSegment("eth1")
	if !firstOK || !secondOK || first.CIDR() != "10.98.1.1/24" || second.CIDR() != "10.98.0.1/24" {
		t.Fatalf("new and retained segments: %+v (%v), %+v (%v)", first, firstOK, second, secondOK)
	}
}

func TestDHCPRenameStopsServerAndWatcher(t *testing.T) {
	for _, action := range []string{"scan", "status", "disable", "disable-old-name"} {
		t.Run(action, func(t *testing.T) {
			iface := cabledEth("eth0")
			iface.Index = 42
			h := newManagerHarness(t, []Interface{iface})
			configureDHCPControl(t, h, map[string]bool{"eth0": true})
			serverStopped, watcherStopped := make(chan struct{}), make(chan struct{})
			serve, watch := h.mgr.serve, h.mgr.watch
			h.mgr.serve = func(ctx context.Context, link string, seg CameraSegment, pool *LeasePool, lease func(net.HardwareAddr, net.IP, string)) error {
				defer close(serverStopped)
				return serve(ctx, link, seg, pool, lease)
			}
			h.mgr.watch = func(ctx context.Context, link string, packet func(*Packet)) error {
				if link == "eth0" {
					defer close(watcherStopped)
				}
				return watch(ctx, link, packet)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			claimEth0(t, h, ctx)
			h.mu.Lock()
			h.ifaces[0].Name = "camera0"
			h.mu.Unlock()
			switch action {
			case "scan":
				h.mgr.scanOnce(ctx)
			case "status":
				h.mgr.DHCPStatus()
			case "disable":
				if _, err := h.mgr.SetDHCPEnabled("camera0", false); err != nil {
					t.Fatal(err)
				}
			case "disable-old-name":
				if _, err := h.mgr.SetDHCPEnabled("eth0", false); err != nil {
					t.Fatal(err)
				}
			}
			for _, stopped := range []chan struct{}{serverStopped, watcherStopped} {
				select {
				case <-stopped:
				case <-time.After(3 * time.Second):
					t.Fatal("renamed port retained a server or watcher")
				}
			}
			if h.mgr.isClaimed("eth0") || h.mgr.dhcpAllowed("camera0") {
				t.Fatal("claim or permission followed renamed port")
			}
			if got := h.removedAddresses(); len(got) != 1 || got[0] != "camera0 10.98.0.1/24" {
				t.Fatalf("address cleanup must use kernel identity: %v", got)
			}
			// A callback already queued by the old watcher must not affect a
			// later interface that happens to receive its previous name.
			h.feed("eth0", &Packet{Type: Offer, ServerID: net.ParseIP("192.168.1.1")})
			if h.mgr.State("eth0") == LinkDisqualified {
				t.Fatal("cancelled watcher changed the old name's guard")
			}
		})
	}
}

func TestDHCPStatusReportsFailedRestorationAndRecovery(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0")})
	configureDHCPControl(t, h, map[string]bool{"eth0": true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	claimEth0(t, h, ctx)
	h.stripAddress("eth0", SegmentFor(0).CIDR())
	h.mu.Lock()
	h.addErr = errors.New("device busy")
	h.mu.Unlock()
	h.mgr.scanOnce(ctx)
	got := h.mgr.DHCPStatus().Interfaces[0]
	if got.State != "degraded" || !strings.Contains(got.Detail, "device busy") {
		t.Fatalf("failed restoration status: %+v", got)
	}
	h.mu.Lock()
	h.addErr = nil
	h.mu.Unlock()
	h.mgr.scanOnce(ctx)
	if got := h.mgr.DHCPStatus().Interfaces[0]; got.State != "serving" || strings.Contains(got.Detail, "device busy") {
		t.Fatalf("recovered status: %+v", got)
	}
}

func TestDHCPDisableDoesNotRemoveAddressFromReplacementInterface(t *testing.T) {
	iface := cabledEth("eth0")
	iface.Index = 42
	h := newManagerHarness(t, []Interface{iface})
	configureDHCPControl(t, h, map[string]bool{"eth0": true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	claimEth0(t, h, ctx)
	h.mu.Lock()
	h.ifaces[0].Index = 43 // Another interface now occupies the same name.
	h.mu.Unlock()
	if _, err := h.mgr.SetDHCPEnabled("eth0", false); err != nil {
		t.Fatal(err)
	}
	if h.mgr.isClaimed("eth0") || len(h.removedAddresses()) != 0 {
		t.Fatal("disable retained the old claim or modified the replacement interface")
	}
}
