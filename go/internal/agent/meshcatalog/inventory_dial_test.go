package meshcatalog

import (
	"context"
	"crypto/tls"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

func TestInventoryColdRetryConsumesActualTCPAndTLSFailures(t *testing.T) {
	for _, tlsFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "TCP refused", true: "TLS wrong pinned asset"}[tlsFailure], func(t *testing.T) {
			f := newFixture(t)
			c, _ := f.newCatalog(t, 533, "default", nil, nil)
			view := bridgeRouteView(t, f.now, 535)
			r, _ := NewRuntime(c, func() localmesh.NodeSnapshot { return view })
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			address := listener.Addr().String()
			serverDone := make(chan error, 1)
			if tlsFailure {
				// A real TLS handshake presents asset534 while the client is pinned535.
				wrong, _ := f.newCatalog(t, 534, "default", nil, nil)
				server, _ := NewRuntime(wrong, func() localmesh.NodeSnapshot { return localmesh.NodeSnapshot{} })
				go func() {
					raw, e := listener.Accept()
					if e != nil {
						serverDone <- e
						return
					}
					defer raw.Close()
					_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
					serverDone <- tls.Server(raw, server.serverTLS()).Handshake()
				}()
			} else {
				_ = listener.Close()
			}
			var dials atomic.Int32
			r.dialPeer = func(ctx context.Context, self, peer netip.Addr) (net.Conn, error) {
				dials.Add(1)
				return (&net.Dialer{}).DialContext(ctx, "tcp4", address)
			}
			r.inventoryFailed(535, false, time.Now()) // one stale-inventory cold retry owed
			r.outbound(context.Background(), 535)
			if dials.Load() != 1 {
				t.Fatalf("dials=%d", dials.Load())
			}
			if tlsFailure {
				select {
				case e := <-serverDone:
					if e == nil {
						t.Fatal("wrong peer TLS unexpectedly succeeded")
					}
				case <-time.After(time.Second):
					t.Fatal("TLS server did not finish")
				}
			}
			if _, allowed := r.inventoryAttempt(535, time.Now()); allowed {
				t.Fatal("failed TCP/TLS left a reusable cold attempt")
			}
			// Both direct outbound entry and the periodic discover path must avoid a
			// connection, not merely reject /3 after completing another TLS handshake.
			r.outbound(context.Background(), 535)
			r.discover(context.Background(), time.Now())
			time.Sleep(30 * time.Millisecond)
			if dials.Load() != 1 {
				t.Fatal("cooldown still made TCP/TLS connections")
			}
			r.mu.Lock()
			inflight := len(r.dialing)
			r.mu.Unlock()
			if inflight != 0 {
				t.Fatal("cooldown scheduled a dial goroutine")
			}
		})
	}
}

func TestInventoryReservedColdDialPreservesLegacyFallback(t *testing.T) {
	for _, protocol := range []string{syncALPNv2, syncALPN} {
		t.Run(protocol, func(t *testing.T) {
			f := newFixture(t)
			clientCatalog, _ := f.newCatalog(t, 533, "default", nil, nil)
			serverCatalog, _ := f.newCatalog(t, 535, "default", nil, nil)
			cv, sv := bridgeRouteView(t, f.now, 535), bridgeRouteView(t, f.now, 533)
			client, _ := NewRuntime(clientCatalog, func() localmesh.NodeSnapshot { return cv })
			server, _ := NewRuntime(serverCatalog, func() localmesh.NodeSnapshot { return sv })
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			serverDone := make(chan error, 1)
			serverAuthenticated := make(chan struct{})
			go func() {
				raw, e := listener.Accept()
				if e != nil {
					serverDone <- e
					return
				}
				defer raw.Close()
				config := server.serverTLS()
				config.NextProtos = []string{protocol}
				conn := tls.Server(raw, config)
				if e = conn.HandshakeContext(ctx); e == nil {
					close(serverAuthenticated)
					server.sessionNegotiated(ctx, 533, conn, conn.ConnectionState().NegotiatedProtocol)
				}
				serverDone <- e
			}()
			client.dialPeer = func(ctx context.Context, self, peer netip.Addr) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp4", listener.Addr().String())
			}
			client.inventoryFailed(535, false, time.Now())
			done := make(chan struct{})
			go func() { defer close(done); client.outbound(ctx, 535) }()
			deadline := time.Now().Add(3 * time.Second)
			for !client.ProjectionReady(535) && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if !client.ProjectionReady(535) {
				t.Fatal("reserved retry broke legacy fallback")
			}
			if cold, allowed := client.inventoryAttempt(535, time.Now()); cold || !allowed {
				t.Fatal("successful legacy fallback retained inventory retry")
			}
			select {
			case <-serverAuthenticated:
			case <-time.After(time.Second):
				t.Fatal("server handshake did not complete")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("client session leaked")
			}
			select {
			case e := <-serverDone:
				if e != nil {
					t.Fatal(e)
				}
			case <-time.After(time.Second):
				t.Fatal("server session leaked")
			}
		})
	}
}

func TestInventoryInboundCooldownRejectsApplicationBeforeInventory(t *testing.T) {
	f := newFixture(t)
	c, _ := f.newCatalog(t, 535, "default", nil, nil)
	r, _ := NewRuntime(c, func() localmesh.NodeSnapshot { return localmesh.NodeSnapshot{} })
	r.inventoryFailed(533, true, time.Now())
	// This entry point is called only after inbound mTLS identity verification.
	// Identifying/rate limiting unauthenticated addresses is not part of inventory.
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	done := make(chan struct{})
	go func() { defer close(done); r.sessionNegotiated(context.Background(), 533, left, syncALPNv3) }()
	_ = right.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	n, err := right.Read(b[:])
	if n != 0 || err == nil {
		t.Fatal("cooling inbound peer received application data")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cooling inbound peer allocated a session")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.peers) != 0 {
		t.Fatal("cooling inbound peer registered")
	}
}
