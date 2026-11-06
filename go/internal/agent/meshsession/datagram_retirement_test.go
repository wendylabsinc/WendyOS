package meshsession

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
)

type rejectedUDPAuthorizer struct{ allowPort }

func (rejectedUDPAuthorizer) UDPToken(uint16) uint64 { return 0 }
func (rejectedUDPAuthorizer) WithAuthorizedUDPToken(uint16, uint64, func(netip.AddrPort) error) error {
	return ErrDenied
}

func TestDeniedUDPControlsPreserveACKAndReturnStreamCredit(t *testing.T) {
	a, b := fixtureCredentials(t)
	server, err := NewServer(b, rejectedUDPAuthorizer{allowPort(func(uint16) bool { return false })})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer server.Close()
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, "127.0.0.1:0") }()
	defer func() {
		cancel()
		server.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("UDP handlers did not join")
		}
	}()
	for server.Addr() == nil {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	config, err := a.PeerTLSWithTickets(b.Device, ALPN, "app-quic")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := quic.DialAddr(ctx, server.Addr().String(), config, quicConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "done")
	for i := 0; i < 24; i++ {
		child, cancel := context.WithTimeout(ctx, time.Second)
		stream, err := conn.OpenStreamSync(child)
		cancel()
		if err != nil {
			t.Fatalf("UDP control %d leaked credit: %v", i, err)
		}
		stream.SetDeadline(time.Now().Add(time.Second))
		hello := udpOpenRequest(12345)
		if _, err := stream.Write(hello); err != nil {
			t.Fatal(err)
		}
		var ack [16]byte
		if _, err := io.ReadFull(stream, ack[:]); err != nil || ack[4] != 0 {
			t.Fatalf("UDP denial ACK=%v error=%v", ack, err)
		}
		stream.CancelRead(1)
		stream.CancelWrite(1)
		if conn.Context().Err() != nil {
			t.Fatal("denied UDP control closed authenticated session")
		}
	}
	// The standard API still returns the exact authorization error.
	if flow, err := DialUDP(ctx, a, b.Device, netip.MustParseAddrPort(server.Addr().String()), 12345); flow != nil || !errors.Is(err, ErrDenied) {
		t.Fatalf("DialUDP denied flow=%v error=%v", flow, err)
	}
}
