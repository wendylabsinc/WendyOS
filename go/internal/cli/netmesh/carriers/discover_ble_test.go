package carriers

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/bleprovider"
	"github.com/wendylabsinc/wendy/go/internal/shared/ble/scan"
)

func TestBoundedBLEDiscoveryUsesLatestFreshHints(t *testing.T) {
	self, peer := testDevice(1), testDevice(2)
	now := time.Now()
	adv, err := bleprovider.NewAdvertisement(peer.UUID(), fmt.Sprintf("tenant:%s:default", self.Tenant()), 129)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := adv.MarshalBinary()
	device := func(address string) scan.BLEDeviceInfo {
		return scan.BLEDeviceInfo{Address: address, LastSeen: now, ServiceDataSeen: now, ServiceData: map[string][]byte{bleprovider.ServiceUUID(self.Tenant()): raw}}
	}
	window, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan []scan.BLEDeviceInfo)
	type result struct {
		items []Candidate
		err   error
	}
	done := make(chan result, 1)
	go func() {
		items, err := collectBLECandidates(context.Background(), window, self, updates)
		done <- result{items, err}
	}()
	updates <- []scan.BLEDeviceInfo{device("old")}
	updates <- []scan.BLEDeviceInfo{device("latest")}
	cancel()
	r := <-done
	if r.err != nil || len(r.items) != 1 || r.items[0].Address != "latest" || r.items[0].PSM != 129 || r.items[0].Device.IsValid() {
		t.Fatalf("incorrect discovery snapshot/identity: %+v %v", r.items, r.err)
	}
	// Timeout expiry must re-evaluate freshness, not return a stale candidate
	// that was fresh when an earlier snapshot arrived.
	stale := device("stale")
	stale.LastSeen = now.Add(-31 * time.Second)
	window, cancel = context.WithCancel(context.Background())
	updates = make(chan []scan.BLEDeviceInfo)
	go func() {
		items, err := collectBLECandidates(context.Background(), window, self, updates)
		done <- result{items, err}
	}()
	updates <- []scan.BLEDeviceInfo{stale}
	cancel()
	r = <-done
	if r.err != nil || len(r.items) != 0 {
		t.Fatalf("stale radio returned: %+v %v", r.items, r.err)
	}
}

func TestBLEDiscoveryHasFiniteResultsAndDoesNotHideScanFailure(t *testing.T) {
	self := testDevice(1)
	closed := make(chan []scan.BLEDeviceInfo)
	close(closed)
	if _, err := collectBLECandidates(context.Background(), context.Background(), self, closed); err == nil {
		t.Fatal("unexpected scanner closure reported as no nearby peers")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DiscoverBLE(ctx, self, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled scan touched startup or hid cancellation: %v", err)
	}
	for _, duration := range []time.Duration{0, -time.Second, 31 * time.Second} {
		if _, err := DiscoverBLE(context.Background(), self, duration); err == nil {
			t.Fatalf("unbounded duration accepted: %v", duration)
		}
	}
}

func TestBLEDiscoveryBoundsRadioAndJoinsScanner(t *testing.T) {
	previous := startBLEDiscoveryScan
	t.Cleanup(func() { startBLEDiscoveryScan = previous })
	self, peer := testDevice(1), testDevice(2)
	want := errors.New("Bluetooth permission unavailable")
	startBLEDiscoveryScan = func(context.Context, scan.Options) (<-chan []scan.BLEDeviceInfo, error) { return nil, want }
	if _, err := DiscoverBLE(context.Background(), self, time.Second); !errors.Is(err, want) {
		t.Fatalf("startup failure hidden as empty list: %v", err)
	}
	adv, err := bleprovider.NewAdvertisement(peer.UUID(), fmt.Sprintf("tenant:%s:default", self.Tenant()), 129)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := adv.MarshalBinary()
	stopped := make(chan struct{})
	startBLEDiscoveryScan = func(ctx context.Context, options scan.Options) (<-chan []scan.BLEDeviceInfo, error) {
		if options.MaxDevices != 256 || options.Preflight == nil || len(options.Services) != 0 {
			t.Fatalf("unbounded or incorrectly filtered scanner: %+v", options)
		}
		updates := make(chan []scan.BLEDeviceInfo)
		go func() {
			now := time.Now()
			select {
			case updates <- []scan.BLEDeviceInfo{{Address: "native radio ID", LastSeen: now, ServiceDataSeen: now, ServiceData: map[string][]byte{bleprovider.ServiceUUID(self.Tenant()): raw}}}:
			case <-ctx.Done():
			}
			<-ctx.Done()
			close(stopped)
			close(updates)
		}()
		return updates, nil
	}
	rows, err := DiscoverBLE(context.Background(), self, 100*time.Millisecond)
	if err != nil || len(rows) != 1 || rows[0].Address != "native radio ID" || rows[0].Device.IsValid() {
		t.Fatalf("native hints changed: %+v %v", rows, err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("discovery returned with a scanner still running")
	}
}
