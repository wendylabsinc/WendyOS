package mcusource

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

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

// TestResolveWendyLiteOnItsOwnService asserts "wendycom" pairings resolve
// through the _wendy-lite._tcp browse — a Wendy Lite board never appears on
// _wendyos._udp — dialing the port the board advertises. The insecure
// WendyCom connect presents no client certificate, so a board advertising
// mtls=false is still a match.
func TestResolveWendyLiteOnItsOwnService(t *testing.T) {
	origDiscover := discoverFn
	t.Cleanup(func() { discoverFn = origDiscover })
	discoverFn = func(context.Context, discovery.DiscoveryOptions) (*models.DevicesCollection, error) {
		t.Fatal("wendycom resolve browsed _wendyos._udp")
		return nil, nil
	}
	stubContinuousBrowse(t, wendyLiteSighting(41, "10.0.0.4", true), wendyLiteSighting(42, "10.0.0.5", false))

	got, ok := resolveLANAddrs(context.Background(), 42, "wendycom")
	if want := []string{"10.0.0.5:5054"}; !ok || !slices.Equal(got, want) {
		t.Fatalf("got %v, ok=%v, want %v", got, ok, want)
	}
}

func TestDiscoverWendyLiteLANDevicesStopsOnceTheBoardAnswers(t *testing.T) {
	stopped := stubContinuousBrowse(t,
		wendyLiteSighting(41, "10.0.0.4", true),
		wendyLiteSighting(42, "10.0.0.5", true),
		wendyLiteSighting(43, "10.0.0.6", true),
	)
	d, ok := discoverWendyLiteLANDevices(context.Background(), 42)
	if !ok || d.AssetID != 42 || d.IPAddress != "10.0.0.5" {
		t.Fatalf("got %+v, ok=%v, want board 42 at 10.0.0.5", d, ok)
	}
	// The browse is cancelled at the match; left running, it would block
	// forever trying to hand over board 43.
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("browse still running after the board answered")
	}
}

func TestDiscoverWendyLiteLANDevicesSkipsUndialableSightings(t *testing.T) {
	noPort := wendyLiteSighting(42, "10.0.0.5", true)
	noPort.Port = 0
	stubContinuousBrowse(t, wendyLiteSighting(42, "", true), noPort, wendyLiteSighting(42, "10.0.0.7", true))

	d, ok := discoverWendyLiteLANDevices(context.Background(), 42)
	if !ok || d.IPAddress != "10.0.0.7" {
		t.Fatalf("got %+v, ok=%v, want the dialable sighting at 10.0.0.7", d, ok)
	}
}

func TestDiscoverWendyLiteLANDevicesGivesUpWithTheContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stubContinuousBrowse(t, wendyLiteSighting(41, "10.0.0.4", true))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if d, ok := discoverWendyLiteLANDevices(ctx, 42); ok {
			t.Fatalf("found %+v, but board 42 never answered", d)
		}
	})
}

// stubContinuousBrowse points browseContinuousFn at a scripted browse for the
// rest of the test and returns its stopped channel (see scriptedBrowse).
func stubContinuousBrowse(t *testing.T, svcs ...discovery.MDNSService) <-chan struct{} {
	t.Helper()
	orig := browseContinuousFn
	t.Cleanup(func() { browseContinuousFn = orig })
	browse, stopped := scriptedBrowse(t, svcs...)
	browseContinuousFn = browse
	return stopped
}

// scriptedBrowse stands in for discovery.BrowseMDNSServicesContinuous: it
// streams svcs, then keeps the browse open until ctx ends, as a live browse
// does. stopped closes once the browse has seen ctx end. Call browse once.
func scriptedBrowse(t *testing.T, svcs ...discovery.MDNSService) (browse func(context.Context, string) (<-chan discovery.MDNSService, error), stopped <-chan struct{}) {
	done := make(chan struct{})
	return func(ctx context.Context, serviceType string) (<-chan discovery.MDNSService, error) {
		if serviceType != discovery.WendyLiteServiceType {
			t.Errorf("browsed %q, want %q", serviceType, discovery.WendyLiteServiceType)
		}
		ch := make(chan discovery.MDNSService)
		go func() {
			defer close(done)
			defer close(ch)
			for _, svc := range svcs {
				select {
				case ch <- svc:
				case <-ctx.Done():
					return
				}
			}
			<-ctx.Done()
		}()
		return ch, nil
	}, done
}

func wendyLiteSighting(assetID int, ip string, mtls bool) discovery.MDNSService {
	return discovery.MDNSService{
		IPAddress: ip,
		Port:      5054,
		TXTRecords: map[string]string{
			"assetid": strconv.Itoa(assetID),
			"mtls":    strconv.FormatBool(mtls),
		},
	}
}
