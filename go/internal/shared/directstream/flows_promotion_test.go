package directstream

import (
	"errors"
	"io"
	"testing"
	"time"
)

func TestReliableFlowsPromotionOwnershipAndShutdown(t *testing.T) {
	a, b := flowPair(t)
	now := int64(0)
	pumpFlows(t, a, b, &now, nil)
	request := OpenRequest{Service: ServiceWendyCom}
	var ids []uint32
	var apps []*RetainedConn
	for i := 0; i < 2; i++ {
		id, err := a.Begin(request, now)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		pumpFlows(t, a, b, &now, func(m *ReliableFlows, e IncomingOpen) {
			if _, err := m.CompleteIncoming(e.ID, 0); err != nil {
				t.Fatal(err)
			}
		})
		app, err := a.Connection(id)
		if err != nil {
			t.Fatal(err)
		}
		apps = append(apps, app)
	}
	raw, peer := promotionPair()
	defer peer.Close()
	if _, err := a.CommitPromotedFlow(ids[0], raw); !errors.Is(err, ErrBounds) {
		t.Fatal("live carrier admitted", err)
	}
	a.Suspend()
	b.Suspend()
	// Both peers' byte state has been reconciled before this owner boundary.
	av, err := a.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	bv, err := b.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err = a.ApplyRecovery(bv, now); err != nil {
		t.Fatal(err)
	}
	if err = b.ApplyRecovery(av, now); err != nil {
		t.Fatal(err)
	}
	if got, err := a.PromotionRequest(ids[0]); err != nil || got != request {
		t.Fatal(got, err)
	}
	if _, err = apps[0].Write([]byte("prefix")); err != nil {
		t.Fatal(err)
	}
	done, err := a.CommitPromotedFlow(ids[0], raw)
	if err != nil {
		t.Fatal(err)
	}
	if a.Quiescent() {
		t.Fatal("live promoted flow reported quiescent")
	}
	if _, err = a.Snapshot(); !errors.Is(err, ErrPromoted) {
		t.Fatal("raw bytes snapshotted", err)
	}
	if err = a.Bind(2); !errors.Is(err, ErrPromoted) {
		t.Fatal("BLE rebound", err)
	}
	if _, err = a.Begin(request, now); !errors.Is(err, ErrPromoted) {
		t.Fatal("new BLE flow", err)
	}
	if _, err = a.CommitPromotedFlow(ids[0], raw); !errors.Is(err, ErrPromoted) {
		t.Fatal("double transfer", err)
	}
	if same, err := a.Connection(ids[0]); err != nil || same != apps[0] {
		t.Fatal("app socket changed", err)
	}
	peer.SetDeadline(time.Now().Add(time.Second))
	promotionRead(t, peer, "prefix")
	promotionWait(t, done)
	// Partial handoff must still allow other existing flows to transfer. Leave
	// the second prefix blocked and ensure session Close both aborts and joins it.
	raw2, peer2 := promotionPair()
	defer peer2.Close()
	if _, err = apps[1].Write([]byte("blocked")); err != nil {
		t.Fatal(err)
	}
	done2, err := a.CommitPromotedFlow(ids[1], raw2)
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() { a.Close(io.ErrClosedPipe); close(closed) }()
	promotionWait(t, closed)
	promotionWait(t, done2)
	for _, app := range apps {
		if _, err = app.Write([]byte("late")); err == nil {
			t.Fatal("live app after session close")
		}
	}
	a.budget.mu.Lock()
	defer a.budget.mu.Unlock()
	if a.budget.tx.Available() != 8192 || a.budget.rx.Available() != 8192 {
		t.Fatal("promotion shutdown retained slabs")
	}
}

func TestPromotedGracefulEOFRetiresWithoutReset(t *testing.T) {
	a, b := flowPair(t)
	now := int64(0)
	pumpFlows(t, a, b, &now, nil)
	id, err := a.Begin(OpenRequest{Service: ServiceWendyCom}, now)
	if err != nil {
		t.Fatal(err)
	}
	pumpFlows(t, a, b, &now, func(m *ReliableFlows, e IncomingOpen) {
		if _, err := m.CompleteIncoming(e.ID, 0); err != nil {
			t.Fatal(err)
		}
	})
	appA, _ := a.Connection(id)
	appB, _ := b.Connection(id)
	a.Suspend()
	b.Suspend()
	av, _ := a.Snapshot()
	bv, _ := b.Snapshot()
	if err = a.ApplyRecovery(bv, now); err != nil {
		t.Fatal(err)
	}
	if err = b.ApplyRecovery(av, now); err != nil {
		t.Fatal(err)
	}
	if err = a.BindPromotedControl(2); err != nil {
		t.Fatal(err)
	}
	if err = b.BindPromotedControl(2); err != nil {
		t.Fatal(err)
	}
	rawA, rawB := promotionPair()
	doneA, err := a.CommitPromotedFlow(id, rawA)
	if err != nil {
		t.Fatal(err)
	}
	doneB, err := b.CommitPromotedFlow(id, rawB)
	if err != nil {
		t.Fatal(err)
	}
	promotionWait(t, doneA)
	promotionWait(t, doneB)
	if err = appA.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	var p [1]byte
	if n, err := appB.Read(p[:]); n != 0 || err != io.EOF {
		t.Fatal(n, err)
	}
	if appB.promotedFinished() {
		t.Fatal("single half-close retired live sender")
	}
	if err = appB.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if n, err := appA.Read(p[:]); n != 0 || err != io.EOF {
		t.Fatal(n, err)
	}
	frame, ready := takeFlow(t, a, &now)
	if !ready || frame.Kind != FrameCloseACK {
		t.Fatal("graceful close emitted reset", frame, ready)
	}
	if f, again, err := a.Prepare(now); err != nil || again {
		t.Fatal("close confirmation repeated while waiting for peer", f, again, err)
	}
	deliverFlow(t, b, frame, now, nil)
	pumpFlows(t, a, b, &now, nil)
	if !a.Quiescent() || !b.Quiescent() {
		t.Fatal("orderly EOF left flow metadata")
	}
	for _, app := range []*RetainedConn{appA, appB} {
		if n, err := app.Read(p[:]); n != 0 || err != io.EOF {
			t.Fatal("retirement changed EOF into failure", n, err)
		}
		if err := app.CloseWrite(); err != nil {
			t.Fatal(err)
		}
	}
}
