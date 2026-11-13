package babel

import (
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel/internal/wire"
)

func TestSequenceRequestTriggersEveryLink(t *testing.T) {
	e := newTestEngine(t, 1)
	prefix := netip.MustParsePrefix("fd00::1/128")
	step(t, e, 0, AddLink{testLink(1)})
	step(t, e, 0, AddLink{testLink(2)})
	step(t, e, 0, Originate{prefix, 0})
	// Drain the initial triggered updates; neither periodic update is due yet.
	for now := 100 * time.Millisecond; now <= 500*time.Millisecond; now += 100 * time.Millisecond {
		step(t, e, now, Tick{})
	}
	if e.trigger {
		t.Fatal("initial trigger has not drained")
	}
	receiveMessages(t, e, 500*time.Millisecond, 1, wire.Message{
		Type: wire.SeqRequest, Prefix: prefix, RouterID: 1, Seqno: 1, HopCount: 64,
	})
	fx := step(t, e, 600*time.Millisecond, Tick{})
	seen := map[LinkID]bool{}
	for _, d := range fx.Datagrams {
		messages, err := wire.Decode(d.Payload, testLink(d.Link).Local)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range messages {
			if m.Type == wire.Update && m.Prefix == prefix && m.RouterID == 1 && m.Seqno == 1 && m.Metric == 0 {
				seen[d.Link] = true
			}
		}
	}
	if !seen[1] || !seen[2] {
		t.Fatalf("new origin sequence did not reach every link before periodic update: %v", seen)
	}
}

func TestLearnedSequenceTriggersRelayWithoutMetricChange(t *testing.T) {
	e := newTestEngine(t, 1)
	prefix := netip.MustParsePrefix("fd00::3/128")
	step(t, e, 0, AddLink{testLink(1)})
	step(t, e, 0, AddLink{testLink(2)})
	reachable(t, e, 1)
	reachable(t, e, 2)
	update := wire.Message{Type: wire.Update, Prefix: prefix, RouterID: 3, Seqno: 65535, Metric: 0, Interval: 400}
	receiveMessages(t, e, 0, 1, update)
	for now := 100 * time.Millisecond; now <= 500*time.Millisecond; now += 100 * time.Millisecond {
		step(t, e, now, Tick{})
	}
	receiveMessages(t, e, 600*time.Millisecond, 1, update)
	if e.trigger {
		t.Fatal("unchanged periodic advertisement scheduled a trigger")
	}
	update.Seqno = 0 // Serial wrap must propagate like any other sequence change.
	receiveMessages(t, e, 700*time.Millisecond, 1, update)
	fx := step(t, e, 800*time.Millisecond, Tick{})
	for _, d := range fx.Datagrams {
		if d.Link != 2 {
			continue
		}
		messages, err := wire.Decode(d.Payload, testLink(2).Local)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range messages {
			if m.Type == wire.Update && m.Prefix == prefix && m.RouterID == 3 && m.Seqno == 0 && m.Metric == 96 {
				return
			}
		}
	}
	t.Fatal("same-metric sequence change did not propagate through the relay")
}
