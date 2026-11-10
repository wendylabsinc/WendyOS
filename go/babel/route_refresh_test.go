package babel

import (
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel/internal/wire"
)

func refreshMessages(t *testing.T, e *Engine, packets []Datagram) []wire.Message {
	t.Helper()
	var out []wire.Message
	for _, packet := range packets {
		messages, err := wire.Decode(packet.Payload, e.links[packet.Link].Local)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, messages...)
	}
	return out
}

func TestRefreshRouteRecoversRejectedUpdateFromActualResponse(t *testing.T) {
	prefix := netip.MustParsePrefix("fd00::2/128")
	allowed := uint16(0)
	receiver, err := New(Config{RouterID: 1, HelloInterval: time.Second, UpdateInterval: 16 * time.Second, AcceptRouteUpdate: func(id RouterID, p netip.Prefix, seq uint16) bool { return id == 2 && p == prefix && seq == allowed }})
	if err != nil {
		t.Fatal(err)
	}
	origin, err := New(Config{RouterID: 2, HelloInterval: time.Second, UpdateInterval: 16 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	left := testLink(1)
	right := left
	right.Local, right.Peer = left.Peer, left.Local
	step(t, receiver, 0, AddLink{left})
	step(t, origin, 0, AddLink{right})
	reachable(t, receiver, 1)
	reachable(t, origin, 1)
	step(t, origin, 0, Originate{Prefix: prefix})
	// Learn the original sequence through a real standard prefix request/reply.
	request := wire.Pack([]wire.Message{{Type: wire.Request, Prefix: prefix}}, 512)[0]
	initial := step(t, origin, 0, Receive{ID: 1, Packet: request})
	for _, packet := range initial.Datagrams {
		step(t, receiver, 0, Receive{ID: 1, Packet: packet.Payload})
	}
	if !receiver.HasUsableOrigin(2, prefix, 0) {
		t.Fatal("initial actual route absent")
	}
	// The newer finite packet arrives before its independent signed permission.
	seqRequest := wire.Pack([]wire.Message{{Type: wire.SeqRequest, Prefix: prefix, RouterID: 2, Seqno: 1, HopCount: 64}}, 512)[0]
	changed := step(t, origin, 100*time.Millisecond, Receive{ID: 1, Packet: seqRequest})
	for _, packet := range changed.Datagrams {
		step(t, receiver, 100*time.Millisecond, Receive{ID: 1, Packet: packet.Payload})
	}
	if receiver.HasUsableOrigin(2, prefix, 1) || origin.Snapshot().Seqno != 1 {
		t.Fatal("unverified sequence admitted or origin did not advance exactly once")
	}
	allowed = 1
	refresh := step(t, receiver, 101*time.Millisecond, RefreshRoute{Prefix: prefix})
	if receiver.HasUsableOrigin(2, prefix, 1) {
		t.Fatal("refresh fabricated a finite candidate")
	}
	requests := 0
	for _, message := range refreshMessages(t, receiver, refresh.Datagrams) {
		if message.Type == wire.Request && message.Prefix == prefix && !message.Wildcard {
			requests++
		}
		if message.Type == wire.SeqRequest {
			t.Fatal("refresh requested a new origin sequence")
		}
	}
	if requests != 1 {
		t.Fatalf("prefix requests=%d", requests)
	}
	for _, packet := range refresh.Datagrams {
		response := step(t, origin, 101*time.Millisecond, Receive{ID: 1, Packet: packet.Payload})
		for _, reply := range response.Datagrams {
			step(t, receiver, 101*time.Millisecond, Receive{ID: 1, Packet: reply.Payload})
		}
	}
	if !receiver.HasUsableOrigin(2, prefix, 1) {
		t.Fatal("actual request response did not restore the exact finite tuple")
	}
	route := receiver.selected[prefix]
	if route.Unreachable || route.RouterID != 2 || route.Seqno != 1 || route.Link != 1 || origin.Snapshot().Seqno != 1 {
		t.Fatalf("response changed routing semantics: route=%+v origin=%+v", route, origin.Snapshot())
	}
}

func TestHasUsableOriginRequiresActualLiveFeasibleAuthorizedCandidate(t *testing.T) {
	prefix := netip.MustParsePrefix("fd00::2/128")
	for _, cause := range []string{"usable", "wrong origin", "wrong prefix", "wrong sequence", "unauthorized", "retracted", "expired", "peer removed", "peer unreachable", "infeasible", "local", "stopped"} {
		t.Run(cause, func(t *testing.T) {
			allowed := true
			e, err := New(Config{RouterID: 1, HelloInterval: time.Second, UpdateInterval: 16 * time.Second, AcceptRouteUpdate: func(RouterID, netip.Prefix, uint16) bool { return allowed }})
			if err != nil {
				t.Fatal(err)
			}
			step(t, e, 0, AddLink{testLink(1)})
			step(t, e, 0, AddLink{testLink(2)})
			receiveMessages(t, e, 0, 1, wire.Message{Type: wire.Hello, Interval: 1000}, wire.Message{Type: wire.IHU, Metric: 96, Interval: 2000})
			receiveMessages(t, e, 0, 1, wire.Message{Type: wire.Update, Prefix: prefix, RouterID: 2, Seqno: 7, Metric: 0, Interval: 100})
			id, p, seq := RouterID(2), prefix, uint16(7)
			switch cause {
			case "wrong origin":
				id = 3
			case "wrong prefix":
				p = netip.MustParsePrefix("fd00::3/128")
			case "wrong sequence":
				seq = 8
			case "unauthorized":
				allowed = false
			case "retracted":
				receiveMessages(t, e, 0, 1, wire.Message{Type: wire.Update, Prefix: prefix, Metric: Infinity, Interval: 100})
			case "expired":
				step(t, e, 4*time.Second, Tick{})
			case "peer removed":
				step(t, e, 0, RemoveLink{ID: 1})
			case "peer unreachable":
				receiveMessages(t, e, 0, 1, wire.Message{Type: wire.IHU, Metric: Infinity, Interval: 2000})
			case "infeasible":
				step(t, e, 100*time.Millisecond, Tick{})
				receiveMessages(t, e, 100*time.Millisecond, 1, wire.Message{Type: wire.Update, Prefix: prefix, RouterID: 2, Seqno: 7, Metric: 96, Interval: 100})
			case "local":
				step(t, e, 0, Originate{Prefix: prefix})
				id = 1
				seq = e.seq
			case "stopped":
				e.stopped = true
			}
			before, checkpoint := e.Snapshot(), e.Checkpoint()
			if got := e.HasUsableOrigin(id, p, seq); got != (cause == "usable") {
				t.Fatalf("%s usable=%v snapshot=%+v", cause, got, before)
			}
			if !reflect.DeepEqual(before, e.Snapshot()) || !reflect.DeepEqual(checkpoint, e.Checkpoint()) {
				t.Fatal("query mutated routing or safety state")
			}
		})
	}
}

func TestHasUsableOriginWaitsForForwardingCommit(t *testing.T) {
	e := newTestEngine(t, 1)
	prefix := netip.MustParsePrefix("fd00::2/128")
	step(t, e, 0, AddLink{testLink(1)})
	reachable(t, e, 1)
	packet := wire.Pack([]wire.Message{{Type: wire.Update, Prefix: prefix, RouterID: 2, Seqno: 1, Metric: 0, Interval: 400}}, 512)[0]
	effect, err := e.Step(0, Receive{ID: 1, Packet: packet})
	if err != nil || effect.Revision == 0 {
		t.Fatal(effect, err)
	}
	if e.HasUsableOrigin(2, prefix, 1) {
		t.Fatal("uncommitted forwarding qualified as a usable handoff")
	}
	if _, err = e.Commit(effect.Revision, true); err != nil {
		t.Fatal(err)
	}
	if !e.HasUsableOrigin(2, prefix, 1) {
		t.Fatal("committed finite candidate did not qualify")
	}
}

func TestRefreshRouteCoalescesWithinExistingRequestBudget(t *testing.T) {
	e, err := New(Config{RouterID: 1, MaxRequests: 2})
	if err != nil {
		t.Fatal(err)
	}
	step(t, e, 0, AddLink{testLink(1)})
	step(t, e, 0, AddLink{testLink(2)})
	before, scheduled := e.NextDeadline()
	p := netip.MustParsePrefix("fd00::2/128")
	first := step(t, e, 0, RefreshRoute{Prefix: p})
	count := func(packets []Datagram) int {
		n := 0
		for _, m := range refreshMessages(t, e, packets) {
			if m.Type == wire.Request && !m.Wildcard {
				n++
			}
		}
		return n
	}
	if count(first.Datagrams) != 2 || len(e.requests) != 1 {
		t.Fatal("refresh did not query both existing links once")
	}
	if after, ok := e.NextDeadline(); ok != scheduled || after != before {
		t.Fatal("coalescing changed detection scheduling")
	}
	if count(step(t, e, 100*time.Millisecond, RefreshRoute{Prefix: p}).Datagrams) != 0 {
		t.Fatal("same prefix was not coalesced")
	}
	step(t, e, 100*time.Millisecond, RefreshRoute{Prefix: netip.MustParsePrefix("fd00::3/128")})
	rejected := e.counters.Rejected
	blocked := step(t, e, 100*time.Millisecond, RefreshRoute{Prefix: netip.MustParsePrefix("fd00::4/128")})
	if count(blocked.Datagrams) != 0 || len(e.requests) != 2 || e.counters.Rejected != rejected+1 {
		t.Fatal("refresh exceeded MaxRequests")
	}
	renewed := step(t, e, 1100*time.Millisecond, RefreshRoute{Prefix: p})
	if count(renewed.Datagrams) != 2 || len(e.requests) != 1 {
		t.Fatal("expired coalescing did not release its bounded entry")
	}
}

func TestRefreshRouteCannotStarveSequenceRecovery(t *testing.T) {
	e, err := New(Config{RouterID: 1, HelloInterval: time.Second, UpdateInterval: 4 * time.Second, MaxRequests: 1})
	if err != nil {
		t.Fatal(err)
	}
	step(t, e, 0, AddLink{testLink(1)})
	step(t, e, 0, AddLink{testLink(2)})
	reachable(t, e, 1)
	prefix := netip.MustParsePrefix("fd00::2/128")
	receiveMessages(t, e, 0, 1, wire.Message{Type: wire.Update, Prefix: prefix, RouterID: 2, Seqno: 7, Metric: 0, Interval: 400})
	step(t, e, 100*time.Millisecond, Tick{})
	step(t, e, 100*time.Millisecond, RefreshRoute{Prefix: netip.MustParsePrefix("fd00::9/128")})
	receiveMessages(t, e, 100*time.Millisecond, 1, wire.Message{Type: wire.Update, Prefix: prefix, RouterID: 2, Seqno: 7, Metric: 96, Interval: 400})
	q := e.requests[sourceKey{prefix, 2}]
	if len(e.requests) != 1 || q == nil || q.message.Type != wire.SeqRequest {
		t.Fatalf("refresh blocked real feasibility recovery: %+v", e.requests)
	}
	step(t, e, 100*time.Millisecond, RefreshRoute{Prefix: netip.MustParsePrefix("fd00::8/128")})
	if len(e.requests) != 1 || e.requests[sourceKey{prefix, 2}] != q {
		t.Fatal("optional refresh displaced sequence recovery")
	}
}

func TestRefreshRouteRejectsInvalidPrefixWithoutStateChange(t *testing.T) {
	e := newTestEngine(t, 1)
	for _, prefix := range []netip.Prefix{{}, netip.MustParsePrefix("fe80::/64"), netip.MustParsePrefix("ff00::/8"), netip.MustParsePrefix("::1/128")} {
		before := e.Snapshot()
		if _, err := e.Step(time.Second, RefreshRoute{Prefix: prefix}); err != ErrConfig {
			t.Fatalf("prefix=%s error=%v", prefix, err)
		}
		if !reflect.DeepEqual(before, e.Snapshot()) {
			t.Fatal("invalid refresh changed state")
		}
	}
}

// A newer actual path can have a higher metric than the still-authorized old
// tuple. Handoff must be able to recognize that received candidate before
// removing the old permission; selection then changes directly finite->finite.
func TestHasUsableOriginRecognizesNonselectedReplacement(t *testing.T) {
	prefix := netip.MustParsePrefix("fd00::2/128")
	allowed := map[uint16]bool{7: true, 8: true}
	e, err := New(Config{RouterID: 1, HelloInterval: time.Second, UpdateInterval: 16 * time.Second, AcceptRouteUpdate: func(id RouterID, p netip.Prefix, seq uint16) bool { return id == 2 && p == prefix && allowed[seq] }})
	if err != nil {
		t.Fatal(err)
	}
	step(t, e, 0, AddLink{testLink(1)})
	step(t, e, 0, AddLink{testLink(2)})
	reachable(t, e, 1)
	reachable(t, e, 2)
	receiveMessages(t, e, 0, 1, wire.Message{Type: wire.Update, Prefix: prefix, RouterID: 2, Seqno: 7, Metric: 0, Interval: 1600})
	step(t, e, 100*time.Millisecond, Tick{})
	receiveMessages(t, e, 100*time.Millisecond, 2, wire.Message{Type: wire.Update, Prefix: prefix, RouterID: 2, Seqno: 8, Metric: 192, Interval: 1600})
	if e.selected[prefix].Seqno != 7 || !e.HasUsableOrigin(2, prefix, 8) {
		t.Fatalf("actual nonselected replacement not usable: %+v", e.Snapshot())
	}
	delete(allowed, 7)
	transition := step(t, e, 101*time.Millisecond, Tick{})
	if e.selected[prefix].Seqno != 8 || e.selected[prefix].Link != 2 || e.selected[prefix].Unreachable {
		t.Fatalf("replacement was not selected: %+v", e.Snapshot())
	}
	for _, r := range transition.Routes {
		if r.Prefix == prefix && r.Unreachable {
			t.Fatal("handoff exposed an intermediate unreachable FIB")
		}
	}
	if e.HasUsableOrigin(2, prefix, 7) || !e.HasUsableOrigin(2, prefix, 8) {
		t.Fatal("retired authorization remained usable")
	}
}
