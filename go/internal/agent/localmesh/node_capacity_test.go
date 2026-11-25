package localmesh

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

func fillCarrierNodeCapacity(n *Node) {
	for id := babel.LinkID(3); id <= 64; id++ {
		n.peers[id] = &nodePeer{id: id, device: selectionDevice(int(id) + 100), cost: 128, retire: make(chan struct{}), link: &handoffActorLink{}}
	}
	n.nextLink = 64
	n.refresh()
}

func TestNodeCapacityReclaimsOneOwnedStandbyBeforeRetry(t *testing.T) {
	f := newCarrierStandbyFixture(t)
	f.addCarrier(128, "infrastructure-wifi")
	f.pump(0)
	f.advance(6*time.Second, 0)
	f.assertSelected(2)
	n := f.nodes[0]
	fillCarrierNodeCapacity(n)
	old, working := n.peers[1], n.peers[2]
	newPeer := selectionDevice(999)
	add := &nodePeer{device: newPeer, cost: 128, transport: "infrastructure-wifi", link: &handoffActorLink{}, control: make(chan ControlMessage, 128), done: make(chan struct{}), retire: make(chan struct{})}
	if err := n.handle(nodeEvent{add: add}); !errors.Is(err, ErrCarrierCapacityRetry) {
		t.Fatalf("new peer did not receive intentional local deferral: %v", err)
	}
	if len(n.peers) != 64 || n.nextLink != 64 || !old.retiring() || !old.capacityRetire.Load() || working.retiring() || n.capacityRetiring != old.id {
		t.Fatal("capacity pressure dropped the last working path or allocated a 65th attachment")
	}
	for i := 0; i < 3; i++ {
		if err := n.handle(nodeEvent{add: add}); !errors.Is(err, ErrCarrierCapacityRetry) {
			t.Fatalf("pending reclamation became a failed peer: %v", err)
		}
		if len(n.peers) != 64 || n.capacityRetiring != old.id || working.retiring() {
			t.Fatal("repeated demand reclaimed another incarnation before owned removal")
		}
	}
	// Only the ordinary exact-incarnation removal releases Node capacity.
	if err := n.handle(nodeEvent{id: old.id, remove: true}); err != nil {
		t.Fatal(err)
	}
	if n.capacityRetiring != 0 || len(n.peers) != 63 {
		t.Fatal("ordinary removal did not release capacity")
	}
	if err := n.handle(nodeEvent{add: add}); err != nil {
		t.Fatal(err)
	}
	if len(n.peers) != 64 || add.id != 65 || n.peers[add.id] != add || working.retiring() {
		t.Fatal("new direct peer did not use the normally released slot")
	}
}

func TestNodeCapacityNeverReclaimsUniqueOrUnusablePaths(t *testing.T) {
	for _, mode := range []string{"all-unique", "same-peer-upgrade", "retiring-alternate", "stale-incarnation"} {
		t.Run(mode, func(t *testing.T) {
			f := newCarrierStandbyFixture(t)
			n := f.nodes[0]
			if mode != "all-unique" {
				f.addCarrier(128, "infrastructure-wifi")
				f.pump(0)
				f.advance(6*time.Second, 0)
			}
			fillCarrierNodeCapacity(n)
			// The all-unique fixture has only BLE; add one more unrelated
			// incumbent to reach the same hard ceiling without redundancy.
			if len(n.peers) == 63 {
				n.peers[2] = &nodePeer{id: 2, device: selectionDevice(102), cost: 128, retire: make(chan struct{}), link: &handoffActorLink{}}
				n.refresh()
			}
			candidate := selectionDevice(999)
			if mode == "same-peer-upgrade" {
				candidate = n.peers[1].device
			}
			if mode == "retiring-alternate" {
				n.peers[2].signalRetire()
			}
			if mode == "stale-incarnation" {
				view := n.Snapshot()
				for i := range view.Links {
					if view.Links[i].ID == 1 {
						view.Links[i].ID = 1000
					}
				}
				n.view.Store(&view)
			}
			deferred := n.reclaimGlobalRedundancy(candidate)
			if deferred != (mode == "retiring-alternate") || n.peers[1].retiring() || n.capacityRetiring != 0 || len(n.peers) != 64 {
				t.Fatal("global capacity used stale or unusable evidence to retire a path")
			}
		})
	}
}

func TestNodeCapacityDeferralDoesNotPenalizeVerifiedProviderCandidate(t *testing.T) {
	f := newCarrierStandbyFixture(t)
	f.addCarrier(128, "infrastructure-wifi")
	f.pump(0)
	f.advance(6*time.Second, 0)
	n := f.nodes[0]
	fillCarrierNodeCapacity(n)
	next := selectionDevice(999)
	p := NewConnectionPolicy(selectionDevice(1), true, ConnectionLimits{Outgoing: 1, Attempts: 1, Total: 1})
	lease := p.Reserve("next", next, true)
	ctx := WithConnectionLease(context.Background(), lease)
	link := &handoffActorLink{}
	done := make(chan error, 1)
	go func() { done <- n.attachLink(ctx, next, 128, link) }()
	select {
	case add := <-n.events:
		add.result <- n.handle(add)
	case <-time.After(time.Second):
		t.Fatal("verified attachment did not request actor admission")
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrCarrierCapacityRetry) {
			t.Fatalf("intentional deferral lost: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("deferred attachment did not finish through its owner")
	}
	if !lease.Retired() || p.Counts().Total != 1 {
		t.Fatal("intentional deferral changed capacity or looked like a failed peer")
	}
	if !link.closed {
		t.Fatal("rejected setup did not close its own exact transport")
	}
	lease.Release()
	if p.Counts().Total != 0 {
		t.Fatal("deferred candidate did not release through its owner")
	}
}
