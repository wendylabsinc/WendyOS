package localmesh

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

// A and B have a fully primed LAN and a warm BLE. C is then learned through A
// while only the A->B LAN update is delayed. The same peer can therefore be
// directly reachable on LAN while an installed transit destination still uses
// BLE. Every candidate and proof comes from actual Nodes, not a trusted-map or
// FIB substitution.
type carrierTransitFixture struct {
	*carrierStandbyFixture
	third       *Node
	thirdLink   *handoffActorLink
	heldLAN     [][]byte
	thirdPrefix netip.Prefix
}

func newCarrierTransitFixture(t *testing.T) *carrierTransitFixture {
	t.Helper()
	f := &carrierTransitFixture{carrierStandbyFixture: newCarrierStandbyFixture(t)}
	f.addCarrier(128, "infrastructure-wifi")
	f.pump(0)
	f.advance(6*time.Second, 0)
	f.assertSelected(2)
	credentials := testCredentialsFor(t, testPrincipal(testTenantUUID, testDeviceCID))
	third := newActorTestNode(t, credentials, &handoffActorNetwork{})
	f.third = third
	third.routingClock = func() time.Duration { return f.elapsed }
	third.directory.monotonicNow = func() time.Time { return f.epoch.Add(f.elapsed) }
	t.Cleanup(func() { third.cancel(); _ = third.Stop() })
	f.thirdPrefix = netip.PrefixFrom(credentials.Device.MeshAddress(), 128)
	if err := third.step(babel.Originate{Prefix: f.thirdPrefix}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []*Node{f.nodes[0], f.nodes[1], third} {
		for _, peer := range []*Node{f.nodes[0], f.nodes[1], third} {
			if _, err := n.cache.Put(peer.Credentials.Certificate.Certificate, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
	}
	linkToC, linkToA := &handoffActorLink{}, &handoffActorLink{}
	for _, add := range []struct {
		n    *Node
		peer *Node
		link *handoffActorLink
	}{{f.nodes[0], third, linkToC}, {third, f.nodes[0], linkToA}} {
		p := &nodePeer{device: add.peer.Credentials.Device, cost: 128, transport: "infrastructure-wifi", link: add.link,
			control: make(chan ControlMessage, 4096), done: make(chan struct{}), retire: make(chan struct{})}
		if err := add.n.handle(nodeEvent{add: p}); err != nil {
			t.Fatal(err)
		}
	}
	f.links[0][3], f.thirdLink = linkToC, linkToA
	third.peers[1].sync.Close()
	third.peers[1].sync = NewSynchronizer(third.directory, third.cache)
	proofC := carrierTransitOwnProof(t, third)
	for _, target := range []*Node{f.nodes[0], f.nodes[1]} {
		if err := target.handleControl(target.peers[1], &ControlMessage{Kind: "manifest", Manifest: &proofC}); err != nil {
			t.Fatal(err)
		}
	}
	proofA := carrierTransitOwnProof(t, f.nodes[0])
	if err := third.handleControl(third.peers[1], &ControlMessage{Kind: "manifest", Manifest: &proofA}); err != nil {
		t.Fatal(err)
	}
	for until := f.elapsed + 3*time.Second; f.elapsed < until && !f.transitOn(1); {
		f.advanceTransit(f.elapsed+100*time.Millisecond, true)
	}
	f.assertTransit(1)
	if !f.nodes[1].HasCheaperLink(f.nodes[0].Credentials.Device, 4096) {
		t.Fatal("fixture lost the already installed LAN route to direct peer A")
	}
	if len(f.heldLAN) == 0 {
		t.Fatal("fixture delayed no actual LAN datagrams")
	}
	return f
}

func carrierTransitOwnProof(t *testing.T, n *Node) SignedManifest {
	t.Helper()
	for _, record := range n.directory.Records(time.Now()) {
		if record.Principal == n.Credentials.Device.Principal() {
			return record
		}
	}
	t.Fatal("actual signed own proof missing")
	return SignedManifest{}
}

func (f *carrierTransitFixture) pumpTransit(holdLAN bool) {
	f.t.Helper()
	for round := 0; round < 100; round++ {
		work := false
		deliver := func(target *Node, id babel.LinkID, packets [][]byte) {
			for _, packet := range packets {
				work = true
				if len(packet) == 0 || packet[0] != PacketBabel {
					f.t.Fatal("non-Babel transit datagram")
				}
				if err := target.handle(nodeEvent{id: id, packet: append([]byte(nil), packet[1:]...)}); err != nil {
					f.t.Fatal(err)
				}
			}
		}
		for i := range f.nodes {
			for id, link := range f.links[i] {
				packets := link.packets
				link.packets = nil
				if i == 0 && id == 2 && holdLAN {
					f.heldLAN = append(f.heldLAN, packets...)
					continue
				}
				if i == 0 && id == 3 {
					deliver(f.third, 1, packets)
				} else {
					deliver(f.nodes[1-i], id, packets)
				}
			}
		}
		packets := f.thirdLink.packets
		f.thirdLink.packets = nil
		deliver(f.nodes[0], 3, packets)
		if !work {
			return
		}
	}
	f.t.Fatal("transit packet exchange did not settle")
}

func (f *carrierTransitFixture) advanceTransit(until time.Duration, holdLAN bool) {
	f.t.Helper()
	for f.elapsed < until {
		f.elapsed = min(until, f.elapsed+100*time.Millisecond)
		for _, n := range []*Node{f.nodes[0], f.nodes[1], f.third} {
			if err := n.step(babel.Tick{}); err != nil {
				f.t.Fatal(err)
			}
		}
		f.pumpTransit(holdLAN)
	}
}

func (f *carrierTransitFixture) transitOn(id babel.LinkID) bool {
	for _, route := range f.nodes[1].Snapshot().ForwardingRoutes {
		if route.Prefix == f.thirdPrefix && !route.Local && !route.Unreachable && route.Metric != babel.Infinity {
			return route.Link == id
		}
	}
	return false
}

func (f *carrierTransitFixture) assertTransit(id babel.LinkID) {
	f.t.Helper()
	if !f.transitOn(id) {
		f.t.Fatalf("t=%s transit destination C not installed via link=%d: %+v", f.elapsed, id, f.nodes[1].Snapshot().ForwardingRoutes)
	}
}

func (f *carrierTransitFixture) releaseLANTransit() {
	f.t.Helper()
	for _, packet := range f.heldLAN {
		if err := f.nodes[1].handle(nodeEvent{id: 2, packet: append([]byte(nil), packet[1:]...)}); err != nil {
			f.t.Fatal(err)
		}
	}
	f.heldLAN = nil
	f.pumpTransit(false)
	for until := f.elapsed + time.Second; f.elapsed < until && !f.transitOn(2); {
		f.advanceTransit(f.elapsed+100*time.Millisecond, false)
	}
	f.assertTransit(2)
}

func TestCarrierCapacityKeepsBLEUntilTransitRoutesMoveToLAN(t *testing.T) {
	for _, owner := range []string{"provider-budget", "node-64-budget"} {
		t.Run(owner, func(t *testing.T) {
			f := newCarrierTransitFixture(t)
			n := f.nodes[1]
			ble, lan := n.peers[1], n.peers[2]
			next := selectionDevice(999)
			if owner == "provider-budget" {
				p := NewConnectionPolicy(n.Credentials.Device, true, ConnectionLimits{Outgoing: 1, Attempts: 1, Total: 1})
				p.SetSnapshot(n.Snapshot)
				lease := p.Reserve("ble", ble.device, true)
				defer lease.Release()
				if !lease.Ready(ble.device, func() { t.Fatal("capacity used physical Close instead of owned signal") }) {
					t.Fatal("verified provider ownership missing")
				}
				signals := 0
				BindConnectionLink(WithConnectionLease(context.Background(), lease), ble.id, ble.cost, func() { signals++ })
				advertised := p.OutgoingDiscoveryAvailable()
				if p.ReservePreferred("new", next, true, "", true) != nil || lease.Retired() || signals != 0 || advertised {
					t.Fatal("provider reclaimed or advertised BLE capacity while installed transit destination C still depended on it")
				}
				f.releaseLANTransit()
				if !p.OutgoingDiscoveryAvailable() {
					t.Fatal("fully migrated transit routes left safe redundancy unavailable")
				}
				if p.ReservePreferred("new", next, true, "", true) != nil || !lease.Retired() || signals != 1 {
					t.Fatal("provider did not reclaim exact BLE after all installed routes moved")
				}
			} else {
				fillCarrierNodeCapacity(n)
				add := &nodePeer{device: next, cost: 128, transport: "infrastructure-wifi", link: &handoffActorLink{},
					control: make(chan ControlMessage, 128), done: make(chan struct{}), retire: make(chan struct{})}
				if err := n.handle(nodeEvent{add: add}); err == nil || errors.Is(err, ErrCarrierCapacityRetry) || ble.retiring() || lan.retiring() || len(n.peers) != 64 {
					t.Fatal("global Node budget reclaimed BLE while installed transit destination C still depended on it")
				}
				f.releaseLANTransit()
				if err := n.handle(nodeEvent{add: add}); !errors.Is(err, ErrCarrierCapacityRetry) || !ble.retiring() || lan.retiring() || len(n.peers) != 64 {
					t.Fatal("global Node budget did not safely defer for exact BLE cleanup after transit migration")
				}
			}
		})
	}
}
