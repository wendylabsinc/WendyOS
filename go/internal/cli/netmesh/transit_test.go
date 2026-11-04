package netmesh

import (
	"bytes"
	"context"
	"io"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
)

// Real userspace TCP has no A-to-C carrier: every data and acknowledgement
// packet must pass through B's selected forwarding table and packet ports.
func TestNetworkTCPThroughIntermediateRouter(t *testing.T) {
	tenant, _ := meshid.ParseTenant("11111111-1111-4111-8111-111111111111")
	ids := []string{"22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444"}
	peers := make([]meshid.Device, 3)
	networks := make([]*Network, 3)
	var workers sync.WaitGroup
	defer workers.Wait()
	defer func() {
		for _, n := range networks {
			if n != nil {
				_ = n.Close()
			}
		}
	}()
	for i, id := range ids {
		peers[i], _ = meshid.ParseDevice(tenant, id)
		var err error
		networks[i], err = NewNetwork(peers[i])
		if err != nil {
			t.Fatal(err)
		}
	}
	connect := func(a, b int, left, right babel.LinkID) {
		x, err := networks[a].AddPeer(left, peers[b])
		if err != nil {
			t.Fatal(err)
		}
		y, err := networks[b].AddPeer(right, peers[a])
		if err != nil {
			t.Fatal(err)
		}
		for _, pair := range [][2]io.ReadWriteCloser{{x, y}, {y, x}} {
			workers.Add(1)
			go func(from, to io.ReadWriteCloser) { defer workers.Done(); _, _ = io.Copy(to, from) }(pair[0], pair[1])
		}
	}
	connect(0, 1, 1, 1)
	connect(1, 2, 2, 1)
	route := func(peer int, link babel.LinkID) babel.Route {
		return babel.Route{Prefix: netip.PrefixFrom(peers[peer].MeshAddress(), 128), Link: link}
	}
	_ = networks[0].Apply([]babel.Route{route(2, 1)})
	_ = networks[1].Apply([]babel.Route{route(0, 1), route(2, 2)})
	_ = networks[2].Apply([]babel.Route{route(0, 1)})
	listener, err := gonet.ListenTCP(networks[2].stack, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom16(peers[2].MeshAddress().As16()), Port: 9000}, ipv6.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	workers.Add(1)
	go func() {
		defer workers.Done()
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		_, _ = io.Copy(conn, conn)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := networks[0].DialContext(ctx, netip.AddrPortFrom(peers[2].MeshAddress(), 9000).String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	payload := bytes.Repeat([]byte("Babel packet-port transit\n"), 256)
	write := make(chan error, 1)
	go func() { _, err := conn.Write(payload); write <- err }()
	reply := make([]byte, len(payload))
	if _, err = io.ReadFull(conn, reply); err != nil {
		t.Fatal(err)
	}
	if err = <-write; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, reply) {
		t.Fatal("transit TCP payload changed")
	}
}
