package babel

import (
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel/internal/wire"
)

func TestNeighbourSnapshotDistinguishesExpiredHoldFromInfinityIHU(t *testing.T) {
	for _, hello := range []time.Duration{time.Second, 4 * time.Second} {
		t.Run(hello.String(), func(t *testing.T) {
			e := newTestEngine(t, 1)
			step(t, e, 0, AddLink{testLink(1)})
			receiveMessages(t, e, 0, 1,
				wire.Message{Type: wire.Hello, Interval: centis(hello)},
				wire.Message{Type: wire.IHU, Interval: centis(2 * hello), Metric: 96})
			if n := e.Snapshot().Neighbours[0]; !n.Reachable || n.LivenessExpired {
				t.Fatalf("fresh two-way neighbour: %+v", n)
			}
			// A peer can explicitly report that it cannot hear us. This makes
			// the route unusable, but is not expiry of either advertised hold.
			receiveMessages(t, e, hello/2, 1, wire.Message{Type: wire.IHU, Interval: centis(2 * hello), Metric: Infinity})
			if n := e.Snapshot().Neighbours[0]; n.Reachable || n.LivenessExpired {
				t.Fatalf("explicit Infinity was mislabeled as expired: %+v", n)
			}
			step(t, e, hold(hello)-time.Nanosecond, Tick{})
			if e.Snapshot().Neighbours[0].LivenessExpired {
				t.Fatal("Hello expired before its existing hold")
			}
			step(t, e, hold(hello), Tick{})
			if !e.Snapshot().Neighbours[0].LivenessExpired {
				t.Fatal("Hello expiry missing at its existing hold")
			}
		})
	}
}

func TestNeighbourSnapshotRenewsHelloAndReportsIHUExpiry(t *testing.T) {
	e := newTestEngine(t, 1)
	step(t, e, 0, AddLink{testLink(1)})
	reachable(t, e, 1) // Hello hold3.5s; IHU hold7s.
	for seq := uint16(1); seq <= 6; seq++ {
		receiveMessages(t, e, time.Duration(seq)*time.Second, 1, wire.Message{Type: wire.Hello, Seqno: seq, Interval: 100})
	}
	step(t, e, 7*time.Second-time.Nanosecond, Tick{})
	if n := e.Snapshot().Neighbours[0]; !n.Reachable || n.LivenessExpired {
		t.Fatalf("fresh Hello was not retained before IHU expiry: %+v", n)
	}
	step(t, e, 7*time.Second, Tick{})
	if n := e.Snapshot().Neighbours[0]; n.Reachable || !n.LivenessExpired {
		t.Fatalf("IHU hold did not expire independently: %+v", n)
	}
	receiveMessages(t, e, 7*time.Second, 1, wire.Message{Type: wire.IHU, Interval: 200, Metric: 96})
	if n := e.Snapshot().Neighbours[0]; !n.Reachable || n.LivenessExpired {
		t.Fatalf("fresh IHU did not restore liveness: %+v", n)
	}
}

func TestNeighbourSnapshotRenewalBeforeHoldPreservesLiveness(t *testing.T) {
	for _, hello := range []time.Duration{time.Second, 4 * time.Second} {
		t.Run(hello.String(), func(t *testing.T) {
			e := newTestEngine(t, 1)
			step(t, e, 0, AddLink{testLink(1)})
			messages := []wire.Message{
				{Type: wire.Hello, Interval: centis(hello)},
				{Type: wire.IHU, Interval: centis(2 * hello), Metric: 96},
			}
			receiveMessages(t, e, 0, 1, messages...)
			messages[0].Seqno = 1
			receiveMessages(t, e, hold(hello)-time.Nanosecond, 1, messages...)
			step(t, e, hold(hello), Tick{})
			if n := e.Snapshot().Neighbours[0]; !n.Reachable || n.LivenessExpired {
				t.Fatalf("renewed promise expired at its old deadline: %+v", n)
			}
		})
	}
}
