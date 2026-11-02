package localmesh

import (
	"context"
	"encoding/binary"
	"io"
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// This actor fixture runs real Node/Synchronizer/Directory/Babel and the
// forwarding barrier with deterministic elapsed time and byte-exact datagrams.
// No Run, socket, TUN, netlink, device, broker or system clock is used.
type handoffActorNetwork struct {
	routes      []babel.Route
	watch       netip.Prefix
	interrupted [][]babel.Route
}

func (*handoffActorNetwork) ReconcileBase() error { return nil }
func (*handoffActorNetwork) AddPeer(babel.LinkID, meshid.Device) (io.ReadWriteCloser, error) {
	return handoffActorSink{}, nil
}
func (*handoffActorNetwork) RemovePeer(babel.LinkID) error { return nil }
func (n *handoffActorNetwork) Apply(r []babel.Route) error {
	n.routes = append([]babel.Route(nil), r...)
	if n.watch.IsValid() {
		finite := false
		for _, route := range r {
			if route.Prefix == n.watch && !route.Unreachable && !route.Local {
				finite = true
			}
		}
		if !finite {
			n.interrupted = append(n.interrupted, append([]babel.Route(nil), r...))
		}
	}
	return nil
}
func (*handoffActorNetwork) Stop() error  { return nil }
func (*handoffActorNetwork) Close() error { return nil }

type handoffActorSink struct{}

func (handoffActorSink) Read([]byte) (int, error)    { return 0, io.EOF }
func (handoffActorSink) Write(b []byte) (int, error) { return len(b), nil }
func (handoffActorSink) Close() error                { return nil }

type handoffActorLink struct {
	packets [][]byte
	closed  bool
}

func (l *handoffActorLink) SendDatagram(b []byte) error {
	l.packets = append(l.packets, append([]byte(nil), b...))
	return nil
}
func (*handoffActorLink) ReceiveDatagram(context.Context) ([]byte, error) { return nil, io.EOF }
func (*handoffActorLink) WriteControl(ControlMessage) error               { return nil }
func (*handoffActorLink) ReadControl() (ControlMessage, error)            { return ControlMessage{}, io.EOF }
func (l *handoffActorLink) Close() error                                  { l.closed = true; return nil }

type handoffActorFixture struct {
	t        *testing.T
	nodes    [2]*Node
	networks [2]*handoffActorNetwork
	links    [2]*handoffActorLink
	epoch    time.Time
	elapsed  time.Duration
}

func newHandoffActorFixture(t *testing.T) *handoffActorFixture {
	t.Helper()
	f := &handoffActorFixture{t: t, epoch: time.Now()}
	a, b := testCredentials(t)
	for i, c := range []*Credentials{a, b} {
		f.networks[i] = &handoffActorNetwork{}
		n, err := NewNodeWithNetwork(context.Background(), t.TempDir(), c, "route-handoff-actor", 50052, f.networks[i])
		if err != nil {
			t.Fatal(err)
		}
		f.nodes[i] = n
		n.routingClock = func() time.Duration { return f.elapsed }
		n.directory.monotonicNow = func() time.Time { return f.epoch.Add(f.elapsed) }
		t.Cleanup(func() { n.cancel(); _ = n.Stop() })
		f.step(i, babel.Originate{Prefix: netip.PrefixFrom(c.Device.MeshAddress(), 128)})
	}
	for i, n := range f.nodes {
		peer := f.nodes[1-i].Credentials
		if _, err := n.cache.Put(peer.Certificate.Certificate, f.now()); err != nil {
			t.Fatal(err)
		}
		l := &handoffActorLink{}
		f.links[i] = l
		if err := n.handle(nodeEvent{add: &nodePeer{device: peer.Device, cost: 1024, transport: "review", link: l, control: make(chan ControlMessage, 4096), done: make(chan struct{})}}); err != nil {
			t.Fatal(err)
		}
	}
	// Direct-message mode is an existing supported Synchronizer fixture path;
	// signature/trust admission stays real, but this test does not cover HAVE/WANT.
	for _, n := range f.nodes {
		n.peers[1].sync.Close()
		n.peers[1].sync = NewSynchronizer(n.directory, n.cache)
	}
	f.proof(0, f.ownProof(1))
	f.proof(1, f.ownProof(0))
	f.pump()
	f.advance(2 * time.Second)
	f.assertForwarded(true, 0)
	f.networks[1].watch = netip.PrefixFrom(f.nodes[0].Credentials.Device.MeshAddress(), 128)
	return f
}
func (f *handoffActorFixture) now() time.Time { return time.Now() }
func (f *handoffActorFixture) step(i int, e babel.Event) {
	f.t.Helper()
	if err := f.nodes[i].step(e); err != nil {
		f.t.Fatal(err)
	}
}
func (f *handoffActorFixture) proof(i int, w SignedManifest) {
	f.t.Helper()
	if err := f.nodes[i].handleControl(f.nodes[i].peers[1], &ControlMessage{Kind: "manifest", Manifest: &w}); err != nil {
		f.t.Fatal(err)
	}
}
func (f *handoffActorFixture) ownProof(i int) SignedManifest {
	f.t.Helper()
	n := f.nodes[i]
	for _, r := range n.directory.Records(f.now()) {
		if r.Principal == n.Credentials.Device.Principal() {
			return r
		}
	}
	f.t.Fatal("own signed proof missing")
	return SignedManifest{}
}
func (f *handoffActorFixture) pump() {
	f.t.Helper()
	for round := 0; round < 100; round++ {
		work := false
		for i, l := range f.links {
			packets := l.packets
			l.packets = nil
			for _, p := range packets {
				work = true
				if len(p) == 0 || p[0] != PacketBabel {
					f.t.Fatal("non Babel packet")
				}
				f.step(1-i, babel.Receive{ID: 1, Packet: p[1:]})
			}
		}
		if !work {
			return
		}
	}
	f.t.Fatal("packet exchange did not settle")
}
func (f *handoffActorFixture) advance(until time.Duration) {
	f.t.Helper()
	for f.elapsed < until {
		f.elapsed = min(until, f.elapsed+100*time.Millisecond)
		for i := range f.nodes {
			f.step(i, babel.Tick{})
		}
		f.pump()
	}
}
func (f *handoffActorFixture) assertForwarded(want bool, seq uint16) {
	f.t.Helper()
	prefix := netip.PrefixFrom(f.nodes[0].Credentials.Device.MeshAddress(), 128)
	for _, rs := range [][]babel.Route{f.nodes[1].routing.Snapshot().Routes, f.networks[1].routes, f.nodes[1].Snapshot().ForwardingRoutes} {
		got := false
		for _, r := range rs {
			if r.Prefix == prefix && !r.Unreachable && !r.Local {
				got = true
				if r.Seqno != seq {
					f.t.Fatalf("wrong seq:%d want%d", r.Seqno, seq)
				}
			}
		}
		if got != want {
			f.t.Fatalf("t=%v forwarded=%v want%v routes=%+v", f.elapsed, got, want, rs)
		}
	}
}
func (f *handoffActorFixture) lateProof() SignedManifest {
	f.t.Helper()
	origin := f.nodes[0]
	prefix := netip.PrefixFrom(origin.Credentials.Device.MeshAddress(), 128)
	before := f.nodes[1].routing.Snapshot().Counters.Rejected
	origin.nextPublication = time.Time{} // Publication coalescing is covered separately.
	f.step(0, babel.Receive{ID: 1, Packet: handoffActorSequenceRequest(prefix, babel.RouterID(origin.state.RouterID), 1)})
	// Deliver every new finite update and repeated trigger before its proof.
	f.pump()
	f.advance(4 * time.Second)
	f.assertForwarded(true, 0)
	if f.nodes[1].routing.Snapshot().Counters.Rejected <= before {
		f.t.Fatal("new independently ordered finite update was not rejected")
	}
	if origin.routing.Snapshot().Seqno != 1 {
		f.t.Fatal("fixture did not advance exactly once")
	}
	w := f.ownProof(0)
	m, err := DecodeManifest(w.Body)
	if err != nil || m.Route == nil || m.Route.Seqno != 1 {
		f.t.Fatal("new signed proof missing")
	}
	return w
}
func (f *handoffActorFixture) request() {
	prefix := f.nodes[0].Credentials.Device.MeshAddress().As16()
	packet := append([]byte{42, 2, 0, 20, 9, 18, 2, 128}, prefix[:]...)
	// RFC8966 Route Request TLV(9), not Seqno Request(10): no origin bump.
	if err := f.nodes[1].Send(babel.Datagram{Link: 1, Payload: packet}); err != nil {
		f.t.Fatal(err)
	}
	f.pump()
}

// The control proof and routing datagrams are independent carriers. Both orders
// must preserve a finite applied FIB until a real matching finite update arrives;
// neither a proof nor a route request may manufacture a candidate or sequence.
func TestNodeRouteHandoffBothCarrierOrdersStayFinite(t *testing.T) {
	for _, order := range []string{"proof-before-update", "update-before-proof"} {
		t.Run(order, func(t *testing.T) {
			f := newHandoffActorFixture(t)
			var proof SignedManifest
			if order == "update-before-proof" {
				proof = f.lateProof()
			} else {
				origin := f.nodes[0]
				origin.nextPublication = time.Time{}
				f.step(0, babel.Receive{ID: 1, Packet: handoffActorSequenceRequest(netip.PrefixFrom(origin.Credentials.Device.MeshAddress(), 128), babel.RouterID(origin.state.RouterID), 1)})
				proof = f.ownProof(0)
			}
			// No datagram delivery between real handleControl and inspecting all three
			// views: engine route, applied PacketNetwork FIB, immutable Node snapshot.
			f.proof(1, proof)
			f.assertForwarded(true, 0)
			current := 0
			for _, m := range f.nodes[1].Snapshot().Devices {
				if m.Device == f.nodes[0].Credentials.Device.UUID() {
					current++
					if m.Route == nil || m.Route.Seqno != 1 {
						t.Fatal("directory snapshot exported old body during handoff")
					}
				}
			}
			if current != 1 {
				t.Fatalf("current donor manifestcount=%d", current)
			}
			if order == "update-before-proof" {
				f.elapsed = 5 * time.Second
				f.step(1, babel.Tick{}) // Hold every carrier response behind this observation.
				f.assertForwarded(true, 0)
				f.request() // An actual prefix RouteRequest response supplies candidate1.
			} else {
				f.pump()
				f.advance(3 * time.Second)
			}
			f.assertForwarded(true, 1)
			f.advance(8 * time.Second) // Let bounded request coalescing expire.
			if len(f.networks[1].interrupted) != 0 {
				t.Fatalf("ordinary handoff applied unreachable FIB:%+v", f.networks[1].interrupted)
			}
			if got := f.nodes[0].routing.Snapshot().Seqno; got != 1 {
				t.Fatalf("handoff churned origin sequence:%d", got)
			}
			old := RouteOrigin{Prefix: netip.PrefixFrom(f.nodes[0].Credentials.Device.MeshAddress(), 128), RouterID: babel.RouterID(f.nodes[0].state.RouterID), Seqno: 0}
			if f.nodes[1].directory.RouteAuthorizations(f.now())[old] {
				t.Fatal("old tuple remained admitted after real usable new candidate")
			}
			if pending := f.nodes[1].routing.Snapshot().PendingRequests; pending != 0 {
				t.Fatalf("handoff left sequence requests:%d", pending)
			}
		})
	}
}

func TestNodeRouteHandoffRetainedProofCannotSurvivePeerLoss(t *testing.T) {
	f := newHandoffActorFixture(t)
	f.proof(1, f.lateProof())
	f.assertForwarded(true, 0)
	if err := f.nodes[1].handle(nodeEvent{id: 1, remove: true}); err != nil {
		t.Fatal(err)
	}
	f.assertForwarded(false, 0)
}

func TestNodeRouteHandoffWithdrawalDropsRetainedAndCurrent(t *testing.T) {
	f := newHandoffActorFixture(t)
	proof := f.lateProof()
	f.proof(1, proof)
	f.assertForwarded(true, 0)
	m, err := DecodeManifest(proof.Body)
	if err != nil {
		t.Fatal(err)
	}
	m.Revision++
	m.Withdraw = true
	origin := f.nodes[0].Credentials
	withdrawn, err := SignManifest(m, origin.Certificate.Certificate, origin.Signer, f.now())
	if err != nil {
		t.Fatal(err)
	}
	f.proof(1, withdrawn)
	f.assertForwarded(false, 0)
	f.request()
	f.assertForwarded(false, 0)
}

// Encode the real RFC8966 Seqno Request TLV; public packet processing still
// decides whether/when the origin publishes its next exact route proof.
func handoffActorSequenceRequest(prefix netip.Prefix, id babel.RouterID, seq uint16) []byte {
	packet := make([]byte, 36)
	copy(packet, []byte{42, 2, 0, 32, 10, 30, 2, 128})
	binary.BigEndian.PutUint16(packet[8:10], seq)
	packet[10] = 64
	binary.BigEndian.PutUint64(packet[12:20], uint64(id))
	addr := prefix.Addr().As16()
	copy(packet[20:], addr[:])
	return packet
}
