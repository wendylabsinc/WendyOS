package localmesh

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
)

func waitAssemblyExpiry(t *testing.T, a *IPAssembler, ctx context.Context) {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for a.Snapshot().ExpiredPackets != 1 {
		select {
		case <-ctx.Done():
			t.Fatal("owner did not expire idle assembly", ctx.Err())
		case <-ticker.C:
		}
	}
	if a.Snapshot().PendingBytes != 0 {
		t.Fatal("expired buffer retained")
	}
}

func TestIPAssemblyExpiryKeepsQUICDataAndControlUsable(t *testing.T) {
	a, b := testCredentials(t)
	serverTLS, err := b.PeerTLS(a.Device)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := quic.ListenAddr("127.0.0.1:0", serverTLS, QUICConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), ipAssemblyLifetime+5*time.Second)
	defer cancel()
	accept := make(chan *quic.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept(ctx)
		if err != nil {
			acceptErr <- err
		} else {
			accept <- conn
		}
	}()
	clientTLS, err := a.PeerTLS(b.Device)
	if err != nil {
		t.Fatal(err)
	}
	client, err := quic.DialAddr(ctx, listener.Addr().String(), clientTLS, QUICConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseWithError(0, "test done")
	var server *quic.Conn
	select {
	case server = <-accept:
	case err := <-acceptErr:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	defer server.CloseWithError(0, "test done")
	serverStream := make(chan *quic.Stream, 1)
	go func() {
		stream, err := OpenControl(ctx, server, b.Device, a.Device)
		if err != nil {
			acceptErr <- err
		} else {
			serverStream <- stream
		}
	}()
	clientStream, err := OpenControl(ctx, client, a.Device, b.Device)
	if err != nil {
		t.Fatal(err)
	}
	var control *quic.Stream
	select {
	case control = <-serverStream:
	case err := <-acceptErr:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var assembler IPAssembler
	_, pieces := assemblyPacket(t, TunnelMTU, 1)
	result := make(chan []byte, 1)
	var receiveTimeouts atomic.Int32
	go func() {
		assembler.Receive(pieces[0], time.Now())
		d, err := assembler.receiveDatagram(ctx, func(actual context.Context) ([]byte, error) {
			d, err := server.ReceiveDatagram(actual)
			if actual != ctx && errors.Is(err, context.DeadlineExceeded) {
				receiveTimeouts.Add(1)
			}
			return d, err
		})
		if err != nil {
			acceptErr <- err
		} else {
			result <- d
		}
	}()
	waitAssemblyExpiry(t, &assembler, ctx)
	if receiveTimeouts.Load() != 1 {
		t.Fatal("QUIC receiver did not witness the owned assembly deadline")
	}
	// This is the same authenticated QUIC connection, after its owned receive
	// deadline expired. The reliable control stream remains independently usable.
	if err := WriteControl(clientStream, ControlMessage{Kind: "bundle", Bundle: a.Certificate.Certificate}); err != nil {
		t.Fatal(err)
	}
	m, err := ReadControl(control)
	if err != nil || m.Kind != "bundle" {
		t.Fatal("control lost after receive timeout", err)
	}
	_, whole := assemblyPacket(t, 80, 2)
	if err := client.SendDatagram(whole[0]); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if !bytes.Equal(got, whole[0]) {
			t.Fatal("wrong datagram after expiry")
		}
	case err := <-acceptErr:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if client.Context().Err() != nil || server.Context().Err() != nil {
		t.Fatal("assembly expiry closed QUIC")
	}
}
