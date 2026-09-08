package discovery

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/discoverycache"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// testFilter is a LANFilter whose verdict a test can change mid-session.
type testFilter struct {
	mu      sync.Mutex
	exclude func(models.LANDevice) bool
	changed chan struct{}
}

func (f *testFilter) Exclude(dev models.LANDevice) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exclude(dev)
}

func (f *testFilter) Changed() <-chan struct{} { return f.changed }

// set swaps the verdict and, when the filter was built with a change channel,
// tells the session to re-check what it already listed.
func (f *testFilter) set(fn func(models.LANDevice) bool) {
	f.mu.Lock()
	f.exclude = fn
	f.mu.Unlock()
	if f.changed != nil {
		f.changed <- struct{}{}
	}
}

func excludeVMBoards(dev models.LANDevice) bool { return strings.HasPrefix(dev.DeviceType, "vm-") }

func cacheHas(t *testing.T, path, id string) bool {
	t.Helper()
	c, err := discoverycache.LoadFrom(path)
	if err != nil {
		t.Fatalf("loading cache: %v", err)
	}
	for _, e := range c.Entries() {
		if e.ID == id {
			return true
		}
	}
	return false
}

// A sighting's advertised device type is worth keeping until a probe says
// better; a probe-verified type is never downgraded by a later announcement.
func TestApplySightingKeepsTXTDeviceTypeWhenNothingBetterIsKnown(t *testing.T) {
	stored := models.LANDevice{ID: "dev-1", DisplayName: "sim"}
	sighted := models.LANDevice{ID: "dev-1", DisplayName: "sim", DeviceType: "vm-arm64"}
	if got := applySighting(stored, sighted).DeviceType; got != "vm-arm64" {
		t.Fatalf("DeviceType = %q, want the sighting's vm-arm64 when the stored row has none", got)
	}
	stored.DeviceType = "jetson-orin-nano"
	if got := applySighting(stored, sighted).DeviceType; got != "jetson-orin-nano" {
		t.Fatalf("DeviceType = %q, want the stored type kept over the advertisement", got)
	}
	if !mdnsFieldsChanged(models.LANDevice{ID: "dev-1"}, models.LANDevice{ID: "dev-1", DeviceType: "vm-arm64"}) {
		t.Fatal("a newly advertised device type must count as a change, or consumers never see it")
	}
}

// A row the filter rejects at load is a stale leftover (a VM cached before the
// filter existed): it must neither be shown nor probed, and the next save must
// forget it rather than let it re-seed every later session for the TTL.
func TestStreamExcludedCachedEntryIsDroppedAndForgotten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	seedCache(t, path,
		discoverycache.Entry{ID: "vm-1", DisplayName: "Gentle Forest", Hostname: "wendyos-gentle-forest.local", IP: "10.0.2.15", Port: 50051},
		discoverycache.Entry{ID: "dev-1", DisplayName: "orin", Hostname: "orin.local", IP: "10.0.0.5", Port: 50051})

	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))

	prober := func(_ context.Context, dev models.LANDevice) (models.LANDevice, error) {
		if dev.ID == "vm-1" {
			t.Errorf("excluded cached row was probed: %+v", dev)
		}
		dev.AgentVersion = "1"
		return dev, nil
	}
	filter := &testFilter{exclude: func(dev models.LANDevice) bool { return dev.Hostname == "wendyos-gentle-forest.local" }}

	events, stop := startStream(t, StreamOptions{UseCache: true, Prober: prober, Exclude: filter})
	for _, ev := range collectEvents(t, events, 2, 5*time.Second) { // orin: cached, then confirmed
		if ev.Device.ID != "dev-1" {
			t.Fatalf("excluded cached row reached the consumer: %+v", ev)
		}
	}
	expectQuiet(t, events, 150*time.Millisecond)
	stop()

	if cacheHas(t, path, "vm-1") {
		t.Fatal("excluded cached row survived the session's save")
	}
	if !cacheHas(t, path, "dev-1") {
		t.Fatal("the real device's entry was lost")
	}
}

// A sighting the filter rejects outright -- here by the devicetype TXT record
// -- never becomes a row: no event, no probe, no cache entry.
func TestStreamExcludedSightingIsNeitherEmittedNorProbedNorCached(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))

	var probed atomic.Int32
	prober := func(_ context.Context, dev models.LANDevice) (models.LANDevice, error) {
		probed.Add(1)
		return dev, nil
	}
	filter := &testFilter{exclude: excludeVMBoards}

	events, stop := startStream(t, StreamOptions{UseCache: true, Prober: prober, Exclude: filter})
	leak := wendyService("vm-1", "Gentle Forest", "wendyos-gentle-forest.local", "10.0.2.15", 50051)
	leak.TXTRecords["devicetype"] = "vm-arm64"
	fb.emit(t, leak)
	fb.emit(t, wendyService("dev-1", "orin", "orin.local", "10.0.0.5", 50051))

	for _, ev := range collectEvents(t, events, 2, 5*time.Second) { // orin: found, then probed
		if ev.Device.ID != "dev-1" {
			t.Fatalf("excluded sighting reached the consumer: %+v", ev)
		}
	}
	expectQuiet(t, events, 150*time.Millisecond)
	stop()

	if n := probed.Load(); n != 1 {
		t.Fatalf("probes = %d, want only the real device's", n)
	}
	if cacheHas(t, path, "vm-1") {
		t.Fatal("excluded sighting was persisted")
	}
}

// A sighting with no devicetype record (an older agent) is listed, then its
// probe reports a VM board: the row that was already shown must be taken back,
// and the cache entry the sighting wrote must go with it.
func TestStreamProbeRevealingASimulatorRetractsTheRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))

	prober := func(_ context.Context, dev models.LANDevice) (models.LANDevice, error) {
		dev.DeviceType = "vm-arm64"
		dev.AgentVersion = "1"
		return dev, nil
	}
	filter := &testFilter{exclude: excludeVMBoards}

	events, stop := startStream(t, StreamOptions{UseCache: true, Prober: prober, Exclude: filter})
	fb.emit(t, wendyService("vm-1", "Gentle Forest", "wendyos-gentle-forest.local", "192.168.64.5", 50051))

	got := collectEvents(t, events, 2, 5*time.Second)
	if got[0].Kind != LANFound || got[0].Device.ID != "vm-1" {
		t.Fatalf("first event must list the sighting: %+v", got[0])
	}
	if got[1].Kind != LANRetracted || got[1].Device.ID != "vm-1" {
		t.Fatalf("second event must retract it once the probe reveals a VM: %+v", got[1])
	}
	expectQuiet(t, events, 150*time.Millisecond)
	stop()

	if cacheHas(t, path, "vm-1") {
		t.Fatal("retracted row is still cached")
	}
}

// The filter can learn things after a row was listed (the CLI finds out a VM's
// hostname a moment later). When it says so, the session re-checks what it
// has and retracts the rows that now match.
func TestStreamFilterChangeRetractsAnAlreadyListedRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))

	prober := func(_ context.Context, dev models.LANDevice) (models.LANDevice, error) {
		dev.AgentVersion = "1"
		return dev, nil
	}
	filter := &testFilter{exclude: func(models.LANDevice) bool { return false }, changed: make(chan struct{}, 1)}

	events, stop := startStream(t, StreamOptions{UseCache: true, Prober: prober, Exclude: filter})
	fb.emit(t, wendyService("vm-1", "Gentle Forest", "wendyos-gentle-forest.local", "10.0.2.15", 50051))
	fb.emit(t, wendyService("dev-1", "orin", "orin.local", "10.0.0.5", 50051))
	collectEvents(t, events, 4, 5*time.Second) // both found, both probed

	filter.set(func(dev models.LANDevice) bool { return dev.Hostname == "wendyos-gentle-forest.local" })

	got := collectEvents(t, events, 1, 5*time.Second)
	if got[0].Kind != LANRetracted || got[0].Device.ID != "vm-1" {
		t.Fatalf("expected the VM's row retracted after the filter changed, got %+v", got[0])
	}
	expectQuiet(t, events, 150*time.Millisecond)
	stop()

	if cacheHas(t, path, "vm-1") {
		t.Fatal("retracted row is still cached")
	}
	if !cacheHas(t, path, "dev-1") {
		t.Fatal("the real device's entry was lost")
	}
}

// The batch collector is what JSON discover and MCP's device_list read; a row
// retracted mid-scan must not be in what it returns.
func TestCollectLANDropsRetractedRows(t *testing.T) {
	shrinkDuration(t, &collectSettle, 50*time.Millisecond)

	path := filepath.Join(t.TempDir(), "devices.json")
	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))

	prober := func(_ context.Context, dev models.LANDevice) (models.LANDevice, error) {
		if dev.ID == "vm-1" {
			dev.DeviceType = "vm-arm64"
		}
		dev.AgentVersion = "1"
		return dev, nil
	}
	go func() {
		fb.emit(t, wendyService("vm-1", "Gentle Forest", "wendyos-gentle-forest.local", "10.0.2.15", 50051))
		fb.emit(t, wendyService("dev-1", "orin", "orin.local", "10.0.0.5", 50051))
	}()

	got, err := CollectLAN(context.Background(), StreamOptions{UseCache: true, Prober: prober, Exclude: &testFilter{exclude: excludeVMBoards}}, 5*time.Second)
	if err != nil {
		t.Fatalf("CollectLAN error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "dev-1" {
		t.Fatalf("CollectLAN = %+v, want only the real device", got)
	}
}

func TestCollectLANRetractionOfOnlyResultCancelsSettle(t *testing.T) {
	backend := func(ctx context.Context, _ string, emit func(MDNSService)) error {
		emit(wendyService("vm", "Guest", "guest.local", "10.0.2.15", 50051))
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return nil
		}
		classified := wendyService("vm", "Guest", "guest.local", "10.0.2.15", 50051)
		classified.TXTRecords["devicetype"] = "vm-arm64"
		emit(classified)
		// Beyond the first sighting's settle window, within the scan budget.
		select {
		case <-time.After(700 * time.Millisecond):
		case <-ctx.Done():
			return nil
		}
		emit(wendyService("physical", "Pi", "pi.local", "192.168.1.20", 50051))
		<-ctx.Done()
		return nil
	}
	useStreamSeams(t, backend, nil)
	got, err := CollectLAN(context.Background(), StreamOptions{Exclude: &testFilter{exclude: excludeVMBoards}}, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "physical" {
		t.Fatalf("scan lost the later physical device: %+v", got)
	}
}

func TestExcludedSightingRetractsHostnameAliasAndCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	seedCache(t, path,
		discoverycache.Entry{ID: "guest", DisplayName: "guest", Hostname: "guest.local", IP: "10.0.2.15"},
		discoverycache.Entry{ID: "physical", DisplayName: "Pi", Hostname: "pi.local", IP: "192.168.1.20"})
	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))
	events, stop := startStream(t, StreamOptions{UseCache: true, Exclude: &testFilter{exclude: excludeVMBoards}})
	collectEvents(t, events, 2, time.Second)
	leak := wendyService("real-id", "Guest", "GUEST.local.", "10.0.2.15", 50051)
	leak.TXTRecords["devicetype"] = "vm-arm64"
	fb.emit(t, leak)
	got := collectEvents(t, events, 1, time.Second)
	if got[0].Kind != LANRetracted || got[0].Device.ID != "guest" {
		t.Fatalf("alias not retracted: %+v", got)
	}
	expectQuiet(t, events, 100*time.Millisecond)
	stop()
	if cacheHas(t, path, "guest") || cacheHas(t, path, "real-id") {
		t.Fatal("excluded VM remains cached")
	}
	if !cacheHas(t, path, "physical") {
		t.Fatal("unrelated physical device was removed")
	}
}
