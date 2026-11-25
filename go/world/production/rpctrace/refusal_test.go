package rpctrace

import (
	"context"
	"math"
	"sync"
	"testing"
)

func TestRefusalFixedFirstPredicateAndInterruptions(t *testing.T) {
	s := sink(t, Config{})
	ctx, finish := Begin(context.Background(), s, labels())
	Refuse(ctx, 0, ReasonPinDead, 0)
	Refuse(ctx, PointPeerBeforeScope, 255, 0)
	Refuse(ctx, PointPeerBeforeScope, ReasonPinDead, 255)
	Refuse(ctx, PointPeerAfterScope, ReasonPinClosed, ReasonPinDead)
	Refuse(ctx, PointAuthorize, ReasonInvalidBinding, 0)
	PinInterrupted(ctx, PointResponse, 2)
	PinInterrupted(ctx, PointPeerBeforeScope, 1)
	PinInterrupted(ctx, PointPeerBeforeScope, 3)
	PinInterrupted(ctx, PointAuthority, 1)
	PinInterrupted(ctx, PointPeerWatcher, 9)
	finish(Unavailable)
	Refuse(ctx, PointAuthority, ReasonPublisherDone, 0)
	PinInterrupted(ctx, PointResponse, 8)
	rows, _ := drain(t, s)
	if len(rows) != 1 || rows[0].Version != 2 || rows[0].Refusal == nil || *rows[0].Refusal != (Refusal{Point: "peer_after_scope", Reason: "pin_closed", WatcherReason: "pin_dead"}) {
		t.Fatalf("first predicate changed: %+v", rows)
	}
	counts := rows[0].PinInterruptions
	if len(counts) != 2 || counts[0] != (PinInterruption{"peer_before_scope", 4}) || counts[1] != (PinInterruption{"response", 2}) {
		t.Fatalf("invalid, unordered or post-finish counts: %+v", counts)
	}
}

func TestRefusalConcurrentSaturationAndLifetimeBudget(t *testing.T) {
	s := sink(t, Config{})
	ctx, finish := Begin(context.Background(), s, labels())
	c := fromContext(ctx)
	c.pinInterruptions[0] = math.MaxUint64 - 1
	var group sync.WaitGroup
	for range 32 {
		group.Go(func() {
			PinInterrupted(ctx, PointPeerBeforeScope, 8)
			PinInterrupted(ctx, PointPeerAfterScope, 1)
			Refuse(ctx, PointPeerAfterScope, ReasonPinEvent, 0)
		})
	}
	group.Wait()
	finish(Unavailable)
	rows, _ := drain(t, s)
	if len(rows) != 1 || len(rows[0].PinInterruptions) != 2 || rows[0].PinInterruptions[0].Count != math.MaxUint64 || rows[0].PinInterruptions[1].Count != 32 {
		t.Fatal("diagnostic counters overflowed or lost observations")
	}
	// New optional fields consume the same lifetime byte budget.
	limited := sink(t, Config{MaxBytes: 1})
	next, done := Begin(context.Background(), limited, labels())
	copy := fromContext(next)
	copy.pinInterruptions = c.pinInterruptions
	Refuse(next, PointPeerAfterScope, ReasonPinEvent, 0)
	done(Unavailable)
	if limited.Stats().EnqueuedBytes != 0 || limited.Stats().DroppedBudget != 1 {
		t.Fatal("diagnostic fields bypassed the byte budget")
	}
	drain(t, limited)
}

func TestRefusalNilDiagnosticsAllocateNothing(t *testing.T) {
	ctx := context.Background()
	if allocations := testing.AllocsPerRun(1000, func() {
		if Enabled(ctx) {
			t.Fatal("untraced context enabled diagnostics")
		}
		Refuse(ctx, PointPeerBeforeScope, ReasonPinDead, 0)
		PinInterrupted(ctx, PointPeerBeforeScope, 2)
	}); allocations != 0 {
		t.Fatalf("nil diagnostics allocated %g", allocations)
	}
	for _, method := range []Method{MethodPeerWatch, MethodAuthorityWatch} {
		s := sink(t, Config{})
		l := labels()
		l.Method = method
		ctx, done := Begin(context.Background(), s, l)
		PinInterrupted(ctx, PointPeerWatcher, 1)
		done(OK)
		rows, _ := drain(t, s)
		if len(rows) != 1 || rows[0].Refusal != nil || len(rows[0].Stages) != 1 || rows[0].Stages[0].Stage != "total" {
			t.Fatal("watch event fabricated a failed predicate or earlier work duration")
		}
	}
}
