package localmesh

import (
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
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

func TestNodeCapacityHoldDefersAdditionalCarrierButAllowsRecovery(t *testing.T) {
	for _, mode := range []string{"faster", "equal", "recovery"} {
		t.Run(mode, func(t *testing.T) {
			f, n, old := newNodeCapacityHoldFixture(t)
			if err := n.handle(nodeEvent{add: nodeCapacityHoldCandidate(selectionDevice(999), 128)}); !errors.Is(err, ErrCarrierCapacityRetry) {
				t.Fatal(err)
			}
			if err := n.handle(nodeEvent{id: old.id, remove: true}); err != nil {
				t.Fatal(err)
			}
			deadline := n.capacityHoldUntil
			cost := uint16(64)
			if mode == "equal" {
				cost = 128
			}
			if mode == "recovery" {
				n.peers[2].signalRetire()
				n.refresh()
				cost = 4096
			}
			add := nodeCapacityHoldCandidate(old.device, cost)
			err := n.handle(nodeEvent{add: add})
			if mode != "recovery" {
				if !errors.Is(err, ErrCarrierCapacityRetry) || len(n.peers) != 63 || n.capacityHoldUntil != deadline {
					t.Fatal("known additional carrier stole or renewed the held last global slot")
				}
				return
			}
			if err != nil || len(n.peers) != 64 || n.capacityHoldUntil != deadline || f.elapsed >= deadline {
				t.Fatal("last working-path recovery was blocked or erased external demand")
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

func TestNodeCapacityCanReclaimUnusedLowerRankedCarrier(t *testing.T) {
	f := newCarrierStandbyFixture(t)
	f.addCarrier(128, "infrastructure-wifi")
	f.pump(0)
	f.advance(6*time.Second, 0)
	f.assertSelected(2)
	n := f.nodes[0]
	// The engine's current link cost can exceed the carrier's configured
	// scheduling rank. Exercise that real routing transition deterministically:
	// BLE now carries the actual FIB, while the configured-cheaper LAN is unused.
	if err := n.step(babel.SetCost{ID: 2, Cost: 8192}); err != nil {
		t.Fatal(err)
	}
	view := n.Snapshot()
	if !view.HasInstalledRouteOnLink(1) || view.HasInstalledRouteOnLink(2) || n.peers[2].cost != 128 {
		t.Fatal("fixture did not move the real installed FIB off configured-cheaper LAN")
	}
	fillCarrierNodeCapacity(n)
	add := nodeCapacityHoldCandidate(selectionDevice(999), 128)
	if err := n.handle(nodeEvent{add: add}); !errors.Is(err, ErrCarrierCapacityRetry) {
		t.Fatalf("unused configured-cheaper carrier was not reclaimable: %v", err)
	}
	if !n.peers[2].retiring() || n.peers[1].retiring() || n.capacityRetiring != 2 || len(n.peers) != 64 {
		t.Fatal("capacity confused configured rank with the actual working installed path")
	}
}

func TestNodeHeldLastSlotDefersConfiguredCheapReturnUntilOpportunityEnds(t *testing.T) {
	for _, ending := range []string{"expiry", "new-peer-admission"} {
		t.Run(ending, func(t *testing.T) {
			f := newCarrierStandbyFixture(t)
			f.addCarrier(128, "infrastructure-wifi")
			f.pump(0)
			f.advance(6*time.Second, 0)
			n := f.nodes[0]
			if err := n.step(babel.SetCost{ID: 2, Cost: 8192}); err != nil {
				t.Fatal(err)
			}
			if !n.Snapshot().HasInstalledRouteOnLink(1) || n.Snapshot().HasInstalledRouteOnLink(2) {
				t.Fatal("real Babel did not select BLE over the configured-cheap LAN")
			}
			old := n.peers[2]
			fillCarrierNodeCapacity(n)
			if err := n.handle(nodeEvent{add: nodeCapacityHoldCandidate(selectionDevice(999), 128)}); !errors.Is(err, ErrCarrierCapacityRetry) {
				t.Fatal(err)
			}
			if n.capacityRetiring != old.id {
				t.Fatal("unused configured-cheap LAN was not reclaimed")
			}
			if err := n.handle(nodeEvent{id: old.id, remove: true}); err != nil {
				t.Fatal(err)
			}
			deadline := n.capacityHoldUntil
			retry := nodeCapacityHoldCandidate(old.device, 128)
			if err := n.handle(nodeEvent{add: retry}); !errors.Is(err, ErrCarrierCapacityRetry) || len(n.peers) != 63 || n.capacityHoldUntil != deadline {
				t.Fatal("configured-cheap return stole the held last global opportunity")
			}
			if ending == "expiry" {
				f.advance(deadline, 0)
			} else {
				newPeer := nodeCapacityHoldCandidate(selectionDevice(999), 128)
				if err := n.handle(nodeEvent{add: newPeer}); err != nil {
					t.Fatal(err)
				}
				if n.capacityHoldUntil != 0 {
					t.Fatal("new actual Node admission did not clear opportunity")
				}
				if err := n.handle(nodeEvent{id: newPeer.id, remove: true}); err != nil {
					t.Fatal(err)
				}
			}
			if err := n.handle(nodeEvent{add: retry}); err != nil {
				t.Fatal(err)
			}
			if len(n.peers) != 64 || n.peers[1].retiring() || !n.Snapshot().HasInstalledRouteOnLink(1) {
				t.Fatal("outside hold the upgrade was denied or dropped the working BLE")
			}
		})
	}
}

func TestNodeHeldOpportunityAllowsOnlyUnreservedExtraCapacity(t *testing.T) {
	f := newCarrierStandbyFixture(t)
	f.addCarrier(128, "infrastructure-wifi")
	f.pump(0)
	f.advance(6*time.Second, 0)
	n := f.nodes[0]
	n.capacityHoldUntil = f.elapsed + FailedConnectPenalty
	for id := babel.LinkID(3); len(n.peers) < 62; id++ {
		n.peers[id] = &nodePeer{id: id, device: selectionDevice(100 + int(id)), cost: 128, retire: make(chan struct{}), link: &handoffActorLink{}}
		n.nextLink = id
	}
	n.refresh()
	extra := nodeCapacityHoldCandidate(n.peers[2].device, 64)
	if err := n.handle(nodeEvent{add: extra}); err != nil || len(n.peers) != 63 {
		t.Fatal("unreserved extra global slot was hidden")
	}
	if err := n.handle(nodeEvent{add: nodeCapacityHoldCandidate(extra.device, 128)}); !errors.Is(err, ErrCarrierCapacityRetry) || len(n.peers) != 63 {
		t.Fatal("second addition consumed the reserved global opportunity")
	}
	if err := n.handle(nodeEvent{add: nodeCapacityHoldCandidate(selectionDevice(999), 128)}); err != nil || len(n.peers) != 64 || n.capacityHoldUntil != 0 {
		t.Fatal("new distinct peer did not receive the held last opportunity")
	}
}

func TestNodeSamePeerRecoveryDoesNotConsumeExternalDemandReceipt(t *testing.T) {
	_, n, old := newNodeCapacityHoldFixture(t)
	if err := n.handle(nodeEvent{add: nodeCapacityHoldCandidate(selectionDevice(999), 128)}); !errors.Is(err, ErrCarrierCapacityRetry) {
		t.Fatal(err)
	}
	if err := n.handle(nodeEvent{id: old.id, remove: true}); err != nil {
		t.Fatal(err)
	}
	deadline := n.capacityHoldUntil
	if err := n.handle(nodeEvent{id: 2, remove: true}); err != nil {
		t.Fatal(err)
	}
	// Both old Node incarnations are gone. A physical owner's blocked Close
	// may still consume its independent provider lease (covered by the owned
	// lifecycle fixture); absence from this map must not turn recovery into a
	// new external peer and erase the global receipt.
	recovery := nodeCapacityHoldCandidate(old.device, 4096)
	if err := n.handle(nodeEvent{add: recovery}); err != nil {
		t.Fatal(err)
	}
	if n.capacityHoldUntil != deadline {
		t.Fatal("same-peer recovery erased the unserved external demand receipt")
	}
}
