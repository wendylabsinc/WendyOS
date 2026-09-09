// Package scan is a generic, continuously-streaming BLE scanner for the
// central (client) side. It complements the sibling central package, which
// connects to a peripheral but, as the README says, "does no scanning or
// discovery of its own".
//
// Nothing here knows about any particular device or protocol: the service UUIDs
// to look for are an argument, and the results carry only what a BLE
// advertisement can actually supply. An L2CAP PSM is deliberately absent — it
// is not advertised, and can only be learned from a GATT read after connecting.
//
// The Address a scan reports is the string central.Connect expects on the same
// platform, so results feed straight into a connection.
package scan

import (
	"context"
	"sort"
	"time"
)

// BLEDeviceInfo is one device seen during a scan.
type BLEDeviceInfo struct {
	// Address identifies the device to ble.Connect. Its meaning is
	// platform-dependent: a CoreBluetooth peripheral UUID on macOS (the OS
	// never exposes the hardware MAC), an "AA:BB:CC:DD:EE:FF" MAC on Linux and
	// Windows.
	Address string

	// Name is the advertised local name, "" when none is available. It is a
	// display label, not an identity: macOS and Linux fall back to an
	// OS-cached name, so it may outlive the advertisement it came from.
	Name string

	// ServiceUUIDs are the advertised service UUIDs, canonicalized to
	// uppercase 128-bit form.
	ServiceUUIDs []string

	// RSSI is signal strength in dBm from the most recent sighting; 0 when the
	// platform does not report it.
	RSSI int
}

// Options configures a continuous scan.
type Options struct {
	// Services filters results: a device is reported when it advertises at
	// least one of these. Empty reports every device seen. Any spelling is
	// accepted — 16-bit, 32-bit or full 128-bit, either case.
	Services []string

	// Interval bounds how often the stream emits, and how often the platform
	// backend is sampled. Zero uses DefaultInterval.
	Interval time.Duration

	// Preflight optionally gates the scan before the radio is touched, for a
	// caller that has its own way of testing BLE availability. A non-nil error
	// fails DiscoverBluetoothContinuous. Nil skips the check.
	//
	// This is a func rather than a built-in probe so that a caller needing to
	// run RunBLECheck in a subprocess (see its doc comment) owns that policy —
	// how to re-invoke itself is not something this package can know.
	Preflight func(context.Context) error
}

// DefaultInterval is the emit and sampling cadence when Options.Interval is
// zero. One second keeps a device picker feeling live without resampling the
// backend more often than advertisements typically arrive.
const DefaultInterval = time.Second

// streamBuffer decouples the scan loop from a consumer that renders between
// reads. Snapshots are coalesced rather than queued, so a small buffer is
// enough; a full buffer simply makes the loop skip an emit and try again on the
// next tick with even fresher data.
const streamBuffer = 4

// newScannerFn opens the platform backend. A package seam, not a const, so the
// engine below can be tested with no radio present — mirroring bleScanFn in
// shared/discovery/bluetooth_darwin.go. Production never reassigns it.
var newScannerFn = newScanner

// scanner is what each platform file implements. Backends report whatever they
// can currently see; accumulating across samples is the engine's job, so a
// backend that forgets a device (BlueZ evicting a stale entry, a watcher
// restarting) cannot shrink the emitted array.
type scanner interface {
	// Snapshot returns the devices visible now. A non-nil error ends the scan
	// after this call's devices, if any, are merged and emitted — a backend
	// may report something new in the same call that it reports a fatal error.
	// ctx is the scan's own lifetime context, so a backend whose read can block
	// (a D-Bus round trip, say) can return promptly once the caller cancels
	// instead of wedging the sampling loop.
	Snapshot(ctx context.Context) ([]BLEDeviceInfo, error)
	// Close stops scanning and releases the backend. Safe to call once.
	Close()
}

// DiscoverBluetoothContinuous streams the full set of matching devices seen so
// far, re-emitted whenever it changes, until ctx is cancelled. The channel is
// closed when the session ends.
//
// Each emitted slice is complete and sorted by RSSI descending (strongest
// first) — a consumer replaces its whole list rather than applying a delta.
// Devices accumulate for the life of the stream and are never removed: BLE
// offers no "device went away" signal, and a disappearance timeout would make
// a device blink out of a picker between advertisements.
//
// The returned error reports only that the scan could not be started at all
// (Preflight said no, or the platform has no BLE support). A backend that fails
// mid-stream ends the stream by closing the channel.
func DiscoverBluetoothContinuous(ctx context.Context, opts Options) (<-chan []BLEDeviceInfo, error) {
	if opts.Preflight != nil {
		if err := opts.Preflight(ctx); err != nil {
			return nil, err
		}
	}

	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	want := canonicalUUIDs(opts.Services)

	sc, err := newScannerFn(ctx, want)
	if err != nil {
		return nil, err
	}

	out := make(chan []BLEDeviceInfo, streamBuffer)
	go func() {
		defer close(out)
		defer sc.Close()
		runScan(ctx, sc, want, interval, out)
	}()
	return out, nil
}

// runScan samples the backend on every tick and emits the accumulated set
// whenever it differs from what was last sent. Coalescing on the tick is what
// keeps RSSI churn — which changes on nearly every advertising packet — from
// spinning the channel.
func runScan(ctx context.Context, sc scanner, want []string, interval time.Duration, out chan<- []BLEDeviceInfo) {
	store := newDeviceStore()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Sample once up front so a caller sees whatever the backend already knows
	// (CoreBluetooth and BlueZ both hold a cache) without waiting a full tick.
	if !sample(ctx, sc, want, store, out) {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !sample(ctx, sc, want, store, out) {
				return
			}
		}
	}
}

// sample takes one backend reading and emits if anything is pending. It reports
// whether the scan should continue.
func sample(ctx context.Context, sc scanner, want []string, store *deviceStore, out chan<- []BLEDeviceInfo) bool {
	devices, err := sc.Snapshot(ctx)

	// Merge and try to emit whatever the backend returned even when err is
	// non-nil: a backend that just died may still report something new in this
	// same call (windowsScanner's accumulated map, say), and dropping it on the
	// floor would lose it for good — the stream is ending either way, so this
	// is the last chance to flush it.
	for _, d := range devices {
		if d.Address == "" {
			continue
		}
		d.ServiceUUIDs = canonicalUUIDs(d.ServiceUUIDs)
		if !matchesServices(d.ServiceUUIDs, want) {
			continue
		}
		store.merge(d)
	}
	if store.pending {
		// Non-blocking send: a consumer that is behind simply misses this emit
		// and gets a superset on the next tick, with pending still set so the
		// retry happens even if no new advertisement arrives in the meantime.
		// Blocking here instead would stall sampling behind rendering.
		snapshot := store.snapshot()
		select {
		case <-ctx.Done():
			return false
		case out <- snapshot:
			store.pending = false
		default:
		}
	}

	return err == nil
}

// deviceStore accumulates sightings across samples, keyed by address.
type deviceStore struct {
	byAddress map[string]BLEDeviceInfo
	// pending is set when the store changed since the last delivered emit, so a
	// dropped emit is retried on the following tick.
	pending bool
}

func newDeviceStore() *deviceStore {
	return &deviceStore{byAddress: make(map[string]BLEDeviceInfo)}
}

// merge folds one sighting in and reports whether it changed the store.
// Later readings win: under an accumulate-forever policy, keeping the strongest
// RSSI ever seen (as the one-shot macOS scan does) would pin a device to its
// best-ever reading for the whole session, so a stale value could never
// recover.
func (s *deviceStore) merge(d BLEDeviceInfo) bool {
	prev, existed := s.byAddress[d.Address]
	if existed && !deviceChanged(prev, d) {
		return false
	}
	// Never let a sighting that omits a field erase what an earlier one
	// established: macOS reports a name only once the local-name AD field or
	// the OS cache supplies it, and BlueZ populates UUIDs asynchronously after
	// a device first appears.
	if d.Name == "" {
		d.Name = prev.Name
	}
	if len(d.ServiceUUIDs) == 0 {
		d.ServiceUUIDs = prev.ServiceUUIDs
	}
	if d.RSSI == 0 {
		d.RSSI = prev.RSSI
	}
	s.byAddress[d.Address] = d
	s.pending = true
	return true
}

// snapshot returns the accumulated set, sorted strongest signal first.
func (s *deviceStore) snapshot() []BLEDeviceInfo {
	out := make([]BLEDeviceInfo, 0, len(s.byAddress))
	for _, d := range s.byAddress {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RSSI != out[j].RSSI {
			return out[i].RSSI > out[j].RSSI
		}
		// Stable tiebreak so an unchanged set never reorders between emits —
		// map iteration order alone would shuffle equal-RSSI devices.
		return out[i].Address < out[j].Address
	})
	return out
}

// deviceChanged reports whether an incoming sighting carries anything new,
// treating an empty field as "no news" to match merge's field-preserving
// behavior.
func deviceChanged(prev, next BLEDeviceInfo) bool {
	if next.Name != "" && next.Name != prev.Name {
		return true
	}
	if next.RSSI != 0 && next.RSSI != prev.RSSI {
		return true
	}
	if len(next.ServiceUUIDs) != 0 && !sameUUIDs(prev.ServiceUUIDs, next.ServiceUUIDs) {
		return true
	}
	return false
}

func sameUUIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
