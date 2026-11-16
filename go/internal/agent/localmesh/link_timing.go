package localmesh

import (
	"net/netip"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

func babelLinkForPeer(p *nodePeer, local, peer netip.Addr) babel.Link {
	link := babel.Link{ID: p.id, Local: local, Peer: peer, Cost: p.cost, MaxPacket: DatagramLimit - 1}
	switch p.link.(type) {
	case *streamNodeLink, *operatorRouterNodeLink:
		// BLE can queue reliable control frames for several seconds (measured
		// gaps exceeded 6.75s). Keep its 4s Hello / 14s hold, including unknown
		// stream media conservatively. Known fast streams inherit the node's
		// 1s Hello: applying the BLE hold to Wi-Fi kept a dead fast route
		// selected for about 14s despite an already-live BLE alternative.
		switch p.transport {
		case "tcp", "tls-stream", "infrastructure-wifi", "ethernet", "nan":
		default:
			link.HelloInterval = babel.DefaultHelloInterval
		}
	}
	return link
}
