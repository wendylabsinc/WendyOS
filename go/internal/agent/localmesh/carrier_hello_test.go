package localmesh

import "testing"

func TestRouterHelloCapabilityIsAuthenticatedBoundedAndSeparate(t *testing.T) {
	self, peer := testDevices(t)
	good := LinkHello{Version: LinkProtocolVersion, Device: peer.UUID(), Peer: self.UUID(), MTU: TunnelMTU, Datagram: DatagramLimit, IPDatagram: WholeIPDatagramLimit}
	if !validRouterHello(&good, self, peer) {
		t.Fatal("whole router capability rejected")
	}
	for _, limit := range []int{0, -1, 1099, 1101, 1328, 1330, 1 << 30} {
		bad := good
		bad.IPDatagram = limit
		if validRouterHello(&bad, self, peer) {
			t.Fatal("invalid capability accepted", limit)
		}
	}
	good.IPDatagram = DatagramLimit
	if !validRouterHello(&good, self, peer) {
		t.Fatal("new constrained router rejected")
	}
	for _, change := range []func(*LinkHello){func(h *LinkHello) { h.Version = 1 }, func(h *LinkHello) { h.MTU++ }, func(h *LinkHello) { h.Datagram++ }, func(h *LinkHello) { h.Device = self.UUID() }} {
		bad := good
		change(&bad)
		if validRouterHello(&bad, self, peer) {
			t.Fatal("bad protocol/identity accepted")
		}
	}
	if validRouterHello(nil, self, peer) {
		t.Fatal("missing hello accepted")
	}
}

func TestStreamHelloDoesNotInheritRouterCapabilities(t *testing.T) {
	self, peer := testDevices(t)
	old := LinkHello{Version: StreamProtocolVersion, Device: peer.UUID(), Peer: self.UUID(), MTU: TunnelMTU, Datagram: DatagramLimit}
	if !validStreamHelloVersion(&old, self, peer, StreamProtocolVersion) {
		t.Fatal("unchanged stream hello rejected")
	}
	if validRouterHello(&old, self, peer) {
		t.Fatal("old stream admitted as new router")
	}
	router := old
	router.Version = LinkProtocolVersion
	router.IPDatagram = WholeIPDatagramLimit
	if !validRouterHello(&router, self, peer) || validStreamHelloVersion(&router, self, peer, StreamProtocolVersion) {
		t.Fatal("router/stream capability boundary crossed")
	}
	router.Version = StreamProtocolVersion
	if validStreamHelloVersion(&router, self, peer, StreamProtocolVersion) {
		t.Fatal("stream accepted router receive capability")
	}
	// An explicit different stream role can retain its own version without
	// adopting router capability fields or changing this role's version.
	leaf := old
	leaf.Version = StreamProtocolVersion + 1
	if !validStreamHelloVersion(&leaf, self, peer, leaf.Version) || validStreamHelloVersion(&leaf, self, peer, StreamProtocolVersion) {
		t.Fatal("explicit role version not independent")
	}
	leaf.IPDatagram = WholeIPDatagramLimit
	if validStreamHelloVersion(&leaf, self, peer, leaf.Version) {
		t.Fatal("leaf accepted router capability")
	}
}
