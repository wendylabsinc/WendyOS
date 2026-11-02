package meshsharing

import (
	"context"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshcatalog"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type reportingControlBackend struct {
	*controlTestBackend
	ended chan controlEnd
}

type delayedGatewayProbe struct {
	*fakeProbe
	delay time.Duration
}

func (p *delayedGatewayProbe) Gateway(context.Context, netip.Addr, netip.Addr) bool {
	time.Sleep(p.delay)
	return true
}

func TestGatewayDiagnosticsDistinguishExpiryDuringProbe(t *testing.T) {
	now := time.Now()
	self, donor := policySelf(t), policyDonorDevice(t)
	var events []string
	node := &fakeNode{view: gatewayView(donor, now), offers: []meshcatalog.Record{gatewayOffer(donor, now)}, events: &events}
	probe := &delayedGatewayProbe{fakeProbe: &fakeProbe{gateway: true}}
	controller, err := NewWithDeps(self, node, &fakePolicy{events: &events}, probe)
	if err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.InfoLevel)
	controller.SetLogger(zap.New(core))
	config := Config{Participate: true, Roam: true}
	for range 2 {
		if _, err := controller.Step(context.Background(), config); err != nil {
			t.Fatal(err)
		}
	}
	cutoff := time.Now().Add(250 * time.Millisecond).UnixMilli()
	node.view.Devices[0].Expires, node.offers[0].Expires = cutoff, cutoff
	probe.delay = 300 * time.Millisecond
	decision, err := controller.Step(context.Background(), config)
	if err != nil || decision.Mode != "offline" {
		t.Fatalf("expired decision: %+v %v", decision, err)
	}
	ended := logs.FilterMessage("Wendy Net gateway selection ended").All()
	if len(ended) != 1 {
		t.Fatalf("selection changes: %+v", ended)
	}
	fields := ended[0].ContextMap()
	if fields["reason"] != "signed-candidates-expired" || fields["eligible_candidates"] != int64(0) || fields["observed_candidates"] != int64(1) {
		t.Fatalf("expiry was mislabeled as failed reachability: %+v", fields)
	}
}

func (b *reportingControlBackend) reportControlEnd(event controlEnd) { b.ended <- event }

func TestGatewayControlReportsAuthenticatedTermination(t *testing.T) {
	for _, reason := range []string{"peer-closed", "local-shutdown", "policy-revoked", "heartbeat-too-frequent", "credential-expired"} {
		t.Run(reason, func(t *testing.T) {
			makeCreds := controlTestCredentials(t)
			client, gateway := makeCreds(controlTestTenant, controlTestClient), makeCreds(controlTestTenant, controlTestGateway)
			if reason == "credential-expired" {
				verify := gateway.Verify
				cutoff := time.Now().Add(500 * time.Millisecond)
				gateway.Verify = func(chain [][]byte, now time.Time) (localmesh.Identity, error) {
					identity, err := verify(chain, now)
					if err == nil && cutoff.Before(identity.NotAfter) {
						identity.NotAfter = cutoff
					}
					return identity, err
				}
			}
			backend := &reportingControlBackend{newControlBackend(), make(chan controlEnd, 1)}
			backend.leaseGone = make(chan struct{})
			left, right := net.Pipe()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			defer left.Close()
			defer right.Close()
			done := make(chan error, 1)
			go func() {
				done <- serveGatewayLease(ctx, gateway, backend, controlAddressConn{right, &net.TCPAddr{IP: net.IP(client.Device.MeshAddress().AsSlice()), Port: 12345}})
			}()
			conn, _, err := negotiateGatewayLease(ctx, client, gateway.Device, left)
			if err != nil {
				t.Fatal(err)
			}
			conn.SetDeadline(time.Now().Add(time.Second))
			switch reason {
			case "peer-closed":
				conn.Close()
			case "local-shutdown":
				cancel()
			case "policy-revoked":
				close(backend.leaseGone)
			case "heartbeat-too-frequent":
				if err := writeControl(conn, []byte{leaseHeartbeat}); err != nil {
					t.Fatal(err)
				}
				var ack [1]byte
				if _, err := io.ReadFull(conn, ack[:]); err != nil {
					t.Fatal(err)
				}
				if err := writeControl(conn, []byte{leaseHeartbeat}); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case event := <-backend.ended:
				if event.reason != reason || event.role != "gateway" || event.phase != "heartbeat-read" || event.peer != client.Device || event.lease.Slot != 257 || event.expires.IsZero() || event.elapsed <= 0 || event.cleanupErr != nil {
					t.Fatalf("termination: %+v", event)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("missing termination event")
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

// This reproduces the controller's independent health withdrawal even while
// signed routing/offer inputs survive. It identifies that path in diagnostics;
// it deliberately changes no health or lease-retention policy.
func TestGatewayDiagnosticsDistinguishProbeWithdrawal(t *testing.T) {
	now := time.Now()
	self, donor := policySelf(t), policyDonorDevice(t)
	var events []string
	node := &fakeNode{view: gatewayView(donor, now), offers: []meshcatalog.Record{gatewayOffer(donor, now)}, events: &events}
	probe := &fakeProbe{gateway: true}
	controller, err := NewWithDeps(self, node, &fakePolicy{events: &events}, probe)
	if err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.InfoLevel)
	controller.SetLogger(zap.New(core))
	config := Config{Participate: true, Roam: true}
	for range 2 {
		if _, err := controller.Step(context.Background(), config); err != nil {
			t.Fatal(err)
		}
	}
	probe.gateway = false
	for i := range 3 {
		decision, err := controller.Step(context.Background(), config)
		if err != nil {
			t.Fatal(err)
		}
		if (decision.Mode == "offline") != (i == 2) {
			t.Fatalf("health step %d: %+v", i, decision)
		}
	}
	changes := logs.FilterMessage("Wendy Net gateway probe health changed").All()
	if len(changes) != 2 || changes[1].ContextMap()["healthy"] != false || changes[1].ContextMap()["consecutive_failures"] != int64(3) {
		t.Fatalf("health changes: %+v", changes)
	}
	ended := logs.FilterMessage("Wendy Net gateway selection ended").All()
	if len(ended) != 1 {
		t.Fatalf("selection changes: %+v", ended)
	}
	fields := ended[0].ContextMap()
	if fields["reason"] != "no-reachable-candidate" || fields["eligible_candidates"] != int64(1) || fields["route_present"] != true || fields["manifest_expires_unix_ms"] != node.view.Devices[0].Expires || fields["offer_expires_unix_ms"] != node.offers[0].Expires {
		t.Fatalf("withdrawal evidence: %+v", fields)
	}
	probe.gateway = true
	for range 2 {
		if _, err := controller.Step(context.Background(), config); err != nil {
			t.Fatal(err)
		}
	}
	if logs.FilterMessage("Wendy Net gateway selected").Len() != 2 {
		t.Fatal("missing health recovery")
	}
}
