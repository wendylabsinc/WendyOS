package discovery

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/ble"
	"github.com/wendylabsinc/wendy/go/internal/shared/ble/scan"
)

// swapBLELiteSeams installs fake scan and probe backends for the duration of a
// test, restoring the real ones afterwards. It also shrinks the retry delay so
// a retry test doesn't wait a quarter of a minute.
func swapBLELiteSeams(
	t *testing.T,
	scanFn func(context.Context, scan.Options) (<-chan []scan.BLEDeviceInfo, error),
	probeFn func(string, time.Duration) (*ble.LiteInfo, error),
) {
	t.Helper()
	origScan, origProbe, origDelay := bleLiteScanFn, bleLiteProbeFn, bleLiteProbeRetryDelay
	t.Cleanup(func() {
		bleLiteScanFn, bleLiteProbeFn, bleLiteProbeRetryDelay = origScan, origProbe, origDelay
	})
	bleLiteScanFn, bleLiteProbeFn = scanFn, probeFn
	bleLiteProbeRetryDelay = 10 * time.Millisecond
}

// staticScan returns a scan seam that emits one snapshot and then stays open
// until ctx is done, which is how the real scanner behaves once every device in
// range has been seen.
func staticScan(devices ...scan.BLEDeviceInfo) func(context.Context, scan.Options) (<-chan []scan.BLEDeviceInfo, error) {
	return func(ctx context.Context, _ scan.Options) (<-chan []scan.BLEDeviceInfo, error) {
		ch := make(chan []scan.BLEDeviceInfo, 1)
		ch <- devices
		go func() {
			<-ctx.Done()
			close(ch)
		}()
		return ch, nil
	}
}

// startBLELiteStream starts a stream and guarantees its goroutine has exited
// before the seam restoration registered by swapBLELiteSeams runs — cleanups
// run last-registered-first, and the loop reads bleLiteProbeRetryDelay that the
// restoration writes.
func startBLELiteStream(t *testing.T) (<-chan []BLELiteDevice, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := BLELiteDeviceDiscoverContinuous(ctx)
	if err != nil {
		cancel()
		t.Fatalf("BLELiteDeviceDiscoverContinuous: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		for range ch { //nolint:revive — drain until the stream goroutine is gone
		}
	})
	return ch, cancel
}

// nextEmit reads one snapshot, failing the test if none arrives in time.
func nextEmit(t *testing.T, ch <-chan []BLELiteDevice) []BLELiteDevice {
	t.Helper()
	select {
	case devices, ok := <-ch:
		if !ok {
			t.Fatal("stream closed before emitting")
		}
		return devices
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an emit")
		return nil
	}
}

func TestBLELiteDiscoveryEmitsOnlyProbedDevices(t *testing.T) {
	var gotServices []string
	scanFn := func(ctx context.Context, opts scan.Options) (<-chan []scan.BLEDeviceInfo, error) {
		gotServices = opts.Services
		return staticScan(
			scan.BLEDeviceInfo{Address: "silent", Name: "wendy-silent", RSSI: -20},
			scan.BLEDeviceInfo{Address: "talker", Name: "wendy-talker", RSSI: -50},
		)(ctx, opts)
	}
	swapBLELiteSeams(t, scanFn, func(address string, _ time.Duration) (*ble.LiteInfo, error) {
		if address != "talker" {
			return nil, ble.ErrLiteInfoUnavailable
		}
		return &ble.LiteInfo{PSM: 128, DeviceID: "abc", DisplayName: "Talker"}, nil
	})

	ch, _ := startBLELiteStream(t)

	devices := nextEmit(t, ch)
	if len(devices) != 1 {
		t.Fatalf("got %d devices, want only the one that answered: %+v", len(devices), devices)
	}
	got := devices[0]
	if got.Address != "talker" || got.Name != "wendy-talker" || got.RSSI != -50 {
		t.Errorf("sighting fields not carried through: %+v", got)
	}
	if got.Info.PSM != 128 || got.Info.DisplayName != "Talker" {
		t.Errorf("info not carried through: %+v", got.Info)
	}
	if len(gotServices) != 1 || gotServices[0] != ble.LiteInfoServiceUUID {
		t.Errorf("scan filtered on %v, want [%s]", gotServices, ble.LiteInfoServiceUUID)
	}
}

func TestBLELiteDiscoverySortsByRSSIDescending(t *testing.T) {
	swapBLELiteSeams(t,
		staticScan(
			scan.BLEDeviceInfo{Address: "far", RSSI: -80},
			scan.BLEDeviceInfo{Address: "near", RSSI: -30},
		),
		func(address string, _ time.Duration) (*ble.LiteInfo, error) {
			return &ble.LiteInfo{PSM: 128, DeviceName: address}, nil
		})

	ch, _ := startBLELiteStream(t)

	// The first emit carries whichever device was probed first; the second
	// carries both, which is the one that must be ordered.
	nextEmit(t, ch)
	devices := nextEmit(t, ch)
	if len(devices) != 2 {
		t.Fatalf("got %d devices, want 2: %+v", len(devices), devices)
	}
	if devices[0].Address != "near" || devices[1].Address != "far" {
		t.Errorf("got order %s, %s; want strongest signal first", devices[0].Address, devices[1].Address)
	}
}

func TestNextBLELiteProbePrefersKnownRSSIOverUnreported(t *testing.T) {
	entries := map[string]*bleLiteEntry{
		"unreported": {rssi: 0},
		"weak":       {rssi: -90},
	}
	if got := nextBLELiteProbe(entries); got != "weak" {
		t.Errorf("got %q, want %q: a known signal must outrank an unreported one", got, "weak")
	}
}

func TestNextBLELiteProbeTiesBreakByAddressWhenAllUnreported(t *testing.T) {
	entries := map[string]*bleLiteEntry{
		"b": {rssi: 0},
		"a": {rssi: 0},
	}
	if got := nextBLELiteProbe(entries); got != "a" {
		t.Errorf("got %q, want %q", got, "a")
	}
}

func TestBLELiteDiscoveryRetriesThenAbandons(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	swapBLELiteSeams(t,
		staticScan(scan.BLEDeviceInfo{Address: "deaf", RSSI: -40}),
		func(string, time.Duration) (*ble.LiteInfo, error) {
			mu.Lock()
			attempts++
			mu.Unlock()
			return nil, ble.ErrLiteInfoUnavailable
		})

	ch, _ := startBLELiteStream(t)

	// Long enough for many retry ticks, so a missing cap would show up as far
	// more attempts than the budget.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := attempts >= bleLiteProbeAttempts
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	got := attempts
	mu.Unlock()
	if got != bleLiteProbeAttempts {
		t.Errorf("probed %d times, want exactly %d before giving up", got, bleLiteProbeAttempts)
	}
	select {
	case devices := <-ch:
		t.Errorf("emitted %+v; a device that never answered must not appear", devices)
	default:
	}
}

func TestBLELiteDiscoveryProbesOneAtATime(t *testing.T) {
	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	swapBLELiteSeams(t,
		staticScan(
			scan.BLEDeviceInfo{Address: "a", RSSI: -30},
			scan.BLEDeviceInfo{Address: "b", RSSI: -40},
			scan.BLEDeviceInfo{Address: "c", RSSI: -50},
		),
		func(address string, _ time.Duration) (*ble.LiteInfo, error) {
			mu.Lock()
			inFlight++
			if inFlight > maxInFlight {
				maxInFlight = inFlight
			}
			mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			mu.Lock()
			inFlight--
			mu.Unlock()
			return &ble.LiteInfo{PSM: 128, DeviceName: address}, nil
		})

	ch, _ := startBLELiteStream(t)

	for {
		if len(nextEmit(t, ch)) == 3 {
			break
		}
	}
	mu.Lock()
	got := maxInFlight
	mu.Unlock()
	if got != 1 {
		t.Errorf("%d probes ran at once; the radio can only serve one", got)
	}
}

func TestBLELiteDiscoveryStartErrorPropagates(t *testing.T) {
	wantErr := errors.New("no Bluetooth on this platform")
	swapBLELiteSeams(t,
		func(context.Context, scan.Options) (<-chan []scan.BLEDeviceInfo, error) {
			return nil, wantErr
		},
		func(string, time.Duration) (*ble.LiteInfo, error) {
			t.Error("probed despite the scan failing to start")
			return nil, nil
		})

	if _, err := BLELiteDeviceDiscoverContinuous(context.Background()); !errors.Is(err, wantErr) {
		t.Errorf("got error %v, want %v", err, wantErr)
	}
}

func TestBLELiteDiscoveryCancelClosesStream(t *testing.T) {
	swapBLELiteSeams(t,
		staticScan(scan.BLEDeviceInfo{Address: "a", RSSI: -30}),
		func(address string, _ time.Duration) (*ble.LiteInfo, error) {
			return &ble.LiteInfo{PSM: 128, DeviceName: address}, nil
		})

	ch, cancel := startBLELiteStream(t)
	nextEmit(t, ch)
	cancel()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("stream stayed open after cancellation")
		}
	}
}

func TestBLELiteDiscoveryScanEndEndsStream(t *testing.T) {
	swapBLELiteSeams(t,
		func(context.Context, scan.Options) (<-chan []scan.BLEDeviceInfo, error) {
			ch := make(chan []scan.BLEDeviceInfo)
			close(ch)
			return ch, nil
		},
		func(string, time.Duration) (*ble.LiteInfo, error) {
			t.Error("probed despite the scan having ended")
			return nil, nil
		})

	ch, err := BLELiteDeviceDiscoverContinuous(context.Background())
	if err != nil {
		t.Fatalf("BLELiteDeviceDiscoverContinuous: %v", err)
	}
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("emitted a device from a scan that never reported one")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream stayed open after the scan ended")
	}
}

// TestBLELiteDiscoveryWiresPreflight proves BLELiteDeviceDiscoverContinuous
// passes a non-nil Preflight into scan.Options.
func TestBLELiteDiscoveryWiresPreflight(t *testing.T) {
	var gotOpts scan.Options
	swapBLELiteSeams(t,
		func(_ context.Context, opts scan.Options) (<-chan []scan.BLEDeviceInfo, error) {
			gotOpts = opts
			ch := make(chan []scan.BLEDeviceInfo)
			close(ch)
			return ch, nil
		},
		func(string, time.Duration) (*ble.LiteInfo, error) { return nil, nil })

	ch, err := BLELiteDeviceDiscoverContinuous(context.Background())
	if err != nil {
		t.Fatalf("BLELiteDeviceDiscoverContinuous: %v", err)
	}
	for range ch { //nolint:revive — drain until the stream goroutine exits
	}
	if gotOpts.Preflight == nil {
		t.Fatal("BLELiteDeviceDiscoverContinuous did not wire a Preflight into scan.Options")
	}
}

// TestBLELiteDiscoveryPreflightFailurePropagates proves a failing Preflight
// surfaces as BLELiteDeviceDiscoverContinuous's returned error. This does not
// swap bleLiteScanFn — it runs the real scan.DiscoverBluetoothContinuous,
// which checks Preflight before touching any scanner backend, so this never
// reaches cgo.
func TestBLELiteDiscoveryPreflightFailurePropagates(t *testing.T) {
	origPreflight := blePreflightFn
	t.Cleanup(func() { blePreflightFn = origPreflight })

	wantErr := errors.New("bluetooth unavailable - your terminal may not have Bluetooth permission")
	blePreflightFn = func(context.Context) error { return wantErr }

	if _, err := BLELiteDeviceDiscoverContinuous(context.Background()); !errors.Is(err, wantErr) {
		t.Errorf("got error %v, want %v", err, wantErr)
	}
}
