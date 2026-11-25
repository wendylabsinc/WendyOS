package localmesh

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

type livenessCloseLink struct {
	nodeLink
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	calls   atomic.Int32
}

func (l *livenessCloseLink) Close() error {
	l.calls.Add(1)
	l.once.Do(func() { close(l.entered) })
	<-l.release
	return nil
}

func retirementSignalled(p *nodePeer) bool {
	select {
	case <-p.retire:
		return true
	default:
		return false
	}
}

func TestBabelRetirementExpiresExactConfirmedLinkWithoutBlockingActor(t *testing.T) {
	n, runtime, _ := carrierSelectionNetwork(t, true)
	for _, p := range runtime.peers {
		p.retire = make(chan struct{})
	}
	parent, stop := context.WithCancel(context.Background())
	defer stop()
	runtime.ctx = parent
	p := runtime.peers[1]
	link := &livenessCloseLink{entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	releaseClose := func() { releaseOnce.Do(func() { close(link.release) }) }
	defer releaseClose()
	p.link = link
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	closed := make(chan struct{})
	go func() { runtime.closePeerOnDone(ctx, cancel, p); close(closed) }()
	refreshCarrierSelection(n, runtime)
	if !p.babelConfirmed || retirementSignalled(p) {
		t.Fatal("healthy LAN was not confirmed or was retired")
	}
	var retiredAt time.Duration
	for n.elapsed < 8*time.Second {
		n.elapsed += 100 * time.Millisecond
		for i := range n.nodes {
			n.step(i, babel.Tick{})
		}
		deliverCarrierSelectionExcept(n, 1)
		// Close is deliberately blocked. Repeated actor refreshes must still
		// make progress, without a new goroutine/Close for every expired tick.
		refreshCarrierSelection(n, runtime)
		if retiredAt == 0 && retirementSignalled(p) {
			retiredAt = n.elapsed
		}
	}
	if retiredAt == 0 || retiredAt > 3*time.Second+3500*time.Millisecond+100*time.Millisecond {
		t.Fatalf("retirement missed the existing LAN Hello hold: at=%s", retiredAt)
	}
	n.assertForwarded(1, true, 0)
	if retirementSignalled(runtime.peers[2]) {
		t.Fatal("retired the surviving BLE link")
	}
	select {
	case <-link.entered:
	case <-time.After(time.Second):
		t.Fatal("existing lifetime owner did not close expired carrier")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("retirement did not cancel attachment workers")
	}
	stop() // Shutdown must not make the actor call the blocked Close again.
	runtime.retireExpiredBabelPeers(n.nodes[1].routing.Snapshot())
	select {
	case <-closed:
		t.Fatal("fixture did not actually block Close")
	default:
	}
	releaseClose()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("lifetime owner did not join after Close completed")
	}
	if calls := link.calls.Load(); calls != 1 {
		t.Fatalf("expired incarnation closed %d times", calls)
	}
}

func TestBabelRetirementPreservesInitialGraceAndProofGap(t *testing.T) {
	t.Run("unconfirmed-startup", func(t *testing.T) {
		peer := selectionDevice(10)
		p := &nodePeer{id: 1, device: peer, retire: make(chan struct{})}
		runtime := &Node{peers: map[babel.LinkID]*nodePeer{1: p}}
		routing, err := NewRouting(babel.Config{RouterID: 1, HelloInterval: time.Second}, nil, &routingRecorder{})
		if err != nil {
			t.Fatal(err)
		}
		left, _ := testLinks(selectionDevice(1), peer)
		left.ID = 1
		if err := routing.Step(0, babel.AddLink{Link: left}); err != nil {
			t.Fatal(err)
		}
		if err := routing.Step(time.Minute, babel.Tick{}); err != nil {
			t.Fatal(err)
		}
		runtime.retireExpiredBabelPeers(routing.Snapshot())
		if p.babelConfirmed || retirementSignalled(p) {
			t.Fatal("unconfirmed startup acquired a new retirement deadline")
		}
	})
	t.Run("proof-expiry-with-renewed-hello-ihu", func(t *testing.T) {
		n, runtime, signer := carrierSelectionNetwork(t, true)
		p := runtime.peers[1]
		p.retire = make(chan struct{})
		refreshCarrierSelection(n, runtime)
		if !p.babelConfirmed {
			t.Fatal("fixture never established two-way Babel liveness")
		}
		manifest := routeManifest(signer.Device, n.now())
		manifest.Revision = 2
		manifest.Expires = n.now().Add(time.Second).UnixMilli()
		manifest.Route = &RouteOrigin{Prefix: n.nodes[0].device.prefix, RouterID: n.nodes[0].device.routerID, Seqno: n.nodes[0].routing.Snapshot().Seqno}
		proof, err := SignManifest(manifest, signer.Certificate.Certificate, signer.Signer, n.now())
		if err != nil {
			t.Fatal(err)
		}
		n.accept(1, proof)
		// Pass both the newer short proof and the original minute-long proof's
		// deadline, independently of any safe retained-origin handoff policy.
		n.advance(70 * time.Second)
		n.assertForwarded(1, false, 0)
		refreshCarrierSelection(n, runtime)
		if retirementSignalled(p) || runtime.HasCheaperLink(p.device, 4096) {
			t.Fatal("proof expiry either closed a live neighbour or left it forwarding-ready")
		}
		// The engine separately proves Infinity IHUs are not hold expiry.
		snapshot := n.nodes[1].routing.Snapshot()
		for i := range snapshot.Neighbours {
			if snapshot.Neighbours[i].Link == p.id {
				snapshot.Neighbours[i].Reachable = false
				snapshot.Neighbours[i].LivenessExpired = false
			}
		}
		runtime.retireExpiredBabelPeers(snapshot)
		if retirementSignalled(p) {
			t.Fatal("explicit unreachable IHU was treated as elapsed liveness")
		}
	})
}

func TestBabelRetirementCannotReviveFromLateTraffic(t *testing.T) {
	n, runtime, _ := carrierSelectionNetwork(t, true)
	for _, p := range runtime.peers {
		p.retire = make(chan struct{})
	}
	refreshCarrierSelection(n, runtime)
	p := runtime.peers[1]
	for n.elapsed < 8*time.Second {
		n.elapsed += 100 * time.Millisecond
		for i := range n.nodes {
			n.step(i, babel.Tick{})
		}
		deliverCarrierSelectionExcept(n, 1)
		refreshCarrierSelection(n, runtime)
	}
	if !p.babelRetiring || !retirementSignalled(p) {
		t.Fatal("fixture did not retire the previously confirmed LAN")
	}
	// A genuine later Hello/IHU exchange can revive the independent engine
	// while the owned transport closer is still blocked. That incarnation must
	// nevertheless never become eligible to retire the surviving BLE carrier.
	n.advance(12 * time.Second)
	refreshCarrierSelection(n, runtime)
	if runtime.HasCheaperLink(p.device, 4096) {
		t.Fatal("terminally retiring LAN regained forwarding readiness")
	}
	// Generate actual Babel traffic and pass it through the Node actor's queued
	// packet path. Retirement rejects it before it can change engine state.
	runtime.started = time.Now().Add(-n.elapsed)
	var queued []byte
	for queued == nil && n.elapsed < 14*time.Second {
		n.elapsed += 100 * time.Millisecond
		n.step(0, babel.Tick{})
		for _, d := range n.nodes[0].io.sent {
			if d.Link == p.id {
				queued = append([]byte(nil), d.Payload...)
				break
			}
		}
		n.nodes[0].io.sent = nil
	}
	if queued == nil {
		t.Fatal("fixture generated no late Babel packet")
	}
	before := runtime.routing.Snapshot()
	if err := runtime.handle(nodeEvent{id: p.id, packet: queued}); err != nil {
		t.Fatal(err)
	}
	if after := runtime.routing.Snapshot(); !reflect.DeepEqual(before, after) {
		t.Fatal("queued Babel traffic changed a terminally retiring incarnation")
	}
	if !retirementSignalled(p) || !p.babelRetiring {
		t.Fatal("late traffic reversed terminal retirement")
	}
}

func TestPeerLifetimeOwnerHandlesCancellationAndJoins(t *testing.T) {
	for _, cause := range []string{"node", "attachment", "retirement"} {
		t.Run(cause, func(t *testing.T) {
			parent, stop := context.WithCancel(context.Background())
			defer stop()
			ctx, cancel := context.WithCancel(parent)
			defer cancel()
			link := &livenessCloseLink{entered: make(chan struct{}), release: make(chan struct{})}
			close(link.release)
			p := &nodePeer{link: link, retire: make(chan struct{})}
			runtime := &Node{ctx: parent}
			done := make(chan struct{})
			go func() { runtime.closePeerOnDone(ctx, cancel, p); close(done) }()
			switch cause {
			case "node":
				stop()
			case "attachment":
				cancel()
			case "retirement":
				close(p.retire)
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("lifetime owner did not stop")
			}
			if ctx.Err() == nil || link.calls.Load() != 1 {
				t.Fatal("cancellation did not close exactly one owned incarnation")
			}
		})
	}
}
