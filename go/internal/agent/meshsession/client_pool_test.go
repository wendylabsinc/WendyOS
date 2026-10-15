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
	"github.com/wendylabsinc/WendyOS/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

type countingAuthorizer struct {
	port  uint16
	calls atomic.Int32
}

type authorizerFunc func(uint16, func() (net.Conn, error)) (net.Conn, error)

func (f authorizerFunc) DialAuthorized(port uint16, dial func() (net.Conn, error)) (net.Conn, error) {
	return f(port, dial)
}

type countingPacketConn struct {
	net.PacketConn
	written atomic.Int64
}

func (c *countingPacketConn) WriteTo(packet []byte, addr net.Addr) (int, error) {
	n, err := c.PacketConn.WriteTo(packet, addr)
	c.written.Add(int64(n))
	return n, err
}

func (a *countingAuthorizer) DialAuthorized(port uint16, dial func() (net.Conn, error)) (net.Conn, error) {
	a.calls.Add(1)
	if port != a.port {
		return nil, ErrDenied
	}
	return dial()
}

func TestPooledAppFlowsReauthorizeAndRouteCut(t *testing.T) {
	a, b := fixtureCredentials(t)
	app, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	go func() {
		for {
			conn, err := app.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	port := uint16(app.Addr().(*net.TCPAddr).Port)
	authorizer := &countingAuthorizer{port: port}
	server, err := NewServer(b, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, "127.0.0.1:0") }()
	var endpoint netip.AddrPort
	for i := 0; i < 100; i++ {
		if addr := server.Addr(); addr != nil {
			endpoint = netip.MustParseAddrPort(addr.String())
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !endpoint.IsValid() {
		t.Fatal("QUIC listener did not start")
	}
	client, err := NewClient(a)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	packets := &countingPacketConn{PacketConn: udp}
	// Reconnects reuse this socket to compare cold/warm wire bytes. A single
	// Transport must own its reader across every connection; repeated quic.Dial
	// calls would create competing single-use transports during close/drain.
	transport := &quic.Transport{Conn: packets}
	defer transport.Close()
	client.dial = func(ctx context.Context, address netip.AddrPort, tlsConfig *tls.Config, config *quic.Config) (*quic.Conn, error) {
		return transport.Dial(ctx, net.UDPAddrFromAddrPort(address), tlsConfig, config)
	}
	var firstSession any
	var coldBytes int64
	var coldSetup time.Duration

	// The production dialer resolves from the current signed manifest and
	// Babel route before calling Client.Dial; model that gate for each flow.
	host, _, _ := localmesh.Addresses(a.Org, b.Asset)
	router, _ := localmesh.RouterID(a.Org, b.Asset)
	snapshot := localmesh.NodeSnapshot{
		Devices: []localmesh.Manifest{{Org: a.Org, Asset: b.Asset, Expires: time.Now().Add(time.Minute).UnixMilli()}},
		Routes:  []babel.Route{{Prefix: netip.PrefixFrom(host, 32), RouterID: router}},
	}
	dial := func(port uint16) (net.Conn, error) {
		if _, err := Resolve(snapshot, a.Org, b.Asset); err != nil {
			return nil, err
		}
		return client.Dial(ctx, b.Asset, endpoint, port)
	}
	for i := 0; i < 2; i++ {
		started := time.Now()
		conn, err := dial(port)
		if err != nil {
			t.Fatal(err)
		}
		setup := time.Since(started)
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		if _, err := conn.Write([]byte("warm")); err != nil {
			t.Fatal(err)
		}
		var got [4]byte
		if _, err := io.ReadFull(conn, got[:]); err != nil || string(got[:]) != "warm" {
			t.Fatalf("echo = %q, %v", got, err)
		}
		_ = conn.Close()
		if i == 0 {
			coldSetup = setup
			coldBytes = packets.written.Load()
			client.mu.Lock()
			first := client.peers[b.Asset].conn
			client.mu.Unlock()
			if first == nil {
				t.Fatal("no reusable authenticated session")
			}
			firstSession = first
		} else {
			warmBytes := packets.written.Load() - coldBytes
			t.Logf("org64 fixture client UDP wire bytes: first flow=%d, reused flow=%d; setup: first=%s, reused=%s", coldBytes, warmBytes, coldSetup, setup)
			if warmBytes >= coldBytes {
				t.Fatalf("warm flow did not save handshake bytes: cold=%d warm=%d", coldBytes, warmBytes)
			}
			client.mu.Lock()
			second := client.peers[b.Asset].conn
			client.mu.Unlock()
			if firstSession != second {
				t.Fatal("warm flow repeated the QUIC handshake")
			}
		}
	}
	if _, err := dial(port + 1); !errors.Is(err, ErrDenied) {
		t.Fatalf("denied port on warm session = %v", err)
	}
	if authorizer.calls.Load() != 3 {
		t.Fatalf("each stream must be authorized, calls=%d", authorizer.calls.Load())
	}
	snapshot.Routes[0].Unreachable = true
	if _, err := dial(port); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("route cut reused pooled session: %v", err)
	}
	if authorizer.calls.Load() != 3 {
		t.Fatal("route cut reached remote authorizer")
	}
	// The route watcher must retire the old transport during the cut. Otherwise
	// a heal before QUIC's 45-second idle timeout reuses its dead path.
	cutAt := time.Now()
	client.PruneRoutes(snapshot)
	client.mu.Lock()
	entry := client.peers[b.Asset]
	client.mu.Unlock()
	if entry != nil {
		t.Fatal("route cut retained pooled session")
	}
	old := firstSession.(*quic.Conn)
	select {
	case <-old.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("route cut did not close old QUIC session promptly")
	}
	snapshot.Routes[0].Unreachable = false
	conn, err := dial(port)
	if err != nil {
		t.Fatalf("healed route did not reconnect: %v", err)
	}
	_ = conn.Close()
	if elapsed := time.Since(cutAt); elapsed >= clientIdleLimit {
		t.Fatalf("recovery waited for old session idle timeout: %s", elapsed)
	}
	client.mu.Lock()
	replacement := client.peers[b.Asset]
	client.mu.Unlock()
	if replacement == nil || replacement.conn == old {
		t.Fatal("healed route reused old QUIC session")
	}
	if authorizer.calls.Load() != 4 {
		t.Fatal("reconnected session did not reauthorize")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPooledPortACKTimeoutRetiresAndRetriesFreshSession(t *testing.T) {
	a, b := fixtureCredentials(t)
	app, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	go func() {
		for {
			conn, err := app.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	port := uint16(app.Addr().(*net.TCPAddr).Port)
	var calls atomic.Int32
	var stallAt atomic.Int32
	stallAt.Store(2)
	authorizer := authorizerFunc(func(got uint16, dial func() (net.Conn, error)) (net.Conn, error) {
		if got != port {
			return nil, ErrDenied
		}
		call := calls.Add(1)
		switch {
		case call == 1:
			// A cold BLE setup gets the caller's full budget.
			time.Sleep(120 * time.Millisecond)
		case call == stallAt.Load():
			// Simulate a peer process that accepted an old QUIC session but
			// cannot answer its next app-port request before the bound.
			time.Sleep(250 * time.Millisecond)
		}
		return dial()
	})
	server, err := NewServer(b, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, "127.0.0.1:0") }()
	var endpoint netip.AddrPort
	for i := 0; i < 100; i++ {
		if addr := server.Addr(); addr != nil {
			endpoint = netip.MustParseAddrPort(addr.String())
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !endpoint.IsValid() {
		t.Fatal("QUIC listener did not start")
	}
	client, err := NewClient(a)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.portACKLimit = 60 * time.Millisecond
	warm, err := client.Dial(ctx, b.Asset, endpoint, port)
	if err != nil {
		t.Fatal(err)
	}
	_ = warm.Close()
	client.mu.Lock()
	old := client.peers[b.Asset].conn
	client.mu.Unlock()
	start := time.Now()
	var retryChecks atomic.Int32
	recovered, err := client.DialWithRetryCheck(ctx, b.Asset, endpoint, port, func() error {
		retryChecks.Add(1)
		return nil
	})
	if err != nil {
		t.Fatalf("stalled pooled ACK did not retry fresh session: %v", err)
	}
	defer recovered.Close()
	if time.Since(start) >= time.Second {
		t.Fatal("recovery waited for QUIC idle timeout")
	}
	client.mu.Lock()
	replacement := client.peers[b.Asset].conn
	client.mu.Unlock()
	if replacement == old || calls.Load() != 3 || retryChecks.Load() != 1 {
		t.Fatalf("did not authenticate a fresh session: replacement=%t requests=%d route_checks=%d", replacement != old, calls.Load(), retryChecks.Load())
	}
	select {
	case <-old.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("stalled QUIC session was not closed")
	}
	_ = recovered.SetDeadline(time.Now().Add(time.Second))
	if _, err := recovered.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	var got [2]byte
	if _, err := io.ReadFull(recovered, got[:]); err != nil || string(got[:]) != "ok" {
		t.Fatalf("recovered echo = %q, %v", got, err)
	}
	_ = recovered.Close()
	// If the signed route is withdrawn while the cached stream stalls, a
	// recovery attempt must not create a fresh QUIC session using the old
	// caller-side route check.
	stallAt.Store(4)
	_, err = client.DialWithRetryCheck(ctx, b.Asset, endpoint, port, func() error {
		retryChecks.Add(1)
		return ErrNoRoute
	})
	if !errors.Is(err, ErrNoRoute) || calls.Load() != 4 || retryChecks.Load() != 2 {
		t.Fatalf("withdrawn route retried: err=%v requests=%d route_checks=%d", err, calls.Load(), retryChecks.Load())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
