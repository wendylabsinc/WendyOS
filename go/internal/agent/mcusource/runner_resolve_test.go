package mcusource

import (
	"context"
	"slices"
	"strconv"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// TestTransportSelectionAndResolve asserts resolveLANAddrs picks the right
// port for each transport: "grpc" pairings (agent-hosted sources) dial the
// source's mTLS agent gRPC port — the discovered d.Port — while "tcp"/empty
// pairings (MCU raw-TCP sources) dial the well-known sensorlink.Port instead,
// since d.Port there is the agent's own gRPC port, not where the source's
// SensorPairing/sensorlink service actually listens.
func TestTransportSelectionAndResolve(t *testing.T) {
	orig := discoverFn
	t.Cleanup(func() { discoverFn = orig })

	discoverFn = func(_ context.Context, _ discovery.DiscoveryOptions) (*models.DevicesCollection, error) {
		return &models.DevicesCollection{
			LANDevices: []models.LANDevice{{
				AssetID:   42,
				IsMTLS:    true,
				IPAddress: "10.0.0.5",
				Port:      50051,
			}},
		}, nil
	}

	grpcAddr, ok := resolveLANAddrs(context.Background(), 42, "grpc")
	if !ok || grpcAddr[0] != "10.0.0.5:50051" {
		t.Fatalf("grpc resolve: got %q, ok=%v, want 10.0.0.5:50051", grpcAddr, ok)
	}

	wantTCP := "10.0.0.5:" + strconv.Itoa(sensorlink.Port)
	tcpAddr, ok := resolveLANAddrs(context.Background(), 42, "tcp")
	if !ok || tcpAddr[0] != wantTCP {
		t.Fatalf("tcp resolve: got %q, ok=%v, want %s", tcpAddr, ok, wantTCP)
	}

	emptyAddr, ok := resolveLANAddrs(context.Background(), 42, "")
	if !ok || emptyAddr[0] != wantTCP {
		t.Fatalf("empty-transport resolve: got %q, ok=%v, want %s", emptyAddr, ok, wantTCP)
	}
}

func TestResolveAllInterfacesOnLANOnly(t *testing.T) {
	orig := discoverFn
	t.Cleanup(func() { discoverFn = orig })
	discoverFn = func(_ context.Context, opts discovery.DiscoveryOptions) (*models.DevicesCollection, error) {
		if len(opts.Types) != 1 || opts.Types[0] != models.InterfaceLAN {
			t.Fatalf("unexpected discovery types: %v", opts.Types)
		}
		return &models.DevicesCollection{LANDevices: []models.LANDevice{{AssetID: 42, IsMTLS: true, Port: 50051, IPAddress: "10.0.0.5", Addresses: []string{"10.0.0.5", "fe80::1%usb0", "169.254.1.2", ""}}}}, nil
	}
	got, ok := resolveLANAddrs(context.Background(), 42, "grpc")
	want := []string{"10.0.0.5:50051", "[fe80::1%usb0]:50051", "169.254.1.2:50051"}
	if !ok || !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
