package meshsession

import (
	"context"
	"crypto/tls"
	"errors"
	quic "github.com/quic-go/quic-go"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"
)

func lifecycleEcho(t *testing.T) uint16 {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
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
			go func() { defer conn.Close(); io.Copy(conn, conn) }()
		}
	}()
	return uint16(listener.Addr().(*net.TCPAddr).Port)
}
func lifecycleServer(t *testing.T, creds *localmesh.Credentials, auth Authorizer, guarded bool) netip.AddrPort {
	t.Helper()
	server, err := NewServer(creds, auth)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	listener, err := quic.ListenAddr("127.0.0.1:0", server.TLSConfig(), quicConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept(ctx)
			if err != nil {
				return
			}
			go func() {
				if guarded {
					server.serve(ctx, conn)
					return
				}
				defer conn.CloseWithError(0, "test done")
				go func() { <-ctx.Done(); conn.CloseWithError(0, "test done") }()
				for {
					stream, err := conn.AcceptStream(ctx)
					if err != nil {
						return
					}
					go server.serveStream(ctx, conn, stream)
				}
			}()
		}
	}()
	return netip.MustParseAddrPort(listener.Addr().String())
}
func lifecycleClient(t *testing.T, creds *localmesh.Credentials) *Client {
	t.Helper()
	client, err := NewClient(creds)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}
func lifecycleDial(t *testing.T, client *Client, peer int32, endpoint netip.AddrPort, port uint16) *streamConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := client.Dial(ctx, peer, endpoint, port)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn.(*streamConn)
}
func lifecycleExchange(t *testing.T, conn net.Conn) {
	t.Helper()
	conn.SetDeadline(time.Now().Add(time.Second))
	defer conn.SetDeadline(time.Time{})
	if _, err := conn.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	var got [2]byte
	if _, err := io.ReadFull(conn, got[:]); err != nil || string(got[:]) != "ok" {
		t.Fatalf("exchange %q %v", got, err)
	}
}
func requireSessionClosed(t *testing.T, conn *quic.Conn) {
	t.Helper()
	select {
	case <-conn.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("authenticated session remained open")
	}
}

func TestPooledAppTeardownRevokesOnlyItsStreams(t *testing.T) {
	a, b := fixtureCredentials(t)
	r := meshingress.NewRegistry()
	p1, p2 := lifecycleEcho(t), lifecycleEcho(t)
	if err := r.Claim("one", p1); err != nil {
		t.Fatal(err)
	}
	if err := r.Claim("two", p2); err != nil {
		t.Fatal(err)
	}
	endpoint := lifecycleServer(t, b, r, true)
	client := lifecycleClient(t, a)
	one := lifecycleDial(t, client, b.Asset, endpoint, p1)
	two := lifecycleDial(t, client, b.Asset, endpoint, p2)
	if one.conn != two.conn {
		t.Fatal("not a shared pooled connection")
	}
	lifecycleExchange(t, one)
	lifecycleExchange(t, two)
	r.Release("one")
	one.SetReadDeadline(time.Now().Add(time.Second))
	var got [1]byte
	if _, err := one.Read(got[:]); err == nil {
		t.Fatal("revoked stream survived")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("revoked stream remained open")
	}
	lifecycleExchange(t, two)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if conn, err := client.Dial(ctx, b.Asset, endpoint, p1); err == nil {
		conn.Close()
		t.Fatal("warm stream reused revoked grant")
	}
	if err := r.Claim("replacement", p1); err != nil {
		t.Fatal(err)
	}
	replacement := lifecycleDial(t, client, b.Asset, endpoint, p1)
	lifecycleExchange(t, replacement)
	if replacement.conn != two.conn {
		t.Fatal("app teardown unnecessarily replaced shared session")
	}
}

func TestRetiredActivePoolSessionRevokedOnCloseAndRouteCut(t *testing.T) {
	for _, action := range []string{"close", "route"} {
		t.Run(action, func(t *testing.T) {
			a, b := fixtureCredentials(t)
			port := lifecycleEcho(t)
			endpoint := lifecycleServer(t, b, allowPort(func(p uint16) bool { return p == port }), true)
			client := lifecycleClient(t, a)
			old := lifecycleDial(t, client, b.Asset, endpoint, port)
			client.mu.Lock()
			client.peers[b.Asset].born = time.Now().Add(-2 * clientAgeLimit)
			client.mu.Unlock()
			fresh := lifecycleDial(t, client, b.Asset, endpoint, port)
			if old.conn == fresh.conn {
				t.Fatal("age did not retire old session")
			}
			lifecycleExchange(t, old)
			lifecycleExchange(t, fresh)
			if action == "close" {
				client.Close()
			} else {
				client.PruneRoutes(localmesh.NodeSnapshot{})
			}
			requireSessionClosed(t, old.conn)
			requireSessionClosed(t, fresh.conn)
		})
	}
}

// The opposing endpoint deliberately omits lifetime enforcement, proving that
// each side independently closes active streams at either chain's expiry.
func TestAppSessionExpiryEnforcedAtBothEnds(t *testing.T) {
	for _, side := range []string{"client", "server"} {
		for _, expired := range []string{"local", "peer", "issuer"} {
			t.Run(side+"/"+expired, func(t *testing.T) {
				expires := time.Now().Truncate(time.Second).Add(3 * time.Second)
				aExpiry, bExpiry := time.Now().Add(time.Hour), time.Now().Add(time.Hour)
				if side == "client" && expired == "local" || side == "server" && expired == "peer" {
					aExpiry = expires
				} else {
					bExpiry = expires
				}
				var issuerExpiry []time.Time
				if expired == "issuer" {
					aExpiry, bExpiry = time.Now().Add(time.Hour), time.Now().Add(time.Hour)
					issuerExpiry = []time.Time{expires}
				}
				a, b := fixtureCredentialExpiry(t, aExpiry, bExpiry, issuerExpiry...)
				port := lifecycleEcho(t)
				endpoint := lifecycleServer(t, b, allowPort(func(p uint16) bool { return p == port }), side == "server")
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				var conn *quic.Conn
				if side == "client" {
					client := lifecycleClient(t, a)
					first := lifecycleDial(t, client, b.Asset, endpoint, port)
					conn = first.conn
					lifecycleExchange(t, first)
					warm := lifecycleDial(t, client, b.Asset, endpoint, port)
					if warm.conn != conn {
						t.Fatal("warm session not reused")
					}
					lifecycleExchange(t, warm)
					time.Sleep(time.Until(expires) + 20*time.Millisecond)
					requireSessionClosed(t, conn)
					if flow, err := client.Dial(ctx, b.Asset, endpoint, port); err == nil {
						flow.Close()
						t.Fatal("expired identity opened stream")
					}
				} else {
					cfg, err := a.PeerTLSWithTickets(b.Asset, ALPN, "app-quic")
					if err != nil {
						t.Fatal(err)
					}
					conn, err = quic.DialAddr(ctx, endpoint.String(), cfg, quicConfig())
					if err != nil {
						t.Fatal(err)
					}
					defer conn.CloseWithError(0, "done")
					stream, err := requestPort(ctx, conn, port)
					if err != nil {
						t.Fatal(err)
					}
					flow := &streamConn{Stream: stream, conn: conn}
					lifecycleExchange(t, flow)
					time.Sleep(time.Until(expires) + 20*time.Millisecond)
					requireSessionClosed(t, conn)
					if _, err := requestPort(ctx, conn, port); err == nil {
						t.Fatal("expired server session opened stream")
					}
				}
			})
		}
	}
}

func TestClosingPoolDuringSuccessfulDialClosesLateConnection(t *testing.T) {
	a, b := fixtureCredentials(t)
	port := lifecycleEcho(t)
	endpoint := lifecycleServer(t, b, allowPort(func(p uint16) bool { return p == port }), true)
	client := lifecycleClient(t, a)
	dial := client.dial
	established := make(chan *quic.Conn, 1)
	release := make(chan struct{})
	client.dial = func(ctx context.Context, address netip.AddrPort, cfg *tls.Config, qcfg *quic.Config) (*quic.Conn, error) {
		conn, err := dial(ctx, address, cfg, qcfg)
		if err != nil {
			return nil, err
		}
		established <- conn
		<-release
		return conn, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		flow, err := client.Dial(ctx, b.Asset, endpoint, port)
		if flow != nil {
			flow.Close()
		}
		done <- err
	}()
	var conn *quic.Conn
	select {
	case conn = <-established:
	case <-ctx.Done():
		t.Fatal("dial did not connect")
	}
	client.Close()
	close(release)
	if err := <-done; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("late dial was accepted: %v", err)
	}
	requireSessionClosed(t, conn)
}

func TestRegistryWrappedTCPPreservesHalfClose(t *testing.T) {
	a, b := fixtureCredentials(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
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
		body, err := io.ReadAll(conn)
		if err == nil {
			conn.Write(append([]byte("reply:"), body...))
		}
	}()
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	registry := meshingress.NewRegistry()
	if err := registry.Claim("half", port); err != nil {
		t.Fatal(err)
	}
	endpoint := lifecycleServer(t, b, registry, true)
	client := lifecycleClient(t, a)
	flow := lifecycleDial(t, client, b.Asset, endpoint, port)
	flow.SetDeadline(time.Now().Add(time.Second))
	if _, err := flow.Write([]byte("body")); err != nil {
		t.Fatal(err)
	}
	if err := flow.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(flow)
	if err != nil || string(body) != "reply:body" {
		t.Fatalf("half-close response=%q err=%v", body, err)
	}
}
