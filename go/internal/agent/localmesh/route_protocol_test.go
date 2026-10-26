package localmesh

import "testing"

func TestRouterCarrierRejectsOtherHelloVersions(t *testing.T) {
	self, peer := testDevices(t)
	hello := LinkHello{Version: LinkProtocolVersion, Device: peer.UUID(), Peer: self.UUID(), MTU: TunnelMTU, Datagram: DatagramLimit, IPDatagram: WholeIPDatagramLimit}
	if !validRouterHello(&hello, self, peer) {
		t.Fatal("current router peer rejected")
	}
	for _, version := range []int{0, 1, 3} {
		hello.Version = version
		if validRouterHello(&hello, self, peer) {
			t.Fatalf("other router version %d admitted", version)
		}
	}
	if validRouterHello(nil, self, peer) {
		t.Fatal("missing hello admitted")
	}
}
func TestStreamHopV1RejectsOtherHelloVersions(t *testing.T) {
	self, peer := testDevices(t)
	hello := LinkHello{Version: StreamProtocolVersion, Device: peer.UUID(), Peer: self.UUID(), MTU: TunnelMTU, Datagram: DatagramLimit}
	if !validStreamHelloVersion(&hello, self, peer, StreamProtocolVersion) {
		t.Fatal("current stream peer rejected")
	}
	for _, version := range []int{0, 2, 3} {
		hello.Version = version
		if validStreamHelloVersion(&hello, self, peer, StreamProtocolVersion) {
			t.Fatalf("other stream version %d admitted", version)
		}
	}
}
