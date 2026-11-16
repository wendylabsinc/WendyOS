package meshsession

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/directstream"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestDirectProductionPromotionWithRoutedSockets(t *testing.T) {
	testDirectProductionPromotionDial(t, true)
}

func testDirectProductionPromotionDial(t *testing.T, routed bool) {
	for _, capability := range []uint32{directstream.CapabilityTLS, directstream.CapabilityQUIC} {
		name := "TLS"
		if capability == directstream.CapabilityQUIC {
			name = "QUIC"
		}
		t.Run(name, func(t *testing.T) {
			const flows = 8
			ca, cb := fixtureCredentials(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			endpoints := make(chan net.Conn, flows)
			var opens atomic.Int32
			backend := func(context.Context, meshid.Device, directstream.OpenRequest) (net.Conn, error) {
				opens.Add(1)
				a, b := net.Pipe()
				endpoints <- b
				return a, nil
			}
			recheckAfterHandshake := int32(2)
			if capability == directstream.CapabilityQUIC {
				recheckAfterHandshake++
			}
			var checks atomic.Int32
			var expectedAddress netip.AddrPort
			policy := func(peer meshid.Device, address netip.AddrPort, selected uint32, status DirectSessionStatus) error {
				if peer != cb.Device || address != expectedAddress || selected != directstream.CapabilityVersion|capability || !status.Established || !status.DirectAvailable || status.Promoted || status.Promoting {
					return errors.New("unexpected promotion policy input")
				}
				// Model a route disappearing during the first upper handshake.
				if checks.Add(1) == recheckAfterHandshake {
					return ErrNoRoute
				}
				return nil
			}
			cfg := DirectManagerConfig{Credentials: ca, Backend: backend, PromotionCheck: policy}
			var socketOpens, socketCloses atomic.Int32
			if routed {
				cfg.PromotionDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
					expectedNetwork := "tcp"
					if capability == directstream.CapabilityQUIC {
						expectedNetwork = "udp"
					}
					if network != expectedNetwork || address != expectedAddress.String() {
						return nil, errors.New("wrong routed endpoint")
					}
					conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
					if err != nil {
						return nil, err
					}
					socketOpens.Add(1)
					return &countedPromotionSocket{Conn: conn, closed: &socketCloses}, nil
				}
			}
			a, err := NewDirectManager(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			b, err := NewDirectManager(ctx, DirectManagerConfig{Credentials: cb, Backend: backend})
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			cs, ss := directTestTLSWithCredentials(t, ca, cb)
			digest, _, err := ca.DirectSessionIdentity(cb.Device, cs)
			if err != nil {
				t.Fatal(err)
			}
			x, y := make(chan []byte, 32), make(chan []byte, 32)
			aa := localmesh.DirectCarrierAdmission{Cost: 8192, Link: 1, Self: ca.Device, Peer: cb.Device, Role: 0, IdentityDigest: digest, Selected: directstream.CapabilityVersion | capability, TLS: cs, Records: &directTestCarrier{tx: x, rx: y}}
			ab := localmesh.DirectCarrierAdmission{Self: cb.Device, Peer: ca.Device, Role: 1, IdentityDigest: digest, Selected: aa.Selected, TLS: ss, Records: &directTestCarrier{tx: y, rx: x}}
			if _, err := a.acquire(aa); err != nil {
				t.Fatal(err)
			}
			carriers := make(chan error, 2)
			go func() { carriers <- a.HandleCarrier(ctx, aa) }()
			go func() { carriers <- b.HandleCarrier(ctx, ab) }()
			apps, peers := make([]net.Conn, flows), make([]net.Conn, flows)
			for i := range apps {
				apps[i], err = a.Dial(ctx, cb.Device, directstream.OpenRequest{Service: directstream.ServiceTCP, Port: 28080})
				if err != nil {
					t.Fatal(err)
				}
				defer apps[i].Close()
				peers[i] = <-endpoints
				defer peers[i].Close()
			}
			authorizer := &countingAuthorizer{}
			server, err := NewServer(cb, authorizer)
			if err != nil {
				t.Fatal(err)
			}
			if err := server.SetDirectManager(b); err != nil {
				t.Fatal(err)
			}
			ready, serving := make(chan struct{}), make(chan error, 1)
			go func() {
				if capability == directstream.CapabilityTLS {
					serving <- server.RunTLS(ctx, "127.0.0.1:0", func() { close(ready) })
				} else {
					serving <- server.Run(ctx, "127.0.0.1:0", func() { close(ready) })
				}
			}()
			defer func() {
				cancel()
				server.Close()
				select {
				case err := <-serving:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(time.Second):
					t.Error("listener did not join")
				}
			}()
			select {
			case <-ready:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			addr := server.TLSAddr()
			if capability == directstream.CapabilityQUIC {
				addr = server.Addr()
			}
			address := netip.MustParseAddrPort(addr.String())
			expectedAddress = address
			if err := a.DialPromotion(ctx, cb.Device, address); !errors.Is(err, ErrNoRoute) || checks.Load() != recheckAfterHandshake {
				t.Fatal("route disappearance was not rechecked after handshake", err, checks.Load())
			}
			select {
			case err := <-carriers:
				t.Fatal("rejected attempt fenced old carrier", err)
			default:
			}
			peers[0].SetReadDeadline(time.Now().Add(time.Second))
			if _, err := apps[0].Write([]byte("d")); err != nil {
				t.Fatal("retained app damaged by rejected attempt", err)
			}
			var old [1]byte
			if _, err := io.ReadFull(peers[0], old[:]); err != nil || old[0] != 'd' {
				t.Fatal(old, err)
			}
			done := make(chan struct{})
			var dialErr error
			go func() { dialErr = a.DialPromotion(ctx, cb.Device, address); close(done) }()
			defer func() {
				cancel()
				a.Close()
				b.Close()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("outbound promotion did not join")
				}
			}()
			// The complete profile parks live carriers for reuse after cutover.
			// Both sessions must fence BLE before the transfer below can prove migration.
			for {
				a.mu.Lock()
				as := a.peers[cb.Device].session.Status()
				a.mu.Unlock()
				b.mu.Lock()
				bs := b.peers[ca.Device].session.Status()
				b.mu.Unlock()
				if as.Promoted && bs.Promoted && !as.DirectAvailable && !bs.DirectAvailable {
					break
				}
				select {
				case <-done:
					t.Fatal("promotion ended before handoff", dialErr)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(time.Millisecond):
				}
			}
			if err := a.DialPromotion(ctx, cb.Device, address); !errors.Is(err, directstream.ErrPromoted) {
				t.Fatal("duplicate dial admitted", err)
			}
			for i, app := range apps {
				app.SetDeadline(time.Now().Add(2 * time.Second))
				peers[i].SetDeadline(time.Now().Add(2 * time.Second))
				if _, err := app.Write([]byte{byte(i)}); err != nil {
					t.Fatal(i, err)
				}
				var data [1]byte
				if _, err := io.ReadFull(peers[i], data[:]); err != nil || data[0] != byte(i) {
					t.Fatal(i, data, err)
				}
			}
			a.mu.Lock()
			target := a.peers[cb.Device]
			a.mu.Unlock()
			status := target.session.Status()
			if !status.Established || !status.Busy || !status.Promoted || status.Promoting || status.DirectAvailable || status.BLECost != 8192 || status.BLELink != 1 {
				t.Fatal("wrong post-promotion actor status", status)
			}
			if opens.Load() != flows || authorizer.calls.Load() != 0 {
				t.Fatal("new backend during promotion", opens.Load(), authorizer.calls.Load())
			}
			a.Invalidate(cb.Device)
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("epoch invalidation did not stop outgoing transport")
			}
			if routed && (socketOpens.Load() < 2 || socketCloses.Load() != socketOpens.Load()) {
				t.Fatal("routed sockets not used or leaked", socketOpens.Load(), socketCloses.Load())
			}

		})
	}
}

func (c *countedPromotionSocket) Close() error {
	if c.once.CompareAndSwap(false, true) {
		c.closed.Add(1)
	}
	return c.Conn.Close()
}

type countedPromotionSocket struct {
	net.Conn
	closed *atomic.Int32
	once   atomic.Bool
}

// Route-specific startup must not replace the retained flow stream allowance.
func TestRouteAwareListenerPreservesPromotionCapacity(t *testing.T) {
	peer := fixtureDevice(t, fixtureDeviceB)
	s := &Server{direct: &DirectManager{cfg: DirectManagerConfig{FlowSlots: 3}}}
	s.SetRouteSnapshot(func() localmesh.NodeSnapshot { return directLANView(peer) })
	config := s.listenConfig()
	selected, err := config.GetConfigForClient(&quic.ClientInfo{RemoteAddr: net.UDPAddrFromAddrPort(netip.AddrPortFrom(peer.MeshAddress(), Port))})
	if err != nil {
		t.Fatal(err)
	}
	want := quicConfig().MaxIncomingStreams + 7
	if config.MaxIncomingStreams != want || selected.MaxIncomingStreams != want || selected.InitialRTT != 100*time.Millisecond {
		t.Fatalf("promotion capacity/startup lost: base=%d selected=%d RTT=%v", config.MaxIncomingStreams, selected.MaxIncomingStreams, selected.InitialRTT)
	}
}
