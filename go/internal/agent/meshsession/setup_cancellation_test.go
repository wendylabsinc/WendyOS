package meshsession

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
)

func TestPooledPortACKHonorsCancellationWithoutDeadline(t *testing.T) {
	a, b := fixtureCredentials(t)
	server, err := NewServer(b, allowPort(func(uint16) bool { return false }))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := quic.ListenAddr("127.0.0.1:0", server.TLSConfig(), quicConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverCtx, stopServer := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopServer()
	requested := make(chan struct{})
	serverDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept(serverCtx)
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.CloseWithError(0, "test complete")
		first, err := conn.AcceptStream(serverCtx)
		if err != nil {
			serverDone <- err
			return
		}
		defer first.CancelRead(0)
		defer first.CancelWrite(0)
		var hello [6]byte
		if _, err = io.ReadFull(first, hello[:]); err != nil {
			serverDone <- err
			return
		}
		close(requested) // Deliberately withhold the first port ACK.
		second, err := conn.AcceptStream(serverCtx)
		if err != nil {
			serverDone <- err
			return
		}
		if _, err = io.ReadFull(second, hello[:]); err == nil {
			_, err = second.Write([]byte{1})
		}
		if err == nil {
			var payload [4]byte
			_, err = io.ReadFull(second, payload[:])
			if err == nil {
				_, err = second.Write(payload[:])
			}
		}
		if err == nil {
			err = second.Close()
		}
		serverDone <- err
		<-conn.Context().Done()
	}()
	client, err := NewClient(a)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	endpoint := netip.MustParseAddrPort(listener.Addr().String())
	go func() {
		conn, err := client.Dial(ctx, b.Device, endpoint, 8080)
		if conn != nil {
			conn.Close()
		}
		result <- err
	}()
	select {
	case <-requested:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not receive the setup request")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled setup returned %v", err)
		}
	case <-time.After(time.Second):
		client.Close()
		<-result
		t.Fatal("port ACK wait ignored cancellation without a deadline")
	}
	// Cancellation retires this stream, while another authorized flow can reuse
	// the authenticated connection and outlive its own completed dial context.
	nextCtx, nextCancel := context.WithTimeout(context.Background(), 3*time.Second)
	conn, err := client.Dial(nextCtx, b.Device, endpoint, 8081)
	nextCancel()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if _, err = conn.Write([]byte("echo")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err = io.ReadFull(conn, got); err != nil || string(got) != "echo" {
		t.Fatalf("following flow: %q, %v", got, err)
	}
	if err = <-serverDone; err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
}
