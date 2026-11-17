package directstream

import (
	"bytes"
	"io"
	"testing"
	"time"
)

func flowPair(t *testing.T) (*ReliableFlows, *ReliableFlows) {
	t.Helper()
	a, err := NewReliableFlows(0, 8, 8192, 8192, 4096, 1024)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewReliableFlows(1, 8, 8192, 8192, 4096, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if a.Bind(1) != nil || b.Bind(1) != nil {
		t.Fatal("bind")
	}
	t.Cleanup(func() { a.Close(io.ErrClosedPipe); b.Close(io.ErrClosedPipe) })
	return a, b
}
func takeFlow(t *testing.T, m *ReliableFlows, now *int64) (FlowFrame, bool) {
	t.Helper()
	*now += 100000
	f, ok, err := m.Prepare(*now)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return f, false
	}
	// Queue backpressure leaves exactly the same complete record pending.
	again, ok, err := m.Prepare(*now)
	if !ok || err != nil || again.Kind != f.Kind || again.FlowID != f.FlowID {
		t.Fatal("changed pending record")
	}
	var wire [MaxFlowFrame]byte
	n, err := f.Encode(wire[:])
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeFlowFrame(wire[:n])
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Accepted(); err != nil {
		t.Fatal(err)
	}
	return got, true
}
func deliverFlow(t *testing.T, m *ReliableFlows, f FlowFrame, now int64, opened func(*ReliableFlows, IncomingOpen)) {
	t.Helper()
	event, err := m.Receive(f, now)
	if err != nil {
		t.Fatalf("receive %d flow%d: %v", f.Kind, f.FlowID, err)
	}
	if event != nil {
		if opened == nil {
			t.Fatal("unexpected backend open")
		}
		opened(m, *event)
	}
}
func pumpFlows(t *testing.T, a, b *ReliableFlows, now *int64, opened func(*ReliableFlows, IncomingOpen)) {
	t.Helper()
	idle := 0
	for count := 0; count < 500; count++ {
		sent := false
		for i, m := range []*ReliableFlows{a, b} {
			other := b
			if i == 1 {
				other = a
			}
			if f, ok := takeFlow(t, m, now); ok {
				sent = true
				deliverFlow(t, other, f, *now, opened)
			}
		}
		if sent {
			idle = 0
		} else {
			idle++
			if idle == 2 {
				return
			}
		}
	}
	t.Fatal("flow pump did not quiesce")
}
func recoverFlowPair(t *testing.T, a, b *ReliableFlows, now *int64) {
	t.Helper()
	a.Suspend()
	b.Suspend()
	av, err := a.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	bv, err := b.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	n0, n1 := 0, 0
	c0, c1 := exchangeConfig(0, false, &n0), exchangeConfig(1, false, &n1)
	c0.Local = av
	c1.Local = bv
	*now += 100000
	c0.Apply = func() error { return a.ApplyRecovery(bv, *now) }
	c1.Apply = func() error { return b.ApplyRecovery(av, *now) }
	c, err := NewCoordinatorExchange(c0, 2, 2, DirectBLE, false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewParticipantExchange(c1, exchangeWire(t, c))
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, ok := c.Next(); !ok {
			break
		}
		if err = p.Receive(exchangeWire(t, c)); err != nil {
			t.Fatal(err)
		}
	}
	for {
		if _, ok := p.Next(); !ok {
			break
		}
		if err = c.Receive(exchangeWire(t, p)); err != nil {
			t.Fatal(err)
		}
	}
	if err = p.Receive(exchangeWire(t, c)); err != nil {
		t.Fatal(err)
	}
	if err = c.Receive(exchangeWire(t, p)); err != nil {
		t.Fatal(err)
	}
	if !c.Active() || !p.Active() {
		t.Fatal("incomplete authenticated recovery")
	}
	if err = a.Bind(2); err != nil {
		t.Fatal(err)
	}
	if err = b.Bind(2); err != nil {
		t.Fatal(err)
	}
}
func TestReliableFlowsLostDataAndACKPreserveAppSockets(t *testing.T) {
	for _, drop := range []FrameKind{FrameData, FrameState} {
		t.Run(map[FrameKind]string{FrameData: "DATA", FrameState: "ACK"}[drop], func(t *testing.T) {
			a, b := flowPair(t)
			var now int64
			pumpFlows(t, a, b, &now, nil)
			id, err := a.Begin(OpenRequest{Service: ServiceTCP, Port: 28080}, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = a.Connection(id); err != ErrOpenPending {
				t.Fatal("exposed pre-OPEN socket")
			}
			opens := 0
			var bc *RetainedConn
			opened := func(m *ReliableFlows, e IncomingOpen) {
				opens++
				if e.Request.Port != 28080 {
					t.Fatal("wrong target")
				}
				var err error
				bc, err = m.CompleteIncoming(e.ID, 0)
				if err != nil {
					t.Fatal(err)
				}
			}
			pumpFlows(t, a, b, &now, opened)
			ac, err := a.Connection(id)
			if err != nil {
				t.Fatal(err)
			}
			payload := bytes.Repeat([]byte("abcdefghij"), 300)
			if _, err = ac.Write(payload); err != nil {
				t.Fatal(err)
			}
			cut := false
			for i := 0; i < 20 && !cut; i++ {
				for direction, m := range []*ReliableFlows{a, b} {
					other := b
					if direction == 1 {
						other = a
					}
					f, ok := takeFlow(t, m, &now)
					if !ok {
						continue
					}
					if f.Kind == drop {
						cut = true
						break
					}
					deliverFlow(t, other, f, now, opened)
				}
			}
			if !cut {
				t.Fatal("no cut")
			}
			snap, err := b.flows[id].lane.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			prefix := make([]byte, int(snap.Received))
			if _, err = io.ReadFull(bc, prefix); err != nil {
				t.Fatal(err)
			}
			recoverFlowPair(t, a, b, &now)
			same, err := a.Connection(id)
			if err != nil || same != ac {
				t.Fatal("outgoing socket changed")
			}
			same, err = b.Connection(id)
			if err != nil || same != bc || opens != 1 {
				t.Fatal("backend reopened")
			}
			pumpFlows(t, a, b, &now, opened)
			rest := make([]byte, len(payload)-len(prefix))
			if _, err = io.ReadFull(bc, rest); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(append(prefix, rest...), payload) {
				t.Fatal("replay duplicated/lost bytes")
			}
			ac.CloseWrite()
			bc.CloseWrite()
			pumpFlows(t, a, b, &now, opened)
			if _, err = ac.Read(make([]byte, 1)); err != io.EOF {
				t.Fatalf("app EOF %v", err)
			}
			if _, err = bc.Read(make([]byte, 1)); err != io.EOF {
				t.Fatalf("backend EOF %v", err)
			}
			pumpFlows(t, a, b, &now, opened)
			if !a.Quiescent() || !b.Quiescent() {
				av, _ := a.Snapshot()
				bv, _ := b.Snapshot()
				t.Fatalf("close tombstones did not retire: a=%+v b=%+v", av, bv)
			}
			if a.budget.tx.Available() != 8192 || a.budget.rx.Available() != 8192 || b.budget.tx.Available() != 8192 || b.budget.rx.Available() != 8192 {
				t.Fatal("slabs leaked")
			}
		})
	}
}
func TestReliableFlowsRecoverLostOpenAndResult(t *testing.T) {
	for _, drop := range []FrameKind{FrameOpen, FrameOpenResult} {
		t.Run(map[FrameKind]string{FrameOpen: "OPEN", FrameOpenResult: "RESULT"}[drop], func(t *testing.T) {
			a, b := flowPair(t)
			var now int64
			pumpFlows(t, a, b, &now, nil)
			id, err := a.Begin(OpenRequest{Service: ServiceWendyCom}, now)
			if err != nil {
				t.Fatal(err)
			}
			opens := 0
			var backend *RetainedConn
			opened := func(m *ReliableFlows, e IncomingOpen) {
				opens++
				var err error
				backend, err = m.CompleteIncoming(e.ID, 0)
				if err != nil {
					t.Fatal(err)
				}
			}
			cut := false
			for i := 0; i < 20 && !cut; i++ {
				for direction, m := range []*ReliableFlows{a, b} {
					other := b
					if direction == 1 {
						other = a
					}
					f, ok := takeFlow(t, m, &now)
					if !ok {
						continue
					}
					if f.Kind == drop {
						cut = true
						break
					}
					deliverFlow(t, other, f, now, opened)
				}
			}
			if !cut {
				t.Fatal("no cut")
			}
			saved := backend
			recoverFlowPair(t, a, b, &now)
			pumpFlows(t, a, b, &now, opened)
			app, err := a.Connection(id)
			if err != nil || app == nil || opens != 1 {
				t.Fatal("OPEN recovery", err, opens)
			}
			if drop == FrameOpenResult && backend != saved {
				t.Fatal("redialled backend")
			}
			app.Write([]byte("sensor request"))
			pumpFlows(t, a, b, &now, opened)
			got := make([]byte, 14)
			if _, err = io.ReadFull(backend, got); err != nil || string(got) != "sensor request" {
				t.Fatal(string(got), err)
			}
		})
	}
}

func TestReliableFlowsLostCloseConfirmationNeverReopens(t *testing.T) {
	a, b := flowPair(t)
	var now int64
	pumpFlows(t, a, b, &now, nil)
	id, err := a.Begin(OpenRequest{Service: ServiceWendyCom}, now)
	if err != nil {
		t.Fatal(err)
	}
	var bc *RetainedConn
	pumpFlows(t, a, b, &now, func(m *ReliableFlows, e IncomingOpen) {
		bc, err = m.CompleteIncoming(e.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
	})
	ac, err := a.Connection(id)
	if err != nil {
		t.Fatal(err)
	}
	ac.CloseWrite()
	bc.CloseWrite()
	lost := false
	for round := 0; round < 30; round++ {
		for direction, m := range []*ReliableFlows{a, b} {
			other := b
			if direction == 1 {
				other = a
			}
			f, ok := takeFlow(t, m, &now)
			if !ok {
				continue
			}
			if direction == 0 && f.Kind == FrameCloseACK {
				lost = true
				continue
			}
			deliverFlow(t, other, f, now, nil)
		}
	}
	if !lost || !a.Quiescent() || b.Quiescent() {
		t.Fatal("did not reach asymmetric retirement")
	}
	recoverFlowPair(t, a, b, &now)
	pumpFlows(t, a, b, &now, nil)
	if !a.Quiescent() || !b.Quiescent() {
		t.Fatal("retired stream recreated")
	}
	if _, err = ac.Read(make([]byte, 1)); err != io.EOF {
		t.Fatal("retired app EOF", err)
	}
	if _, err = bc.Read(make([]byte, 1)); err != io.EOF {
		t.Fatal("retired backend EOF", err)
	}
}
func TestReliableFlowsCrossedOpenOrdering(t *testing.T) {
	a, b := flowPair(t)
	var now int64
	pumpFlows(t, a, b, &now, nil)
	accept := func(m *ReliableFlows, e IncomingOpen) {
		if _, err := m.CompleteIncoming(e.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := b.Begin(OpenRequest{Service: ServiceTCP, Port: uint16(100 + i)}, now); err != nil {
			t.Fatal(err)
		}
	}
	pumpFlows(t, a, b, &now, accept)
	if a.cursor != 3 {
		t.Fatalf("fixture did not establish crossed cursor: %d", a.cursor)
	}
	first, err := a.Begin(OpenRequest{Service: ServiceTCP, Port: 200}, now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.Begin(OpenRequest{Service: ServiceTCP, Port: 201}, now)
	if err != nil {
		t.Fatal(err)
	}
	f, ok := takeFlow(t, a, &now)
	if !ok || f.Kind != FrameOpen || f.FlowID != first {
		t.Fatal("OPEN reordered around DATA cursor")
	}
	deliverFlow(t, b, f, now, accept)
	f, ok = takeFlow(t, a, &now)
	if !ok || f.Kind != FrameOpen || f.FlowID != second {
		t.Fatal("second OPEN missing")
	}
	deliverFlow(t, b, f, now, accept)
	pumpFlows(t, a, b, &now, accept)
}

func TestReliableFlowsEarlyFINKeepsGrantingCredit(t *testing.T) {
	a, b := flowPair(t)
	var now int64
	pumpFlows(t, a, b, &now, nil)
	id, err := a.Begin(OpenRequest{Service: ServiceTCP, Port: 28080}, now)
	if err != nil {
		t.Fatal(err)
	}
	var receiver *RetainedConn
	pumpFlows(t, a, b, &now, func(m *ReliableFlows, e IncomingOpen) {
		var err error
		receiver, err = m.CompleteIncoming(e.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
	})
	sender, err := a.Connection(id)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("early FIN"), 350) // Exceeds the initial 1 KiB receive grant.
	if _, err = sender.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err = sender.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	// FIN has priority over queued DATA. It announces an offset, not permission
	// to stop credit grants before the remaining bytes reach that offset.
	pumpFlows(t, a, b, &now, nil)
	receiver.SetReadDeadline(time.Now().Add(time.Second))
	got, err := io.ReadAll(receiver)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("early FIN lost credit: got=%d want=%d err=%v", len(got), len(payload), err)
	}
}

func TestGracefulRetirementReleasesSenderList(t *testing.T) {
	a, b := flowPair(t)
	var now int64
	pumpFlows(t, a, b, &now, nil)
	for round := 0; round < 3; round++ {
		id, err := a.Begin(OpenRequest{Service: ServiceTCP, Port: 28080}, now)
		if err != nil {
			t.Fatal(err)
		}
		var peer *RetainedConn
		pumpFlows(t, a, b, &now, func(m *ReliableFlows, open IncomingOpen) {
			peer, err = m.CompleteIncoming(open.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
		})
		local, err := a.Connection(id)
		if err != nil {
			t.Fatal(err)
		}
		if err = local.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		if err = peer.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		pumpFlows(t, a, b, &now, nil)
		for _, m := range []*ReliableFlows{a, b} {
			if !m.Quiescent() || m.budget.senders != nil {
				t.Fatal("retired flow retained by sender list")
			}
		}
	}
}
