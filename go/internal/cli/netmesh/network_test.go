package netmesh

import (
	"context"
	"encoding/binary"
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestNetworkDropsUnroutedAndExpiredPackets(t *testing.T) {
	tenant, _ := meshid.ParseTenant("11111111-1111-4111-8111-111111111111")
	self, _ := meshid.ParseDevice(tenant, "22222222-2222-4222-8222-222222222222")
	peer, _ := meshid.ParseDevice(tenant, "33333333-3333-4333-8333-333333333333")
	network, err := NewNetwork(self)
	if err != nil {
		t.Fatal(err)
	}
	defer network.Close()
	port, err := network.AddPeer(1, peer)
	if err != nil {
		t.Fatal(err)
	}
	queue := port.(*packetPort).queue
	packet := make([]byte, 48)
	packet[0] = 0x60
	packet[7] = 64
	binary.BigEndian.PutUint16(packet[4:6], 8)
	source := self.MeshAddress().As16()
	dest := peer.MeshAddress().As16()
	copy(packet[8:24], source[:])
	copy(packet[24:40], dest[:])
	network.forward(packet, false)
	if len(queue) != 0 {
		t.Fatal("unrouted packet forwarded")
	}
	route := babel.Route{Prefix: netip.PrefixFrom(peer.MeshAddress(), 128), Link: 1}
	network.Apply([]babel.Route{route})
	packet[7] = 1
	network.forward(packet, true)
	if len(queue) != 0 {
		t.Fatal("expired transit packet forwarded")
	}
	packet[7] = 64
	network.forward(packet, false)
	if len(queue) != 1 {
		t.Fatal("authorized route not used")
	}
	<-queue
	route.Unreachable = true
	network.Apply([]babel.Route{route})
	network.forward(packet, false)
	if len(queue) != 0 {
		t.Fatal("unreachable route used")
	}
	for _, address := range []string{"example.com:443", "127.0.0.1:80", "[::1]:80", netip.AddrPortFrom(peer.MeshAddress(), 80).String()} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		connection, err := network.DialContext(ctx, address)
		cancel()
		if connection != nil {
			connection.Close()
		}
		if err == nil {
			t.Fatalf("unexpected dial %s", address)
		}
	}
}
