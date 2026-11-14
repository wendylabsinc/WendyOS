package meshsession

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func directLANView(peer meshid.Device) localmesh.NodeSnapshot {
	return localmesh.NodeSnapshot{
		CapturedAt:       time.Now(),
		Devices:          []localmesh.Manifest{{Device: peer.UUID(), Expires: time.Now().Add(time.Minute).UnixMilli(), AppEndpoints: []localmesh.AppEndpoint{{Transport: localmesh.AppQUIC, Port: Port}}}},
		Links:            []localmesh.PeerLink{{ID: 1, Device: peer, Transport: "ethernet", ForwardingReady: true}},
		ForwardingRoutes: []babel.Route{{Prefix: netip.PrefixFrom(peer.MeshAddress(), 128), Link: 1}},
	}
}

func TestQUICStartupRequiresInstalledDirectLAN(t *testing.T) {
	a, b := fixtureCredentials(t)
	for _, tc := range []struct {
		name   string
		change func(*localmesh.NodeSnapshot)
		fast   bool
	}{
		{"ethernet", func(*localmesh.NodeSnapshot) {}, true},
		{"wifi", func(s *localmesh.NodeSnapshot) { s.Links[0].Transport = "infrastructure-wifi" }, true},
		{"ble", func(s *localmesh.NodeSnapshot) { s.Links[0].Transport = "ble" }, false},
		{"unknown quic", func(s *localmesh.NodeSnapshot) { s.Links[0].Transport = "quic" }, false},
		{"LAN first hop multihop", func(s *localmesh.NodeSnapshot) { s.Links[0].Device = a.Device }, false},
		{"different selected link", func(s *localmesh.NodeSnapshot) { s.ForwardingRoutes[0].Link = 2 }, false},
		{"selected BLE beside LAN", func(s *localmesh.NodeSnapshot) {
			l := s.Links[0]
			l.ID = 2
			l.Transport = "ble"
			s.Links = append(s.Links, l)
			s.ForwardingRoutes[0].Link = 2
		}, false},
		{"uninstalled", func(s *localmesh.NodeSnapshot) { s.Routes = s.ForwardingRoutes; s.ForwardingRoutes = nil }, false},
		{"not ready", func(s *localmesh.NodeSnapshot) { s.Links[0].ForwardingReady = false }, false},
		{"retiring", func(s *localmesh.NodeSnapshot) { s.Links[0].Retiring = true }, false},
		{"withdrawn", func(s *localmesh.NodeSnapshot) { s.ForwardingRoutes[0].Unreachable = true }, false},
		{"infinite", func(s *localmesh.NodeSnapshot) { s.ForwardingRoutes[0].Metric = babel.Infinity }, false},
		{"local", func(s *localmesh.NodeSnapshot) { s.ForwardingRoutes[0].Local = true }, false},
		{"stale", func(s *localmesh.NodeSnapshot) { s.CapturedAt = time.Now().Add(-2 * time.Second) }, false},
		{"undated", func(s *localmesh.NodeSnapshot) { s.CapturedAt = time.Time{} }, false},
		{"future", func(s *localmesh.NodeSnapshot) { s.CapturedAt = time.Now().Add(time.Second) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := directLANView(b.Device)
			tc.change(&s)
			got := quicConfigForRoute(s, b.Device, time.Now())
			rtt, burst := time.Second, uint32(4)
			if tc.fast {
				rtt = 100 * time.Millisecond
				burst = 10
			}
			if got.InitialRTT != rtt || got.InitialCongestionWindowPackets != burst {
				t.Fatalf("startup = %s/%d", got.InitialRTT, got.InitialCongestionWindowPackets)
			}
			if got.InitialPacketSize != 1200 || !got.DisablePathMTUDiscovery || got.Allow0RTT {
				t.Fatal("changed MTU or early data policy")
			}
		})
	}
}

func TestRouteAwareDialRechecksBeforeHandshake(t *testing.T) {
	a, b := fixtureCredentials(t)
	for _, protocol := range []string{"tcp", "udp"} {
		for _, change := range []string{"direct LAN", "LAN changes to BLE", "route withdrawn", "runtime stopped"} {
			t.Run(protocol+"/"+change, func(t *testing.T) {
				withdrawn := change == "route withdrawn"
				client, err := NewClient(a)
				if err != nil {
					t.Fatal(err)
				}
				defer client.Close()
				sentinel := errors.New("captured dial")
				var dialed bool
				client.dial = func(_ context.Context, _ netip.AddrPort, _ *tls.Config, cfg *quic.Config) (*quic.Conn, error) {
					dialed = true
					rtt, burst := time.Second, uint32(4)
					if change == "direct LAN" {
						rtt = 100 * time.Millisecond
						burst = 10
					}
					if cfg.InitialRTT != rtt || cfg.InitialCongestionWindowPackets != burst {
						t.Error("cold dial ignored current route profile")
					}
					return nil, sentinel
				}
				// The caller resolved this endpoint from a direct-LAN view.
				resolved, err := ResolveApp(directLANView(b.Device), b.Device)
				if err != nil {
					t.Fatal(err)
				}
				// The route then changes before the cold handshake.
				calls := 0
				snapshot := func() localmesh.NodeSnapshot {
					calls++
					view := directLANView(b.Device)
					if change != "direct LAN" {
						view.Links[0].Transport = "ble"
					}
					if withdrawn {
						view.ForwardingRoutes = nil
					}
					return view
				}
				if change == "runtime stopped" {
					snapshot = nil
				}
				if protocol == "tcp" {
					_, err = client.DialWithRouteSnapshot(context.Background(), b.Device, resolved.Address, 80, snapshot)
				} else {
					_, err = client.DialUDPWithRouteSnapshot(context.Background(), b.Device, resolved.Address, 80, snapshot)
				}
				switch change {
				case "route withdrawn", "runtime stopped":
					if !errors.Is(err, ErrNoRoute) || dialed {
						t.Fatalf("withdrawn dial: %v, dialed=%v", err, dialed)
					}
					if strings.Contains(err.Error(), "QUIC dial") {
						t.Fatalf("route failure reported as a handshake failure: %v", err)
					}
				default:
					if !errors.Is(err, sentinel) || !dialed {
						t.Fatalf("dial: %v, dialed=%v", err, dialed)
					}
				}
				if snapshot != nil && calls == 0 {
					t.Fatal("cold handshake did not recheck the current route")
				}
			})
		}
	}
}

type dropOpeningFlightConn struct {
	net.PacketConn
	writes  atomic.Int32
	started time.Time
}

func (c *dropOpeningFlightConn) WriteTo(p []byte, a net.Addr) (int, error) {
	c.writes.Add(1)
	if time.Since(c.started) < 100*time.Millisecond {
		return len(p), nil
	}
	return c.PacketConn.WriteTo(p, a)
}

func TestDirectLANRecoversDroppedInitial(t *testing.T) {
	a, b := fixtureCredentials(t)
	serverTLS, err := b.PeerTLSWithTickets(a.Device, ALPN, "app-quic")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := quic.ListenAddr("127.0.0.1:0", serverTLS, quicConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	for _, fast := range []bool{false, true} {
		t.Run(map[bool]string{false: "conservative", true: "direct LAN"}[fast], func(t *testing.T) {
			udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer udp.Close()
			wire := &dropOpeningFlightConn{PacketConn: udp}
			transport := &quic.Transport{Conn: wire}
			defer transport.Close()
			cfg, err := a.PeerTLSWithTickets(b.Device, ALPN, "app-quic")
			if err != nil {
				t.Fatal(err)
			}
			qc := quicConfig()
			if fast {
				qc = quicConfigForRoute(directLANView(b.Device), b.Device, time.Now())
			}
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			started := time.Now()
			wire.started = started
			conn, err := transport.Dial(ctx, listener.Addr(), cfg, qc)
			elapsed := time.Since(started)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseWithError(0, "test done")
			server, err := listener.Accept(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer server.CloseWithError(0, "test done")
			t.Logf("dropped opening Initial flight: handshake=%s, client datagrams=%d, initial RTT=%s, burst=%d", elapsed, wire.writes.Load(), qc.InitialRTT, qc.InitialCongestionWindowPackets)
			if fast && elapsed >= 1500*time.Millisecond {
				t.Fatal("direct LAN did not recover before conservative probe budget")
			}
			if !fast && elapsed < 1800*time.Millisecond {
				t.Fatal("conservative startup probed too early")
			}
			if conn.ConnectionState().Used0RTT {
				t.Fatal("unexpected early data")
			}
		})
	}
}

func TestServerQUICStartupFollowsClientRoute(t *testing.T) {
	a, b := fixtureCredentials(t)
	fromClient := &net.UDPAddr{IP: a.Device.MeshAddress().AsSlice(), Port: 4242}
	for _, tc := range []struct {
		name   string
		view   func() localmesh.NodeSnapshot
		remote net.Addr
		fast   bool
	}{
		{"direct LAN client", func() localmesh.NodeSnapshot { return directLANView(a.Device) }, fromClient, true},
		{"BLE client", func() localmesh.NodeSnapshot {
			s := directLANView(a.Device)
			s.Links[0].Transport = "ble"
			return s
		}, fromClient, false},
		{"other device's address", func() localmesh.NodeSnapshot { return directLANView(b.Device) }, fromClient, false},
		{"route withdrawn", func() localmesh.NodeSnapshot {
			s := directLANView(a.Device)
			s.ForwardingRoutes = nil
			return s
		}, fromClient, false},
		{"stale", func() localmesh.NodeSnapshot {
			s := directLANView(a.Device)
			s.CapturedAt = time.Now().Add(-2 * time.Second)
			return s
		}, fromClient, false},
		{"no runtime", nil, fromClient, false},
		{"non-UDP address", func() localmesh.NodeSnapshot { return directLANView(a.Device) }, &net.TCPAddr{IP: fromClient.IP, Port: fromClient.Port}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{}
			s.SetRouteSnapshot(tc.view)
			got, err := s.listenConfig().GetConfigForClient(&quic.ClientInfo{RemoteAddr: tc.remote})
			if err != nil {
				t.Fatal(err)
			}
			rtt, burst := time.Second, uint32(4)
			if tc.fast {
				rtt, burst = 100*time.Millisecond, 10
			}
			if got.InitialRTT != rtt || got.InitialCongestionWindowPackets != burst {
				t.Fatalf("startup = %s/%d", got.InitialRTT, got.InitialCongestionWindowPackets)
			}
			if got.InitialPacketSize != 1200 || !got.DisablePathMTUDiscovery || got.Allow0RTT || got.GetConfigForClient != nil {
				t.Fatal("changed MTU, early data or per-client policy")
			}
		})
	}
}

// meshSourceConn presents a loopback client as its mesh address, as the routed
// TUN does, and drops the server's opening flight.
type meshSourceConn struct {
	net.PacketConn
	mesh    *net.UDPAddr
	client  atomic.Pointer[net.UDPAddr]
	started time.Time
	writes  atomic.Int32
}

func (c *meshSourceConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(p)
	if err != nil {
		return n, addr, err
	}
	c.client.Store(addr.(*net.UDPAddr))
	return n, c.mesh, nil
}

func (c *meshSourceConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	c.writes.Add(1)
	if time.Since(c.started) < 100*time.Millisecond {
		return len(p), nil
	}
	return c.PacketConn.WriteTo(p, c.client.Load())
}

func TestDirectLANServerRecoversDroppedServerFlight(t *testing.T) {
	a, b := fixtureCredentials(t)
	for _, fast := range []bool{false, true} {
		t.Run(map[bool]string{false: "conservative", true: "direct LAN"}[fast], func(t *testing.T) {
			serverTLS, err := b.PeerTLSWithTickets(a.Device, ALPN, "app-quic")
			if err != nil {
				t.Fatal(err)
			}
			udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer udp.Close()
			wire := &meshSourceConn{PacketConn: udp, mesh: &net.UDPAddr{IP: a.Device.MeshAddress().AsSlice(), Port: 4242}, started: time.Now()}
			transport := &quic.Transport{Conn: wire}
			defer transport.Close()
			server := &Server{}
			if fast {
				server.SetRouteSnapshot(func() localmesh.NodeSnapshot { return directLANView(a.Device) })
			}
			listener, err := transport.Listen(serverTLS, server.listenConfig())
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			cfg, err := a.PeerTLSWithTickets(b.Device, ALPN, "app-quic")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			// The client keeps conservative startup so only the server's
			// retransmission timing differs between the two cases.
			started := time.Now()
			conn, err := quic.DialAddr(ctx, udp.LocalAddr().String(), cfg, quicConfig())
			elapsed := time.Since(started)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseWithError(0, "test done")
			t.Logf("dropped server opening flight: handshake=%s, server datagrams=%d", elapsed, wire.writes.Load())
			if fast && elapsed >= 1500*time.Millisecond {
				t.Fatal("direct LAN server did not recover before conservative probe budget")
			}
			if !fast && elapsed < 1800*time.Millisecond {
				t.Fatal("conservative server startup probed too early")
			}
		})
	}
}
