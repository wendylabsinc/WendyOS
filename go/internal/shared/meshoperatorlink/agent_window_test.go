package meshoperatorlink

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
)

func windowCredentials(t testing.TB) (tls.Certificate, tls.Certificate, ClientConfig) {
	t.Helper()
	now := time.Now()
	operator, root := operatorClockFixture(t, now.Add(-time.Hour), now.Add(time.Hour))
	key := operator.PrivateKey.(crypto.Signer)
	uri, _ := url.Parse("spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/22222222-2222-4222-8222-222222222222")
	leaf := &x509.Certificate{SerialNumber: big.NewInt(3), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, URIs: []*url.URL{uri}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, root, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	device := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
	peer, err := meshid.DeviceFromCert(leaf)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := meshoperator.VerifyPrimary(operator.Certificate, []*x509.Certificate{root}, peer.Tenant(), now)
	if err != nil {
		t.Fatal(err)
	}
	return operator, device, ClientConfig{Identity: identity, Signer: key, Instance: meshoperator.InstanceID{1}, Router: peer, Roots: []*x509.Certificate{root}}
}

// Even a wrapper exposing a TCP address is an unknown transport.
type unknownWindowConn struct{ net.Conn }

// Pipeline bounded chunks with a fixed propagation delay in both directions.
// A sleep per Write would impose a per-frame rate limit instead of RTT.
func windowDelayProxy(t testing.TB, destination string, delay time.Duration) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		left, err := listener.Accept()
		if err != nil {
			return
		}
		defer left.Close()
		right, err := net.Dial("tcp", destination)
		if err != nil {
			return
		}
		defer right.Close()
		stop := context.AfterFunc(ctx, func() { left.Close(); right.Close() })
		defer stop()
		type packet struct {
			data []byte
			due  time.Time
		}
		var directions sync.WaitGroup
		for _, ends := range [][2]net.Conn{{left, right}, {right, left}} {
			directions.Add(1)
			go func(src, dst net.Conn) {
				defer directions.Done()
				queue := make(chan packet, 128)
				var reader sync.WaitGroup
				reader.Add(1)
				go func() {
					defer reader.Done()
					defer close(queue)
					for {
						data := make([]byte, 32*1024)
						n, err := src.Read(data)
						if n > 0 {
							select {
							case queue <- packet{data[:n], time.Now().Add(delay)}:
							case <-ctx.Done():
								return
							}
						}
						if err != nil {
							return
						}
					}
				}()
				defer reader.Wait()
				for p := range queue {
					timer := time.NewTimer(time.Until(p.due))
					select {
					case <-timer.C:
					case <-ctx.Done():
						timer.Stop()
						return
					}
					if _, err := dst.Write(p.data); err != nil {
						cancel()
						return
					}
				}
			}(ends[0], ends[1])
		}
		directions.Wait()
	}()
	t.Cleanup(func() { cancel(); listener.Close(); workers.Wait() })
	return listener.Addr().String()
}

func windowPair(t testing.TB, mode string, delay time.Duration, handler func(context.Context, net.Conn) error) (*RouterLink, *RouterLink) {
	t.Helper()
	operator, device, cfg := windowCredentials(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	address := listener.Addr().String()
	if delay > 0 {
		address = windowDelayProxy(t, address, delay)
	}
	type result struct {
		link *RouterLink
		err  error
	}
	results := make(chan result, 1)
	go func() {
		raw, err := listener.Accept()
		if err != nil {
			results <- result{err: err}
			return
		}
		if mode == "wrapped_server" {
			raw = unknownWindowConn{raw}
		}
		server := tls.Server(raw, &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{device}, ClientAuth: tls.RequireAnyClientCert, NextProtos: []string{RouterALPN}})
		if err = server.HandshakeContext(ctx); err != nil {
			raw.Close()
			results <- result{err: err}
			return
		}
		var link *RouterLink
		if mode == "legacy_server" {
			hello, readErr := readControl(server)
			err = readErr
			if err == nil && (hello.Kind != "operator-router-hello" || hello.AgentWindow != agentTCPWindow) {
				err = fmt.Errorf("missing new-client offer: %+v", hello)
			}
			if err == nil {
				err = writeControl(server, control{Kind: "operator-router-ready"})
			}
			if err == nil {
				link, err = routerReady(ctx, server, cfg.Router, cfg.Router, true, time.Now().Add(time.Minute), handler)
			}
		} else {
			link, err = AcceptRouter(ctx, server, cfg.Router.Tenant(), cfg.Roots, handler)
		}
		if err != nil {
			raw.Close()
		}
		results <- result{link, err}
	}()
	raw, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	if mode == "wrapped_client" {
		raw = unknownWindowConn{raw}
	}
	config, err := ClientTLSConfig(cfg, operator)
	if err != nil {
		t.Fatal(err)
	}
	client := tls.Client(raw, config)
	t.Cleanup(func() { raw.Close() })
	if err = client.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}
	var link *RouterLink
	if mode == "legacy_client" {
		if err = writeControl(client, control{Kind: "operator-router-hello", Instance: cfg.Instance}); err != nil {
			t.Fatal(err)
		}
		reply, readErr := readControl(client)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if reply.AgentWindow != 0 {
			t.Fatalf("unsolicited window: %+v", reply)
		}
		link, err = routerReady(ctx, client, cfg.Router, cfg.Router, false, time.Now().Add(time.Minute), nil)
	} else {
		link, err = RouterClient(ctx, client, cfg)
	}
	if err != nil {
		t.Fatal(err)
	}
	server := <-results
	if server.err != nil {
		t.Fatal(server.err)
	}
	t.Cleanup(func() { link.Close(); server.link.Close(); link.Wait(); server.link.Wait() })
	return link, server.link
}

func TestAgentWindowNegotiationAndTransfer(t *testing.T) {
	for _, mode := range []string{"tcp", "legacy_client", "legacy_server", "wrapped_client", "wrapped_server"} {
		t.Run(mode, func(t *testing.T) {
			streams := make(chan net.Conn, 1)
			client, server := windowPair(t, mode, 0, func(ctx context.Context, c net.Conn) error { streams <- c; <-ctx.Done(); return nil })
			want := uint32(agentWindow)
			if mode == "tcp" {
				want = agentTCPWindow
			}
			if client.receiveWindow() != want || server.receiveWindow() != want {
				t.Fatalf("negotiated %d/%d want %d", client.receiveWindow(), server.receiveWindow(), want)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, err := client.OpenAgent(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			remote := <-streams
			for _, pair := range [][2]net.Conn{{conn, remote}, {remote, conn}} {
				data := bytes.Repeat([]byte("management payload"), 32768)
				written := make(chan error, 1)
				go func() { _, err := pair[0].Write(data); written <- err }()
				got := make([]byte, len(data))
				if _, err := io.ReadFull(pair[1], got); err != nil {
					t.Fatal(err)
				}
				if err := <-written; err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(data, got) {
					t.Fatal("management bytes changed")
				}
			}
			if len(conn.(*agentStream).buffer) != int(want) || len(remote.(*agentStream).buffer) != int(want) {
				t.Fatal("unbounded receive ring")
			}
		})
	}
}

func TestAgentWindowBounds(t *testing.T) {
	// Router: the echoed selection is nonzero only when both peers opted in.
	for _, test := range []struct {
		local, offered, want, echoed uint32
		fail                         bool
	}{
		{0, 0, agentWindow, 0, false}, {agentTCPWindow, 0, agentWindow, 0, false}, {0, agentTCPWindow, agentWindow, 0, false},
		{agentTCPWindow, 64 * 1024, 64 * 1024, 64 * 1024, false}, {64 * 1024, agentTCPWindow, 64 * 1024, 64 * 1024, false},
		{agentTCPWindow, agentTCPWindow, agentTCPWindow, agentTCPWindow, false},
		{agentTCPWindow, 1, 0, 0, true}, {agentTCPWindow, agentTCPWindow + 1, 0, 0, true}, {agentTCPWindow, ^uint32(0), 0, 0, true},
		{0, 1, 0, 0, true},
	} {
		got, echoed, err := selectAgentWindow(test.local, test.offered)
		if (err != nil) != test.fail || got != test.want || echoed != test.echoed {
			t.Fatalf("select %+v got %d/%d %v", test, got, echoed, err)
		}
	}
	// Operator: the router may select only within the operator's own offer.
	for _, test := range []struct {
		offered, selected, want uint32
		fail                    bool
	}{
		{0, 0, agentWindow, false}, {agentTCPWindow, 0, agentWindow, false},
		{agentTCPWindow, 64 * 1024, 64 * 1024, false}, {agentTCPWindow, agentWindow, agentWindow, false}, {agentTCPWindow, agentTCPWindow, agentTCPWindow, false},
		{0, agentWindow, 0, true}, {0, agentTCPWindow, 0, true}, {64 * 1024, agentTCPWindow, 0, true},
		{agentTCPWindow, agentWindow - 1, 0, true}, {agentTCPWindow, agentTCPWindow + 1, 0, true}, {agentTCPWindow, ^uint32(0), 0, true},
	} {
		got, err := acceptAgentWindow(test.offered, test.selected)
		if (err != nil) != test.fail || got != test.want {
			t.Fatalf("accept %+v got %d %v", test, got, err)
		}
	}
	for _, window := range []uint32{agentWindow, agentTCPWindow} {
		t.Run(fmt.Sprint(window), func(t *testing.T) {
			newLink := func(server bool) *Link {
				return &Link{server: server, agentWindow: window, agentWake: make(chan struct{}, 1), outControl: make(chan control, 4)}
			}
			for _, credit := range []uint32{0, window + 1, ^uint32(0)} {
				l := newLink(true)
				if _, err := l.agentControl(control{Kind: "operator-agent-open", Stream: 1, Credit: credit}); err == nil {
					t.Fatalf("accepted open credit %d", credit)
				}
				l = newLink(false)
				l.agent = newAgentStream(l, 1, 0)
				if _, err := l.agentControl(control{Kind: "operator-agent-ready", Stream: 1, Credit: credit}); err == nil {
					t.Fatalf("accepted ready credit %d", credit)
				}
			}
			l := newLink(false)
			c := newAgentStream(l, 1, 0)
			l.agent = c
			if _, err := l.agentControl(control{Kind: "operator-agent-credit", Stream: 1, Credit: 1}); err == nil {
				t.Fatal("credit before ready accepted")
			}
			// A smaller initial peer grant remains its bound for later credit updates.
			if _, err := l.agentControl(control{Kind: "operator-agent-ready", Stream: 1, Credit: 1024}); err != nil {
				t.Fatal(err)
			}
			if _, err := l.agentControl(control{Kind: "operator-agent-ready", Stream: 1, Credit: 1024}); err == nil {
				t.Fatal("duplicate ready reset credit")
			}
			for _, credit := range []uint32{0, 1, ^uint32(0)} {
				if _, err := l.agentControl(control{Kind: "operator-agent-credit", Stream: 1, Credit: credit}); err == nil {
					t.Fatalf("accepted excess credit %d", credit)
				}
			}
			c.sendCredit = 0
			if _, err := l.agentControl(control{Kind: "operator-agent-credit", Stream: 1, Credit: 1024}); err != nil {
				t.Fatal(err)
			}
			frame := make([]byte, 4+agentFramePayload)
			binary.BigEndian.PutUint32(frame, 1)
			left := window
			for left > 0 {
				n := min(left, agentFramePayload)
				if err := l.agentData(frame[:4+n]); err != nil {
					t.Fatal(err)
				}
				left -= n
			}
			if c.size != int(window) || c.receiveCredit != 0 {
				t.Fatal("ring accounting", c.size, c.receiveCredit)
			}
			if err := l.agentData(frame[:5]); err == nil {
				t.Fatal("accepted data without receive credit")
			}
		})
	}
}

func TestAgentWindowLegacyWireFixtures(t *testing.T) {
	// Independent bytes matching the native Lite member-selecting parser.
	legacyHello := append([]byte("\xa2\x61k\x75operator-router-hello\x61n\x50"), 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16)
	newHello := append(bytes.Clone(legacyHello), 'a', 'w', 0x1a, 0, 4, 0, 0)
	newHello[0] = 0xa3
	legacyReady := []byte("\xa1\x61k\x75operator-router-ready")
	for i, data := range [][]byte{legacyHello, newHello, legacyReady} {
		var m control
		if err := netwire.UnmarshalExtensibleArtifact(data, &m, maxControlBytes); err != nil {
			t.Fatal(err)
		}
		if i == 1 && m.AgentWindow != agentTCPWindow {
			t.Fatal("new hello lost window")
		}
		if i != 1 && m.AgentWindow != 0 {
			t.Fatal("legacy fixture opts in")
		}
		if i != 2 && m.Instance != (meshoperator.InstanceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}) {
			t.Fatal("lost installation")
		}
	}
	var legacy struct {
		Kind     string                  `cbor:"k"`
		Instance meshoperator.InstanceID `cbor:"n,omitempty"`
	}
	encoded, err := netwire.Marshal(control{Kind: "operator-router-hello", Instance: meshoperator.InstanceID{1}, AgentWindow: agentTCPWindow})
	if err != nil {
		t.Fatal(err)
	}
	if err := netwire.UnmarshalExtensibleArtifact(encoded, &legacy, maxControlBytes); err != nil || legacy.Kind != "operator-router-hello" || legacy.Instance[0] != 1 {
		t.Fatal("old Go parser rejected new hello", err)
	}
}

func BenchmarkAgentWindowTCP(b *testing.B) {
	for _, mode := range []string{"legacy_client", "tcp"} {
		b.Run(mode, func(b *testing.B) {
			delivered := make(chan int, 1)
			client, _ := windowPair(b, mode, 5*time.Millisecond, func(_ context.Context, c net.Conn) error {
				buf := make([]byte, 1024*1024)
				for {
					n, err := io.ReadFull(c, buf)
					if err != nil {
						return err
					}
					delivered <- n
				}
			})
			conn, err := client.OpenAgent(context.Background())
			if err != nil {
				b.Fatal(err)
			}
			data := make([]byte, 1024*1024)
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := conn.Write(data); err != nil {
					b.Fatal(err)
				}
				if n := <-delivered; n != len(data) {
					b.Fatal(n)
				}
			}
			b.StopTimer()
			conn.Close()
		})
	}
}

func TestAgentWindowRejectsUnofferedAndOversizedSelection(t *testing.T) {
	for _, selected := range []uint32{agentTCPWindow, agentTCPWindow + 1, ^uint32(0)} {
		t.Run(fmt.Sprint(selected), func(t *testing.T) {
			operator, device, cfg := windowCredentials(t)
			rawClient, rawServer := net.Pipe()
			defer rawClient.Close()
			defer rawServer.Close()
			config, err := ClientTLSConfig(cfg, operator)
			if err != nil {
				t.Fatal(err)
			}
			client := tls.Client(rawClient, config)
			server := tls.Server(rawServer, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{device}, ClientAuth: tls.RequireAnyClientCert, NextProtos: []string{RouterALPN}})
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() {
				if err := server.HandshakeContext(ctx); err != nil {
					result <- err
					return
				}
				hello, err := readControl(server)
				if err != nil {
					result <- err
					return
				}
				if hello.AgentWindow != 0 {
					result <- fmt.Errorf("unknown underlay offered %d", hello.AgentWindow)
					return
				}
				result <- writeControl(server, control{Kind: "operator-router-ready", AgentWindow: selected})
			}()
			if err := client.HandshakeContext(ctx); err != nil {
				t.Fatal(err)
			}
			if link, err := RouterClient(ctx, client, cfg); err == nil {
				link.Close()
				link.Wait()
				t.Fatal("accepted unoffered window")
			}
			if err := <-result; err != nil {
				t.Fatal(err)
			}
		})
	}
	// Values beyond uint32 must fail decoding before they can wrap into range.
	encoded, err := netwire.Marshal(map[string]any{"k": "operator-router-ready", "w": uint64(1) << 32})
	if err != nil {
		t.Fatal(err)
	}
	var message control
	if err := netwire.UnmarshalExtensibleArtifact(encoded, &message, maxControlBytes); err == nil {
		t.Fatal("overflowing window decoded")
	}
}
