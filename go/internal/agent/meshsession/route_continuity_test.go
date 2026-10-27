package meshsession

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

type interruptedPacketConn struct {
	net.PacketConn
	cut atomic.Bool
}

func (c *interruptedPacketConn) WriteTo(data []byte, address net.Addr) (int, error) {
	if c.cut.Load() {
		return len(data), nil // silent packet loss, not a local socket error
	}
	return c.PacketConn.WriteTo(data, address)
}

func TestActiveTCPRecoversAfterSignedRouteInterruption(t *testing.T) {
	a, b := fixtureCredentials(t)
	port := lifecycleEcho(t)
	endpoint := lifecycleServer(t, b, allowPort(func(p uint16) bool { return p == port }), true)
	client := lifecycleClient(t, a)
	udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	packets := &interruptedPacketConn{PacketConn: udp}
	transport := &quic.Transport{Conn: packets}
	t.Cleanup(func() { client.Close(); transport.Close(); udp.Close() })
	// Use the real principal-derived route endpoint in the pool. Only this
	// test's physical delivery maps to loopback; QUIC, PKI and streams are real.
	client.dial = func(ctx context.Context, _ netip.AddrPort, cfg *tls.Config, qc *quic.Config) (*quic.Conn, error) {
		return transport.Dial(ctx, net.UDPAddrFromAddrPort(endpoint), cfg, qc)
	}
	snapshot := localmesh.NodeSnapshot{Routes: []babel.Route{{Prefix: netip.PrefixFrom(b.Device.MeshAddress(), 128)}}}
	logical, err := Resolve(snapshot, b.Device)
	if err != nil {
		t.Fatal(err)
	}
	old := lifecycleDial(t, client, b.Device, logical, port)
	client.mu.Lock()
	client.peers[b.Device].born = time.Now().Add(-2 * clientAgeLimit)
	client.mu.Unlock()
	active := lifecycleDial(t, client, b.Device, logical, port)
	if old.conn == active.conn {
		t.Fatal("expected one already retired connection and one current connection")
	}
	lifecycleExchange(t, old)
	lifecycleExchange(t, active)

	packets.cut.Store(true)
	snapshot.Routes[0].Unreachable = true
	client.PruneRoutes(snapshot)
	if old.conn.Context().Err() != nil || active.conn.Context().Err() != nil {
		t.Fatal("routing interruption closed an authenticated active TCP session")
	}
	client.mu.Lock()
	cached := client.peers[b.Device]
	client.mu.Unlock()
	if cached != nil {
		t.Fatal("withdrawn route retained a transport for new streams")
	}
	if _, err := Resolve(snapshot, b.Device); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("new flow without a signed route: %v", err)
	}
	if _, err := old.Write([]byte("heal")); err != nil {
		t.Fatal(err)
	}
	old.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	var reply [4]byte
	if _, err := old.Read(reply[:]); err == nil {
		t.Fatal("traffic crossed the interrupted underlay")
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatalf("stream closed instead of awaiting recovery: %v", err)
	}

	snapshot.Routes[0].Unreachable = false
	client.PruneRoutes(snapshot)
	packets.cut.Store(false)
	old.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(old, reply[:]); err != nil || string(reply[:]) != "heal" {
		t.Fatalf("same stream did not retransmit after route recovery: %q, %v", reply, err)
	}
	old.SetDeadline(time.Time{})
	lifecycleExchange(t, old)
	lifecycleExchange(t, active)
	fresh := lifecycleDial(t, client, b.Device, logical, port)
	if fresh.conn == old.conn || fresh.conn == active.conn {
		t.Fatal("new flow reused a retired transport after the heal")
	}
	lifecycleExchange(t, fresh)
	// Identity teardown is deliberately stronger than temporary route loss.
	client.Invalidate(b.Device)
	for _, conn := range []*quic.Conn{old.conn, active.conn, fresh.conn} {
		requireSessionClosed(t, conn)
	}
}
