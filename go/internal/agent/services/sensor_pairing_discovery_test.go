package services_test

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newDiscoveryTestService(t *testing.T, browse services.SensorSourceBrowseFunc, self services.SensorSourceSelf) *services.SensorPairingService {
	t.Helper()
	store := mcusource.NewPairingStore(filepath.Join(t.TempDir(), "p.json"))
	svc := services.NewSensorPairingService(zap.NewNop(), store, func() certs.Scope { return certs.Scope{OrgID: 2} }, nil, nil, nil)
	svc.SetSourceDiscovery(browse, func() services.SensorSourceSelf { return self })
	return svc
}

func TestDiscoverSensorSourcesReturnsWhatThisDeviceSeesExceptItself(t *testing.T) {
	const tenant = "11111111-2222-4333-8444-555555555555"
	principal := certs.DeviceSPIFFEURI(tenant, "lite-441bf6804824")
	sensors := []string{"sensors"}
	var exclude func(models.LANDevice) bool
	browse := func(_ context.Context, _ time.Duration, ex func(models.LANDevice) bool) ([]models.LANDevice, error) {
		exclude = ex
		return []models.LANDevice{
			{ID: "me", DisplayName: "self by asset", AssetID: 5, IPAddress: "10.0.0.99", Sensorlink: true, Caps: sensors},
			{ID: "me2", DisplayName: "self by address", IPAddress: "10.0.0.2", Sensorlink: true, Caps: sensors},
			{ID: "me3", DisplayName: "self by zoned address", IPAddress: "fe80::2%eth0", Sensorlink: true, Caps: sensors},
			{ID: "lo", DisplayName: "loopback", IPAddress: "127.0.0.1", Sensorlink: true, Caps: sensors},
			{ID: "lh", DisplayName: "localhost", IPAddress: "localhost", Sensorlink: true, Caps: sensors},
			{ID: "plain", DisplayName: "not a sensor", IPAddress: "10.0.0.8"},
			{
				ID: "hub", DisplayName: "hub", Hostname: "hub.local", IPAddress: "10.0.0.7",
				Addresses: []string{"10.0.0.7", "::1", "fe80::7%eth0"}, Port: 50052, IsMTLS: true,
				AssetID: 7, OrgID: 2, Sensorlink: true, Caps: []string{"camera", "sensors"},
			},
			// A Wendy Lite board's asset ID never names this agent.
			{ID: "lite-asset", DisplayName: "board with agent's asset id", AssetID: 5, IPAddress: "10.0.0.50", Port: 5054, WendyLite: true, Sensorlink: true, Caps: sensors},
			{
				ID: "441bf6804824", DisplayName: "garage-cam", Hostname: "garage.local", IPAddress: "10.0.0.17",
				Addresses: []string{"10.0.0.17"}, Port: 5054, IsMTLS: true, TenantUUID: tenant,
				PrincipalURI: principal, WendyLite: true, Sensorlink: true, Caps: sensors,
			},
			// No address at all: kept, the agent resolves it by identity.
			{ID: "nameonly", DisplayName: "name only", Hostname: "nameonly.local", Sensorlink: true, Caps: sensors},
		}, nil
	}
	self := services.SensorSourceSelf{AssetID: 5, LocalIPs: []net.IP{net.ParseIP("10.0.0.2"), net.ParseIP("fe80::2")}}
	svc := newDiscoveryTestService(t, browse, self)

	resp, err := svc.DiscoverSensorSources(context.Background(), &agentpbv2.DiscoverSensorSourcesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// The browse itself is told which sightings are this device, so they
	// cannot end the scan before other sources answer.
	if exclude == nil || !exclude(models.LANDevice{AssetID: 5}) || !exclude(models.LANDevice{IPAddress: "10.0.0.2"}) ||
		exclude(models.LANDevice{ID: "hub", AssetID: 7, IPAddress: "10.0.0.7"}) {
		t.Fatal("browse was not given this device's self-match")
	}
	var names []string
	byID := map[string]*agentpbv2.DiscoveredSensorSource{}
	for _, s := range resp.Sources {
		names = append(names, s.DisplayName)
		byID[s.Id] = s
	}
	want := []string{"board with agent's asset id", "garage-cam", "hub", "name only"}
	if !slices.Equal(names, want) {
		t.Fatalf("sources = %v, want %v (sorted, self and non-sensors excluded)", names, want)
	}
	hub := byID["hub"]
	if hub.IpAddress != "10.0.0.7" || !slices.Equal(hub.Addresses, []string{"10.0.0.7", "fe80::7%eth0"}) ||
		hub.Port != 50052 || !hub.IsMtls || hub.AssetId != 7 || hub.OrgId != 2 || hub.Hostname != "hub.local" ||
		hub.WendyLite || !slices.Equal(hub.Caps, []string{"camera", "sensors"}) {
		t.Fatalf("agent source mapped wrong (loopback must be dropped): %+v", hub)
	}
	cam := byID["441bf6804824"]
	if cam.TenantUuid != tenant || cam.PrincipalUri != principal || !cam.WendyLite || cam.Port != 5054 || cam.IpAddress != "10.0.0.17" {
		t.Fatalf("Wendy Lite source mapped wrong: %+v", cam)
	}
}

func TestDiscoverSensorSourcesBoundsTheScan(t *testing.T) {
	var got time.Duration
	calls := 0
	browse := func(_ context.Context, timeout time.Duration, _ func(models.LANDevice) bool) ([]models.LANDevice, error) {
		calls++
		got = timeout
		return nil, nil
	}
	svc := newDiscoveryTestService(t, browse, services.SensorSourceSelf{})
	for _, tc := range []struct {
		timeoutMS uint32
		want      time.Duration
	}{
		{0, 5 * time.Second},
		{2000, 2 * time.Second},
		{60000, 10 * time.Second},
		{^uint32(0), 10 * time.Second},
	} {
		if _, err := svc.DiscoverSensorSources(context.Background(), &agentpbv2.DiscoverSensorSourcesRequest{TimeoutMs: tc.timeoutMS}); err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Fatalf("timeout_ms=%d scanned for %v, want %v", tc.timeoutMS, got, tc.want)
		}
	}

	// The scan ends early enough to answer before the call's deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if _, err := svc.DiscoverSensorSources(ctx, &agentpbv2.DiscoverSensorSourcesRequest{}); err != nil {
		t.Fatal(err)
	}
	if got > 3*time.Second || got < 2*time.Second {
		t.Fatalf("scanned for %v with 4s left, want about 3s", got)
	}

	// Too little time left to scan and answer.
	ctx, cancel = context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	before := calls
	_, err := svc.DiscoverSensorSources(ctx, &agentpbv2.DiscoverSensorSourcesRequest{})
	if status.Code(err) != codes.DeadlineExceeded || calls != before {
		t.Fatalf("err = %v, browsed %d times; want DeadlineExceeded without a scan", err, calls-before)
	}
}

func TestDiscoverSensorSourcesReportsCancellationAndFailures(t *testing.T) {
	started := make(chan struct{})
	svc := newDiscoveryTestService(t, func(ctx context.Context, _ time.Duration, _ func(models.LANDevice) bool) ([]models.LANDevice, error) {
		close(started)
		<-ctx.Done()
		// A browse cut short returns what it had; the client is gone anyway.
		return []models.LANDevice{{ID: "partial", Sensorlink: true}}, ctx.Err()
	}, services.SensorSourceSelf{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	if _, err := svc.DiscoverSensorSources(ctx, &agentpbv2.DiscoverSensorSourcesRequest{}); status.Code(err) != codes.Canceled {
		t.Fatalf("err = %v, want Canceled", err)
	}

	svc = newDiscoveryTestService(t, func(context.Context, time.Duration, func(models.LANDevice) bool) ([]models.LANDevice, error) {
		return nil, errors.New("avahi: bus closed")
	}, services.SensorSourceSelf{})
	_, err := svc.DiscoverSensorSources(context.Background(), &agentpbv2.DiscoverSensorSourcesRequest{})
	if status.Code(err) != codes.Internal || status.Convert(err).Message() != "sensor source discovery failed on this device" {
		t.Fatalf("err = %v, want Internal without backend details", err)
	}
}

func TestDiscoverSensorSourcesLimitsConcurrentScans(t *testing.T) {
	var active, peak, scans atomic.Int32
	release := make(chan struct{})
	svc := newDiscoveryTestService(t, func(ctx context.Context, _ time.Duration, _ func(models.LANDevice) bool) ([]models.LANDevice, error) {
		n := active.Add(1)
		defer active.Add(-1)
		scans.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return []models.LANDevice{{ID: "hub", DisplayName: "hub", IPAddress: "10.0.0.7", Sensorlink: true}}, nil
	}, services.SensorSourceSelf{})

	const callers = 5
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := svc.DiscoverSensorSources(context.Background(), &agentpbv2.DiscoverSensorSourcesRequest{})
			if err == nil && len(resp.Sources) != 1 {
				err = errors.New("missing source")
			}
			errs <- err
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for active.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// A waiting caller that gives up leaves without scanning.
	ctx, cancel := context.WithTimeout(context.Background(), 1100*time.Millisecond)
	defer cancel()
	if _, err := svc.DiscoverSensorSources(ctx, &agentpbv2.DiscoverSensorSourcesRequest{}); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("queued caller err = %v, want DeadlineExceeded", err)
	}
	if active.Load() != 2 {
		t.Fatalf("%d scans running, want 2", active.Load())
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if peak.Load() != 2 || scans.Load() != callers {
		t.Fatalf("peak %d concurrent scans over %d scans, want 2 over %d", peak.Load(), scans.Load(), callers)
	}
}
