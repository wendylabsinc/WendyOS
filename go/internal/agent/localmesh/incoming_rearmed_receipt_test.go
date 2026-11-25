package localmesh

import (
	"context"
	"testing"
)

func TestIncomingReceiptRearmedSameClosingOwnerIsDistinct(t *testing.T) {
	f := newIncomingHoldFixture(t, 4)
	f.demand(t)
	x, y, z := selectionDevice(80), selectionDevice(82), selectionDevice(83)
	delayed := bindReceiptLease(t, f, "delayed-old-receipt", x, "64")
	served := bindReceiptLease(t, f, "genuine-served", y, "128")
	BindConnectionLink(WithConnectionLease(context.Background(), served), 50, 128, func() {})
	f.view.Links = append(f.view.Links, PeerLink{ID: 50, Device: y, Cost: 128, ForwardingReady: true})
	filler := bindReceiptLease(t, f, "fill-unused-slot", z, "128")
	BindConnectionLink(WithConnectionLease(context.Background(), filler), 60, 128, func() {})
	f.view.Links = append(f.view.Links, PeerLink{ID: 60, Device: z, Cost: 128, ForwardingReady: true})
	if f.p.Counts().Total != 4 || f.p.incomingDemandActive() {
		t.Fatal("first receipt not served while original physical owner still closing")
	}
	// Fresh unknown full demand waits for the SAME still-closing owner.
	if f.p.ReclaimIncomingWorker() {
		t.Fatal("pending original close was signaled twice")
	}
	if !f.p.incomingDemandActive() {
		t.Fatal("fresh full unknown demand did not preserve pending opportunity")
	}
	BindConnectionLink(WithConnectionLease(context.Background(), delayed), 51, 64, func() {})
	f.releaseOld()
	if got := f.oldOutgoing(); got != nil {
		got.Release()
		t.Fatal("old receipt token cleared a new opportunity rearmed on the same closing owner")
	}
}

func TestIncomingReceiptRepeatedPendingDemandKeepsOpportunity(t *testing.T) {
	f := newIncomingHoldFixture(t, 2)
	f.demand(t)
	l := bindReceiptLease(t, f, "new-awaiting-admission", selectionDevice(80), "128")
	if f.p.ReclaimIncomingWorker() {
		t.Fatal("repeated pending demand signaled another owner")
	}
	BindConnectionLink(WithConnectionLease(context.Background(), l), 50, 128, func() {})
	f.releaseOld()
	if got := f.oldOutgoing(); got == nil {
		t.Fatal("repeated pending demand replaced the opportunity already captured by a genuine new admission")
	} else {
		got.Release()
	}
}
