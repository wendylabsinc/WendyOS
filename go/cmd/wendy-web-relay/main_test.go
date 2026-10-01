package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestBrokerEndpoint(t *testing.T) {
	for _, endpoint := range []string{"relay.dev.wendy.sh:443", "eu.relay.wendy.sh:443", "https://wendy-cloud-dev-tunnel-broker-nkohwk7hda-uc.a.run.app"} {
		if _, err := brokerEndpoint(endpoint); err != nil {
			t.Fatalf("reject valid broker %q: %v", endpoint, err)
		}
	}
	for _, endpoint := range []string{"localhost:443", "127.0.0.1:443", "relay.wendy.sh:22", "relay.wendy.sh.attacker.test:443", "user@relay.wendy.sh:443", "relay.wendy.sh:443/path", "relay.wendy.sh:443?target=localhost", "relay.wendy.sh:443#fragment"} {
		if _, err := brokerEndpoint(endpoint); err == nil {
			t.Errorf("accepted forbidden destination %q", endpoint)
		}
	}
}

func TestRelayUpstreamFailureBeforeUpgrade(t *testing.T) {
	// A closed local listener provides a refused connection without DNS or TLS.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := listener.Addr().String()
	listener.Close()
	server := httptest.NewServer(relayHandler(target, false, "http://localhost:5173"))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, response, err := websocket.Dial(ctx, server.URL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {"http://localhost:5173"}},
	})
	if ws != nil {
		ws.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusBadGateway {
		t.Fatalf("failed upstream must reject the handshake with 502: response=%v, err=%v", response, err)
	}
}

func TestRelayRejectsOriginBeforeDial(t *testing.T) {
	handler := relayHandler("invalid destination", true, "http://localhost:5173")
	for _, origin := range []string{"", "http://localhost:5174", "https://untrusted.example"} {
		request := httptest.NewRequest(http.MethodGet, "/cloud", nil)
		request.RemoteAddr = "127.0.0.1:12345"
		request.Host = "127.0.0.1:8788"
		request.Header.Set("Origin", origin)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("origin %q: got %d, want 403", origin, response.Code)
		}
	}
}

func TestRelayRequiresLoopbackAddress(t *testing.T) {
	for _, address := range []string{"127.0.0.1:8788", "[::1]:8788"} {
		if !loopbackAddress(address) {
			t.Errorf("rejected loopback address %q", address)
		}
	}
	for _, address := range []string{":8788", "0.0.0.0:8788", "[::]:8788", "192.0.2.1:8788", "localhost:8788", "attacker.test:8788", "127.0.0.1"} {
		if loopbackAddress(address) {
			t.Errorf("accepted unsafe listen address %q", address)
		}
	}
}

func TestRelayRejectsRemoteRequestsWithSpoofedOrigin(t *testing.T) {
	handler := relayHandler("invalid destination", true, "http://localhost:5173")
	for _, tc := range []struct {
		name, remote, host, forwardedHeader string
	}{
		{"remote peer", "192.0.2.1:12345", "127.0.0.1:8788", ""},
		{"remote IPv6 peer", "[2001:db8::1]:12345", "[::1]:8788", ""},
		{"DNS rebinding", "127.0.0.1:12345", "attacker.test:8788", ""},
		{"forwarded", "127.0.0.1:12345", "127.0.0.1:8788", "Forwarded"},
		{"forwarded for", "127.0.0.1:12345", "127.0.0.1:8788", "X-Forwarded-For"},
		{"forwarded host", "127.0.0.1:12345", "127.0.0.1:8788", "X-Forwarded-Host"},
		{"forwarded proto", "127.0.0.1:12345", "127.0.0.1:8788", "X-Forwarded-Proto"},
		{"real IP", "127.0.0.1:12345", "127.0.0.1:8788", "X-Real-IP"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/cloud", nil)
			request.RemoteAddr, request.Host = tc.remote, tc.host
			request.Header.Set("Origin", "http://localhost:5173")
			if tc.forwardedHeader != "" {
				request.Header.Set(tc.forwardedHeader, "192.0.2.1")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden {
				t.Fatalf("got %d, want 403 before upstream dial", response.Code)
			}
		})
	}
}

func TestRelayForwardsBytesAndClosesUpstream(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = io.Copy(conn, conn)
	}()
	server := httptest.NewServer(relayHandler(listener.Addr().String(), false, "http://localhost:5173"))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, server.URL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {"http://localhost:5173"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	// Exceed WebSocket's default message limit, as large gRPC frames can do.
	payload := bytes.Repeat([]byte("relay bytes"), 10000)
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("relay changed the byte stream")
	}
	conn.Close()
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("browser disconnect left the upstream open")
	}
}
