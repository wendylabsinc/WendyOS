package localmesh

import (
	"crypto/ed25519"
	"encoding/binary"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

// These tests run the real Directory signature verifier, Babel engine and
// forwarding barrier. The fake I/O records the FIB and transfers actual Babel
// packets between engines; no test constructs a trusted RouteOrigin map by hand.
type signedRouteTestNode struct {
	device    meshDevice
	routing   *Routing
	io        *routingRecorder
	directory *Directory
	origins   map[RouteOrigin]bool
	peers     map[babel.LinkID]int
}

type signedRouteTestNetwork struct {
	t       *testing.T
	nodes   []*signedRouteTestNode
	epoch   time.Time
	elapsed time.Duration
	chain   [][]byte
	key     ed25519.PrivateKey
}

func newSignedRouteTestNetwork(t *testing.T, count int, configure ...func(*babel.Config)) *signedRouteTestNetwork {
	t.Helper()
	directory, chain, key, _, now := directoryFixture(t)
	n := &signedRouteTestNetwork{t: t, epoch: now, chain: chain, key: key}
	for i, id := range []string{testDeviceAID, testDeviceBID, testDeviceCID}[:count] {
		cache, err := OpenIdentityCache("", DefaultCacheLimits(), directory.cache.verify, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = cache.Put(chain, now); err != nil {
			t.Fatal(err)
		}
		d, err := NewDirectory(testTenant(t), 10, cache)
		if err != nil {
			t.Fatal(err)
		}
		node := &signedRouteTestNode{
			device: newMeshDevice(t, testPrincipal(testTenantUUID, id), byte(i+1)),
			io:     &routingRecorder{}, directory: d, peers: make(map[babel.LinkID]int),
		}
		config := genericMeshConfig(node.device)
		for _, apply := range configure {
			apply(&config)
		}
		config.AcceptRouteUpdate = func(id babel.RouterID, p netip.Prefix, seq uint16) bool {
			return node.origins[RouteOrigin{Prefix: p, RouterID: id, Seqno: seq}]
		}
		node.routing, err = NewRouting(config, nil, node.io)
		if err != nil {
			t.Fatal(err)
		}
		n.nodes = append(n.nodes, node)
	}
	return n
}

// Encode the RFC 8966 Seqno Request TLV to exercise the public routing adapter.
// The production wire codec is internal to the separate Babel module.
func routeSequenceRequest(prefix netip.Prefix, id babel.RouterID, seq uint16) []byte {
	packet := make([]byte, 36)
	copy(packet, []byte{42, 2, 0, 32, 10, 30, 2, 128})
	binary.BigEndian.PutUint16(packet[8:10], seq)
	packet[10] = 64
	binary.BigEndian.PutUint64(packet[12:20], uint64(id))
	address := prefix.Addr().As16()
	copy(packet[20:], address[:])
	return packet
}

func TestSignedRouteSequenceProofAheadOfUpdateConverges(t *testing.T) {
	n := newSignedRouteTestNetwork(t, 3, func(config *babel.Config) {
		config.UpdateInterval = 4 * time.Second
	})
	n.connect(0, 1, 1)
	n.connect(0, 2, 2)
	proof := n.sign(1, time.Minute, false)
	n.accept(1, proof)
	n.accept(2, proof)
	n.step(0, babel.Originate{Prefix: n.nodes[0].device.prefix})
	n.advance(2 * time.Second)
	n.assertForwarded(1, true, 0)
	n.assertForwarded(2, true, 0)

	origin := n.nodes[0]
	n.step(0, babel.Receive{ID: 1, Packet: routeSequenceRequest(origin.device.prefix, origin.device.routerID, 1)})
	if seq := origin.routing.Snapshot().Seqno; seq != 1 {
		t.Fatalf("request did not advance origin sequence: %d", seq)
	}
	// Node.Persist gossips the new signed proof to every peer. Deliver it
	// before the independent Babel datagrams, including to the passive peer.
	proof = n.sign(2, time.Minute, false)
	for _, index := range []int{1, 2} {
		n.accept(index, proof)
		n.step(index, babel.Tick{})
		n.assertForwarded(index, false, 0)
	}
	n.deliver()
	n.advance(2200 * time.Millisecond)
	n.assertForwarded(1, true, 1)
	n.assertForwarded(2, true, 1)
	// Recovery must settle without requesting further origin sequences.
	n.advance(5 * time.Second)
	if seq := origin.routing.Snapshot().Seqno; seq != 1 {
		t.Fatalf("proof/update reordering caused sequence churn: %d", seq)
	}
	for _, index := range []int{1, 2} {
		n.assertForwarded(index, true, 1)
		if requests := n.nodes[index].routing.Snapshot().PendingRequests; requests != 0 {
			t.Fatalf("node %d retained %d sequence requests", index, requests)
		}
	}
}

func TestSignedRouteSequenceUpdateAheadOfProofRecovers(t *testing.T) {
	n := newSignedRouteTestNetwork(t, 3, func(config *babel.Config) {
		config.UpdateInterval = 4 * time.Second
	})
	n.connect(0, 1, 1)
	n.connect(0, 2, 2)
	proof := n.sign(1, time.Minute, false)
	n.accept(1, proof)
	n.accept(2, proof)
	n.step(0, babel.Originate{Prefix: n.nodes[0].device.prefix})
	n.advance(2 * time.Second)
	origin := n.nodes[0]
	rejected := []uint64{0, n.nodes[1].routing.Snapshot().Counters.Rejected, n.nodes[2].routing.Snapshot().Counters.Rejected}
	n.step(0, babel.Receive{ID: 1, Packet: routeSequenceRequest(origin.device.prefix, origin.device.routerID, 1)})
	// The reliable proof stream and Babel datagrams are independently ordered.
	// Deliver both the explicit response and fanout while only sequence zero
	// is authorized; neither neighbour may install the unverified sequence.
	n.advance(2200 * time.Millisecond)
	for _, index := range []int{1, 2} {
		n.assertForwarded(index, true, 0)
		if n.nodes[index].routing.Snapshot().Counters.Rejected <= rejected[index] {
			t.Fatalf("node %d did not reject the update ahead of its proof", index)
		}
	}
	proof = n.sign(2, time.Minute, false)
	for _, index := range []int{1, 2} {
		n.accept(index, proof)
		n.step(index, babel.Tick{})
		n.assertForwarded(index, false, 0)
	}
	// Neither leaf has advertised this route onward, so it has no feasibility
	// history from which to solicit a sequence. The periodic origin update
	// must repair the lost opportunity without weakening proof admission.
	n.deliver()
	n.advance(5 * time.Second)
	for _, index := range []int{1, 2} {
		n.assertForwarded(index, true, 1)
	}
	n.advance(8 * time.Second)
	if seq := origin.routing.Snapshot().Seqno; seq != 1 {
		t.Fatalf("late proof caused sequence churn: %d", seq)
	}
	for _, index := range []int{1, 2} {
		n.assertForwarded(index, true, 1)
		if requests := n.nodes[index].routing.Snapshot().PendingRequests; requests != 0 {
			t.Fatalf("node %d retained %d sequence requests", index, requests)
		}
	}
}

func (n *signedRouteTestNetwork) now() time.Time { return n.epoch.Add(n.elapsed) }

func (n *signedRouteTestNetwork) step(index int, event babel.Event) {
	n.t.Helper()
	node := n.nodes[index]
	node.origins = node.directory.RouteAuthorizations(n.now())
	if err := node.routing.Step(n.elapsed, event); err != nil {
		n.t.Fatal(err)
	}
}

func (n *signedRouteTestNetwork) connect(a, b int, id babel.LinkID) {
	n.t.Helper()
	left, right := testLinks(n.nodes[a].device.device, n.nodes[b].device.device)
	left.ID, right.ID = id, id
	n.nodes[a].peers[id], n.nodes[b].peers[id] = b, a
	n.step(a, babel.AddLink{Link: left})
	n.step(b, babel.AddLink{Link: right})
	n.deliver()
}

func (n *signedRouteTestNetwork) deliver() {
	n.t.Helper()
	for round := 0; round < 100; round++ {
		delivered := false
		for _, node := range n.nodes {
			packets := node.io.sent
			node.io.sent = nil
			for _, packet := range packets {
				delivered = true
				n.step(node.peers[packet.Link], babel.Receive{ID: packet.Link, Packet: packet.Payload})
			}
		}
		if !delivered {
			return
		}
	}
	n.t.Fatal("Babel packet exchange failed to settle")
}

func (n *signedRouteTestNetwork) advance(until time.Duration) {
	n.t.Helper()
	for n.elapsed < until {
		n.elapsed = min(until, n.elapsed+100*time.Millisecond)
		for i := range n.nodes {
			n.step(i, babel.Tick{})
		}
		n.deliver()
	}
}

func (n *signedRouteTestNetwork) sign(revision uint64, lifetime time.Duration, withdraw bool) SignedManifest {
	n.t.Helper()
	origin := n.nodes[0]
	manifest := Manifest{
		Version: ManifestVersionRoute, Device: origin.device.device.UUID(), Revision: revision,
		Issued: n.now().UnixMilli(), Expires: n.now().Add(lifetime).UnixMilli(), AgentPort: 50052,
		Withdraw: withdraw,
		Route:    &RouteOrigin{Prefix: origin.device.prefix, RouterID: origin.device.routerID, Seqno: origin.routing.Snapshot().Seqno},
	}
	proof, err := SignManifest(manifest, n.chain, n.key, n.now())
	if err != nil {
		n.t.Fatal(err)
	}
	return proof
}

func (n *signedRouteTestNetwork) accept(index int, proof SignedManifest) {
	n.t.Helper()
	if changed, err := n.nodes[index].directory.Accept(proof, n.now()); err != nil || !changed {
		n.t.Fatalf("accept signed route: changed=%v err=%v", changed, err)
	}
}

func (n *signedRouteTestNetwork) assertForwarded(index int, want bool, seq uint16) {
	n.t.Helper()
	node := n.nodes[index]
	for _, routes := range [][]babel.Route{node.io.routes, node.routing.Snapshot().Routes} {
		forwarded := false
		for _, route := range routes {
			if route.Prefix == n.nodes[0].device.prefix && !route.Unreachable && !route.Local {
				forwarded = true
				if route.RouterID != n.nodes[0].device.routerID || route.Seqno != seq {
					n.t.Fatalf("forwarded wrong origin or sequence: %+v", route)
				}
			}
		}
		if forwarded != want {
			n.t.Fatalf("node %d forwarded=%v want=%v at %s: %+v", index, forwarded, want, n.elapsed, routes)
		}
	}
}

func TestSignedRouteRoutingRequiresProofAndExpiresWithoutPackets(t *testing.T) {
	n := newSignedRouteTestNetwork(t, 3)
	n.connect(0, 1, 1)
	n.connect(1, 2, 2) // Advertising onward establishes Babel feasibility history.
	n.step(0, babel.Originate{Prefix: n.nodes[0].device.prefix})
	n.advance(3 * time.Second)
	n.assertForwarded(1, false, 0)
	if n.nodes[1].routing.Snapshot().Counters.Rejected == 0 {
		t.Fatal("unsigned routing advertisements were not rejected")
	}

	proof := n.sign(1, 6*time.Second, false)
	forged := proof
	var manifest Manifest
	if err := netwire.Unmarshal(proof.Body, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Route.Seqno++
	forged.Body, _ = netwire.Marshal(manifest)
	if _, err := n.nodes[1].directory.Accept(forged, n.now()); err == nil {
		t.Fatal("changed sequence retained valid authorization")
	}
	n.accept(1, proof)
	n.advance(5 * time.Second) // A periodic update retries the initially denied route.
	n.assertForwarded(1, true, 0)
	n.advance(8900 * time.Millisecond)
	n.assertForwarded(1, true, 0)

	// No packet is delivered at expiry. The Directory snapshot alone must cause
	// the next Tick to remove the route from the applied forwarding table.
	n.elapsed = 9 * time.Second
	n.step(1, babel.Tick{})
	n.assertForwarded(1, false, 0)
	if _, err := n.nodes[1].directory.Accept(proof, n.now()); err == nil {
		t.Fatal("replay revived expired proof")
	}
	n.step(1, babel.Tick{})
	n.assertForwarded(1, false, 0)

	// Babel requests a new origin sequence while the proof is absent. A fresh
	// signed lease must bind that exact sequence before recovery can succeed.
	n.deliver()
	n.advance(10 * time.Second)
	seq := n.nodes[0].routing.Snapshot().Seqno
	if seq == 0 {
		t.Fatal("test did not exercise sequence recovery")
	}
	n.assertForwarded(1, false, 0)
	n.accept(1, n.sign(2, 20*time.Second, false))
	n.advance(12 * time.Second)
	n.assertForwarded(1, true, seq)
}

func TestSignedRouteRoutingRelaysProofWithoutTransferringAuthority(t *testing.T) {
	n := newSignedRouteTestNetwork(t, 3)
	n.connect(0, 1, 1)
	n.connect(1, 2, 2)
	proof := n.sign(1, time.Minute, false)
	// Both routers independently check the destination's original signature.
	n.accept(1, proof)
	n.accept(2, proof)
	n.step(0, babel.Originate{Prefix: n.nodes[0].device.prefix})
	n.advance(4 * time.Second)
	n.assertForwarded(1, true, 0)
	n.assertForwarded(2, true, 0)
	for _, route := range n.nodes[2].io.routes {
		if route.Prefix == n.nodes[0].device.prefix && (route.Link != 2 || route.Metric != 512) {
			t.Fatalf("route did not traverse the relay: %+v", route)
		}
	}

	// The relay can forward the signed origin, but its own router ID has no
	// authority to originate that same destination, even on its admitted link.
	rejected := n.nodes[2].routing.Snapshot().Counters.Rejected
	n.step(1, babel.Originate{Prefix: n.nodes[0].device.prefix})
	n.advance(4200 * time.Millisecond)
	if n.nodes[2].routing.Snapshot().Counters.Rejected <= rejected {
		t.Fatal("relay's forged origin was not rejected")
	}
	n.assertForwarded(2, true, 0)

	withdrawal := n.sign(2, time.Minute, true)
	n.accept(2, withdrawal)
	n.step(2, babel.Tick{})
	n.assertForwarded(2, false, 0)
	if _, err := n.nodes[2].directory.Accept(proof, n.now()); err == nil {
		t.Fatal("replay overrode signed withdrawal")
	}
	n.step(2, babel.Tick{})
	n.assertForwarded(2, false, 0)
}
