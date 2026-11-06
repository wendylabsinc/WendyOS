//go:build linux

package localmesh

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

type tcpReadStartedConn struct {
	net.Conn
	started chan struct{}
	once    sync.Once
}

func (c *tcpReadStartedConn) Read(b []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	return c.Conn.Read(b)
}

func TestTCPIncomingHandshakeIdentifiesSchedulingPeer(t *testing.T) {
	a, b := testCredentials(t)
	policy := NewConnectionPolicy(a.Device, true, ConnectionLimits{2, 2, 4})
	lease := policy.Reserve("incoming-socket", meshid.Device{}, false)
	defer lease.Release()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	raw, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	inbound, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer inbound.Close()
	read := &tcpReadStartedConn{Conn: lease.TrackIncoming(inbound), started: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- attachTCP(WithConnectionLease(ctx, lease), nil, a, b.Device, read, true) }()
	select {
	case <-read.started:
	case <-ctx.Done():
		t.Fatal("QUIC did not start reading the incoming connection")
	}
	if out := policy.Reserve(b.Device.Principal(), b.Device, true); out != nil {
		out.Release()
		t.Error("identified incoming handshake did not suppress a crossed outgoing attempt")
	}
	if lease.WasReady() {
		t.Error("a preface scheduling hint bypassed TLS authentication")
	}
	cancel()
	if err := <-done; err == nil {
		t.Error("silent peer was admitted")
	}
	lease.Release()
	if out := policy.Reserve(b.Device.Principal(), b.Device, true); out == nil {
		t.Error("failed incoming handshake retained outgoing suppression")
	} else {
		out.Release()
	}
}

func TestTCPAcceptPreservesHandshakeFailure(t *testing.T) {
	a, b := testCredentials(t)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	raw, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	inbound, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer inbound.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// The connected peer sends no QUIC handshake. A nil Node must never be
	// reached, and the original accept failure must remain available to callers.
	err = attachTCP(ctx, nil, a, b.Device, inbound, true)
	if err == nil || errors.Unwrap(err) == nil {
		t.Fatalf("accept failure lost its cause: %v", err)
	}
}
