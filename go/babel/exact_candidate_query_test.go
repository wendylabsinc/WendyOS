package babel

import (
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel/internal/wire"
)

func TestExactLinkCandidateQueryIncludesWarmStandbyWithoutMutatingState(t *testing.T) {
	prefix := netip.MustParsePrefix("fd00::2/128")
	for _, mode := range []string{"warm", "wrong-link", "zero-link", "wrong-sequence", "policy-expired", "not-two-way", "infeasible", "expired", "removed"} {
		t.Run(mode, func(t *testing.T) {
			allowed := true
			e, err := New(Config{RouterID: 1, HelloInterval: time.Second, AcceptRouteUpdate: func(id RouterID, p netip.Prefix, seq uint16) bool {
				return allowed && id == 2 && p == prefix && seq == 7
			}})
			if err != nil {
				t.Fatal(err)
			}
			step(t, e, 0, AddLink{testLink(1)})
			backup := testLink(2)
			backup.Cost = 4096
			step(t, e, 0, AddLink{backup})
			reachable(t, e, 1)
			reachable(t, e, 2)
			for _, link := range []LinkID{1, 2} {
				receiveMessages(t, e, 0, link, wire.Message{Type: wire.Update, Prefix: prefix, RouterID: 2, Seqno: 7, Metric: 0, Interval: 1600})
			}
			step(t, e, 100*time.Millisecond, Tick{}) // Establish the advertised feasibility distance.
			if e.selected[prefix].Link != 1 || !e.HasUsableOriginOnLink(2, prefix, 7, 2) {
				t.Fatal("fixture did not establish a nonselected warm candidate")
			}
			link, seq := LinkID(2), uint16(7)
			switch mode {
			case "wrong-link":
				link = 99
			case "zero-link":
				link = 0
			case "wrong-sequence":
				seq++
			case "policy-expired":
				allowed = false
			case "not-two-way":
				receiveMessages(t, e, 101*time.Millisecond, 2, wire.Message{Type: wire.IHU, Interval: 200, Metric: Infinity})
			case "infeasible":
				receiveMessages(t, e, 101*time.Millisecond, 2, wire.Message{Type: wire.Update, Prefix: prefix, RouterID: 2, Seqno: 7, Metric: 96, Interval: 1600})
			case "expired":
				step(t, e, time.Minute, Tick{})
			case "removed":
				step(t, e, 101*time.Millisecond, RemoveLink{ID: 2})
			}
			before, checkpoint := e.Snapshot(), e.Checkpoint()
			if got := e.HasUsableOriginOnLink(2, prefix, seq, link); got != (mode == "warm") {
				t.Fatalf("exact candidate=%v for %s", got, mode)
			}
			if !reflect.DeepEqual(before, e.Snapshot()) || !reflect.DeepEqual(checkpoint, e.Checkpoint()) {
				t.Fatal("diagnostic query mutated routing or forwarding state")
			}
		})
	}
}
