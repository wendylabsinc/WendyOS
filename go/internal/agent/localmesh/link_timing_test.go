package localmesh

import (
	"net/netip"
	"testing"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

func TestFramedStreamUsesCarrierAppropriateBabelTiming(t *testing.T) {
	local, remote := netip.MustParseAddr("fe80::1"), netip.MustParseAddr("fe80::2")
	p := &nodePeer{id: 1, cost: 4096, link: &streamNodeLink{}}
	link := babelLinkForPeer(p, local, remote)
	if link.HelloInterval != babel.DefaultHelloInterval {
		t.Fatal("stream kept an aggressively short Hello interval")
	}
	p.link = &quicNodeLink{}
	if got := babelLinkForPeer(p, local, remote).HelloInterval; got != 0 {
		t.Fatal("datagram link did not inherit node timing", got)
	}
}
