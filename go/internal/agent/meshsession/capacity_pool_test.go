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
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
)

// The control streams for 32 UDP flows plus eight TCP streams consume all
// advertised v2 bidi stream credits. Waiting for a ninth TCP or 33rd UDP stream
// is local capacity pressure, not evidence that the shared session is stale.
func TestPoolFullMixedStreamCapacityPreservesEstablishedFlows(t *testing.T) {
	a, b := fixtureCredentials(t)
	registry := meshingress.NewRegistry()
	udpPort := udpLifecycleApp(t, registry, "full-budget-udp")
	tcpPort := lifecycleEcho(t)
	if err := registry.Claim("full-budget-tcp", tcpPort, "10.3.0.2", tcpPort); err != nil {
		t.Fatal(err)
	}
	address := lifecycleServer(t, b, udpLifecycleAuthorization{testUDPAuthorization{registry}}, true)
	client := lifecycleClient(t, a)
	var dials atomic.Int32
	originalDial := client.dial
	client.dial = func(ctx context.Context, endpoint netip.AddrPort, config *tls.Config, options *quic.Config) (*quic.Conn, error) {
		dials.Add(1)
		return originalDial(ctx, endpoint, config, options)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	udpFlows := make([]*DatagramFlow, 0, udpmessage.PeerFlows)
	defer func() {
		for _, flow := range udpFlows {
			flow.Close()
		}
	}()
	for i := range udpmessage.PeerFlows {
		flow, err := client.DialUDP(ctx, b.Device, address, udpPort)
		if err != nil {
			t.Fatalf("UDP flow %d of %d: %v", i+1, udpmessage.PeerFlows, err)
		}
		udpFlows = append(udpFlows, flow)
	}
	tcpFlows := make([]net.Conn, 0, 8)
	defer func() {
		for _, flow := range tcpFlows {
			flow.Close()
		}
	}()
	for i := range 8 {
		flow, err := client.Dial(ctx, b.Device, address, tcpPort)
		if err != nil {
			t.Fatalf("TCP flow %d with full UDP budget: %v", i+1, err)
		}
		tcpFlows = append(tcpFlows, flow)
	}
	connection := udpFlows[0].conn
	client.portACKLimit = 100 * time.Millisecond
	assertHealthy := func() {
		t.Helper()
		if connection.Context().Err() != nil || dials.Load() != 1 {
			t.Fatalf("capacity wait retired healthy session: context=%v, handshakes=%d", connection.Context().Err(), dials.Load())
		}
		for i, flow := range tcpFlows {
			if flow.(*streamConn).conn != connection {
				t.Fatalf("TCP sibling %d is not on the shared session", i)
			}
			lifecycleExchange(t, flow)
		}
		for i, flow := range udpFlows {
			if flow.conn != connection {
				t.Fatalf("UDP sibling %d is not on the shared session", i)
			}
			payload := []byte{byte(i), 'o', 'k'}
			if err := flow.Send(payload); err != nil {
				t.Fatalf("UDP sibling %d send: %v", i, err)
			}
			got, err := readUDPMessage(flow, ctx)
			if err != nil || string(got) != string(payload) {
				t.Fatalf("UDP sibling %d echo %q: %v", i, got, err)
			}
		}
	}
	assertHealthy()
	started := time.Now()
	extraTCP, err := client.Dial(ctx, b.Device, address, tcpPort)
	if extraTCP != nil {
		extraTCP.Close()
		t.Fatal("TCP overflow was admitted beyond all 40 held stream credits")
	}
	requireBoundedCapacityFailure(t, started, err)
	assertHealthy()
	started = time.Now()
	extraUDP, err := client.DialUDP(ctx, b.Device, address, udpPort)
	if extraUDP != nil {
		extraUDP.Close()
		t.Fatal("UDP overflow was admitted beyond all 40 held stream credits")
	}
	requireBoundedCapacityFailure(t, started, err)
	assertHealthy()

	// Peer MAX_STREAMS updates are asynchronous; retry bounded admissions until
	// the remote endpoint has observed the closed stream and returned its credit.
	tcpFlows[0].Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		fresh, err := client.Dial(ctx, b.Device, address, tcpPort)
		if err == nil {
			if fresh.(*streamConn).conn != connection {
				fresh.Close()
				t.Fatal("returned TCP credit opened another QUIC session")
			}
			tcpFlows[0] = fresh
			break
		}
		if time.Now().After(deadline) || connection.Context().Err() != nil || dials.Load() != 1 {
			t.Fatalf("closed TCP did not return credit on the same session: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	assertHealthy()
}

func TestPoolLegacyStreamCapacityPreservesEstablishedFlows(t *testing.T) {
	a, b := fixtureCredentials(t)
	port := lifecycleEcho(t)
	server, err := NewServer(b, allowPort(func(p uint16) bool { return p == port }))
	if err != nil {
		t.Fatal(err)
	}
	config := server.TLSConfig()
	config.NextProtos = []string{ALPN}
	options := quicConfig()
	options.MaxIncomingStreams = 8
	listener, err := quic.ListenAddr("127.0.0.1:0", config, options)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer listener.Close()
	defer server.Close()
	go func() {
		for {
			conn, err := listener.Accept(ctx)
			if err != nil {
				return
			}
			go server.serve(ctx, conn)
		}
	}()
	address := netip.MustParseAddrPort(listener.Addr().String())
	client := lifecycleClient(t, a)
	var dials atomic.Int32
	originalDial := client.dial
	client.dial = func(ctx context.Context, endpoint netip.AddrPort, config *tls.Config, options *quic.Config) (*quic.Conn, error) {
		dials.Add(1)
		return originalDial(ctx, endpoint, config, options)
	}
	flows := make([]net.Conn, 0, 8)
	defer func() {
		for _, flow := range flows {
			flow.Close()
		}
	}()
	for range 8 {
		flow, err := client.Dial(ctx, b.Device, address, port)
		if err != nil {
			t.Fatal(err)
		}
		flows = append(flows, flow)
	}
	connection := flows[0].(*streamConn).conn
	if localmesh.ApplicationProtocol(connection.ConnectionState().TLS.NegotiatedProtocol) != ALPN {
		t.Fatal("fixture did not negotiate legacy app protocol")
	}
	client.portACKLimit = 100 * time.Millisecond
	started := time.Now()
	extra, err := client.Dial(ctx, b.Device, address, port)
	if extra != nil {
		extra.Close()
		t.Fatal("legacy TCP overflow exceeded eight held stream credits")
	}
	requireBoundedCapacityFailure(t, started, err)
	if connection.Context().Err() != nil || dials.Load() != 1 {
		t.Fatalf("legacy capacity wait retired healthy session: context=%v, handshakes=%d", connection.Context().Err(), dials.Load())
	}
	for _, flow := range flows {
		lifecycleExchange(t, flow)
	}
	flows[0].Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		fresh, err := client.Dial(ctx, b.Device, address, port)
		if err == nil {
			if fresh.(*streamConn).conn != connection {
				fresh.Close()
				t.Fatal("returned legacy credit opened another QUIC session")
			}
			flows[0] = fresh
			break
		}
		if time.Now().After(deadline) || connection.Context().Err() != nil || dials.Load() != 1 {
			t.Fatalf("closed legacy TCP did not return stream credit: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, flow := range flows {
		lifecycleExchange(t, flow)
	}
	if dials.Load() != 1 {
		t.Fatalf("legacy capacity handling used %d handshakes", dials.Load())
	}
}

func requireBoundedCapacityFailure(t *testing.T, started time.Time, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("stream-credit exhaustion returned no error")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stream-credit refusal exceeded the 100ms warm admission bound: %v after %s", err, elapsed)
	}
}

// A peer may reset one admission stream after receiving the complete request.
// That explicit stream refusal must not become a stale-session retry or close
// already admitted siblings, for either TCP or UDP setup.
func TestPoolPerStreamResetPreservesEstablishedSession(t *testing.T) {
	for _, protocol := range []string{"tcp", "udp"} {
		t.Run(protocol, func(t *testing.T) {
			a, b := fixtureCredentials(t)
			server, err := NewServer(b, allowPort(func(uint16) bool { return true }))
			if err != nil {
				t.Fatal(err)
			}
			listener, err := quic.ListenAddr("127.0.0.1:0", server.TLSConfig(), quicConfig())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			defer listener.Close()
			defer server.Close()
			var requests atomic.Int32
			refusedRequest := make(chan string, 1)
			go func() {
				for {
					conn, err := listener.Accept(ctx)
					if err != nil {
						return
					}
					go func() {
						defer conn.CloseWithError(0, "reset fixture complete")
						for {
							stream, err := conn.AcceptStream(ctx)
							if err != nil {
								return
							}
							go func() {
								defer stream.CancelRead(0)
								defer stream.Close()
								var request [6]byte
								if _, err := io.ReadFull(stream, request[:]); err != nil {
									return
								}
								if string(request[:4]) == "WAU4" {
									var rest [10]byte
									if _, err := io.ReadFull(stream, rest[:]); err != nil {
										return
									}
								}
								if requests.Add(1) == 2 {
									stream.CancelWrite(7)
									refusedRequest <- string(request[:4])
									return
								}
								if _, err := stream.Write([]byte{1}); err != nil {
									return
								}
								_, _ = io.Copy(stream, stream)
							}()
						}
					}()
				}
			}()
			address := netip.MustParseAddrPort(listener.Addr().String())
			client := lifecycleClient(t, a)
			var dials atomic.Int32
			originalDial := client.dial
			client.dial = func(ctx context.Context, endpoint netip.AddrPort, config *tls.Config, options *quic.Config) (*quic.Conn, error) {
				dials.Add(1)
				return originalDial(ctx, endpoint, config, options)
			}
			first := lifecycleDial(t, client, b.Device, address, 1234)
			lifecycleExchange(t, first)
			client.portACKLimit = 100 * time.Millisecond
			if protocol == "tcp" {
				extra, dialErr := client.Dial(ctx, b.Device, address, 1234)
				if extra != nil {
					extra.Close()
					t.Fatal("reset TCP admission was retried or accepted")
				}
				err = dialErr
			} else {
				extra, dialErr := client.DialUDP(ctx, b.Device, address, 1234)
				if extra != nil {
					extra.Close()
					t.Fatal("reset UDP admission was retried or accepted")
				}
				err = dialErr
			}
			var reset *quic.StreamError
			if !errors.As(err, &reset) || reset.ErrorCode != 7 {
				t.Fatalf("explicit reset admission = %v", err)
			}
			select {
			case magic := <-refusedRequest:
				want := "WAS1"
				if protocol == "udp" {
					want = "WAU4"
				}
				if magic != want {
					t.Fatalf("refused request magic %q, want %q", magic, want)
				}
			case <-ctx.Done():
				t.Fatal("peer did not observe full request before reset")
			}
			if first.conn.Context().Err() != nil || dials.Load() != 1 {
				t.Fatalf("stream reset retired healthy session: context=%v, handshakes=%d", first.conn.Context().Err(), dials.Load())
			}
			lifecycleExchange(t, first)
			fresh := lifecycleDial(t, client, b.Device, address, 1234)
			if fresh.conn != first.conn || dials.Load() != 1 {
				t.Fatal("new admission after reset did not reuse the original session")
			}
			lifecycleExchange(t, fresh)
		})
	}
}
