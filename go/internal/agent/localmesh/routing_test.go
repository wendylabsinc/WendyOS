package localmesh

import (
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

type routingRecorder struct {
	calls     []string
	routes    []babel.Route
	datagrams []babel.Datagram
	sent      []babel.Datagram
	failure   string
}

func (r *routingRecorder) call(s string) error {
	r.calls = append(r.calls, s)
	if s == r.failure {
		return errors.New("injected " + s)
	}
	return nil
}
func (r *routingRecorder) Apply(routes []babel.Route) error {
	r.routes = append([]babel.Route(nil), routes...)
	return r.call("apply")
}
func (r *routingRecorder) Persist(babel.Checkpoint) error { return r.call("persist") }
func (r *routingRecorder) Resume() error                  { return r.call("resume") }
func (r *routingRecorder) Stop() error                    { return r.call("stop") }
func (r *routingRecorder) Send(d babel.Datagram) error {
	r.datagrams = append(r.datagrams, d)
	r.sent = append(r.sent, d)
	return r.call("send")
}

// meshDevice is a device whose mesh address the tests can predict, plus a fixed
// router ID so a peer's advertisements are recognisable.
type meshDevice struct {
	device   meshid.Device
	prefix   netip.Prefix
	routerID babel.RouterID
}

func newMeshDevice(t *testing.T, principal string, macLast byte) meshDevice {
	t.Helper()
	device := testDevice(t, principal)
	routerID, err := babel.RouterIDFromMAC(net.HardwareAddr{0x02, 0, 0, 0, 0, macLast})
	if err != nil {
		t.Fatal(err)
	}
	return meshDevice{device: device, prefix: netip.PrefixFrom(device.MeshAddress(), 128), routerID: routerID}
}

// genericMeshConfig exercises the routing adapter without production origin
// authorization. Signed-origin integration is covered in signed_route_routing_test.go.
func genericMeshConfig(self meshDevice) babel.Config {
	return babel.Config{
		RouterID:       self.routerID,
		HelloInterval:  time.Second,
		UpdateInterval: time.Second,
		AcceptRoute:    func(_ babel.RouterID, p netip.Prefix) bool { return admittedPrefix(p) },
	}
}

func TestRoutingBarrierFailureStops(t *testing.T) {
	self := newMeshDevice(t, testPrincipal(testTenantUUID, testDeviceAID), 1)
	for _, failure := range []string{"apply", "persist", "resume"} {
		t.Run(failure, func(t *testing.T) {
			io := &routingRecorder{failure: failure}
			r, err := NewRouting(genericMeshConfig(self), nil, io)
			if err != nil {
				t.Fatal(err)
			}
			err = r.Step(0, babel.Originate{Prefix: self.prefix})
			if err == nil {
				t.Fatal("failure ignored")
			}
			if io.calls[len(io.calls)-1] != "stop" {
				t.Fatal("forwarding not stopped", io.calls)
			}
			for _, s := range io.calls {
				if s == "send" {
					t.Fatal("advertised failed state")
				}
			}
			if err = r.Step(0, babel.Tick{}); !errors.Is(err, babel.ErrStopped) {
				t.Fatal("failed engine reused")
			}
		})
	}
}

func TestRoutingPersistsBeforeResumeAndSend(t *testing.T) {
	self := newMeshDevice(t, testPrincipal(testTenantUUID, testDeviceAID), 1)
	io := &routingRecorder{}
	r, _ := NewRouting(genericMeshConfig(self), nil, io)
	if err := r.Step(0, babel.Originate{Prefix: self.prefix}); err != nil {
		t.Fatal(err)
	}
	if len(io.calls) != 3 || io.calls[0] != "apply" || io.calls[1] != "persist" || io.calls[2] != "resume" {
		t.Fatal(io.calls)
	}
	peer := newMeshDevice(t, testPrincipal(testTenantUUID, testDeviceBID), 2)
	local, remote, err := meshid.LinkLocalAddresses(self.device, peer.device)
	if err != nil {
		t.Fatal(err)
	}
	io.calls = nil
	if err := r.Step(0, babel.AddLink{Link: babel.Link{ID: 1, Local: local, Peer: remote, Cost: 256}}); err != nil {
		t.Fatal(err)
	}
	persisted, resumed := false, false
	for _, call := range io.calls {
		persisted = persisted || call == "persist"
		resumed = resumed || call == "resume"
	}
	if !persisted || !resumed {
		t.Fatal("link change not persisted before resuming", io.calls)
	}
}

// TestLinkLocalAddressesAreComplementaryFromBothEnds: each end derives the pair
// on its own, from the full UUIDs, and the two views agree without negotiation.
func TestLinkLocalAddressesAreComplementaryFromBothEnds(t *testing.T) {
	a := testDevice(t, testPrincipal(testTenantUUID, testDeviceAID))
	b := testDevice(t, testPrincipal(testTenantUUID, testDeviceBID))
	aSelf, aPeer, err := meshid.LinkLocalAddresses(a, b)
	if err != nil {
		t.Fatal(err)
	}
	bSelf, bPeer, err := meshid.LinkLocalAddresses(b, a)
	if err != nil {
		t.Fatal(err)
	}
	if aSelf != bPeer || aPeer != bSelf {
		t.Fatalf("ends disagree: %s/%s vs %s/%s", aSelf, aPeer, bSelf, bPeer)
	}
	if aSelf == aPeer {
		t.Fatal("both ends chose the same address")
	}
	if !aSelf.IsLinkLocalUnicast() || !aPeer.IsLinkLocalUnicast() {
		t.Fatal("not link-local")
	}
	if aSelf.Zone() != "" || aPeer.Zone() != "" {
		t.Fatal("link-locals must be unzoned; the link ID carries the scope")
	}
	// The pair is decided by UUID order, not by which end asks.
	low, high := a, b
	if meshid.Compare(a, b) > 0 {
		low, high = b, a
	}
	if lowSelf, _, _ := meshid.LinkLocalAddresses(low, high); lowSelf.String() != "fe80::1" {
		t.Fatalf("lower UUID got %s", lowSelf)
	}
	if _, highAddr, _ := meshid.LinkLocalAddresses(high, low); highAddr.String() != "fe80::1" {
		t.Fatalf("lower UUID's peer view got %s", highAddr)
	}
	if _, _, err := meshid.LinkLocalAddresses(a, a); err == nil {
		t.Fatal("a device paired with itself")
	}
}

// TestInstanceRouterIDUsesProfileForms: the instance identity is the permanent
// MAC in 0x0000||MAC48 form when one exists, otherwise 0x0001||RANDOM48, and it
// never lands on a value the engine reserves.
func TestInstanceRouterIDUsesProfileForms(t *testing.T) {
	first, err := instanceRouterID()
	if err != nil {
		t.Fatal(err)
	}
	again, err := instanceRouterID()
	if err != nil {
		t.Fatal(err)
	}
	if first == 0 || first == babel.RouterID(^uint64(0)) {
		t.Fatalf("router ID %016x is reserved", first)
	}
	switch tag := first >> 48; tag {
	case 0x0000:
		if first&((1<<48)-1) == 0 {
			t.Fatalf("MAC form has an empty MAC: %016x", first)
		}
		if mac, ok := permanentMAC(); ok {
			if again != first {
				t.Fatalf("permanent MAC router ID changed: %016x -> %016x", first, again)
			}
			want, err := babel.RouterIDFromMAC(mac)
			if err != nil {
				t.Fatal(err)
			}
			if first != want {
				t.Fatalf("router ID %016x is not the permanent MAC %016x", first, want)
			}
		}
	case 0x0001:
		if first&((1<<48)-1) == 0 {
			t.Fatalf("random form has an empty random part: %016x", first)
		}
	default:
		t.Fatalf("router ID %016x is neither profile form", first)
	}
}

// TestUsableMACExcludesUnusableAddresses: an all-zero, truncated or multicast
// address cannot seed a permanent router ID.
func TestUsableMACExcludesUnusableAddresses(t *testing.T) {
	for _, mac := range []net.HardwareAddr{{0, 0, 0, 0, 0, 0}, {0x02, 0}, {0x01, 0, 0, 0, 0, 1}, {0x33, 0, 0, 0, 0, 1}} {
		if usableMAC(mac) {
			t.Fatalf("accepted %02x as a permanent address", mac)
		}
	}
	if !usableMAC(net.HardwareAddr{0x02, 0, 0, 0, 0, 1}) {
		t.Fatal("refused a unicast factory address")
	}
	if id := babel.RouterIDFromRandom([6]byte{1, 2, 3, 4, 5, 6}); id>>48 != 0x0001 || id&((1<<48)-1) == 0 {
		t.Fatalf("tagged random form is wrong: %016x", id)
	}
}

// The generic routing adapter can run with no origin policy. Production Node
// supplies the signed-origin policy and does not use this permissive fixture.
func TestGenericRoutingAdapterAcceptsPeerMeshRoute(t *testing.T) {
	self := newMeshDevice(t, testPrincipal(testTenantUUID, testDeviceAID), 1)
	peer := newMeshDevice(t, testPrincipal(testTenantUUID, testDeviceBID), 2)
	io := &routingRecorder{}
	r, err := NewRouting(genericMeshConfig(self), nil, io)
	if err != nil {
		t.Fatal(err)
	}
	// NewRouting takes no Directory: nothing in this routing instance can
	// consult a manifest, a signed origin claim or an address-allocation proof.
	selfLink, peerLink := testLinks(self.device, peer.device)
	bringUpMesh(t, r, io, self, peer, selfLink, peerLink, peer.prefix)
	var applied bool
	for _, call := range io.calls {
		applied = applied || call == "apply"
	}
	if !applied {
		t.Fatal("peer advertisement never reached the forwarding plane", io.calls)
	}
	var found bool
	for _, route := range r.engine.Snapshot().Routes {
		if route.Prefix == peer.prefix && !route.Local && route.Link == selfLink.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("wendy net route from a peer was not installed: routes=%+v counters=%+v", r.engine.Snapshot().Routes, r.engine.Snapshot().Counters)
	}
}

// The structural filter accepts only mesh host routes. Signed ownership is a
// separate production policy applied after this boundary.
func TestRouteAdmissionRefusesNonMeshHostPrefixes(t *testing.T) {
	self := newMeshDevice(t, testPrincipal(testTenantUUID, testDeviceAID), 1)
	peer := newMeshDevice(t, testPrincipal(testTenantUUID, testDeviceBID), 2)
	for _, tc := range []struct {
		name   string
		prefix netip.Prefix
	}{
		{"prefix outside the wendy net pool", netip.MustParsePrefix("2001:db8:445::1/128")},
		{"another ULA pool", netip.MustParsePrefix("fd00::445:1/128")},
		{"contained aggregate", netip.MustParsePrefix("fd45:b891:567f:1::/64")},
		{"narrow aggregate", netip.MustParsePrefix("fd45:b891:567f::2/127")},
		{"half the mesh pool", netip.MustParsePrefix("fd45:b891:567f::/49")},
		{"the mesh pool itself", netip.MustParsePrefix("fd45:b891:567f::/48")},
		{"a block wider than the mesh pool", netip.MustParsePrefix("fd45:b891:5600::/40")},
		{"default route", netip.MustParsePrefix("::/0")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			io := &routingRecorder{}
			r, err := NewRouting(genericMeshConfig(self), nil, io)
			if err != nil {
				t.Fatal(err)
			}
			selfLink, peerLink := testLinks(self.device, peer.device)
			bringUpMesh(t, r, io, self, peer, selfLink, peerLink, tc.prefix)
			for _, route := range r.engine.Snapshot().Routes {
				if route.Prefix == tc.prefix && !route.Local {
					t.Fatalf("refused advertisement was installed: %+v", route)
				}
			}
		})
	}
}

// The structural boundary permits exact device addresses only.
func TestAdmittedPrefixRequiresMeshHostRoute(t *testing.T) {
	for _, tc := range []struct {
		prefix netip.Prefix
		want   bool
	}{
		{netip.MustParsePrefix("fd45:b891:567f:4000:8000::445/128"), true},
		{netip.MustParsePrefix("fd45:b891:567f:1::/64"), false},
		{netip.MustParsePrefix("fd45:b891:567f::/49"), false},
		{netip.MustParsePrefix("fd45:b891:567f::2/127"), false},
		{netip.Prefix{}, false},
		{netip.MustParsePrefix("fd45:b891:567f::/48"), false},
		{netip.MustParsePrefix("fd45:b891:5600::/40"), false},
		{netip.MustParsePrefix("fd00::445:1/128"), false},
		{netip.MustParsePrefix("2001:db8:445::1/128"), false},
		{netip.MustParsePrefix("fe80::2/128"), false},
		{netip.MustParsePrefix("10.88.1.189/32"), false},
		{netip.MustParsePrefix("::/0"), false},
	} {
		if got := admittedPrefix(tc.prefix); got != tc.want {
			t.Errorf("%s: admitted=%v want=%v", tc.prefix, got, tc.want)
		}
	}
}

// bringUpMesh runs the real handshake: both ends bring the link up, exchange
// Hellos and IHUs, and only then originate their own mesh address.
func bringUpMesh(t *testing.T, r *Routing, io *routingRecorder, self, peer meshDevice, selfLink, peerLink babel.Link, advertised netip.Prefix) {
	t.Helper()
	if err := r.Step(0, babel.AddLink{Link: selfLink}); err != nil {
		t.Fatal(err)
	}
	engine, peerOut := newPeerEngine(t, peer, peerLink)
	deliver := func(at time.Duration) {
		for _, d := range io.sent {
			stepPeer(t, engine, at, babel.Receive{ID: peerLink.ID, Packet: d.Payload})
		}
		io.sent = nil
		for _, d := range peerOut {
			if err := r.Step(at, babel.Receive{ID: selfLink.ID, Packet: d.Payload}); err != nil {
				t.Fatal(err)
			}
		}
		peerOut = nil
	}
	deliver(time.Second)
	if err := r.Step(2*time.Second, babel.Originate{Prefix: self.prefix}); err != nil {
		t.Fatal(err)
	}
	peerOut = peerOriginate(t, engine, 2*time.Second, advertised)
	deliver(3 * time.Second)
	effects := stepPeer(t, engine, 4*time.Second, babel.Tick{})
	peerOut = effects.Datagrams
	if err := r.Step(4*time.Second, babel.Tick{}); err != nil {
		t.Fatal(err)
	}
	deliver(5 * time.Second)
}

func peerOriginate(t *testing.T, engine *babel.Engine, at time.Duration, prefix netip.Prefix) []babel.Datagram {
	t.Helper()
	effects := stepPeer(t, engine, at, babel.Originate{Prefix: prefix})
	return effects.Datagrams
}

// TestRoutingIgnoresOversizedAndMalformedUpdates: the engine's packet budget and
// wire decoder still decide what is even parsed, before any policy runs.
func TestRoutingIgnoresOversizedAndMalformedUpdates(t *testing.T) {
	self := newMeshDevice(t, testPrincipal(testTenantUUID, testDeviceAID), 1)
	r, err := NewRouting(genericMeshConfig(self), nil, &routingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	link, _ := testLinks(self.device, newMeshDevice(t, testPrincipal(testTenantUUID, testDeviceBID), 2).device)
	link.MaxPacket = 512
	if err = r.Step(0, babel.AddLink{Link: link}); err != nil {
		t.Fatal(err)
	}
	before := r.engine.Snapshot().Counters.Malformed
	if err = r.Step(time.Second, babel.Receive{ID: link.ID, Packet: make([]byte, 4096)}); err != nil {
		t.Fatal(err)
	}
	if err = r.Step(2*time.Second, babel.Receive{ID: link.ID, Packet: []byte{0xff, 0x00, 0x11, 0x22, 0x33}}); err != nil {
		t.Fatal(err)
	}
	if after := r.engine.Snapshot().Counters.Malformed; after < before+2 {
		t.Fatalf("oversized or malformed packet was not counted: %d -> %d", before, after)
	}
	if routes := r.engine.Snapshot().Routes; len(routes) != 0 {
		t.Fatalf("junk installed routes: %+v", routes)
	}
}

func testLinks(self, peer meshid.Device) (babel.Link, babel.Link) {
	selfAddr, peerAddr, _ := meshid.LinkLocalAddresses(self, peer)
	const id = babel.LinkID(1)
	return babel.Link{ID: id, Local: selfAddr, Peer: peerAddr, Cost: 256, MaxPacket: 1200},
		babel.Link{ID: id, Local: peerAddr, Peer: selfAddr, Cost: 256, MaxPacket: 1200}
}

func newPeerEngine(t *testing.T, peer meshDevice, link babel.Link) (*babel.Engine, []babel.Datagram) {
	t.Helper()
	engine, err := babel.New(babel.Config{
		RouterID:       peer.routerID,
		HelloInterval:  time.Second,
		UpdateInterval: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	effects := stepPeer(t, engine, 0, babel.AddLink{Link: link})
	return engine, effects.Datagrams
}

// feedPeer delivers what the neighbour already put on the wire when its own
// link came up, so this engine sees an established neighbour before routes.
func feedPeer(t *testing.T, r *Routing, id babel.LinkID, datagrams []babel.Datagram) {
	t.Helper()
	for _, d := range datagrams {
		if err := r.Step(500*time.Millisecond, babel.Receive{ID: id, Packet: d.Payload}); err != nil {
			t.Fatal(err)
		}
	}
}

// stepPeer drives the far end the way a forwarding plane would: every revision
// the engine asks for is applied and committed before the next event.
func stepPeer(t *testing.T, engine *babel.Engine, at time.Duration, event babel.Event) babel.Effects {
	t.Helper()
	effects, err := engine.Step(at, event)
	if err != nil {
		t.Fatal(err)
	}
	if effects.Revision != 0 {
		committed, err := engine.Commit(effects.Revision, true)
		if err != nil {
			t.Fatal(err)
		}
		effects.Datagrams = append(effects.Datagrams, committed.Datagrams...)
	}
	return effects
}

// peerAdvertise answers a request with the peer's advertisement, exactly as a
// real neighbour would, and returns the datagrams to feed back.
func peerAdvertise(t *testing.T, engine *babel.Engine, request []byte) []babel.Datagram {
	t.Helper()
	effects := stepPeer(t, engine, 2*time.Second, babel.Receive{ID: 1, Packet: request})
	if len(effects.Datagrams) == 0 {
		t.Fatal("peer sent no advertisement")
	}
	return effects.Datagrams
}
