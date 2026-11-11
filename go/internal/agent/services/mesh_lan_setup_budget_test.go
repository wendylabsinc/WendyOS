package services

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

func TestLateLANDiscoveryKeepsSeparateSetupBudget(t *testing.T) {
	d, peer, listener := multihomedTLSFixture(t)
	d.lanEvents = func(ctx context.Context, _ discovery.StreamOptions) <-chan discovery.LANEvent {
		out := make(chan discovery.LANEvent)
		go func() {
			defer close(out)
			select {
			case <-time.After(lanDiscoveryBudget - 100*time.Millisecond):
			case <-ctx.Done():
				return
			}
			dev := models.LANDevice{PrincipalURI: peer.Principal(), IPAddress: "192.0.2.10", IsMTLS: true, Port: 50052, AppTLSPort: 43023}
			select {
			case out <- discovery.LANEvent{Kind: discovery.LANFound, Device: dev}:
			case <-ctx.Done():
				return
			}
			<-ctx.Done()
		}()
		return out
	}
	d.appSocketDial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		select {
		case <-time.After(200 * time.Millisecond):
			return listener.Dial(ctx)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	control, err := d.meshDialApplicationTLS(ctx, netip.MustParseAddrPort("192.0.2.10:43023"), peer, 8080)
	if err != nil {
		t.Fatalf("control exact TLS/app admission: %v", err)
	}
	control.Close()
	flow, _, err := NewMeshAppDialer(d).DialDevice(ctx, peer, 8080)
	if flow != nil {
		flow.Close()
	}
	if err != nil {
		t.Fatalf("late valid sighting lost setup budget despite live caller context: %v; caller=%v", err, ctx.Err())
	}
}

type delayedLANHandshake struct {
	net.Conn
	once sync.Once
}

func (c *delayedLANHandshake) Write(payload []byte) (int, error) {
	c.once.Do(func() { time.Sleep(1100 * time.Millisecond) })
	return c.Conn.Write(payload)
}

func TestApplicationLANPreservesAuthenticatedSetupBudget(t *testing.T) {
	for _, datagrams := range []bool{false, true} {
		t.Run(map[bool]string{false: "TCP", true: "UDP"}[datagrams], func(t *testing.T) {
			d, peer, listener := multihomedTLSFixture(t)
			multihomedTLSEvents(d, peer, nil)
			d.appSocketDial = func(ctx context.Context, _, _ string) (net.Conn, error) {
				raw, err := listener.Dial(ctx)
				if err != nil {
					return nil, err
				}
				return &delayedLANHandshake{Conn: raw}, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if datagrams {
				flow, err := d.DialApplicationUDPWithoutMesh(ctx, peer, 8081, meshsession.UDPOptions{})
				if err != nil {
					t.Fatal(err)
				}
				flow.Close()
			} else {
				flow, _, err := NewMeshAppDialer(d).DialDevice(ctx, peer, 8080)
				if err != nil {
					t.Fatal(err)
				}
				flow.Close()
			}
		})
	}
}
