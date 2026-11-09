package localmesh

import (
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

// This fixture uses real Node actor inputs, certificate-backed Directory proof
// admission, Babel packets, and the applied forwarding barrier. Only transport
// bytes and deterministic elapsed time are supplied by the test.
type carrierStandbyFixture struct {
	t        *testing.T
	nodes    [2]*Node
	networks [2]*handoffActorNetwork
	links    [2]map[babel.LinkID]*handoffActorLink
	elapsed  time.Duration
	epoch    time.Time
}

func newCarrierStandbyFixture(t *testing.T) *carrierStandbyFixture {
	t.Helper()
	f := &carrierStandbyFixture{t: t, epoch: time.Now()}
	a, b := testCredentials(t)
	for i, credentials := range []*Credentials{a, b} {
		f.networks[i] = &handoffActorNetwork{}
		n := newActorTestNode(t, credentials, f.networks[i])
		f.nodes[i] = n
		f.links[i] = make(map[babel.LinkID]*handoffActorLink)
		n.routingClock = func() time.Duration { return f.elapsed }
		n.directory.monotonicNow = func() time.Time { return f.epoch.Add(f.elapsed) }
		t.Cleanup(func() { n.cancel(); _ = n.Stop() })
		f.step(i, babel.Originate{Prefix: netip.PrefixFrom(credentials.Device.MeshAddress(), 128)})
	}
	for i, n := range f.nodes {
		if _, err := n.cache.Put(f.nodes[1-i].Credentials.Certificate.Certificate, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	f.addCarrier(4096, "ble")
	for i, n := range f.nodes {
		// Direct-message Synchronizer mode keeps cryptographic proof verification
		// real without introducing unrelated HAVE/WANT transport scheduling.
		n.peers[1].sync.Close()
		n.peers[1].sync = NewSynchronizer(n.directory, n.cache)
		var proof SignedManifest
		for _, record := range f.nodes[1-i].directory.Records(time.Now()) {
			if record.Principal == f.nodes[1-i].Credentials.Device.Principal() {
				proof = record
			}
		}
		if len(proof.Body) == 0 {
			t.Fatal("peer's actual signed origin proof missing")
		}
		if err := n.handleControl(n.peers[1], &ControlMessage{Kind: "manifest", Manifest: &proof}); err != nil {
			t.Fatal(err)
		}
	}
	f.pump(0)
	f.advance(3*time.Second, 0)
	f.assertSelected(1)
	return f
}

func (f *carrierStandbyFixture) step(index int, event babel.Event) {
	f.t.Helper()
	if err := f.nodes[index].step(event); err != nil {
		f.t.Fatal(err)
	}
}

func (f *carrierStandbyFixture) addCarrier(cost uint16, transport string) {
	f.t.Helper()
	for i, n := range f.nodes {
		link := &handoffActorLink{}
		peer := &nodePeer{device: f.nodes[1-i].Credentials.Device, cost: cost, transport: transport,
			link: link, control: make(chan ControlMessage, 4096), done: make(chan struct{}), retire: make(chan struct{})}
		if err := n.handle(nodeEvent{add: peer}); err != nil {
			f.t.Fatal(err)
		}
		f.links[i][peer.id] = link
	}
}

func (f *carrierStandbyFixture) pump(failed babel.LinkID) {
	f.t.Helper()
	for round := 0; round < 100; round++ {
		work := false
		for i := range f.nodes {
			for id, link := range f.links[i] {
				packets := link.packets
				link.packets = nil
				for _, packet := range packets {
					if id == failed {
						continue
					}
					work = true
					if len(packet) == 0 || packet[0] != PacketBabel {
						f.t.Fatal("non-Babel datagram")
					}
					if err := f.nodes[1-i].handle(nodeEvent{id: id, packet: append([]byte(nil), packet[1:]...)}); err != nil {
						f.t.Fatal(err)
					}
				}
			}
		}
		if !work {
			return
		}
	}
	f.t.Fatal("carrier packet exchange did not settle")
}

func (f *carrierStandbyFixture) advance(until time.Duration, failed babel.LinkID) {
	f.t.Helper()
	for f.elapsed < until {
		f.elapsed = min(until, f.elapsed+100*time.Millisecond)
		for i := range f.nodes {
			f.step(i, babel.Tick{})
		}
		f.pump(failed)
	}
}

func (f *carrierStandbyFixture) assertSelected(id babel.LinkID) {
	f.t.Helper()
	for i, n := range f.nodes {
		prefix := netip.PrefixFrom(f.nodes[1-i].Credentials.Device.MeshAddress(), 128)
		for _, routes := range [][]babel.Route{n.routing.Snapshot().Routes, f.networks[i].routes, n.Snapshot().ForwardingRoutes} {
			found := false
			for _, route := range routes {
				if route.Prefix == prefix && !route.Local && !route.Unreachable && route.Metric != babel.Infinity {
					if route.Link != id {
						f.t.Fatalf("t=%s node=%d peer route uses link=%d want=%d", f.elapsed, i, route.Link, id)
					}
					found = true
				}
			}
			if !found {
				f.t.Fatalf("t=%s node=%d has no installed selected peer route", f.elapsed, i)
			}
		}
		ready := false
		for _, link := range n.Snapshot().Links {
			if link.ID == id {
				ready = link.ForwardingReady
			}
		}
		if !ready {
			f.t.Fatalf("t=%s node=%d selected link=%d lacks forwarding readiness", f.elapsed, i, id)
		}
	}
}

func TestCarrierStandbyBLESurvivesLANUpgradeAndSilentLoss(t *testing.T) {
	f := newCarrierStandbyFixture(t)
	blePeers := [2]*nodePeer{f.nodes[0].peers[1], f.nodes[1].peers[1]}
	bleLinks := [2]*handoffActorLink{f.links[0][1], f.links[1][1]}
	policies := [2]*PeerSelection{NewPeerSelection(f.nodes[0].Snapshot), NewPeerSelection(f.nodes[1].Snapshot)}
	// The upgrade is admitted while the existing authenticated BLE remains the
	// actual selected FIB. Attachment alone cannot displace that working path.
	f.addCarrier(128, "infrastructure-wifi")
	f.assertSelected(1)
	for i, policy := range policies {
		if !policy.AllowRadio(f.nodes[1-i].Credentials.Device, RadioBLE) {
			t.Fatal("unprimed LAN vetoed the existing BLE path")
		}
	}
	f.pump(0)
	f.advance(6*time.Second, 0)
	f.assertSelected(2)
	for i, policy := range policies {
		peer := f.nodes[1-i].Credentials.Device
		if !policy.HasLAN(peer) || !policy.AllowRadio(peer, RadioBLE) {
			t.Fatal("installed LAN caused spare-capacity BLE standby to be pruned")
		}
		for _, link := range f.nodes[i].Snapshot().Links {
			if link.ID == 1 && link.ForwardingReady {
				t.Fatal("unselected BLE standby was mislabeled as selected forwarding")
			}
		}
		f.networks[i].watch = netip.PrefixFrom(peer.MeshAddress(), 128)
		f.networks[i].interrupted = nil
	}
	// Drop only LAN datagrams. The already-admitted BLE keeps exchanging its
	// normal Babel traffic; no socket close or new Attach is supplied to repair it.
	droppedAt := f.elapsed
	var switchedAt time.Duration
	for f.elapsed < droppedAt+4*time.Second {
		f.advance(f.elapsed+100*time.Millisecond, 2)
		bothBLE := true
		for _, n := range f.nodes {
			ready := false
			for _, link := range n.Snapshot().Links {
				if link.ID == 1 && link.ForwardingReady {
					ready = true
				}
			}
			bothBLE = bothBLE && ready
		}
		if bothBLE && switchedAt == 0 {
			switchedAt = f.elapsed
		}
	}
	if switchedAt == 0 || switchedAt > droppedAt+3500*time.Millisecond+100*time.Millisecond {
		t.Fatalf("warm BLE missed existing LAN hold: dropped=%s switched=%s", droppedAt, switchedAt)
	}
	f.assertSelected(1)
	for i, n := range f.nodes {
		if n.nextLink != 2 || len(n.peers) != 2 || n.peers[1] != blePeers[i] || f.links[i][1] != bleLinks[i] || bleLinks[i].closed {
			t.Fatal("failover replaced or closed the original BLE incarnation")
		}
		if len(f.networks[i].interrupted) != 0 {
			t.Fatal("warm carrier handoff applied an unreachable peer FIB")
		}
	}
}
