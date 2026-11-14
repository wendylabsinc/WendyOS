package services

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

func TestApplicationTLSReadinessUpdatePrecedesLegacyDenial(t *testing.T) {
	d, peer, listener := multihomedTLSFixture(t)
	d.lanEvents = func(ctx context.Context, _ discovery.StreamOptions) <-chan discovery.LANEvent {
		out := make(chan discovery.LANEvent, 2)
		device := models.LANDevice{PrincipalURI: peer.Principal(), IPAddress: "192.0.2.10", Port: 50052, IsMTLS: true}
		out <- discovery.LANEvent{Kind: discovery.LANFound, Device: device}
		device.AppTLSPort = 43023
		out <- discovery.LANEvent{Kind: discovery.LANUpdated, Device: device}
		go func() { <-ctx.Done(); close(out) }()
		return out
	}
	var legacyAttempts int
	d.appLANDial = func(context.Context, string, meshid.Device, uint16) (net.Conn, error) {
		legacyAttempts++
		return nil, meshsession.ErrDenied // MeshDial is disabled independently of app TLS.
	}
	d.appSocketDial = func(ctx context.Context, _, _ string) (net.Conn, error) { return listener.Dial(ctx) }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	flow, _, err := NewMeshAppDialer(d).DialDevice(ctx, peer, 8080)
	if flow != nil {
		flow.Close()
	}
	if err != nil {
		t.Fatalf("ready dedicated TLS update ignored; legacyAttempts=%d, err=%v", legacyAttempts, err)
	}
}
