//go:build linux

package meshsharing

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshcatalog"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// These fixtures never install routes or invoke kernel configuration. Only the
// actual policy liveness accessor and heartbeat worker run, over net.Pipe.
func borrowedLeaseFixture(t *testing.T) (*linuxTunnelPolicy, *borrowedTunnel, net.Conn) {
	t.Helper()
	client, gateway := net.Pipe()
	t.Cleanup(func() { client.Close(); gateway.Close() })
	b := &borrowedTunnel{
		grantedTunnel: grantedTunnel{peer: policyDonorDevice(t), lease: tunnelLease{Slot: 7}, tunnel: &internetTunnel{link: &netlink.Ip6tnl{LinkAttrs: netlink.LinkAttrs{Index: 9}}}},
		control:       &borrowedControlConn{expiryConn: &expiryConn{Conn: client, expires: time.Now().Add(time.Minute)}},
		done:          make(chan struct{}), stop: make(chan struct{}), started: time.Now(),
	}
	b.controlLive.Store(true)
	return &linuxTunnelPolicy{borrowed: b}, b, gateway
}

func TestActiveGatewayLeaseRequiresCurrentBoundedInstalledControl(t *testing.T) {
	for _, name := range []string{"live", "wrong peer", "policy closed", "lease invalid", "tunnel absent", "link absent", "worker ended", "stop closed", "done closed", "stop absent", "done absent", "locally closed", "unbounded control", "zero expiry", "expired credential"} {
		t.Run(name, func(t *testing.T) {
			p, b, _ := borrowedLeaseFixture(t)
			peer := b.peer
			control := b.control.(*borrowedControlConn)
			switch name {
			case "wrong peer":
				peer = policySelf(t)
			case "policy closed":
				p.closed = true
			case "lease invalid":
				b.lease = tunnelLease{}
			case "tunnel absent":
				b.tunnel = nil
			case "link absent":
				b.tunnel.link = nil
			case "worker ended":
				b.controlLive.Store(false)
			case "stop closed":
				close(b.stop)
			case "done closed":
				close(b.done)
			case "stop absent":
				b.stop = nil
			case "done absent":
				b.done = nil
			case "locally closed":
				b.control.Close()
			case "unbounded control":
				b.control = control.expiryConn.Conn
			case "zero expiry":
				control.expires = time.Time{}
			case "expired credential":
				control.expires = time.Now().Add(-time.Second)
			}
			if got := p.ActiveGatewayLease(peer); got != (name == "live") {
				t.Fatalf("%s active=%v", name, got)
			}
			if name == "live" {
				// Reconciliation can reuse the installed connection, without calling any
				// negotiation or kernel path, only while this proof remains current.
				if err := p.SetGateway(context.Background(), peer); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestBorrowerControlCloseWithdrawsDespiteCleanupLock(t *testing.T) {
	p, b, gateway := borrowedLeaseFixture(t)
	var events []string
	donor := b.peer
	now := time.Now()
	node := &fakeNode{view: gatewayView(donor, now), offers: []meshcatalog.Record{gatewayOffer(donor, now)}, events: &events}
	policy := &activeLeasePolicy{fakePolicy: &fakePolicy{events: &events}, active: p.ActiveGatewayLease}
	probe := &fakeProbe{gateway: true}
	c, err := NewWithDeps(policySelf(t), node, policy, probe)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Participate: true, Roam: true}
	for range 2 {
		if _, err := c.Step(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	probe.gateway = false
	for range 3 {
		d, err := c.Step(context.Background(), cfg)
		if err != nil || d.Mode != "roaming" {
			t.Fatalf("live control lost to probes: %+v %v", d, err)
		}
	}
	p.borrowers.Add(1)
	go p.watchBorrowed(b)
	gateway.SetDeadline(time.Now().Add(2 * time.Second))
	var request [1]byte
	if _, err := io.ReadFull(gateway, request[:]); err != nil || request[0] != leaseHeartbeat {
		t.Fatalf("heartbeat=%v err=%v", request, err)
	}
	// The worker has to invalidate liveness before it can acquire p.mu for
	// cleanup; b.done remains open throughout this deliberately blocked window.
	p.mu.Lock()
	gateway.Close()
	limit := time.Now().Add(2 * time.Second)
	for b.controlLive.Load() && time.Now().Before(limit) {
		time.Sleep(time.Millisecond)
	}
	live := b.controlLive.Load()
	select {
	case <-b.done:
		t.Error("worker bypassed held cleanup lock")
	default:
	}
	// Prevent this fixture's worker from invoking netlink during cleanup.
	p.borrowed = nil
	p.mu.Unlock()
	if live {
		t.Fatal("worker failure stayed live while cleanup lock was held")
	}
	if p.ActiveGatewayLease(donor) {
		t.Fatal("ended control remained active")
	}
	d, err := c.Step(context.Background(), cfg)
	if err != nil || d.Mode != "offline" || d.GatewayDevice.IsValid() {
		t.Fatalf("dead control survived next step: %+v %v", d, err)
	}
	select {
	case <-b.done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not finish")
	}
	p.borrowers.Wait()
}

func TestBorrowerLocalCloseImmediatelyInvalidatesLease(t *testing.T) {
	p, b, _ := borrowedLeaseFixture(t)
	if !p.ActiveGatewayLease(b.peer) {
		t.Fatal("live fixture rejected")
	}
	// No heartbeat operation is in flight: Close must itself invalidate the
	// proof, rather than waiting up to one heartbeat interval for another I/O.
	if err := b.control.Close(); err != nil {
		t.Fatal(err)
	}
	if !b.controlLive.Load() {
		t.Fatal("fixture unexpectedly depended on a worker")
	}
	if p.ActiveGatewayLease(b.peer) {
		t.Fatal("locally closed control remained active")
	}
}

var _ interface{ ActiveGatewayLease(meshid.Device) bool } = (*linuxTunnelPolicy)(nil)
