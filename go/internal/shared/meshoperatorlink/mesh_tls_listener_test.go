package meshoperatorlink

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestMeshTLSListenerCancellationAndIdleCleanup(t *testing.T) {
	l := NewMeshTLSListener()
	defer l.Close()
	for range 100 {
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { _, err := l.DialContext(ctx); result <- err }()
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	l.mu.Lock()
	count := len(l.pipes)
	l.mu.Unlock()
	if count != 0 {
		t.Fatal("canceled handoffs retained pipes", count)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	dialed := make(chan net.Conn, 1)
	go func() { c, _ := l.DialContext(ctx); dialed <- c }()
	server, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	client := <-dialed
	if client == nil {
		t.Fatal("local handoff failed")
	}
	if _, _, err := AgentTransportCredentials(nil).ServerHandshake(server); err == nil {
		t.Fatal("ordinary inner TLS pipe inherited operator attachment identity")
	}
	written := make(chan error, 1)
	go func() { _, err := client.Write([]byte("TLS bytes still require a handshake")); written <- err }()
	data := make([]byte, len("TLS bytes still require a handshake"))
	if _, err := io.ReadFull(server, data); err != nil {
		t.Fatal(err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("listener shutdown left active pipe open")
	}
	if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
	if _, err := l.DialContext(ctx); !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
	l.mu.Lock()
	count = len(l.pipes)
	l.mu.Unlock()
	if count != 0 {
		t.Fatal("listener shutdown retained pipes", count)
	}
}

func TestMeshTLSListenerClosingEitherEndReleasesOwnership(t *testing.T) {
	l := NewMeshTLSListener()
	defer l.Close()
	for range 100 {
		result := make(chan net.Conn, 1)
		go func() { c, _ := l.DialContext(context.Background()); result <- c }()
		server, err := l.Accept()
		if err != nil {
			t.Fatal(err)
		}
		client := <-result
		_ = server.Close()
		if _, err := client.Write([]byte{1}); err == nil {
			t.Fatal("server close did not close corresponding dial endpoint")
		}
	}
	l.mu.Lock()
	count := len(l.pipes)
	l.mu.Unlock()
	if count != 0 {
		t.Fatal("closed handoffs retained ownership", count)
	}
}

func TestMeshTLSListenerOrdinaryCloseDoesNotRevokeAcceptedBytes(t *testing.T) {
	l := NewMeshTLSListener()
	defer l.Close()
	dialed := make(chan net.Conn, 1)
	go func() { c, _ := l.DialContext(context.Background()); dialed <- c }()
	server, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	client := (<-dialed).(*meshTLSConn)
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-client.Closed():
	default:
		t.Fatal("ordinary full close was not reported")
	}
	select {
	case <-client.Done():
		t.Fatal("ordinary EOF was advertised as owner revocation")
	default:
	}
	l.Close()
	select {
	case <-client.Done():
	default:
		t.Fatal("owner shutdown did not revoke its stream")
	}
}
