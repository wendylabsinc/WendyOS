//go:build linux

package meshsharing

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshcatalog"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// This variant reuses the actual donor, transit, borrower and app namespaces,
// TLS peers, kernel tunnels, NAT, DNS and withdrawal checks. Only the first
// borrower TLS response read is held; no TLS or lease message is fabricated.
func TestSharingControllerSlowLeaseAcrossNamespaces(t *testing.T) {
	if os.Getenv("WENDY_MESH_NAMESPACE_TEST") != "1" {
		t.Skip("requires owned Linux namespaces")
	}
	t.Setenv("WENDY_SHARING_SLOW_LEASE", "1")
	TestSharingPolicyDualStackAcrossIPv6Namespaces(t)
}

type delayedLeaseConn struct {
	net.Conn
	ctx     context.Context
	once    sync.Once
	started chan struct{}
	closed  atomic.Bool
}

func (c *delayedLeaseConn) Read(p []byte) (int, error) {
	c.once.Do(func() {
		close(c.started)
		timer := time.NewTimer(16 * time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-c.ctx.Done():
		}
	})
	return c.Conn.Read(p)
}
func (c *delayedLeaseConn) Close() error { c.closed.Store(true); return c.Conn.Close() }

// resolved runs outside this existing kernel fixture. Observe the Controller's
// resolver-selection boundary, then require real DNS packets against the donor
// in the shared borrower assertions. All tunnel/lease policy methods are real.
type leaseControllerPolicy struct {
	*linuxTunnelPolicy
	dnsInterface, dnsAddress string
}

func (p *leaseControllerPolicy) SetDNS(_ context.Context, iface, address string) error {
	p.dnsInterface, p.dnsAddress = iface, address
	return nil
}

func namespaceSlowControllerLease(t *testing.T, policy *linuxTunnelPolicy, donor meshid.Device) {
	t.Helper()
	var dials atomic.Int32
	started := make(chan struct{})
	var raw *delayedLeaseConn
	policy.dialer.dial = func(ctx context.Context, gateway meshid.Device) (net.Conn, tunnelLease, error) {
		if dials.Add(1) != 1 {
			return nil, tunnelLease{}, fmt.Errorf("slow lease opened a replacement TCP connection")
		}
		bounded, cancel := context.WithTimeout(ctx, leaseDialTimeout)
		defer cancel()
		dialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IP(policy.credentials.Device.MeshAddress().AsSlice())}}
		conn, err := dialer.DialContext(bounded, "tcp6", net.JoinHostPort(gateway.MeshAddress().String(), fmt.Sprint(leaseControlPort)))
		if err != nil {
			return nil, tunnelLease{}, err
		}
		raw = &delayedLeaseConn{Conn: conn, ctx: bounded, started: started}
		return negotiateGatewayLease(bounded, policy.credentials, gateway, raw)
	}
	now := time.Now()
	var events []string
	view := gatewayView(donor, now)
	// The controlled directory snapshot uses logical mesh link 8; actual
	// underlay paths are the namespace fixture veths. Resolver application
	// is observed, not delegated to a host resolved instance.
	node := &fakeNode{view: view, offers: []meshcatalog.Record{gatewayOffer(donor, now)}, events: &events}
	observed := &leaseControllerPolicy{linuxTunnelPolicy: policy}
	controller, err := NewWithDeps(policy.credentials.Device, node, observed, &fakeProbe{gateway: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := controller.Close(); err != nil {
			t.Errorf("controller cleanup: %v", err)
		}
	})
	cfg := Config{Participate: true, Roam: true}
	step := func() Decision {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		decision, err := controller.Step(ctx, cfg)
		if err != nil {
			t.Fatalf("slow lease controller step: %v", err)
		}
		return decision
	}
	step() // The production health tracker requires two successful probes.
	begin := time.Now()
	pending := 0
	for time.Since(begin) < 25*time.Second {
		decision := step()
		if decision.Mode == "roaming" {
			if pending < 3 || time.Since(begin) < 16*time.Second || dials.Load() != 1 || observed.dnsInterface != "wlmp8" || observed.dnsAddress != donor.MeshAddress().String() {
				t.Fatalf("slow lease recovery lacked retained negotiation/resolver: pending=%d elapsed=%s dials=%d dns=%s/%s", pending, time.Since(begin), dials.Load(), observed.dnsInterface, observed.dnsAddress)
			}
			t.Logf("SLOW_LEASE_CONTROLLER_VERIFIED pending_steps=%d dials=1 elapsed=%s", pending, time.Since(begin))
			return
		}
		if decision.Mode != "offline" || decision.GatewayDevice != donor || !strings.Contains(decision.Reason, "negotiating") {
			t.Fatalf("pending slow lease claimed the wrong state: %+v", decision)
		}
		select {
		case <-started:
		default:
			t.Fatal("pending lease never reached real TLS response read")
		}
		// The channel synchronizes publication of raw; no unprotected goroutine data
		// is read before its first TLS read has reached the hold barrier.
		policy.mu.Lock()
		retained := policy.dialer.pending != nil && policy.borrowed == nil
		policy.mu.Unlock()
		if !retained || raw.closed.Load() || dials.Load() != 1 || observed.dnsAddress != "" {
			t.Fatal("pending controller withdrew the live TLS lease before recovery")
		}
		pending++
	}
	t.Fatal("slow lease did not recover within the unchanged handshake budget")
}
