package ipcam

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestDHCPCompetingServerPreemptsUnrelatedConfigurationWrite(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0"), cabledEth("eth1")})
	configureDHCPControl(t, h, map[string]bool{"eth0": true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	serve := h.mgr.serve
	h.mgr.serve = func(ctx context.Context, link string, seg CameraSegment, pool *LeasePool, lease func(net.HardwareAddr, net.IP, string)) error {
		defer close(stopped)
		return serve(ctx, link, seg, pool, lease)
	}
	h.mgr.scanOnce(ctx)
	waitFor(t, "both watchers", func() bool { return len(h.watchedLinks()) == 2 })
	h.feed("eth0", &Packet{Type: Discover})
	h.advance(unansweredWindow)
	h.mgr.scanOnce(ctx)
	waitFor(t, "eth0 server", func() bool { return len(h.servedLinks()) == 1 })
	entered, resume := make(chan struct{}), make(chan struct{})
	write := h.mgr.writeAllowlist
	h.mgr.writeAllowlist = func(path string, allowed map[string]bool) error {
		close(entered)
		<-resume
		return write(path, allowed)
	}
	configured := make(chan error, 1)
	go func() { _, err := h.mgr.SetDHCPEnabled("eth1", true); configured <- err }()
	<-entered
	observed := make(chan struct{})
	go func() {
		// A real watcher delivers packets serially. Even a full metadata queue
		// behind the stalled write must not hide a later competing reply.
		for i := 0; i < 64; i++ {
			h.feed("eth0", &Packet{Type: Discover})
		}
		h.feed("eth0", &Packet{Type: Offer, ServerID: net.ParseIP("192.168.1.1")})
		close(observed)
	}()
	blocked := false
	select {
	case <-stopped:
	case <-time.After(300 * time.Millisecond):
		blocked = true
	}
	close(resume)
	if err := <-configured; err != nil {
		t.Fatal(err)
	}
	<-observed
	<-stopped
	if blocked {
		t.Fatal("eth0 kept serving after a competing OFFER because an unrelated eth1 configuration write held scanMu")
	}
}

func TestDHCPCompetingServerPreemptsLeaseWriteAndDiagnostics(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	serve := h.mgr.serve
	h.mgr.serve = func(ctx context.Context, link string, seg CameraSegment, pool *LeasePool, lease func(net.HardwareAddr, net.IP, string)) error {
		defer close(stopped)
		return serve(ctx, link, seg, pool, lease)
	}
	claimEth0(t, h, ctx)
	h.mu.Lock()
	pool := h.poolsSeen["eth0"]
	h.mu.Unlock()
	entered, resume := make(chan struct{}), make(chan struct{})
	pool.save = func([]savedLease) error { close(entered); <-resume; return nil }
	saved := make(chan error, 1)
	go func() { _, err := pool.Lease(net.HardwareAddr{2, 0, 0, 0, 0, 1}, nil); saved <- err }()
	<-entered
	diagnosed := make(chan struct{})
	go func() { h.mgr.Leases(); close(diagnosed) }()
	observed := make(chan struct{})
	go func() { h.feed("eth0", &Packet{Type: Offer, ServerID: net.ParseIP("192.168.1.1")}); close(observed) }()
	blocked := false
	select {
	case <-stopped:
	case <-time.After(300 * time.Millisecond):
		blocked = true
	}
	close(resume)
	if err := <-saved; err != nil {
		t.Fatal(err)
	}
	<-diagnosed
	<-observed
	if blocked {
		t.Fatal("lease persistence or diagnostics blocked competing-server cancellation")
	}
}

func TestDHCPWatcherExitDoesNotLoseWithdrawal(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0")})
	configureDHCPControl(t, h, map[string]bool{"eth0": true})
	fail := make(chan struct{})
	h.mgr.watch = func(ctx context.Context, link string, onPacket func(*Packet)) error {
		h.mu.Lock()
		h.watched = append(h.watched, link)
		h.onPacket[link] = onPacket
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil
		case <-fail:
			return errors.New("packet socket read failed")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	claimEth0(t, h, ctx)
	// Model a scan/configuration operation holding the cleanup lock while the
	// watcher observes a competing reply, then encounters a read error.
	h.mgr.scanMu.Lock()
	h.feed("eth0", &Packet{Type: Offer, ServerID: net.ParseIP("10.98.0.2")})
	close(fail)
	deadline := time.Now().Add(time.Second)
	for {
		h.mgr.mu.Lock()
		watching := h.mgr.running["eth0"] != nil
		h.mgr.mu.Unlock()
		if !watching {
			break
		}
		if time.Now().After(deadline) {
			h.mgr.scanMu.Unlock()
			t.Fatal("watcher did not exit")
		}
		time.Sleep(time.Millisecond)
	}
	h.mgr.scanMu.Unlock()
	h.mgr.scanOnce(ctx)
	if h.mgr.isClaimed("eth0") {
		t.Fatalf("cancelled DHCP server retained its claim/address after a scan: %+v", h.mgr.DHCPStatus())
	}
	if got := h.removedAddresses(); len(got) != 1 || got[0] != "eth0 10.98.0.1/24" {
		t.Fatalf("pending withdrawal did not remove the address: %v", got)
	}
	h.mgr.scanOnce(ctx)
	if h.mgr.guard.State("eth0") != LinkDisqualified || h.mgr.isClaimed("eth0") || len(h.addedAddresses()) != 1 {
		t.Fatal("scan restored a disqualified claim")
	}
}
