package meshsession

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/directstream"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestAutomaticPromotionEpochAndRetry(t *testing.T) {
	self, peer := fixtureDevice(t, fixtureDeviceA), fixtureDevice(t, fixtureDeviceB)
	now := time.Now()
	origin := localmesh.RouteOrigin{Prefix: netip.PrefixFrom(peer.MeshAddress(), 128), RouterID: 2, Seqno: 7}
	route := babel.Route{Prefix: origin.Prefix, RouterID: 2, Seqno: 7, Metric: 96, Link: 2, NextHop: netip.MustParseAddr("fe80::2")}
	snapshot := localmesh.NodeSnapshot{Self: self, CapturedAt: now, Routes: []babel.Route{route}, ForwardingRoutes: []babel.Route{route},
		Links:               []localmesh.PeerLink{{ID: 2, Device: peer, Cost: 96, Transport: "nan", ForwardingReady: true}},
		RouteAuthorizations: map[localmesh.RouteOrigin]bool{origin: true}, RouteProofs: map[localmesh.RouteOrigin]localmesh.RouteProof{origin: {Principal: peer.Principal(), Deadline: now.Add(time.Minute)}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session := &DirectSession{ctx: ctx}
	status := DirectSessionStatus{Established: true, Busy: true, DirectAvailable: true, BLECost: 1024}
	session.status.Store(status)
	target := &managedDirectSession{session: session, selected: directstream.CapabilityVersion | directstream.CapabilityTLS}
	m := &DirectManager{ctx: ctx, peers: map[meshid.Device]*managedDirectSession{peer: target}, cfg: DirectManagerConfig{PromotionSnapshot: func() localmesh.NodeSnapshot { return snapshot }}}
	started, release := make(chan struct{}, 2), make(chan struct{})
	dial := func(ctx context.Context, p meshid.Device, address netip.AddrPort, epoch *managedDirectSession) error {
		if p != peer || epoch != target || address.Addr() != peer.MeshAddress() {
			t.Error("wrong epoch or endpoint")
		}
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return errors.New("setup failed")
	}
	status.Busy = false
	session.status.Store(status)
	m.pollAutomaticPromotions(now, dial)
	m.workers.Wait()
	if len(started) != 0 {
		t.Fatal("idle epoch dialed")
	}
	status.Busy = true
	session.status.Store(status)
	m.pollAutomaticPromotions(now, dial)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("eligible epoch did not dial")
	}
	m.pollAutomaticPromotions(now.Add(time.Second), dial)
	close(release)
	m.workers.Wait()
	if len(started) != 0 {
		t.Fatal("duplicate dial while pending")
	}
	m.pollAutomaticPromotions(now, dial)
	m.workers.Wait()
	if len(started) != 0 {
		t.Fatal("failed setup ignored backoff")
	}
	m.pollAutomaticPromotions(target.promotionRetry, dial)
	m.workers.Wait()
	if len(started) != 1 {
		t.Fatal("failed setup was never retried")
	}
	<-started
	status.Promoted = true
	session.status.Store(status)
	m.pollAutomaticPromotions(time.Now().Add(2*time.Second), dial)
	m.workers.Wait()
	if len(started) != 0 {
		t.Fatal("committed epoch redialed")
	}
	status.Promoted = false
	status.Promoting = true
	session.status.Store(status)
	address, err := ResolvePromotion(snapshot, peer, true, 1024, target.selected, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = m.checkPromotion(ctx, target, peer, address); err != nil {
		t.Fatalf("preferred dial never reached arbitration: %v", err)
	}
	session.cfg.Role = 1
	if err = m.checkPromotion(ctx, target, peer, address); !errors.Is(err, directstream.ErrPromoted) {
		t.Fatalf("nonpreferred competitor: %v", err)
	}
	session.cfg.Role = 0
	status.Promoting = false
	session.status.Store(status)

}

func TestAutomaticPromotionLoopCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := &DirectManager{ctx: ctx, cfg: DirectManagerConfig{PromotionSnapshot: func() localmesh.NodeSnapshot { return localmesh.NodeSnapshot{} }}}
	m.workers.Add(1)
	go m.runAutomaticPromotions()
	cancel()
	done := make(chan struct{})
	go func() { m.workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watcher did not join cancellation")
	}
}
