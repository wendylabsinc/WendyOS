package localmesh

import "github.com/wendylabsinc/wendy/go/internal/shared/meshid"

// StreamALPN identifies the routing/control protocol carried over a TLS stream.
// Carriers must negotiate this exact capability before admitting the stream.
const StreamALPN = "wendy-local-mesh-ble/1"

// StreamProtocolVersion is independent of router QUIC frame capabilities.
// Leaf carriers enforce their own version at their explicit admission boundary.
const StreamProtocolVersion = 1

// validStreamHelloVersion deliberately refuses router-only IP capabilities.
// The caller supplies its explicit stream role's version, independently of
// LinkProtocolVersion, which applies only to router QUIC.
func validStreamHelloVersion(h *LinkHello, self, peer meshid.Device, version int) bool {
	return h != nil && h.Version == version && h.Device == peer.UUID() && h.Peer == self.UUID() && h.MTU == TunnelMTU && h.Datagram == DatagramLimit && h.IPDatagram == 0
}
