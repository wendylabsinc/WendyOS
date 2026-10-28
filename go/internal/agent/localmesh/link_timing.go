package localmesh

import (
	"net/netip"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

func babelLinkForPeer(p *nodePeer, local, peer netip.Addr) babel.Link {
	link := babel.Link{ID: p.id, Local: local, Peer: peer, Cost: p.cost, MaxPacket: DatagramLimit - 1}
	switch p.link.(type) {
	case *streamNodeLink, *operatorRouterNodeLink:
		// BLE's reliable stream can queue control frames for several seconds.
		// The former 1s Hello expired after 3.5s, below observed 6.75s transmit gaps.
		// Use Babel's existing 4s default for framed router streams; datagram
		// carriers continue to inherit the node's faster configuration.
		link.HelloInterval = babel.DefaultHelloInterval
	}
	return link
}
