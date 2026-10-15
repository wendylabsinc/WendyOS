package babel

import (
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/babel/internal/wire"
)

func newTestEngine(t testing.TB, id RouterID) *Engine {
	t.Helper()
	e, err := New(Config{RouterID: id, HelloInterval: time.Second, UpdateInterval: 4 * time.Second, IPv4ViaIPv6: true})
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func step(t testing.TB, e *Engine, now time.Duration, ev Event) Effects {
	t.Helper()
	fx, err := e.Step(now, ev)
	if err != nil {
		t.Fatal(err)
	}
	if fx.Revision != 0 {
		packets, err := e.Commit(fx.Revision, true)
		if err != nil {
			t.Fatal(err)
		}
		fx.Datagrams = packets.Datagrams
	}
	return fx
}
func testLink(id LinkID) Link {
	return Link{ID: id, Local: netip.MustParseAddr("fe80::1"), Peer: netip.MustParseAddr("fe80::2"), Cost: 96}
}
func receiveMessages(t testing.TB, e *Engine, now time.Duration, id LinkID, ms ...wire.Message) {
	t.Helper()
	for _, p := range wire.Pack(ms, 512) {
		step(t, e, now, Receive{id, p})
	}
}
func reachable(t testing.TB, e *Engine, id LinkID) {
	receiveMessages(t, e, 0, id, wire.Message{Type: wire.Hello, Interval: 100}, wire.Message{Type: wire.IHU, Metric: 96, Interval: 200})
}

func TestSerialArithmetic(t *testing.T) {
	for _, tc := range []struct {
		a, b  uint16
		newer bool
	}{{0, 65535, true}, {65535, 0, false}, {1, 1, false}, {32768, 0, false}, {0, 32768, false}, {32767, 0, true}} {
		if newer(tc.a, tc.b) != tc.newer {
			t.Fatal(tc)
		}
	}
	if sum(65534, 2) != Infinity || sum(1, 2) != 3 {
		t.Fatal("saturation")
	}
}
func TestBarrier(t *testing.T) {
	e := newTestEngine(t, 1)
	fx, err := e.Step(0, Originate{netip.MustParsePrefix("fd00::1/128"), 0})
	if err != nil || fx.Revision == 0 || len(fx.Datagrams) != 0 {
		t.Fatal(fx, err)
	}
	if _, err = e.Step(0, Tick{}); err != ErrPending {
		t.Fatal(err)
	}
	if _, err = e.Commit(fx.Revision+1, true); err != ErrRevision {
		t.Fatal(err)
	}
	if _, err = e.Commit(fx.Revision, false); err != ErrStopped {
		t.Fatal(err)
	}
	if _, err = e.Step(0, Tick{}); err != ErrStopped {
		t.Fatal(err)
	}
}
func TestScopedLinksAndStale(t *testing.T) {
	e := newTestEngine(t, 1)
	step(t, e, 0, AddLink{testLink(1)})
	step(t, e, 0, AddLink{testLink(2)})
	reachable(t, e, 1)
	if e.cost(e.links[2]) != Infinity {
		t.Fatal("scope leak")
	}
	step(t, e, 0, RemoveLink{1})
	if _, err := e.Step(0, AddLink{testLink(1)}); err != ErrConfig {
		t.Fatal(err)
	}
	step(t, e, 0, Receive{1, []byte{42, 2, 0, 0}})
	if e.Snapshot().Counters.Stale != 1 {
		t.Fatal("stale")
	}
}
func TestHoldDownAndFeasibility(t *testing.T) {
	e := newTestEngine(t, 1)
	step(t, e, 0, AddLink{testLink(1)})
	step(t, e, 0, AddLink{testLink(2)}) // advertise to another link (split horizon)
	reachable(t, e, 1)
	p := netip.MustParsePrefix("fd00::2/128")
	receiveMessages(t, e, 0, 1, wire.Message{Type: wire.Update, Prefix: p, RouterID: 2, Seqno: 42, Metric: 0, Interval: 400})
	step(t, e, 100*time.Millisecond, Tick{}) // advertise and establish FD
	s := e.sources[sourceKey{p, 2}]
	if s == nil || s.metric != 96 {
		t.Fatalf("FD %+v", s)
	}
	receiveMessages(t, e, 100*time.Millisecond, 1, wire.Message{Type: wire.Update, Prefix: p, RouterID: 2, Seqno: 42, Metric: 96, Interval: 400})
	r := e.selected[p]
	if !r.Unreachable || len(e.requests) == 0 {
		t.Fatalf("unsafe or no recovery: %+v", e.Snapshot())
	}
	receiveMessages(t, e, 100*time.Millisecond, 1, wire.Message{Type: wire.Update, Prefix: p, RouterID: 2, Seqno: 43, Metric: 96, Interval: 400})
	if r = e.selected[p]; r.Unreachable || r.Metric != 192 {
		t.Fatal(r)
	}
	receiveMessages(t, e, 100*time.Millisecond, 1, wire.Message{Type: wire.Update, Prefix: p, Metric: Infinity, Interval: 400})
	if !e.selected[p].Unreachable {
		t.Fatal("missing drop")
	}
}
func TestHelloExpiryAndUnscheduled(t *testing.T) {
	e := newTestEngine(t, 1)
	step(t, e, 0, AddLink{testLink(1)})
	reachable(t, e, 1)
	receiveMessages(t, e, 3*time.Second, 1, wire.Message{Type: wire.Hello, Seqno: 1, Interval: 0})
	step(t, e, 4*time.Second, Tick{})
	if e.cost(e.links[1]) != Infinity {
		t.Fatal("unscheduled hello renewed promise")
	}
}
func TestSeqRequestIncrementOnlyOne(t *testing.T) {
	e := newTestEngine(t, 1)
	step(t, e, 0, AddLink{testLink(1)})
	p := netip.MustParsePrefix("fd00::1/128")
	step(t, e, 0, Originate{p, 0})
	receiveMessages(t, e, 0, 1, wire.Message{Type: wire.SeqRequest, Prefix: p, RouterID: 1, Seqno: 100, HopCount: 64})
	if e.seq != 1 {
		t.Fatal(e.seq)
	}
}
func TestLimitsAndOwnership(t *testing.T) {
	e, err := New(Config{RouterID: 1, MaxLinks: 1})
	if err != nil {
		t.Fatal(err)
	}
	step(t, e, 0, AddLink{testLink(1)})
	if _, err = e.Step(0, AddLink{testLink(2)}); err != ErrLimit {
		t.Fatal(err)
	}
	s := e.Snapshot()
	s.Neighbours[0].Link = 99
	if e.Snapshot().Neighbours[0].Link != 1 {
		t.Fatal("alias")
	}
	if _, err = e.Step(-1, Tick{}); err != ErrTime {
		t.Fatal(err)
	}
}
func TestDeterministicReplay(t *testing.T) {
	a := newTestEngine(t, 1)
	b := newTestEngine(t, 1)
	for _, ev := range []Event{AddLink{testLink(1)}, AddLink{testLink(2)}, Originate{netip.MustParsePrefix("fd00::/64"), 0}, Tick{}, Withdraw{netip.MustParsePrefix("fd00::/64")}} {
		fa := step(t, a, 0, ev)
		fb := step(t, b, 0, ev)
		if !reflect.DeepEqual(fa, fb) {
			t.Fatal("different effects")
		}
	}
}

func TestErrata7373NoSelectedRoute(t *testing.T) {
	e := newTestEngine(t, 1)
	step(t, e, 0, AddLink{testLink(1)})
	reachable(t, e, 1)
	p := netip.MustParsePrefix("fd00::2/128")
	e.sources[sourceKey{p, 2}] = &source{seq: 7, metric: 96, expires: time.Minute}
	receiveMessages(t, e, 0, 1, wire.Message{Type: wire.Update, Prefix: p, RouterID: 2, Seqno: 7, Metric: 100, Interval: 400})
	q := e.requests[sourceKey{p, 2}]
	if q == nil || q.message.Seqno != 8 {
		t.Fatalf("no recovery from empty selection: %+v", q)
	}
}

func TestSequenceWrapRecovery(t *testing.T) {
	e := newTestEngine(t, 1)
	step(t, e, 0, AddLink{testLink(1)})
	reachable(t, e, 1)
	p := netip.MustParsePrefix("fd00::2/128")
	e.sources[sourceKey{p, 2}] = &source{seq: 65535, metric: 96, expires: time.Minute}
	receiveMessages(t, e, 0, 1, wire.Message{Type: wire.Update, Prefix: p, RouterID: 2, Seqno: 0, Metric: 200, Interval: 400})
	if r := e.selected[p]; r.Metric != 296 || r.Unreachable {
		t.Fatal(r)
	}
}

func TestUnknownPrefixRequestRetractsAndAcks(t *testing.T) {
	e := newTestEngine(t, 1)
	step(t, e, 0, AddLink{testLink(1)})
	ms := []wire.Message{{Type: wire.Request, Prefix: netip.MustParsePrefix("fd00::dead/128")}, {Type: wire.AckRequest, Opaque: 123, Interval: 100}}
	fx := step(t, e, 0, Receive{1, wire.Pack(ms, 512)[0]})
	var ack, retraction bool
	for _, p := range fx.Datagrams {
		got, err := wire.Decode(p.Payload, e.links[1].Local)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range got {
			ack = ack || (m.Type == wire.Ack && m.Opaque == 123)
			retraction = retraction || (m.Type == wire.Update && m.Metric == Infinity)
		}
	}
	if !ack || !retraction {
		t.Fatal(ack, retraction)
	}
}

func TestChurnResourceBound(t *testing.T) {
	e, err := New(Config{RouterID: 1, MaxRoutes: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"fd00::1/128", "fd00::2/128"} {
		p := netip.MustParsePrefix(s)
		step(t, e, 0, Originate{p, 0})
		step(t, e, 0, Withdraw{p})
	}
	if _, err = e.Step(0, Originate{netip.MustParsePrefix("fd00::3/128"), 0}); err != ErrLimit {
		t.Fatal("drop state not bounded", err)
	}
}

func TestCheckpointRestartSafety(t *testing.T) {
	e := newTestEngine(t, 1)
	step(t, e, 0, AddLink{testLink(1)})
	step(t, e, 0, AddLink{testLink(2)})
	reachable(t, e, 1)
	p := netip.MustParsePrefix("fd00::2/128")
	receiveMessages(t, e, 0, 1, wire.Message{Type: wire.Update, Prefix: p, RouterID: 2, Seqno: 7, Metric: 0, Interval: 400})
	step(t, e, 100*time.Millisecond, Tick{})
	state := e.Checkpoint()
	r, err := Restore(e.cfg, state)
	if err != nil {
		t.Fatal(err)
	}
	step(t, r, 0, AddLink{testLink(3)})
	reachable(t, r, 3)
	// A neighbour still pointing at the crashed relay advertises a worse metric.
	receiveMessages(t, r, 0, 3, wire.Message{Type: wire.Update, Prefix: p, RouterID: 2, Seqno: 7, Metric: 192, Interval: 400})
	if route := r.selected[p]; !route.Unreachable {
		t.Fatal("forgot safety history", route)
	}
	receiveMessages(t, r, 0, 3, wire.Message{Type: wire.Update, Prefix: p, RouterID: 2, Seqno: 8, Metric: 192, Interval: 400})
	if route := r.selected[p]; route.Unreachable || route.Metric != 288 {
		t.Fatal(route)
	}
	state.Sources[0].Metric = 999
	if e.Checkpoint().Sources[0].Metric == 999 {
		t.Fatal("checkpoint aliases engine")
	}
}

func TestSeqRequestForwardingHopAndDuplicate(t *testing.T) {
	e := newTestEngine(t, 1)
	for i := LinkID(1); i <= 3; i++ {
		step(t, e, 0, AddLink{testLink(i)})
		reachable(t, e, i)
	}
	p := netip.MustParsePrefix("fd00::9/128")
	receiveMessages(t, e, 0, 1, wire.Message{Type: wire.Update, Prefix: p, RouterID: 9, Seqno: 7, Metric: 0, Interval: 400})
	step(t, e, 100*time.Millisecond, Tick{})
	q := wire.Message{Type: wire.SeqRequest, Prefix: p, RouterID: 9, Seqno: 8, HopCount: 5}
	fx := step(t, e, 100*time.Millisecond, Receive{2, wire.Pack([]wire.Message{q}, 512)[0]})
	count := 0
	for _, d := range fx.Datagrams {
		ms, err := wire.Decode(d.Payload, e.links[d.Link].Local)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range ms {
			if m.Type == wire.SeqRequest {
				count++
				if d.Link != 1 || m.HopCount != 4 {
					t.Fatal(d.Link, m)
				}
			}
		}
	}
	if count != 1 {
		t.Fatal("forwarded count", count)
	}
	fx = step(t, e, 100*time.Millisecond, Receive{3, wire.Pack([]wire.Message{q}, 512)[0]})
	for _, d := range fx.Datagrams {
		ms, _ := wire.Decode(d.Payload, e.links[d.Link].Local)
		for _, m := range ms {
			if m.Type == wire.SeqRequest {
				t.Fatal("duplicate forwarded")
			}
		}
	}
	q.Seqno = 9
	q.HopCount = 1
	fx = step(t, e, 100*time.Millisecond, Receive{2, wire.Pack([]wire.Message{q}, 512)[0]})
	for _, d := range fx.Datagrams {
		ms, _ := wire.Decode(d.Payload, e.links[d.Link].Local)
		for _, m := range ms {
			if m.Type == wire.SeqRequest {
				t.Fatal("hop limit ignored")
			}
		}
	}
}

func TestExpiryAndSourceGC(t *testing.T) {
	e := newTestEngine(t, 1)
	for i := LinkID(1); i <= 2; i++ {
		step(t, e, 0, AddLink{testLink(i)})
		reachable(t, e, i)
	}
	p := netip.MustParsePrefix("fd00::9/128")
	receiveMessages(t, e, 0, 1, wire.Message{Type: wire.Update, Prefix: p, RouterID: 9, Seqno: 7, Metric: 0, Interval: 100})
	step(t, e, 100*time.Millisecond, Tick{})
	if _, ok := e.NextDeadline(); !ok {
		t.Fatal("no deadline")
	}
	step(t, e, 4*time.Second, Tick{})
	if !e.selected[p].Unreachable {
		t.Fatal("not expired")
	}
	step(t, e, 20*time.Second, Tick{})
	if len(e.routes) != 0 {
		t.Fatal("candidate not GCed")
	}
	step(t, e, 4*time.Minute, Tick{})
	if len(e.sources) != 0 || len(e.drops) != 0 {
		t.Fatal("history not GCed")
	}
}

func TestIPv4CapabilityAndReservedPrefixes(t *testing.T) {
	e, err := New(Config{RouterID: 1})
	if err != nil {
		t.Fatal(err)
	}
	step(t, e, 0, AddLink{testLink(1)})
	reachable(t, e, 1)
	receiveMessages(t, e, 0, 1, wire.Message{Type: wire.Update, Prefix: netip.MustParsePrefix("192.0.2.1/32"), RouterID: 2, Interval: 100, V4ViaV6: true})
	if len(e.routes) != 0 {
		t.Fatal("unsupported route selected")
	}
	for _, s := range []string{"fe80::/64", "ff00::/8", "127.0.0.1/32", "0.0.0.0/32", "::/128", "224.0.0.0/8"} {
		p := netip.MustParsePrefix(s)
		if _, err = e.Step(0, Originate{p, 0}); err != ErrConfig {
			t.Fatal(s, err)
		}
	}
	step(t, e, 0, Originate{netip.MustParsePrefix("::/0"), 0})
}

func TestRestoreCannotShortenSafetyHolds(t *testing.T) {
	e, err := New(Config{RouterID: 1, UpdateInterval: time.Minute, SourceGC: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	p := netip.MustParsePrefix("fd00::1/128")
	step(t, e, 0, Originate{p, 0})
	state := e.Checkpoint()
	r, err := Restore(Config{RouterID: 1}, state)
	if err != nil {
		t.Fatal(err)
	}
	step(t, r, 0, Tick{})
	step(t, r, time.Minute, Tick{})
	if !r.selected[p].Unreachable || r.cfg.SourceGC < 10*time.Minute {
		t.Fatal("shortened restart hold", r.cfg, r.selected)
	}
	// A second crash must preserve the first restore's longer outstanding hold.
	second := r.Checkpoint()
	if second.DropHold < 150*time.Second {
		t.Fatal("re-checkpoint shortened hold", second.DropHold)
	}
}
