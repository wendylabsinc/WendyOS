package services

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"net/netip"
	"testing"
	"time"
)

// Self advertisements must not settle a peer lookup before a cold remote
// resolve completes; an app capability may arrive as a later update too.
func TestApplicationLANLookupWaitsPastUnrelatedAndIncompleteSightings(t *testing.T) {
	d, _ := testDialer()
	peer := dialerTestTarget.Device
	d.lanEvents = func(ctx context.Context, _ discovery.StreamOptions) <-chan discovery.LANEvent {
		out := make(chan discovery.LANEvent)
		go func() {
			defer close(out)
			for _, dev := range []models.LANDevice{
				{PrincipalURI: "unrelated", IPAddress: "10.0.0.1", IsMTLS: true, AppTLSPort: 43023},
				{PrincipalURI: peer.Principal(), IPAddress: "10.0.0.2", IsMTLS: true},
			} {
				select {
				case out <- discovery.LANEvent{Kind: discovery.LANFound, Device: dev}:
				case <-ctx.Done():
					return
				}
			}
			select {
			case <-time.After(750 * time.Millisecond):
			case <-ctx.Done():
				return
			}
			dev := models.LANDevice{PrincipalURI: peer.Principal(), IPAddress: "192.168.2.9", IsMTLS: true, AppTLSPort: 43023}
			select {
			case out <- discovery.LANEvent{Kind: discovery.LANUpdated, Device: dev}:
			case <-ctx.Done():
				return
			}
			<-ctx.Done()
		}()
		return out
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	address, found, err := dialApplicationLAN(ctx, d, peer, true, func(_ context.Context, candidate applicationLANAddress) (netip.AddrPort, error) {
		return candidate.address, nil
	})
	if err != nil || !found || address != netip.MustParseAddrPort("192.168.2.9:43023") {
		t.Fatalf("cold peer endpoint=%v found=%v", address, found)
	}
}

func TestApplicationLANLookupRemainsBoundedWithoutTheTarget(t *testing.T) {
	d, _ := testDialer()
	stopped := make(chan struct{})
	d.lanEvents = func(ctx context.Context, _ discovery.StreamOptions) <-chan discovery.LANEvent {
		out := make(chan discovery.LANEvent)
		go func() { defer close(out); defer close(stopped); <-ctx.Done() }()
		return out
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, found, _ := dialApplicationLAN(ctx, d, dialerTestTarget.Device, true, func(_ context.Context, candidate applicationLANAddress) (netip.AddrPort, error) {
		return candidate.address, nil
	}); found {
		t.Fatal("missing peer manufactured an endpoint")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("peer lookup left its browser running")
	}
}
