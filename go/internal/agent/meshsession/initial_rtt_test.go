package meshsession

import (
	"context"
	"net"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
)

// A normal BLE path can take longer than 200ms to return its first reply.
// Observe real QUIC packets sent to a silent loopback endpoint: sending another
// Initial flight before even half a second amplifies traffic before the path
// has had a reasonable opportunity to reply. This exercises the configured
// connection, not just a Config field or a mocked timer.
func TestAppQUICDoesNotProbeBeforeBLEFirstReplyBudget(t *testing.T) {
	a, b := fixtureCredentials(t)
	cfg, err := a.PeerTLSWithTickets(b.Device, ALPN, "app-quic")
	if err != nil {
		t.Fatal(err)
	}
	sink, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	deadline, _ := ctx.Deadline()
	if err := sink.SetReadDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	done := make(chan error, 1)
	go func() {
		conn, err := quic.DialAddr(ctx, sink.LocalAddr().String(), cfg, quicConfig())
		if conn != nil {
			_ = conn.CloseWithError(0, "test complete")
		}
		done <- err
	}()
	initial, repeated := 0, 0
	buf := make([]byte, 2048)
	for {
		n, _, err := sink.ReadFrom(buf)
		if err != nil {
			if e, ok := err.(net.Error); !ok || !e.Timeout() {
				t.Fatal(err)
			}
			break
		}
		if n < 1200 {
			t.Fatalf("unexpected short handshake datagram: %d", n)
		}
		if time.Since(started) < 100*time.Millisecond {
			initial++
		} else {
			repeated++
		}
	}
	if err := <-done; err == nil {
		t.Fatal("silent endpoint unexpectedly completed a handshake")
	}
	if initial == 0 || repeated != 0 {
		t.Fatalf("Initial flight repeated before a slow path could reply: opening=%d repeated=%d", initial, repeated)
	}
}
