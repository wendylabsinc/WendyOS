package localmesh

import (
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func newNodeCapacityHoldFixture(t *testing.T) (*carrierStandbyFixture, *Node, *nodePeer) {
	t.Helper()
	f := newCarrierStandbyFixture(t)
	f.addCarrier(128, "infrastructure-wifi")
	f.pump(0)
	f.advance(6*time.Second, 0)
	f.assertSelected(2)
	n := f.nodes[0]
	fillCarrierNodeCapacity(n)
	return f, n, n.peers[1]
}

func nodeCapacityHoldCandidate(peer meshid.Device, cost uint16) *nodePeer {
	return &nodePeer{device: peer, cost: cost, transport: "test-carrier", link: &handoffActorLink{},
		control: make(chan ControlMessage, 128), done: make(chan struct{}), retire: make(chan struct{})}
}

func TestNodeCapacityHoldStartsOnRemovalAndLetsNewPeerWin(t *testing.T) {
	for _, alreadyClosing := range []bool{false, true} {
		name := "capacity-victim"
		if alreadyClosing {
			name = "already-closing-slot"
		}
		t.Run(name, func(t *testing.T) {
			f, n, old := newNodeCapacityHoldFixture(t)
			if alreadyClosing {
				old.signalRetire()
				n.refresh()
			}
			next := nodeCapacityHoldCandidate(selectionDevice(999), 128)
			if err := n.handle(nodeEvent{add: next}); !errors.Is(err, ErrCarrierCapacityRetry) {
				t.Fatalf("new peer did not wait for exact owned removal: %v", err)
			}
			if !old.retiring() || n.capacityHoldRelease != old.id || n.capacityHoldUntil != 0 || len(n.peers) != 64 {
				t.Fatal("signal allocated a slot or started the removal-anchored hold early")
			}
			// A blocked physical closer may outlive the entire reused penalty.
			// Node capacity is still occupied and no fairness deadline has run.
			f.advance(f.elapsed+FailedConnectPenalty+5*time.Second, 0)
			if n.capacityHoldUntil != 0 || len(n.peers) != 64 {
				t.Fatal("pending closure started or consumed the post-removal hold")
			}
			if err := n.handle(nodeEvent{id: old.id, remove: true}); err != nil {
				t.Fatal(err)
			}
			deadline := f.elapsed + FailedConnectPenalty
			if n.capacityHoldUntil != deadline || n.capacityRetiring != 0 || len(n.peers) != 63 {
				t.Fatal("ordinary Node removal did not anchor the bounded hold")
			}
			backup := nodeCapacityHoldCandidate(old.device, old.cost)
			if err := n.handle(nodeEvent{add: backup}); !errors.Is(err, ErrCarrierCapacityRetry) || n.capacityHoldUntil != deadline || n.nextLink != 64 || len(n.peers) != 63 {
				t.Fatal("old redundancy reclaimed the released slot before the new peer's normal retry")
			}
			// No special wakeup is needed: the new peer's existing retry cadence
			// receives the normally released slot while the old standby defers.
			f.advance(f.elapsed+5*time.Second, 0)
			if err := n.handle(nodeEvent{add: next}); err != nil {
				t.Fatal(err)
			}
			if next.id != 65 || len(n.peers) != 64 || n.peers[next.id] != next || n.capacityHoldUntil != 0 || n.peers[2].retiring() {
				t.Fatal("successful new-peer admission did not consume the opportunity safely")
			}
		})
	}
}

func TestNodeCapacityHoldExpiresWithoutBeingRenewedByOldBackup(t *testing.T) {
	f, n, old := newNodeCapacityHoldFixture(t)
	if err := n.handle(nodeEvent{add: nodeCapacityHoldCandidate(selectionDevice(999), 128)}); !errors.Is(err, ErrCarrierCapacityRetry) {
		t.Fatal(err)
	}
	if err := n.handle(nodeEvent{id: old.id, remove: true}); err != nil {
		t.Fatal(err)
	}
	deadline := f.elapsed + FailedConnectPenalty
	backup := nodeCapacityHoldCandidate(old.device, old.cost)
	for _, until := range []time.Duration{f.elapsed + 5*time.Second, f.elapsed + 15*time.Second, deadline - time.Nanosecond} {
		f.advance(until, 0)
		if err := n.handle(nodeEvent{add: backup}); !errors.Is(err, ErrCarrierCapacityRetry) || n.capacityHoldUntil != deadline || len(n.peers) != 63 {
			t.Fatal("old backup either bypassed or self-renewed the bounded hold")
		}
	}
	// The new leaf vanished. Exactly at the existing monotonic penalty bound,
	// the old standby is again eligible without a timer callback or demand bit.
	f.advance(deadline, 0)
	if err := n.handle(nodeEvent{add: backup}); err != nil {
		t.Fatal(err)
	}
	if backup.id != 65 || len(n.peers) != 64 || n.capacityHoldUntil != 0 {
		t.Fatal("expired new-peer opportunity left standby admission permanently suppressed")
	}
}

func TestNodeCapacityHoldAllowsFasterAndRecoveryCarriers(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		name := "faster-upgrade"
		if recovery {
			name = "last-working-path-recovery"
		}
		t.Run(name, func(t *testing.T) {
			f, n, old := newNodeCapacityHoldFixture(t)
			if err := n.handle(nodeEvent{add: nodeCapacityHoldCandidate(selectionDevice(999), 128)}); !errors.Is(err, ErrCarrierCapacityRetry) {
				t.Fatal(err)
			}
			if err := n.handle(nodeEvent{id: old.id, remove: true}); err != nil {
				t.Fatal(err)
			}
			deadline := n.capacityHoldUntil
			cost := uint16(64)
			if recovery {
				// The remaining LAN incarnation is now terminally closing.
				// Its old installed route cannot veto restoring useful BLE.
				n.peers[2].signalRetire()
				n.refresh()
				cost = 4096
			}
			add := nodeCapacityHoldCandidate(old.device, cost)
			if err := n.handle(nodeEvent{add: add}); err != nil {
				t.Fatal(err)
			}
			if add.id != 65 || len(n.peers) != 64 || n.capacityHoldUntil != deadline || f.elapsed >= deadline {
				t.Fatal("hold blocked an upgrade/recovery or confused it with a new peer")
			}
			if !recovery && n.peers[2].retiring() {
				t.Fatal("faster unprimed upgrade dropped the last working LAN")
			}
		})
	}
}

func TestNodeCapacityHoldClearsPendingAnchorAfterNewAdmission(t *testing.T) {
	f := newCarrierStandbyFixture(t)
	f.addCarrier(128, "infrastructure-wifi")
	f.pump(0)
	f.advance(6*time.Second, 0)
	n := f.nodes[0]
	unrelated := nodeCapacityHoldCandidate(selectionDevice(998), 128)
	if err := n.handle(nodeEvent{add: unrelated}); err != nil {
		t.Fatal(err)
	}
	fillCarrierNodeCapacity(n)
	// The capacity filler represents other admitted slots; retain this actual
	// actor-admitted unrelated incarnation so its ordinary Remove can free one.
	n.peers[unrelated.id] = unrelated
	n.refresh()
	old := n.peers[1]
	next := nodeCapacityHoldCandidate(selectionDevice(999), 128)
	if err := n.handle(nodeEvent{add: next}); !errors.Is(err, ErrCarrierCapacityRetry) {
		t.Fatal(err)
	}
	if n.capacityHoldRelease != old.id {
		t.Fatal("full new-peer demand did not select its exact pending release")
	}
	if err := n.handle(nodeEvent{id: unrelated.id, remove: true}); err != nil {
		t.Fatal(err)
	}
	if err := n.handle(nodeEvent{add: next}); err != nil {
		t.Fatal(err)
	}
	if n.capacityHoldRelease != 0 || n.capacityHoldUntil != 0 || len(n.peers) != 64 {
		t.Fatal("actual new-peer admission left its already-served demand armed")
	}
	if err := n.handle(nodeEvent{id: old.id, remove: true}); err != nil {
		t.Fatal(err)
	}
	if n.capacityHoldUntil != 0 || n.capacityHoldRelease != 0 || len(n.peers) != 63 {
		t.Fatal("late completion started a fairness hold after the new peer already won")
	}
}
