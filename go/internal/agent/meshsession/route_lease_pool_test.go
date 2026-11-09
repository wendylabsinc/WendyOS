package meshsession

import (
	"context"
	"crypto/tls"
	"errors"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
)

// An admitted announcement's receiver-local lease controls destination proof,
// including while routing is unavailable. Signed UTC expiry supplies duration,
// so it must neither kill a live lease nor preserve an elapsed one.
func TestPooledDestinationProofUsesAdmittedLease(t *testing.T) {
	for _, action := range []string{"preserve-admitted", "expire-admitted"} {
		t.Run(action, func(t *testing.T) {
			a, b := fixtureCredentials(t)
			registry := meshingress.NewRegistry()
			udpPort := udpLifecycleApp(t, registry, "lease-udp")
			tcpPort := lifecycleEcho(t)
			if err := registry.Claim("lease-tcp", tcpPort, "10.3.0.2", tcpPort); err != nil {
				t.Fatal(err)
			}
			physical := lifecycleServer(t, b, udpLifecycleAuthorization{testUDPAuthorization{registry}}, true)
			client := lifecycleClient(t, a)
			var dials atomic.Int32
			var previous atomic.Pointer[quic.Conn]
			var overlap atomic.Bool
			originalDial := client.dial
			client.dial = func(ctx context.Context, _ netip.AddrPort, config *tls.Config, options *quic.Config) (*quic.Conn, error) {
				dials.Add(1)
				if old := previous.Load(); old != nil && old.Context().Err() == nil {
					overlap.Store(true)
				}
				conn, err := originalDial(ctx, physical, config, options)
				if err == nil {
					previous.Store(conn)
				}
				return conn, err
			}
			now := time.Now()
			snapshot := localmesh.NodeSnapshot{
				Routes: []babel.Route{{Prefix: netip.PrefixFrom(b.Device.MeshAddress(), 128)}},
				Devices: []localmesh.Manifest{{Device: b.Device.UUID(), Issued: now.Add(-time.Minute).UnixMilli(),
					Expires: now.Add(time.Minute).UnixMilli(), AdmittedUntil: now.Add(time.Minute),
					AppEndpoints: []localmesh.AppEndpoint{{Transport: localmesh.AppQUIC, Port: Port}}}},
			}
			if action == "preserve-admitted" {
				snapshot.Devices[0].Issued = now.Add(-2 * time.Minute).UnixMilli()
				snapshot.Devices[0].Expires = now.Add(-time.Minute).UnixMilli()
			}
			endpoint, err := ResolveApp(snapshot, b.Device)
			if err != nil {
				t.Fatal("live admitted destination was not resolvable", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			tcp := lifecycleDial(t, client, b.Device, endpoint.Address, tcpPort)
			udp, err := client.DialUDP(ctx, b.Device, endpoint.Address, udpPort)
			if err != nil {
				t.Fatal(err)
			}
			defer udp.Close()
			if udp.conn != tcp.conn || dials.Load() != 1 {
				t.Fatal("initial TCP and UDP did not share one authenticated session")
			}
			exchangeUDP := func(flow *DatagramFlow) {
				t.Helper()
				if err := flow.Send([]byte("admitted-lease")); err != nil {
					t.Fatal(err)
				}
				if got, err := readUDPMessage(flow, ctx); err != nil || string(got) != "admitted-lease" {
					t.Fatalf("UDP exchange %q: %v", got, err)
				}
			}
			lifecycleExchange(t, tcp)
			exchangeUDP(udp)
			if action == "preserve-admitted" {
				client.PruneRoutes(snapshot)
				lifecycleExchange(t, tcp)
				exchangeUDP(udp)
				// Losing reachability cancels UDP without invalidating the live
				// signed destination or interrupting its established TCP stream.
				routes := snapshot.Routes
				snapshot.Routes = nil
				client.PruneRoutes(snapshot)
				if _, err := ResolveApp(snapshot, b.Device); !errors.Is(err, ErrNoRoute) {
					t.Fatal("route outage permitted new admission", err)
				}
				select {
				case <-udp.Done():
				case <-ctx.Done():
					t.Fatal("route outage retained UDP flow")
				}
				lifecycleExchange(t, tcp)
				snapshot.Routes = routes
			} else {
				snapshot.Devices[0].AdmittedUntil = now.Add(-time.Second)
				client.PruneRoutes(snapshot)
				if tcp.conn.Context().Err() == nil {
					t.Fatal("elapsed admitted lease retained TCP despite future signed expiry")
				}
				select {
				case <-udp.Done():
				case <-ctx.Done():
					t.Fatal("elapsed admitted lease retained UDP")
				}
				if _, err := ResolveApp(snapshot, b.Device); !errors.Is(err, ErrNoRoute) {
					t.Fatal("elapsed admitted lease permitted new admission", err)
				}
				snapshot.Devices[0].AdmittedUntil = time.Now().Add(time.Minute)
			}
			// A restored route/lease needs fresh caller-side resolution. Route
			// healing reuses the transport; expired proof requires replacement.
			endpoint, err = ResolveApp(snapshot, b.Device)
			if err != nil {
				t.Fatal("restored admitted destination was not resolvable", err)
			}
			client.PruneRoutes(snapshot)
			fresh := lifecycleDial(t, client, b.Device, endpoint.Address, tcpPort)
			freshUDP, err := client.DialUDP(ctx, b.Device, endpoint.Address, udpPort)
			if err != nil {
				t.Fatal(err)
			}
			defer freshUDP.Close()
			wantDials := int32(1)
			if action == "expire-admitted" {
				wantDials = 2
			}
			if freshUDP.conn != fresh.conn || (fresh.conn == tcp.conn) != (action == "preserve-admitted") || dials.Load() != wantDials || overlap.Load() {
				t.Fatalf("restored admission violated session reuse/closure: dials=%d overlap=%v", dials.Load(), overlap.Load())
			}
			lifecycleExchange(t, fresh)
			exchangeUDP(freshUDP)
		})
	}
}
