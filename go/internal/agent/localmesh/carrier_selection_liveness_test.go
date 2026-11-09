package localmesh

import (
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

// Real signed Directory admission, Babel packets and applied forwarding feed
// Node.refresh. Keeping both peer objects attached reproduces silent LAN loss
// without substituting a socket-close event for Babel expiry.
func carrierSelectionNetwork(t *testing.T, signed bool) (*signedRouteTestNetwork, *Node, *Credentials) {
	t.Helper()
	n := newSignedRouteTestNetwork(t, 2, func(c *babel.Config) { c.UpdateInterval = 16 * time.Second })
	// Node.refresh consults real wall time. Use a real currently valid test CA
	// and admission dates, while the Directory's elapsed lease and Babel timers
	// follow this deterministic network clock. The generic network's fixed
	// January 2027 cache fixture cannot be mixed with a live Node wall clock.
	n.epoch = time.Now()
	signer := newManifestTestAuthority(t, n.epoch).issue(t, n.epoch, nil, n.epoch.Add(time.Hour))
	for _, node := range n.nodes {
		cache, err := OpenIdentityCache("", DefaultCacheLimits(), signer.Verify, n.epoch)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cache.Put(signer.Certificate.Certificate, n.epoch); err != nil {
			t.Fatal(err)
		}
		node.directory, err = NewDirectoryWithClock(signer.Device.Tenant(), 10, cache, n.now)
		if err != nil {
			t.Fatal(err)
		}
	}
	for id := babel.LinkID(1); id <= 2; id++ {
		left, right := testLinks(n.nodes[0].device.device, n.nodes[1].device.device)
		left.ID, right.ID = id, id
		left.Cost, right.Cost = 128, 128
		if id == 2 {
			left.Cost, right.Cost = 4096, 4096
			left.HelloInterval, right.HelloInterval = babel.DefaultHelloInterval, babel.DefaultHelloInterval
		}
		n.nodes[0].peers[id], n.nodes[1].peers[id] = 1, 0
		n.step(0, babel.AddLink{Link: left})
		n.step(1, babel.AddLink{Link: right})
		n.deliver()
	}
	if signed {
		n.accept(1, carrierSelectionProof(n, signer))
	}
	n.step(0, babel.Originate{Prefix: n.nodes[0].device.prefix})
	n.advance(3 * time.Second)
	peer := n.nodes[0].device.device
	receiver := n.nodes[1]
	runtime := &Node{Credentials: &Credentials{Device: receiver.device.device}, directory: receiver.directory,
		routing: receiver.routing, peers: map[babel.LinkID]*nodePeer{
			1: {id: 1, device: peer, cost: 128},
			2: {id: 2, device: peer, cost: 4096},
		}}
	return n, runtime, signer
}

func carrierSelectionProof(n *signedRouteTestNetwork, signer *Credentials) SignedManifest {
	n.t.Helper()
	origin := n.nodes[0]
	manifest := routeManifest(signer.Device, n.now())
	manifest.Expires = n.now().Add(time.Minute).UnixMilli()
	manifest.Route = &RouteOrigin{Prefix: origin.device.prefix, RouterID: origin.device.routerID, Seqno: origin.routing.Snapshot().Seqno}
	proof, err := SignManifest(manifest, signer.Certificate.Certificate, signer.Signer, n.now())
	if err != nil {
		n.t.Fatal(err)
	}
	return proof
}

func refreshCarrierSelection(n *signedRouteTestNetwork, runtime *Node) {
	runtime.forwardRoutes = append([]babel.Route(nil), n.nodes[1].io.routes...)
	runtime.refresh()
}

func TestCarrierSelectionSilentLANDoesNotVetoBLE(t *testing.T) {
	n, runtime, _ := carrierSelectionNetwork(t, true)
	peer := n.nodes[0].device.device
	policy := NewPeerSelection(runtime.Snapshot)
	check := func(wantCheaper bool) {
		t.Helper()
		refreshCarrierSelection(n, runtime)
		if got := runtime.HasCheaperLink(peer, 4096); got != wantCheaper {
			t.Fatalf("cheaper LAN=%v want=%v at %s", got, wantCheaper, n.elapsed)
		}
		if got := policy.AllowRadio(peer, RadioBLE); got == wantCheaper {
			t.Fatalf("BLE allowed=%v with cheaper LAN=%v at %s", got, wantCheaper, n.elapsed)
		}
		if got := policy.HasLAN(peer); got != wantCheaper {
			t.Fatalf("usable LAN=%v want=%v", got, wantCheaper)
		}
	}
	check(true) // Healthy cheaper forwarding still intentionally sheds BLE.
	for n.elapsed < 8*time.Second {
		n.elapsed += 100 * time.Millisecond
		for i := range n.nodes {
			n.step(i, babel.Tick{})
		}
		deliverCarrierSelectionExcept(n, 1)
	}
	check(false)
	n.assertForwarded(1, true, 0)
	if len(runtime.Snapshot().Links) != 2 {
		t.Fatal("fixture removed a carrier instead of detecting silent loss")
	}
	for _, route := range n.nodes[1].io.routes {
		if route.Prefix == n.nodes[0].device.prefix && !route.Unreachable && route.Link != 2 {
			t.Fatalf("surviving BLE did not carry the installed peer route: %+v", route)
		}
	}
	// Unauthenticated discovery cannot turn the stale LAN back into a veto.
	policy.Seen(peer, RadioNAN)
	policy.Seen(selectionDevice(50), RadioBLE)
	check(false)
	// Normal Babel traffic independently restores the cheaper route while BLE
	// remains useful; no retirement/reconnect is needed to prime that route.
	n.advance(12 * time.Second)
	check(true)
	for i := 0; i < 5; i++ {
		n.advance(n.elapsed + time.Second)
		check(true)
	}
}

func TestCarrierSelectionRequiresSignedDirectForwarding(t *testing.T) {
	n, runtime, signer := carrierSelectionNetwork(t, false)
	refreshCarrierSelection(n, runtime)
	peer := n.nodes[0].device.device
	policy := NewPeerSelection(runtime.Snapshot)
	policy.Seen(peer, RadioNAN)
	if runtime.HasCheaperLink(peer, 4096) || policy.HasLAN(peer) || !policy.AllowRadio(peer, RadioBLE) {
		t.Fatal("attached Babel neighbour without authorized forwarding displaced BLE")
	}
	n.accept(1, carrierSelectionProof(n, signer))
	n.advance(20 * time.Second)
	refreshCarrierSelection(n, runtime)
	if !runtime.HasCheaperLink(peer, 4096) || policy.AllowRadio(peer, RadioBLE) {
		t.Fatal("verified and installed cheaper route never became eligible")
	}
}

func deliverCarrierSelectionExcept(n *signedRouteTestNetwork, failed babel.LinkID) {
	n.t.Helper()
	for round := 0; round < 100; round++ {
		delivered := false
		for _, node := range n.nodes {
			packets := node.io.sent
			node.io.sent = nil
			for _, packet := range packets {
				if packet.Link == failed {
					continue
				}
				delivered = true
				n.step(node.peers[packet.Link], babel.Receive{ID: packet.Link, Packet: packet.Payload})
			}
		}
		if !delivered {
			return
		}
	}
	n.t.Fatal("carrier packet exchange failed to settle")
}
