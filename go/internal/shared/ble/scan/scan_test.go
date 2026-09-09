package scan

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

// fakeScanner is a scanner whose readings the test controls. It stands in for a
// real radio via the newScannerFn seam.
type fakeScanner struct {
	mu       sync.Mutex
	readings [][]BLEDeviceInfo
	err      error
	// errWithLastReading, if set, is returned alongside the final configured
	// reading instead of nil — simulating a backend that reports something new
	// in the very call that it reports a fatal error, the way windowsScanner's
	// accumulated map can.
	errWithLastReading error
	calls              int
	closed             bool
}

// Snapshot returns each queued reading in turn, then repeats the last one — a
// real backend keeps reporting what it can see, it does not run out.
func (f *fakeScanner) Snapshot(_ context.Context) ([]BLEDeviceInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if len(f.readings) == 0 {
		return nil, nil
	}
	if f.calls-1 < len(f.readings)-1 {
		return f.readings[f.calls-1], nil
	}
	return f.readings[len(f.readings)-1], f.errWithLastReading
}

func (f *fakeScanner) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
}

func (f *fakeScanner) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// withFakeScanner installs f as the backend for the duration of the test.
func withFakeScanner(t *testing.T, f *fakeScanner) {
	t.Helper()
	orig := newScannerFn
	newScannerFn = func(context.Context, []string) (scanner, error) { return f, nil }
	t.Cleanup(func() { newScannerFn = orig })
}

// testInterval keeps the tests fast; the engine samples on this cadence.
const testInterval = 5 * time.Millisecond

// recv reads one emit with a generous timeout so a slow CI machine does not
// produce a flake.
func recv(t *testing.T, ch <-chan []BLEDeviceInfo) []BLEDeviceInfo {
	t.Helper()
	select {
	case devices, ok := <-ch:
		if !ok {
			t.Fatal("channel closed while awaiting an emit")
		}
		return devices
	case <-time.After(2 * time.Second):
		t.Fatal("timed out awaiting an emit")
		return nil
	}
}

func TestDiscoverEmitsFirstReadingImmediately(t *testing.T) {
	withFakeScanner(t, &fakeScanner{readings: [][]BLEDeviceInfo{
		{{Address: "AA:BB:CC:DD:EE:FF", Name: "one", RSSI: -50}},
	}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := DiscoverBluetoothContinuous(ctx, Options{Interval: time.Hour})
	if err != nil {
		t.Fatalf("DiscoverBluetoothContinuous: %v", err)
	}

	// Interval is an hour, so anything received proves the up-front sample
	// happened rather than the first tick.
	got := recv(t, ch)
	if len(got) != 1 || got[0].Name != "one" {
		t.Fatalf("first emit = %+v, want the single device", got)
	}
}

func TestDiscoverSortsByRSSIDescending(t *testing.T) {
	withFakeScanner(t, &fakeScanner{readings: [][]BLEDeviceInfo{{
		{Address: "03", Name: "weak", RSSI: -90},
		{Address: "01", Name: "strong", RSSI: -30},
		{Address: "02", Name: "middle", RSSI: -60},
	}}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := DiscoverBluetoothContinuous(ctx, Options{Interval: time.Hour})
	if err != nil {
		t.Fatalf("DiscoverBluetoothContinuous: %v", err)
	}

	got := recv(t, ch)
	want := []string{"strong", "middle", "weak"}
	names := make([]string, len(got))
	for i, d := range got {
		names[i] = d.Name
	}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("emit order = %v, want %v (strongest first)", names, want)
	}
}

func TestDiscoverAccumulatesAndNeverRemoves(t *testing.T) {
	// The second reading drops the first device entirely, as BlueZ does when it
	// evicts a stale entry. It must still appear.
	withFakeScanner(t, &fakeScanner{readings: [][]BLEDeviceInfo{
		{{Address: "01", Name: "first", RSSI: -50}},
		{{Address: "02", Name: "second", RSSI: -40}},
	}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := DiscoverBluetoothContinuous(ctx, Options{Interval: testInterval})
	if err != nil {
		t.Fatalf("DiscoverBluetoothContinuous: %v", err)
	}

	// Read until both devices are present, or time out.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case got, ok := <-ch:
			if !ok {
				t.Fatal("channel closed before both devices appeared")
			}
			if len(got) == 2 {
				names := map[string]bool{got[0].Name: true, got[1].Name: true}
				if !names["first"] || !names["second"] {
					t.Fatalf("emit = %+v, want both first and second", got)
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for both devices to accumulate")
		}
	}
}

func TestDiscoverPreservesFieldsASparseSightingOmits(t *testing.T) {
	// A device commonly splits its name and its service UUIDs across the
	// advertisement and the scan response, so a later reading may carry only
	// a refreshed RSSI.
	withFakeScanner(t, &fakeScanner{readings: [][]BLEDeviceInfo{
		{{Address: "01", Name: "named", ServiceUUIDs: []string{"180F"}, RSSI: -50}},
		{{Address: "01", RSSI: -55}},
	}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := DiscoverBluetoothContinuous(ctx, Options{Interval: testInterval})
	if err != nil {
		t.Fatalf("DiscoverBluetoothContinuous: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case got, ok := <-ch:
			if !ok {
				t.Fatal("channel closed before the RSSI update arrived")
			}
			if len(got) != 1 {
				t.Fatalf("emit = %+v, want one device", got)
			}
			if got[0].RSSI != -55 {
				continue // not yet the second reading
			}
			if got[0].Name != "named" {
				t.Errorf("Name = %q after a sparse sighting, want it preserved", got[0].Name)
			}
			if len(got[0].ServiceUUIDs) != 1 {
				t.Errorf("ServiceUUIDs = %v after a sparse sighting, want them preserved", got[0].ServiceUUIDs)
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for the RSSI update")
		}
	}
}

func TestDiscoverFiltersByService(t *testing.T) {
	wanted := "7565e9eb-4c20-4b67-9272-d708b397b631"
	withFakeScanner(t, &fakeScanner{readings: [][]BLEDeviceInfo{{
		{Address: "01", Name: "wanted", ServiceUUIDs: []string{wanted}, RSSI: -50},
		{Address: "02", Name: "other", ServiceUUIDs: []string{"180F"}, RSSI: -40},
		{Address: "03", Name: "silent", RSSI: -30},
	}}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Filter given in a different case and spelling than the device reports, to
	// prove canonicalization runs on both sides.
	ch, err := DiscoverBluetoothContinuous(ctx, Options{
		Services: []string{"7565E9EB-4C20-4B67-9272-D708B397B631"},
		Interval: time.Hour,
	})
	if err != nil {
		t.Fatalf("DiscoverBluetoothContinuous: %v", err)
	}

	got := recv(t, ch)
	if len(got) != 1 || got[0].Name != "wanted" {
		t.Fatalf("emit = %+v, want only the device advertising the wanted service", got)
	}
	if got[0].ServiceUUIDs[0] != CanonicalUUID(wanted) {
		t.Errorf("ServiceUUIDs = %v, want canonicalized", got[0].ServiceUUIDs)
	}
}

func TestDiscoverEmptyServicesReportsEverything(t *testing.T) {
	withFakeScanner(t, &fakeScanner{readings: [][]BLEDeviceInfo{{
		{Address: "01", ServiceUUIDs: []string{"180F"}, RSSI: -50},
		{Address: "02", RSSI: -40},
	}}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := DiscoverBluetoothContinuous(ctx, Options{Interval: time.Hour})
	if err != nil {
		t.Fatalf("DiscoverBluetoothContinuous: %v", err)
	}

	if got := recv(t, ch); len(got) != 2 {
		t.Fatalf("emit = %+v, want both devices when no filter is set", got)
	}
}

func TestDiscoverSkipsAddresslessDevices(t *testing.T) {
	withFakeScanner(t, &fakeScanner{readings: [][]BLEDeviceInfo{{
		{Address: "", Name: "no address", RSSI: -50},
		{Address: "01", Name: "fine", RSSI: -40},
	}}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := DiscoverBluetoothContinuous(ctx, Options{Interval: time.Hour})
	if err != nil {
		t.Fatalf("DiscoverBluetoothContinuous: %v", err)
	}

	got := recv(t, ch)
	if len(got) != 1 || got[0].Name != "fine" {
		t.Fatalf("emit = %+v, want only the addressed device", got)
	}
}

// TestDiscoverDoesNotReemitUnchangedSet is the coalescing guarantee: without it
// an unchanging device would push an emit on every single tick.
func TestDiscoverDoesNotReemitUnchangedSet(t *testing.T) {
	withFakeScanner(t, &fakeScanner{readings: [][]BLEDeviceInfo{
		{{Address: "01", Name: "steady", RSSI: -50}},
	}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := DiscoverBluetoothContinuous(ctx, Options{Interval: testInterval})
	if err != nil {
		t.Fatalf("DiscoverBluetoothContinuous: %v", err)
	}

	recv(t, ch) // the initial emit

	// Many ticks pass with an identical reading; nothing more should arrive.
	select {
	case got, ok := <-ch:
		if ok {
			t.Fatalf("re-emitted an unchanged set: %+v", got)
		}
		t.Fatal("channel closed unexpectedly")
	case <-time.After(20 * testInterval):
	}
}

func TestDiscoverStopsAndClosesBackendOnCancel(t *testing.T) {
	fake := &fakeScanner{readings: [][]BLEDeviceInfo{
		{{Address: "01", Name: "one", RSSI: -50}},
	}}
	withFakeScanner(t, fake)

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := DiscoverBluetoothContinuous(ctx, Options{Interval: testInterval})
	if err != nil {
		t.Fatalf("DiscoverBluetoothContinuous: %v", err)
	}
	recv(t, ch)

	cancel()

	// Draining to closure proves the goroutine returned.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				if !fake.isClosed() {
					t.Error("backend was not closed when the stream ended")
				}
				return
			}
		case <-deadline:
			t.Fatal("channel was not closed after ctx cancellation")
		}
	}
}

func TestDiscoverEndsStreamOnBackendError(t *testing.T) {
	withFakeScanner(t, &fakeScanner{err: errors.New("adapter went away")})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := DiscoverBluetoothContinuous(ctx, Options{Interval: testInterval})
	if err != nil {
		t.Fatalf("DiscoverBluetoothContinuous: %v", err)
	}

	select {
	case got, ok := <-ch:
		if ok {
			t.Fatalf("emitted %+v despite a failing backend", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not end when the backend failed")
	}
}

// TestDiscoverFlushesFinalReadingBeforeEndingOnBackendError guards the fix for
// a backend (windowsScanner) that can report a newly-seen device in the very
// same call that it reports the fatal error which ends the backend for good.
// That device must still reach the consumer before the stream closes.
func TestDiscoverFlushesFinalReadingBeforeEndingOnBackendError(t *testing.T) {
	fake := &fakeScanner{
		readings: [][]BLEDeviceInfo{
			{{Address: "01", Name: "first", RSSI: -50}},
			{{Address: "01", Name: "first", RSSI: -50}, {Address: "02", Name: "second", RSSI: -40}},
		},
		errWithLastReading: errors.New("watcher died"),
	}
	withFakeScanner(t, fake)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := DiscoverBluetoothContinuous(ctx, Options{Interval: testInterval})
	if err != nil {
		t.Fatalf("DiscoverBluetoothContinuous: %v", err)
	}

	recv(t, ch) // the first reading, no error yet

	deadline := time.After(2 * time.Second)
	for {
		select {
		case got, ok := <-ch:
			if !ok {
				t.Fatal("channel closed before the final reading was flushed")
			}
			if len(got) != 2 {
				continue // not yet the reading paired with the error
			}
			select {
			case _, ok := <-ch:
				if ok {
					t.Fatal("stream kept emitting after a fatal backend error")
				}
				return
			case <-time.After(2 * time.Second):
				t.Fatal("stream did not end after the fatal backend error")
			}
		case <-deadline:
			t.Fatal("timed out waiting for the final reading to be flushed")
		}
	}
}

func TestDiscoverPreflightFailureIsReturned(t *testing.T) {
	fake := &fakeScanner{}
	withFakeScanner(t, fake)

	want := errors.New("Bluetooth unavailable")
	_, err := DiscoverBluetoothContinuous(context.Background(), Options{
		Preflight: func(context.Context) error { return want },
	})
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	if fake.isClosed() {
		t.Error("backend was opened despite Preflight failing")
	}
}

func TestDiscoverDefaultsInterval(t *testing.T) {
	// A zero Interval must not mean "tick as fast as possible", which would
	// spin a core; it means DefaultInterval.
	withFakeScanner(t, &fakeScanner{readings: [][]BLEDeviceInfo{
		{{Address: "01", Name: "one", RSSI: -50}},
	}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := DiscoverBluetoothContinuous(ctx, Options{})
	if err != nil {
		t.Fatalf("DiscoverBluetoothContinuous: %v", err)
	}
	// The up-front sample is unconditional, so this arrives without waiting a
	// full DefaultInterval.
	if got := recv(t, ch); len(got) != 1 {
		t.Fatalf("emit = %+v, want one device", got)
	}
}

func TestDeviceStoreMergeReportsChange(t *testing.T) {
	store := newDeviceStore()

	if !store.merge(BLEDeviceInfo{Address: "01", Name: "a", RSSI: -50}) {
		t.Fatal("first sighting reported no change")
	}
	if store.merge(BLEDeviceInfo{Address: "01", Name: "a", RSSI: -50}) {
		t.Error("identical sighting reported a change")
	}
	if !store.merge(BLEDeviceInfo{Address: "01", Name: "a", RSSI: -60}) {
		t.Error("changed RSSI reported no change")
	}
	if !store.merge(BLEDeviceInfo{Address: "01", Name: "b", RSSI: -60}) {
		t.Error("changed name reported no change")
	}
	if store.merge(BLEDeviceInfo{Address: "01"}) {
		t.Error("empty sighting reported a change")
	}

	got := store.snapshot()
	if len(got) != 1 || got[0].Name != "b" || got[0].RSSI != -60 {
		t.Errorf("snapshot = %+v, want the merged latest values", got)
	}
}

// TestDeviceStoreSnapshotIsStable guards the tiebreak: without it, equal-RSSI
// devices would reorder between emits because Go randomizes map iteration, and
// a picker would visibly shuffle.
func TestDeviceStoreSnapshotIsStable(t *testing.T) {
	store := newDeviceStore()
	for _, addr := range []string{"03", "01", "04", "02"} {
		store.merge(BLEDeviceInfo{Address: addr, RSSI: -50})
	}

	first := store.snapshot()
	for i := 0; i < 20; i++ {
		if got := store.snapshot(); !reflect.DeepEqual(got, first) {
			t.Fatalf("snapshot %d = %+v, want the stable order %+v", i, got, first)
		}
	}
}
