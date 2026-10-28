package meshsession

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"io"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

func startTLSAppTest(t *testing.T, credentials *localmesh.Credentials, auth Authorizer) (*Server, netip.AddrPort) {
	t.Helper()
	server, err := NewServer(credentials, auth)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.RunTLS(ctx, "127.0.0.1:0") }()
	t.Cleanup(func() {
		cancel()
		server.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("TLS workers did not stop")
		}
	})
	until := time.Now().Add(3 * time.Second)
	for server.TLSAddr() == nil {
		if time.Now().After(until) {
			t.Fatal("TLS listener did not start")
		}
		time.Sleep(time.Millisecond)
	}
	return server, netip.MustParseAddrPort(server.TLSAddr().String())
}
func tlsEchoApp(t *testing.T) uint16 {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	return uint16(listener.Addr().(*net.TCPAddr).Port)
}
func tlsEcho(t *testing.T, conn net.Conn, payload []byte) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	sent := make(chan error, 1)
	go func() { _, err := conn.Write(payload); sent <- err }()
	received := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, received); err != nil {
		t.Fatal(err)
	}
	if err := <-sent; err != nil || !bytes.Equal(payload, received) {
		t.Fatal("TLS echo mismatch", err)
	}
}
func TestTLSAppAuthorizationIdentityAndContextLifetime(t *testing.T) {
	a, b, c := fixtureCredentialsWithThird(t)
	port := tlsEchoApp(t)
	auth := &countingAuthorizer{port: port}
	server, address := startTLSAppTest(t, b, auth)
	client, err := NewClient(a)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	flow, err := client.DialTLS(ctx, b.Device, address, port)
	if err != nil {
		t.Fatal(err)
	}
	defer flow.Close()
	cancel() // A successful stream outlives the context used only for its setup.
	payload := bytes.Repeat([]byte("authenticated TLS app bytes"), 6000)
	tlsEcho(t, flow, payload)
	if _, err := client.DialTLS(context.Background(), b.Device, address, port+1); !errors.Is(err, ErrDenied) {
		t.Fatal("port denial lost", err)
	}
	if _, err := client.DialTLS(context.Background(), c.Device, address, port); err == nil {
		t.Fatal("wrong device pin accepted")
	}
	if auth.calls.Load() != 2 {
		t.Fatal("unauthenticated peer reached authorizer", auth.calls.Load())
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	var byte [1]byte
	if _, err := flow.Read(byte[:]); err == nil {
		t.Fatal("closed server left active TLS app stream")
	}
}
func TestTLSAppHalfClosePreservesResponse(t *testing.T) {
	a, b := fixtureCredentials(t)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		request, _ := io.ReadAll(conn)
		_, _ = conn.Write(append([]byte("received:"), request...))
	}()
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	_, address := startTLSAppTest(t, b, allowPort(func(p uint16) bool { return p == port }))
	client, _ := NewClient(a)
	defer client.Close()
	flow, err := client.DialTLS(context.Background(), b.Device, address, port)
	if err != nil {
		t.Fatal(err)
	}
	defer flow.Close()
	_, err = flow.Write([]byte("request"))
	if err != nil {
		t.Fatal(err)
	}
	if err := flow.(*tlsAppConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = flow.SetReadDeadline(time.Now().Add(3 * time.Second))
	reply, err := io.ReadAll(flow)
	if err != nil || string(reply) != "received:request" {
		t.Fatal(string(reply), err)
	}
}
func TestTLSAppClientShutdownAndCredentialExpiry(t *testing.T) {
	for _, expiry := range []bool{false, true} {
		t.Run(map[bool]string{false: "shutdown", true: "expiry"}[expiry], func(t *testing.T) {
			aExpiry := time.Now().Add(time.Hour)
			if expiry {
				aExpiry = time.Now().Add(3 * time.Second)
			}
			a, b := fixtureCredentialExpiry(t, aExpiry, time.Now().Add(time.Hour))
			port := tlsEchoApp(t)
			_, address := startTLSAppTest(t, b, allowPort(func(p uint16) bool { return p == port }))
			client, _ := NewClient(a)
			defer client.Close()
			flow, err := client.DialTLS(context.Background(), b.Device, address, port)
			if err != nil {
				t.Fatal(err)
			}
			defer flow.Close()
			tlsEcho(t, flow, []byte("alive"))
			_ = flow.SetDeadline(time.Now().Add(8 * time.Second))
			if !expiry {
				_ = client.Close()
			}
			started := time.Now()
			var byte [1]byte
			if _, err := flow.Read(byte[:]); err == nil {
				t.Fatal("stream survived shutdown/expiry")
			}
			if time.Since(started) > 5*time.Second {
				t.Fatal("caller deadline extended credential expiry")
			}
		})
	}
}
func TestTLSAppExpiryBeforePortRequest(t *testing.T) {
	a, b := fixtureCredentialExpiry(t, time.Now().Add(3*time.Second), time.Now().Add(time.Hour))
	auth := &countingAuthorizer{port: 1234}
	_, address := startTLSAppTest(t, b, auth)
	config, err := a.PeerTLS(b.Device)
	if err != nil {
		t.Fatal(err)
	}
	config.NextProtos = []string{TLSALPN}
	raw, err := net.Dial("tcp", address.String())
	if err != nil {
		t.Fatal(err)
	}
	secure := tls.Client(raw, config)
	defer secure.Close()
	if err := secure.Handshake(); err != nil {
		t.Fatal(err)
	}
	_ = secure.SetReadDeadline(time.Now().Add(6 * time.Second))
	var byte [1]byte
	if _, err := secure.Read(byte[:]); err == nil {
		t.Fatal("expired peer could stall before its port request")
	}
	if auth.calls.Load() != 0 {
		t.Fatal("expired handshake reached authorizer")
	}
}
func TestTLSAppMissingALPNAndSetupCancellation(t *testing.T) {
	a, b := fixtureCredentials(t)
	auth := &countingAuthorizer{port: 1234}
	_, address := startTLSAppTest(t, b, auth)
	config, _ := a.PeerTLS(b.Device)
	config.NextProtos = nil
	raw, err := net.Dial("tcp", address.String())
	if err != nil {
		t.Fatal(err)
	}
	secure := tls.Client(raw, config)
	if err := secure.Handshake(); err == nil {
		_ = secure.SetReadDeadline(time.Now().Add(time.Second))
		var byte [1]byte
		if _, err := secure.Read(byte[:]); err == nil {
			t.Fatal("missing ALPN authorized")
		}
	}
	secure.Close()
	if auth.calls.Load() != 0 {
		t.Fatal("missing ALPN reached authorizer")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ready := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	server, _ := NewServer(b, allowPort(func(uint16) bool { return false }))
	defer server.Close()
	cfg := server.TLSConfig()
	cfg.NextProtos = []string{TLSALPN}
	cfg.SessionTicketsDisabled = true
	var requests atomic.Int32
	go func() {
		raw, e := listener.Accept()
		if e != nil {
			return
		}
		conn := tls.Server(raw, cfg)
		defer conn.Close()
		if conn.Handshake() != nil {
			return
		}
		var hello [6]byte
		if _, e = io.ReadFull(conn, hello[:]); e != nil {
			return
		}
		requests.Add(1)
		close(ready)
		<-release
	}()
	client, _ := NewClient(a)
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		flow, e := client.DialTLS(ctx, b.Device, netip.MustParseAddrPort(listener.Addr().String()), 1234)
		if flow != nil {
			flow.Close()
		}
		result <- e
	}()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("port request never arrived")
	}
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled setup succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not wake ACK wait")
	}
	if requests.Load() != 1 {
		t.Fatal("duplicate setup attempts")
	}
}
