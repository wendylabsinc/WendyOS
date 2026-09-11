package discovery

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/ble"
	"github.com/wendylabsinc/wendy/go/internal/shared/ble/permission"
	"github.com/wendylabsinc/wendy/go/internal/shared/ble/scan"
)

// BLELiteDevice is one Wendy Lite board seen over BLE, carrying the content of
// its GATT info service.
type BLELiteDevice struct {
	// Address is what central.Connect and WendyLiteClient.ConnectViaBLE take on
	// this platform: a CoreBluetooth peripheral UUID on macOS, a MAC elsewhere.
	Address string

	// Name is the advertised local name, "" when none was seen. A display
	// label, not an identity — Info carries what the device says about itself.
	Name string

	// RSSI is signal strength in dBm from the most recent sighting; 0 when the
	// platform does not report it.
	RSSI int

	// Info was read over GATT, never synthesized from the advertisement: a
	// device only appears in the stream once this read succeeded.
	Info ble.LiteInfo
}

const (
	// bleLiteProbeTimeout bounds each step of one probe — GAP connect, service
	// discovery, a characteristic read — matching liteclient's bleDialTimeout.
	bleLiteProbeTimeout = 10 * time.Second

	// bleLiteProbeAttempts caps how many times one address is probed before it
	// is abandoned for the life of the stream. A cap rather than endless retry
	// because each attempt reaches for the board's single BLE link, and because
	// on Windows every probe fails by design (no GATT client).
	bleLiteProbeAttempts = 3

	// bleLiteStreamBuffer decouples the loop from a consumer that renders
	// between reads. Emits are rare — one per newly identified board — so a
	// small buffer is plenty.
	bleLiteStreamBuffer = 4
)

// bleLiteProbeRetryDelay is how long a failed address waits before its next
// attempt, and the cadence at which the loop wakes to notice one is due. A var
// so tests can shrink it.
var bleLiteProbeRetryDelay = 15 * time.Second

// Package seams so the engine below can be tested with no radio present. Tests
// swap them; production never reassigns them.
var (
	bleLiteScanFn  = scan.DiscoverBluetoothContinuous
	bleLiteProbeFn = ble.ReadLiteInfoAt
	blePreflightFn = permission.Preflight
)

// BLELiteDeviceDiscoverContinuous streams the Wendy Lite boards found over BLE,
// each with the content of its GATT info service, until ctx is cancelled. The
// channel is closed when the stream ends.
//
// Each emitted slice is complete and sorted by RSSI descending — a consumer
// replaces its whole list rather than applying a delta. Devices accumulate for
// the life of the stream and are never removed, inheriting the scanner's
// policy: BLE offers no "device went away" signal.
//
// A board appears only once its info service was read completely — every
// characteristic, not just PSM (see ble.ReadLiteInfo) — which takes a GATT
// connection: Windows has no GATT client at all (see
// ble.ErrLiteInfoUnavailable), so the stream stays empty there. A new emit
// happens when a board is identified, not when its signal moves — RSSI changes
// on nearly every advertising packet, and the value carried is simply the
// freshest one at emit time.
//
// The returned error reports only that the scan could not be started (no BLE
// support on this platform). A scan that fails mid-stream closes the channel.
func BLELiteDeviceDiscoverContinuous(ctx context.Context) (<-chan []BLELiteDevice, error) {
	// Preflight runs permission.Preflight before the scan first touches
	// CoreBluetooth in this process. Ordinary permission denial is fine to let
	// through un-preflighted — macOS reports no peripherals rather than an
	// error, which is the right degradation for a background discovery source
	// — but a sandboxed terminal without Bluetooth TCC access can SIGABRT the
	// whole process on that first touch, which no downstream error handling
	// can recover from.
	sightings, err := bleLiteScanFn(ctx, scan.Options{
		Services:  []string{ble.LiteInfoServiceUUID},
		Preflight: blePreflightFn,
	})
	if err != nil {
		return nil, err
	}

	out := make(chan []BLELiteDevice, bleLiteStreamBuffer)
	go func() {
		defer close(out)
		runBLELiteDiscovery(ctx, sightings, out)
	}()
	return out, nil
}

// bleLiteEntry is what the loop knows about one address. Entries are never
// removed, mirroring the scanner's accumulate-forever policy.
type bleLiteEntry struct {
	name string
	rssi int
	// info is nil until a probe succeeds; a device with no info is not emitted.
	info     *ble.LiteInfo
	attempts int
	retryAt  time.Time
}

type bleLiteProbeResult struct {
	address string
	info    *ble.LiteInfo
	err     error
}

// runBLELiteDiscovery owns all discovery state on a single goroutine — no
// mutex — folding in sightings from the scanner and the results of the GATT
// probes it dispatches.
func runBLELiteDiscovery(ctx context.Context, sightings <-chan []scan.BLEDeviceInfo, out chan<- []BLELiteDevice) {
	entries := make(map[string]*bleLiteEntry)
	// Capacity 1 with at most one probe in flight, so a probe goroutine can
	// always deliver its result and exit even after this loop has returned.
	results := make(chan bleLiteProbeResult, 1)
	// inFlight is the address currently being probed, "" when idle. Probes run
	// one at a time: there is one radio, central.Connection is not
	// goroutine-safe, and a probe occupies the target board's only BLE link.
	inFlight := ""

	// The scanner re-emits only when it sees something new, so an already-known
	// board would never prompt a retry on its own; this tick is what wakes the
	// loop to notice a retry has come due.
	retry := time.NewTicker(bleLiteProbeRetryDelay)
	defer retry.Stop()

	for {
		if inFlight == "" {
			if address := nextBLELiteProbe(entries); address != "" {
				inFlight = address
				startBLELiteProbe(address, results)
			}
		}

		select {
		case <-ctx.Done():
			return

		case devices, ok := <-sightings:
			if !ok {
				return
			}
			for _, d := range devices {
				entry := entries[d.Address]
				if entry == nil {
					entry = &bleLiteEntry{}
					entries[d.Address] = entry
				}
				// The scanner already preserves fields across sightings, so an
				// empty value here means the field was never seen at all.
				if d.Name != "" {
					entry.name = d.Name
				}
				if d.RSSI != 0 {
					entry.rssi = d.RSSI
				}
			}

		case res := <-results:
			inFlight = ""
			entry := entries[res.address]
			if entry == nil {
				continue
			}
			if res.err != nil {
				entry.attempts++
				entry.retryAt = time.Now().Add(bleLiteProbeRetryDelay)
				continue
			}
			entry.info = res.info
			if !emitBLELiteDevices(ctx, entries, out) {
				return
			}

		case <-retry.C:
			// Nothing to do: the dispatch at the top of the loop is what picks
			// up any address whose retry has come due.
		}
	}
}

// nextBLELiteProbe picks the address to probe next: unidentified, still within
// its attempt budget, and past its backoff. Strongest signal first, so the
// board most likely to answer is tried first.
func nextBLELiteProbe(entries map[string]*bleLiteEntry) string {
	now := time.Now()
	best := ""
	bestRSSI := math.MinInt
	for address, entry := range entries {
		if entry.info != nil || entry.attempts >= bleLiteProbeAttempts || now.Before(entry.retryAt) {
			continue
		}
		// RSSI 0 means "not reported" (see BLELiteDevice.RSSI), not an actual
		// reading, so it must never outrank a real (negative) signal.
		rssi := entry.rssi
		if rssi == 0 {
			rssi = math.MinInt
		}
		if best == "" || rssi > bestRSSI || (rssi == bestRSSI && address < best) {
			best, bestRSSI = address, rssi
		}
	}
	return best
}

// startBLELiteProbe reads the info service of one device in the background.
func startBLELiteProbe(address string, results chan<- bleLiteProbeResult) {
	// Read the seam here rather than inside the goroutine: a test that swaps it
	// does so before the call that leads here, and reading it on the new
	// goroutine would race that write.
	probe := bleLiteProbeFn
	go func() {
		info, err := probe(address, bleLiteProbeTimeout)
		results <- bleLiteProbeResult{address: address, info: info, err: err}
	}()
}

// emitBLELiteDevices sends the identified devices, strongest signal first. It
// reports whether the stream should continue.
func emitBLELiteDevices(ctx context.Context, entries map[string]*bleLiteEntry, out chan<- []BLELiteDevice) bool {
	devices := make([]BLELiteDevice, 0, len(entries))
	for address, entry := range entries {
		if entry.info == nil {
			continue
		}
		devices = append(devices, BLELiteDevice{
			Address: address,
			Name:    entry.name,
			RSSI:    entry.rssi,
			Info:    *entry.info,
		})
	}
	sort.Slice(devices, func(i, j int) bool {
		if devices[i].RSSI != devices[j].RSSI {
			return devices[i].RSSI > devices[j].RSSI
		}
		// Stable tiebreak so an unchanged set never reorders between emits.
		return devices[i].Address < devices[j].Address
	})

	select {
	case <-ctx.Done():
		return false
	case out <- devices:
		return true
	}
}
