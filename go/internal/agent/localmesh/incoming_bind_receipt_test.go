package localmesh

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"testing"
	"time"
)

func TestIncomingReceiptLateAliasBindCannotClearFreshDemand(t *testing.T) {
	f := newIncomingHoldFixture(t, 3)
	f.demand(t)
	x := selectionDevice(80)
	// Two different-cost, already verified setups can coexist while Node has
	// not yet published either actual attachment.
	delayed := f.p.ReserveGroup("x-delay64", meshid.Device{}, false, "64")
	if delayed == nil || !delayed.Ready(x, func() {}) {
		t.Fatal("delayed setup missing")
	}
	first := f.p.ReserveGroup("x-first128", meshid.Device{}, false, "128")
	if first == nil || !first.Ready(x, func() {}) {
		t.Fatal("first actual setup missing")
	}
	defer delayed.Release()
	defer first.Release()
	BindConnectionLink(WithConnectionLease(context.Background(), first), 50, 128, func() {})
	f.view.Links = append(f.view.Links, PeerLink{ID: 50, Device: x, Cost: 128, ForwardingReady: true})
	f.releaseOld()
	// A fresh full-budget request can now reclaim a different safe standby.
	b := selectionDevice(81)
	f.view.Links = append(f.view.Links, PeerLink{ID: 60, Device: b, Cost: 128}, PeerLink{ID: 61, Device: b, Cost: 64, ForwardingReady: true})
	spare := f.p.ReserveGroup("b-in128", b, false, "128")
	if spare == nil || !spare.Ready(b, func() {}) {
		t.Fatal("second backup missing")
	}
	defer spare.Release()
	BindConnectionLink(WithConnectionLease(context.Background(), spare), 60, 128, func() {})
	if !f.p.ReclaimIncomingWorker() {
		t.Fatal("second actual demand did not reclaim")
	}
	if !f.p.incomingDemandActive() {
		t.Fatal("second demand absent")
	}
	// This old setup was classified as a new peer at Ready, but by this
	// later Bind it is merely another carrier for already admitted X.
	BindConnectionLink(WithConnectionLease(context.Background(), delayed), 51, 64, func() {})
	spare.Release()
	if got := f.oldOutgoing(); got != nil {
		got.Release()
		t.Fatal("late already-direct alias Bind let old redundancy consume fresh incoming opportunity")
	}
	if !f.p.incomingDemandActive() {
		t.Fatal("late already-direct alias Bind erased fresh distinct-peer demand")
	}
}

func bindReceiptLease(t *testing.T, f *incomingHoldFixture, key string, peer meshid.Device, cost string) *ConnectionLease {
	t.Helper()
	l := f.p.ReserveGroup(key, meshid.Device{}, false, cost)
	if l == nil || !l.Ready(peer, func() {}) {
		t.Fatal("verified setup missing:", key)
	}
	t.Cleanup(l.Release)
	return l
}

func assertReceiptStillProtectsLastSlot(t *testing.T, f *incomingHoldFixture) {
	t.Helper()
	if got := f.oldOutgoing(); got != nil {
		got.Release()
		t.Fatal("stale Bind let old redundancy consume the current distinct-peer opportunity")
	}
}

func addReceiptVictim(t *testing.T, f *incomingHoldFixture) *ConnectionLease {
	t.Helper()
	b := selectionDevice(81)
	f.view.Links = append(f.view.Links, PeerLink{ID: 60, Device: b, Cost: 128}, PeerLink{ID: 61, Device: b, Cost: 64, ForwardingReady: true})
	l := bindReceiptLease(t, f, "second-receipt-victim", b, "128")
	BindConnectionLink(WithConnectionLease(context.Background(), l), 60, 128, func() {})
	if !f.p.ReclaimIncomingWorker() || !l.Retired() {
		t.Fatal("fresh actual demand did not select second owned victim")
	}
	return l
}

func TestIncomingReceiptGenuineNewBindIncludesOwnPublishedLink(t *testing.T) {
	f := newIncomingHoldFixture(t, 2)
	f.demand(t)
	x := selectionDevice(80)
	l := bindReceiptLease(t, f, "genuine-new", x, "128")
	// Node admission may publish this exact incarnation before Bind returns.
	// Its own link is not evidence that X was an already-connected peer.
	f.view.Links = append(f.view.Links, PeerLink{ID: 50, Device: x, Cost: 128})
	BindConnectionLink(WithConnectionLease(context.Background(), l), 50, 128, func() {})
	f.releaseOld()
	if got := f.oldOutgoing(); got == nil {
		t.Fatal("genuinely new admitted peer did not satisfy its captured receipt")
	} else {
		got.Release()
	}
}

func TestIncomingReceiptOtherPublishedDirectLinkBlocksLateBind(t *testing.T) {
	f := newIncomingHoldFixture(t, 2)
	f.demand(t)
	x := selectionDevice(80)
	l := bindReceiptLease(t, f, "late-extra", x, "64")
	// Another incarnation reached Node first, independently of receipt age.
	f.view.Links = append(f.view.Links, PeerLink{ID: 50, Device: x, Cost: 128}, PeerLink{ID: 51, Device: x, Cost: 64})
	BindConnectionLink(WithConnectionLease(context.Background(), l), 51, 64, func() {})
	f.releaseOld()
	assertReceiptStillProtectsLastSlot(t, f)
}

func TestIncomingReceiptReadyBeforeDemandCannotServeLaterReceipt(t *testing.T) {
	for _, repeat := range []bool{false, true} {
		name := "once-ready"
		if repeat {
			name = "repeated-ready"
		}
		t.Run(name, func(t *testing.T) {
			f := newIncomingHoldFixture(t, 2)
			x := selectionDevice(80)
			l := bindReceiptLease(t, f, "ready-before-demand", x, "128")
			f.demand(t)
			if repeat && !l.Ready(x, func() {}) {
				t.Fatal("idempotent Ready failed")
			}
			BindConnectionLink(WithConnectionLease(context.Background(), l), 50, 128, func() {})
			f.releaseOld()
			assertReceiptStillProtectsLastSlot(t, f)
		})
	}
}

func TestIncomingReceiptExpiredOrServedReadyCannotClearReplacement(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "served"
		if expired {
			name = "expired"
		}
		t.Run(name, func(t *testing.T) {
			total := 3
			if expired {
				total = 2
			}
			f := newIncomingHoldFixture(t, total)
			f.demand(t)
			x := selectionDevice(80)
			delayed := bindReceiptLease(t, f, "delayed-first-receipt", x, "64")
			if expired {
				f.releaseOld()
				f.now = f.now.Add(FailedConnectPenalty + time.Nanosecond)
				// An ordinary scheduling check observes expiry before the next demand.
				if f.p.DeferRedundantCarrier(f.peer, 4096) {
					t.Fatal("first receipt did not expire")
				}
			} else {
				y := selectionDevice(82)
				first := bindReceiptLease(t, f, "first-served-receipt", y, "128")
				BindConnectionLink(WithConnectionLease(context.Background(), first), 52, 128, func() {})
				f.view.Links = append(f.view.Links, PeerLink{ID: 52, Device: y, Cost: 128, ForwardingReady: true})
				f.releaseOld()
			}
			second := addReceiptVictim(t, f)
			// X is still absent from the snapshot: this specifically checks receipt
			// identity, rather than the already-connected-alias guard.
			BindConnectionLink(WithConnectionLease(context.Background(), delayed), 51, 64, func() {})
			second.Release()
			assertReceiptStillProtectsLastSlot(t, f)
		})
	}
}
