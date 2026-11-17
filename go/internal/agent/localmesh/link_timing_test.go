package localmesh

import (
	"github.com/wendylabsinc/WendyOS/go/babel"
	"net/netip"
	"testing"
	"time"
)

func TestFramedStreamUsesCarrierAppropriateBabelTiming(t *testing.T) {
	local, remote := netip.MustParseAddr("fe80::1"), netip.MustParseAddr("fe80::2")
	for _, framed := range []struct {
		name string
		link nodeLink
	}{{"device", &streamNodeLink{}}, {"operator", &operatorRouterNodeLink{}}} {
		for _, medium := range []struct {
			name string
			want time.Duration
		}{{"ble", babel.DefaultHelloInterval}, {"", babel.DefaultHelloInterval}, {"tcp", 0}, {"tls-stream", 0}, {"infrastructure-wifi", 0}, {"ethernet", 0}, {"nan", 0}} {
			t.Run(framed.name+"/"+medium.name, func(t *testing.T) {
				p := &nodePeer{id: 1, cost: 4096, transport: medium.name, link: framed.link}
				if got := babelLinkForPeer(p, local, remote).HelloInterval; got != medium.want {
					t.Fatalf("%s Hello interval %s, want %s", medium.name, got, medium.want)
				}
			})
		}
	}
	p := &nodePeer{id: 1, cost: 128, link: &quicNodeLink{}}
	if got := babelLinkForPeer(p, local, remote).HelloInterval; got != 0 {
		t.Fatal("datagram link did not inherit node timing", got)
	}
}
