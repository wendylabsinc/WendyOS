package localmesh

import (
	"context"
	"testing"
)

func TestIncomingReceiptReferenceEndsAtBindOrSetupRelease(t *testing.T) {
	for _, end := range []string{"bind", "release", "retired-bind"} {
		t.Run(end, func(t *testing.T) {
			f := newIncomingHoldFixture(t, 2)
			f.demand(t)
			l := bindReceiptLease(t, f, "receipt-owner", selectionDevice(80), "128")
			if l.demandAtReady == nil || l.demandAtReady.owner != f.old {
				t.Fatal("authentication did not capture the exact pending owner")
			}
			ctx := WithConnectionLease(context.Background(), l)
			if end == "release" {
				l.Release()
			} else {
				if end == "retired-bind" {
					markConnectionRetry(ctx)
				}
				BindConnectionLink(ctx, 50, 128, func() {})
			}
			if l.demandAtReady != nil || l.newPeer {
				t.Fatal("completed setup retained its captured receipt")
			}
			if end == "bind" {
				if f.p.incomingReceipt != nil || f.p.incomingDemandActive() {
					t.Fatal("matched genuine admission did not release the policy receipt")
				}
				return
			}
			if f.p.incomingReceipt == nil || f.p.incomingReceipt.owner != f.old || !f.p.incomingDemandActive() {
				t.Fatal("unadmitted setup removed the policy's still-pending receipt")
			}
			f.releaseOld()
			if f.p.incomingReceipt == nil || f.p.incomingReceipt.owner != f.old || !f.p.incomingDemandActive() {
				t.Fatal("actual owner Release lost the finite opportunity")
			}
			f.now = f.now.Add(FailedConnectPenalty)
			if f.p.incomingDemandActive() || f.p.incomingReceipt != nil {
				t.Fatal("expired receipt retained its victim reference")
			}
		})
	}
}
