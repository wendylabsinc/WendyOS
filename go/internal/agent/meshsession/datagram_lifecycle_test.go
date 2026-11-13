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
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
)

func udpLifecycleApp(t *testing.T, r *meshingress.Registry, owner string) uint16 {
	t.Helper()
	app, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() })
	port := uint16(app.LocalAddr().(*net.UDPAddr).Port)
	if err := r.ClaimUDPForApp(owner, owner, port, "10.3.0.2", port); err != nil {
		t.Fatal(err)
	}
	go func() {
		buf := make([]byte, MaxUDPMessageSize+1)
		for {
			n, from, err := app.ReadFromUDP(buf)
			if err != nil {
				return
			}
			app.WriteToUDP(buf[:n], from)
		}
	}()
	return port
}

type udpLifecycleAuthorization struct{ testUDPAuthorization }

func (a udpLifecycleAuthorization) DialAuthorized(port uint16, dial func(string) (net.Conn, error)) (net.Conn, error) {
	return (&loopbackRegistry{Registry: a.Registry}).DialAuthorized(port, dial)
}

func TestUDPConnectionOwnsOneFlowAndCannotMixTCP(t *testing.T) {
	a, b := fixtureCredentials(t)
	r := meshingress.NewRegistry()
	p1, p2 := udpLifecycleApp(t, r, "one"), udpLifecycleApp(t, r, "two")
	tcp := lifecycleEcho(t)
	if err := r.Claim("tcp", tcp, "10.3.0.2", tcp); err != nil {
		t.Fatal(err)
	}
	endpoint := lifecycleServer(t, b, udpLifecycleAuthorization{testUDPAuthorization{r}}, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	flow, err := DialUDP(ctx, a, b.Device, endpoint, p1)
	if err != nil {
		t.Fatal(err)
	}
	defer flow.Close()
	if s, err := requestAppPort(ctx, flow.conn, p2, udpOpenMagic); !errors.Is(err, ErrDenied) {
		if s != nil {
			s.CancelRead(0)
			s.Close()
		}
		t.Fatalf("second UDP destination on one connection: %v", err)
	}
	if s, err := requestPort(ctx, flow.conn, tcp); !errors.Is(err, ErrDenied) {
		if s != nil {
			s.CancelRead(0)
			s.Close()
		}
		t.Fatalf("TCP on UDP connection: %v", err)
	}
	if err := flow.Send([]byte("original owner")); err != nil {
		t.Fatal(err)
	}
	if data, err := readUDPMessage(flow, ctx); err != nil || string(data) != "original owner" {
		t.Fatalf("original flow: %q %v", data, err)
	}
	tcpFlow, err := Dial(ctx, a, b.Device, endpoint, tcp)
	if err != nil {
		t.Fatal(err)
	}
	defer tcpFlow.Close()
	if s, err := requestAppPort(ctx, tcpFlow.(*streamConn).conn, p1, udpOpenMagic); !errors.Is(err, ErrDenied) {
		if s != nil {
			s.CancelRead(0)
			s.Close()
		}
		t.Fatalf("UDP on TCP connection: %v", err)
	}
	lifecycleExchange(t, tcpFlow)
	// Admitted UDP ownership is decided across concurrently opened controls.
	cfg, err := a.PeerTLSWithTickets(b.Device, ALPN, "app-quic")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := quic.DialAddr(ctx, endpoint.String(), cfg, quicConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "done")
	// A denied control does not acquire the connection's transport ownership.
	if _, err := requestPort(ctx, conn, 0); !errors.Is(err, ErrDenied) {
		t.Fatalf("unpublished TCP port: %v", err)
	}
	results := make(chan error, 2)
	for _, p := range []uint16{p1, p2} {
		go func(port uint16) {
			s, err := requestAppPort(ctx, conn, port, udpOpenMagic)
			if s != nil {
				s.CancelRead(0)
				s.Close()
			}
			results <- err
		}(p)
	}
	success, denied := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			success++
		} else if errors.Is(err, ErrDenied) {
			denied++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || denied != 1 {
		t.Fatalf("concurrent controls admitted=%d denied=%d", success, denied)
	}
}

func TestUDPPortACKHonorsCancellationWithoutDeadline(t *testing.T) {
	for _, action := range []string{"context", "close", "invalidate", "route"} {
		t.Run(action, func(t *testing.T) {
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
			serverCtx, stopServer := context.WithTimeout(context.Background(), 5*time.Second)
			defer stopServer()
			requested := make(chan struct{})
			accepted := make(chan *quic.Conn, 1)
			go func() {
				conn, err := listener.Accept(serverCtx)
				if err != nil {
					return
				}
				accepted <- conn
				defer conn.CloseWithError(0, "done")
				stream, err := conn.AcceptStream(serverCtx)
				if err != nil {
					return
				}
				var hello [6]byte
				if _, err := io.ReadFull(stream, hello[:]); err != nil {
					return
				}
				close(requested)
				<-conn.Context().Done() // Withhold the ACK until the client cancels.
			}()
			client := lifecycleClient(t, a)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				dial := func() (*DatagramFlow, error) {
					endpoint := netip.MustParseAddrPort(listener.Addr().String())
					if action == "context" {
						return DialUDP(ctx, a, b.Device, endpoint, 7777)
					}
					return client.DialUDP(ctx, b.Device, endpoint, 7777)
				}
				flow, err := dial()
				if flow != nil {
					flow.Close()
				}
				result <- err
			}()
			select {
			case <-requested:
			case <-serverCtx.Done():
				t.Fatal("UDP request did not arrive")
			}
			conn := <-accepted
			defer conn.CloseWithError(0, "cleanup")
			switch action {
			case "context":
				cancel()
			case "close":
				client.Close()
			case "invalidate":
				client.Invalidate(b.Device)
			case "route":
				client.PruneRoutes(localmesh.NodeSnapshot{})
			}
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled UDP setup: %v", err)
				}
			case <-time.After(time.Second):
				conn.CloseWithError(0, "cleanup")
				<-result
				t.Fatal("UDP port ACK ignored cancellation")
			}
			requireSessionClosed(t, conn)
		})
	}
}

func TestManagedUDPRevokedOnClientCloseAndRouteCut(t *testing.T) {
	for _, action := range []string{"close", "invalidate", "route"} {
		t.Run(action, func(t *testing.T) {
			a, b := fixtureCredentials(t)
			r := meshingress.NewRegistry()
			port := udpLifecycleApp(t, r, "udp")
			endpoint := lifecycleServer(t, b, testUDPAuthorization{r}, true)
			client := lifecycleClient(t, a)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			first, err := client.DialUDP(ctx, b.Device, endpoint, port)
			if err != nil {
				t.Fatal(err)
			}
			defer first.Close()
			second, err := client.DialUDP(ctx, b.Device, endpoint, port)
			if err != nil {
				t.Fatal(err)
			}
			defer second.Close()
			if err := first.Send([]byte("echo")); err != nil {
				t.Fatal(err)
			}
			if _, err := readUDPMessage(first, ctx); err != nil {
				t.Fatal(err)
			}
			switch action {
			case "close":
				client.Close()
			case "invalidate":
				client.Invalidate(b.Device)
			case "route":
				client.PruneRoutes(localmesh.NodeSnapshot{})
			}
			requireSessionClosed(t, first.conn)
			requireSessionClosed(t, second.conn)
			fresh, err := client.DialUDP(ctx, b.Device, endpoint, port)
			if action == "close" {
				if fresh != nil {
					fresh.Close()
				}
				if !errors.Is(err, net.ErrClosed) {
					t.Fatalf("closed client dial: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer fresh.Close()
				if err := fresh.Send([]byte("restored")); err != nil {
					t.Fatal(err)
				}
				if got, err := readUDPMessage(fresh, ctx); err != nil || string(got) != "restored" {
					t.Fatalf("recovery %q %v", got, err)
				}
			}
		})
	}
}

func TestUDPReplyFailureClosesDedicatedConnection(t *testing.T) {
	a, b := fixtureCredentials(t)
	r := meshingress.NewRegistry()
	app, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	port := uint16(app.LocalAddr().(*net.UDPAddr).Port)
	if err := r.ClaimUDPForApp("app", "app", port, "10.3.0.2", port); err != nil {
		t.Fatal(err)
	}

	endpoint := lifecycleServer(t, b, testUDPAuthorization{r}, true)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	flow, err := DialUDP(ctx, a, b.Device, endpoint, port)
	if err != nil {
		t.Fatal(err)
	}
	defer flow.Close()
	_ = app.Close()
	if err := flow.Send([]byte("request")); err != nil {
		t.Fatal(err)
	}
	requireSessionClosed(t, flow.conn)
}
