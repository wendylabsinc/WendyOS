package providers

import (
	"context"
	"crypto/x509"
	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"net/url"
	"testing"
	"time"
)

type budgetTopologyClient struct {
	peer   *x509.Certificate
	read   func(context.Context) (*pb.LocalMeshTopology, error)
	closed bool
}

func (c *budgetTopologyClient) PeerCertificate() *x509.Certificate { return c.peer }
func (c *budgetTopologyClient) GetDeviceIdentity(time.Duration) (*liteclient.DeviceIdentity, error) {
	return &liteclient.DeviceIdentity{}, nil
}
func (c *budgetTopologyClient) NetTopology(ctx context.Context) (*pb.LocalMeshTopology, error) {
	return c.read(ctx)
}
func (c *budgetTopologyClient) Close() error { c.closed = true; return nil }
func TestLiteTopologySetupAndReadUseSeparateBudgets(t *testing.T) {
	oldConnect, oldSetup, oldRead := connectLiteTopologyClient, liteTopologySetupBudget, liteTopologyReadBudget
	defer func() {
		connectLiteTopologyClient, liteTopologySetupBudget, liteTopologyReadBudget = oldConnect, oldSetup, oldRead
	}()
	liteTopologySetupBudget = 80 * time.Millisecond
	liteTopologyReadBudget = 160 * time.Millisecond
	const principal = "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/00000000-0000-4000-8000-000000000001"
	identity, _ := meshid.DeviceFromPrincipal(principal)
	uri, _ := url.Parse(principal)
	var lifetime context.Context
	client := &budgetTopologyClient{peer: &x509.Certificate{URIs: []*url.URL{uri}}}
	client.read = func(ctx context.Context) (*pb.LocalMeshTopology, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		if lifetime.Err() != nil {
			t.Fatal("completed setup expired during observation")
		}
		return &pb.LocalMeshTopology{SelfPrincipal: principal, SelfTenantId: identity.Tenant().UUID().String(), SelfDeviceId: identity.UUID().String(), SelfMeshAddress: identity.MeshAddress().String()}, nil
	}
	connectLiteTopologyClient = func(ctx context.Context, _ *MicroWendyProvider, _ models.ExternalDevice) (liteTopologyClient, error) {
		lifetime = ctx
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(40 * time.Millisecond):
		}
		return client, nil
	}
	result, err := (&MicroWendyProvider{}).NetTopology(context.Background(), models.ExternalDevice{ConnectionInfo: map[string]string{"type": "LAN"}})
	if err != nil || result == nil || !client.closed {
		t.Fatal(result, client.closed, err)
	}
	if lifetime.Err() == nil {
		t.Fatal("provider did not release stream lifetime")
	}
}
func TestLiteTopologyCallerCanCancelSetup(t *testing.T) {
	previous := connectLiteTopologyClient
	defer func() { connectLiteTopologyClient = previous }()
	started := make(chan struct{})
	connectLiteTopologyClient = func(ctx context.Context, _ *MicroWendyProvider, _ models.ExternalDevice) (liteTopologyClient, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := (&MicroWendyProvider{}).NetTopology(ctx, models.ExternalDevice{}); done <- err }()
	<-started
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled setup succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("setup ignored caller cancellation")
	}
}
