package discovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/discoverycache"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// fakeBackend is a scripted LAN stream backend: the test pushes services onto
// ch and the backend hands them to the engine, staying alive until ctx ends.
type fakeBackend struct{ ch chan MDNSService }

func newFakeBackend() *fakeBackend { return &fakeBackend{ch: make(chan MDNSService)} }

func (f *fakeBackend) fn(ctx context.Context, serviceType string, emit func(MDNSService)) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case svc := <-f.ch:
			emit(svc)
		}
	}
}

// emit pushes svc to the backend, failing the test if the engine never picks
// it up (a hung session would otherwise deadlock the test).
func (f *fakeBackend) emit(t *testing.T, svc MDNSService) {
	t.Helper()
	select {
	case f.ch <- svc:
	case <-time.After(2 * time.Second):
		t.Fatalf("backend emit blocked: %+v", svc)
	}
}

// useStreamSeams points the engine at a fake backend and cache loader for the
// duration of the test. Call it before starting a session so the restore
// runs after the session has been torn down (t.Cleanup is LIFO).
func useStreamSeams(t *testing.T, backend func(context.Context, string, func(MDNSService)) error, load func() (*discoverycache.Cache, error)) {
	t.Helper()
	origBackend, origLoad := lanBackendFn, cacheLoadFn
	t.Cleanup(func() { lanBackendFn, cacheLoadFn = origBackend, origLoad })
	if backend != nil {
		lanBackendFn = backend
	}
	if load != nil {
		cacheLoadFn = load
	}
}

// shrinkDuration retunes one of the engine's timing knobs for this test.
func shrinkDuration(t *testing.T, knob *time.Duration, d time.Duration) {
	t.Helper()
	orig := *knob
	t.Cleanup(func() { *knob = orig })
	*knob = d
}

// cacheLoaderFor returns a cacheLoadFn that reads the cache file at path.
func cacheLoaderFor(path string) func() (*discoverycache.Cache, error) {
	return func() (*discoverycache.Cache, error) { return discoverycache.LoadFrom(path) }
}

// seedCache writes entries to a fresh cache file at path, stamped now.
func seedCache(t *testing.T, path string, entries ...discoverycache.Entry) {
	t.Helper()
	c, err := discoverycache.LoadFrom(path)
	if err != nil {
		t.Fatalf("seed load: %v", err)
	}
	now := time.Now()
	for _, e := range entries {
		c.Upsert(e, now)
	}
	if err := c.Flush(now); err != nil {
		t.Fatalf("seed flush: %v", err)
	}
}

// startStream runs a session and returns its event channel plus a stop func
// that cancels it and drains the channel until the engine has fully shut
// down. stop also runs on cleanup, so the timing/seam overrides above are
// never restored while a session is still reading them.
func startStream(t *testing.T, opts StreamOptions) (<-chan LANEvent, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	events := StreamLAN(ctx, opts)
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			for range events {
			}
		})
	}
	t.Cleanup(stop)
	return events, stop
}

// collectEvents reads exactly n events or fails.
func collectEvents(t *testing.T, ch <-chan LANEvent, n int, timeout time.Duration) []LANEvent {
	t.Helper()
	events := make([]LANEvent, 0, n)
	deadline := time.After(timeout)
	for len(events) < n {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed after %d of %d events: %+v", len(events), n, events)
			}
			events = append(events, ev)
		case <-deadline:
			t.Fatalf("timed out after %d of %d events: %+v", len(events), n, events)
		}
	}
	return events
}

// expectQuiet fails if any further event arrives within d.
func expectQuiet(t *testing.T, ch <-chan LANEvent, d time.Duration) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if ok {
			t.Fatalf("unexpected event: %+v", ev)
		}
		t.Fatal("stream closed unexpectedly")
	case <-time.After(d):
	}
}

// wendyService builds a resolved WendyOS mDNS service entry.
func wendyService(id, display, hostname, ip string, port int) MDNSService {
	return MDNSService{
		InstanceName: display,
		Hostname:     hostname,
		IPAddress:    ip,
		Port:         port,
		TXTRecords: map[string]string{
			"wendyosdevice": id,
			"displayname":   display,
		},
	}
}

func failingProber(err error) LANProber {
	return func(context.Context, models.LANDevice) (models.LANDevice, error) {
		return models.LANDevice{}, err
	}
}

func TestStreamCachedThenProbeConfirms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	seedCache(t, path, discoverycache.Entry{ID: "dev-1", DisplayName: "orin", Hostname: "orin.local", IP: "10.0.0.5", Port: 50051})

	fb := newFakeBackend() // stays silent
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))

	prober := func(_ context.Context, dev models.LANDevice) (models.LANDevice, error) {
		dev.AgentVersion = "9.9.9"
		dev.IsMTLS = true
		return dev, nil
	}

	events, _ := startStream(t, StreamOptions{UseCache: true, Prober: prober})

	got := collectEvents(t, events, 2, 5*time.Second)
	first, second := got[0], got[1]
	if first.Kind != LANCached || first.Device.ID != "dev-1" || first.Probed {
		t.Fatalf("first event must be the cached row: %+v", first)
	}
	if second.Kind != LANFound || !second.Probed || second.Device.AgentVersion != "9.9.9" || !second.Device.IsMTLS {
		t.Fatalf("second event must be the probe confirmation: %+v", second)
	}
	if second.Device.ID != "dev-1" || second.Device.IPAddress != "10.0.0.5" {
		t.Fatalf("probe confirmation must keep the cached identity/address: %+v", second.Device)
	}
}

func TestStreamCachedProbeFailsThenOffline(t *testing.T) {
	grace := 150 * time.Millisecond
	shrinkDuration(t, &offlineGrace, grace)

	path := filepath.Join(t.TempDir(), "devices.json")
	seedCache(t, path, discoverycache.Entry{ID: "dev-1", DisplayName: "orin", Hostname: "orin.local", IP: "10.0.0.5", Port: 50051})

	fb := newFakeBackend() // stays silent: nothing on the network
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))

	start := time.Now()
	events, _ := startStream(t, StreamOptions{UseCache: true, Prober: failingProber(errors.New("dial tcp: connection refused"))})

	got := collectEvents(t, events, 2, 5*time.Second)
	if got[0].Kind != LANCached || got[0].Device.ID != "dev-1" {
		t.Fatalf("first event must be the cached row: %+v", got[0])
	}
	if got[1].Kind != LANOffline || got[1].Device.ID != "dev-1" || got[1].Probed {
		t.Fatalf("second event must mark the same device offline: %+v", got[1])
	}
	// The probe fails immediately; offline still waits out the grace window
	// (the session timer starts after `start`, so this cannot false-fail).
	if elapsed := time.Since(start); elapsed < grace {
		t.Fatalf("offline marker fired %v after start, before the %v grace window", elapsed, grace)
	}
	// A failed probe never produces a confirmation, and the offline marker is
	// emitted once — the next re-probe is offlineRetryDelay away.
	expectQuiet(t, events, 200*time.Millisecond)
}

func TestStreamCachedPendingProbeStaysOnlinePastGrace(t *testing.T) {
	shrinkDuration(t, &offlineGrace, 50*time.Millisecond)

	path := filepath.Join(t.TempDir(), "devices.json")
	seedCache(t, path, discoverycache.Entry{ID: "dev-1", DisplayName: "orin", Hostname: "orin.local", IP: "10.0.0.5", Port: 50051})

	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))

	// A slow agent: the probe is still in flight when the grace window ends.
	slowProber := func(ctx context.Context, dev models.LANDevice) (models.LANDevice, error) {
		<-ctx.Done()
		return models.LANDevice{}, ctx.Err()
	}

	events, _ := startStream(t, StreamOptions{UseCache: true, Prober: slowProber})

	if got := collectEvents(t, events, 1, 5*time.Second)[0]; got.Kind != LANCached {
		t.Fatalf("first event must be the cached row: %+v", got)
	}
	// Offline needs a *failed* probe, not just an elapsed grace window.
	expectQuiet(t, events, 250*time.Millisecond)
}

func TestStreamOfflineDeviceReturnsViaMDNS(t *testing.T) {
	shrinkDuration(t, &offlineGrace, 50*time.Millisecond)

	path := filepath.Join(t.TempDir(), "devices.json")
	seedCache(t, path, discoverycache.Entry{ID: "dev-1", DisplayName: "orin", Hostname: "orin.local", IP: "10.0.0.5", Port: 50051})

	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))

	events, _ := startStream(t, StreamOptions{UseCache: true, Prober: failingProber(errors.New("no route to host"))})

	got := collectEvents(t, events, 2, 5*time.Second)
	if got[0].Kind != LANCached || got[1].Kind != LANOffline {
		t.Fatalf("expected cached then offline, got %+v", got)
	}

	// The device shows up on the network at a new address.
	fb.emit(t, wendyService("dev-1", "orin", "orin.local", "10.0.0.9", 50051))

	back := collectEvents(t, events, 1, 5*time.Second)[0]
	if back.Kind != LANFound || back.Device.ID != "dev-1" || back.Device.IPAddress != "10.0.0.9" {
		t.Fatalf("mDNS sighting must flip the device back online: %+v", back)
	}
	if back.Probed {
		t.Fatalf("mDNS confirmation is not a probe confirmation: %+v", back)
	}
}

func TestStreamProbeRetargetsOnAddressChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	seedCache(t, path, discoverycache.Entry{ID: "dev-1", DisplayName: "orin", Hostname: "orin.local", IP: "10.0.0.5", Port: 50051})

	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))

	// The probe against the stale address is held open until the test
	// releases it, long after the device has moved.
	release := make(chan struct{})
	prober := func(ctx context.Context, dev models.LANDevice) (models.LANDevice, error) {
		if dev.IPAddress == "10.0.0.5" {
			select {
			case <-release:
			case <-ctx.Done():
				return models.LANDevice{}, ctx.Err()
			}
			dev.AgentVersion = "stale"
			return dev, nil
		}
		dev.AgentVersion = "current"
		return dev, nil
	}

	events, _ := startStream(t, StreamOptions{UseCache: true, Prober: prober})
	if got := collectEvents(t, events, 1, 5*time.Second)[0]; got.Kind != LANCached {
		t.Fatalf("first event must be the cached row: %+v", got)
	}

	fb.emit(t, wendyService("dev-1", "orin", "orin.local", "10.0.0.9", 50051))

	got := collectEvents(t, events, 2, 5*time.Second)
	if got[0].Kind != LANFound || got[0].Device.IPAddress != "10.0.0.9" {
		t.Fatalf("mDNS sighting at the new address must confirm the row: %+v", got[0])
	}
	if got[1].Kind != LANUpdated || !got[1].Probed || got[1].Device.AgentVersion != "current" {
		t.Fatalf("the retargeted probe must confirm the new address: %+v", got[1])
	}

	// The in-flight probe against the old address finally answers: its result
	// no longer speaks for this device and must be discarded.
	close(release)
	expectQuiet(t, events, 300*time.Millisecond)
}

func TestStreamOfflineReProbeRecovers(t *testing.T) {
	shrinkDuration(t, &offlineGrace, 50*time.Millisecond)
	shrinkDuration(t, &offlineRetryDelay, 30*time.Millisecond)

	path := filepath.Join(t.TempDir(), "devices.json")
	seedCache(t, path, discoverycache.Entry{ID: "dev-1", DisplayName: "orin", Hostname: "orin.local", IP: "10.0.0.5", Port: 50051})

	fb := newFakeBackend() // silent: recovery must come from the re-probe alone
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))

	// A device mid-boot: the first probe is refused, the retry gets through.
	var calls atomic.Int32
	prober := func(_ context.Context, dev models.LANDevice) (models.LANDevice, error) {
		if calls.Add(1) == 1 {
			return models.LANDevice{}, errors.New("connection refused")
		}
		dev.AgentVersion = "0.19.1"
		return dev, nil
	}

	events, _ := startStream(t, StreamOptions{UseCache: true, Prober: prober})

	got := collectEvents(t, events, 3, 5*time.Second)
	if got[0].Kind != LANCached || got[1].Kind != LANOffline {
		t.Fatalf("expected cached then offline, got %+v", got[:2])
	}
	if got[2].Kind != LANFound || !got[2].Probed || got[2].Device.AgentVersion != "0.19.1" {
		t.Fatalf("the post-offline re-probe must flip the row back online: %+v", got[2])
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("expected exactly one re-probe after offline, prober ran %d times", n)
	}
}

func TestStreamOfflineReProbeFailureDoesNotRepeatOffline(t *testing.T) {
	shrinkDuration(t, &offlineGrace, 50*time.Millisecond)
	shrinkDuration(t, &offlineRetryDelay, 30*time.Millisecond)

	path := filepath.Join(t.TempDir(), "devices.json")
	seedCache(t, path, discoverycache.Entry{ID: "dev-1", DisplayName: "orin", Hostname: "orin.local", IP: "10.0.0.5", Port: 50051})

	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))

	var calls atomic.Int32
	prober := func(context.Context, models.LANDevice) (models.LANDevice, error) {
		calls.Add(1)
		return models.LANDevice{}, errors.New("no route to host")
	}

	events, _ := startStream(t, StreamOptions{UseCache: true, Prober: prober})

	got := collectEvents(t, events, 2, 5*time.Second)
	if got[0].Kind != LANCached || got[1].Kind != LANOffline {
		t.Fatalf("expected cached then offline, got %+v", got)
	}

	// Wait for the single re-probe to run and fail.
	deadline := time.Now().Add(3 * time.Second)
	for calls.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the offline re-probe never ran (prober calls: %d)", calls.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	// An already-offline row stays offline silently, and there is only ever
	// one retry — no polling loop behind it.
	expectQuiet(t, events, 200*time.Millisecond)
	if n := calls.Load(); n != 2 {
		t.Fatalf("expected exactly one re-probe, prober ran %d times", n)
	}
}

func TestStreamClearedTXTRecordsUpdateAndPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")

	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))

	events, stop := startStream(t, StreamOptions{UseCache: true}) // browse-only

	advertised := wendyService("dev-11", "orin", "orin.local", "10.0.0.11", 50051)
	advertised.TXTRecords["tls"] = "true"
	advertised.TXTRecords["orgid"] = "7"
	advertised.TXTRecords["assetid"] = "3"
	advertised.TXTRecords["name"] = "brave-dolphin"
	fb.emit(t, advertised)

	first := collectEvents(t, events, 1, 5*time.Second)[0]
	if first.Kind != LANFound || !first.Device.IsMTLS || first.Device.OrgID != 7 || first.Device.MeshName != "brave-dolphin" {
		t.Fatalf("first sighting must carry the advertised TXT records: %+v", first.Device)
	}

	// The device re-announces without those records (unenrolled, mTLS off).
	fb.emit(t, wendyService("dev-11", "orin", "orin.local", "10.0.0.11", 50051))

	cleared := collectEvents(t, events, 1, 5*time.Second)[0]
	if cleared.Kind != LANUpdated {
		t.Fatalf("dropped TXT records must surface as an update: %+v", cleared)
	}
	if cleared.Device.IsMTLS || cleared.Device.OrgID != 0 || cleared.Device.AssetID != 0 || cleared.Device.MeshName != "" {
		t.Fatalf("a live sighting owns its TXT-derived fields: %+v", cleared.Device)
	}
	stop()

	reloaded, err := discoverycache.LoadFrom(path)
	if err != nil {
		t.Fatalf("reload cache: %v", err)
	}
	fresh := reloaded.Fresh(time.Now())
	if len(fresh) != 1 {
		t.Fatalf("expected exactly one persisted entry, got %+v", fresh)
	}
	if fresh[0].MTLS || fresh[0].OrgID != 0 || fresh[0].AssetID != 0 || fresh[0].MeshName != "" {
		t.Fatalf("stale TXT values must not be re-persisted: %+v", fresh[0])
	}
}

func TestStreamProbeOwnsMTLSAndSurvivesResighting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")

	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))

	// The TXT record advertises mTLS but the agent answers in plaintext.
	prober := func(_ context.Context, dev models.LANDevice) (models.LANDevice, error) {
		dev.AgentVersion = "3.3.3"
		dev.OS = "WendyOS"
		dev.IsMTLS = false
		return dev, nil
	}

	events, _ := startStream(t, StreamOptions{UseCache: true, Prober: prober})

	advertised := wendyService("dev-12", "orin", "orin.local", "10.0.0.12", 50051)
	advertised.TXTRecords["tls"] = "true"
	fb.emit(t, advertised)

	got := collectEvents(t, events, 2, 5*time.Second)
	if got[0].Kind != LANFound || !got[0].Device.IsMTLS {
		t.Fatalf("first sighting must reflect the tls TXT record: %+v", got[0].Device)
	}
	if got[1].Kind != LANUpdated || !got[1].Probed || got[1].Device.IsMTLS {
		t.Fatalf("the probe owns the mTLS mode it actually negotiated: %+v", got[1])
	}
	if got[1].Device.AgentVersion != "3.3.3" {
		t.Fatalf("probe must fill in the version fields: %+v", got[1].Device)
	}

	// A re-announcement at the same address restates tls=true (mDNS owns that
	// field) but must not blank what the probe learned.
	fb.emit(t, advertised)

	resighted := collectEvents(t, events, 1, 5*time.Second)[0]
	if resighted.Kind != LANUpdated || !resighted.Device.IsMTLS {
		t.Fatalf("re-announcement owns the TXT-derived mTLS flag again: %+v", resighted)
	}
	if resighted.Device.AgentVersion != "3.3.3" || resighted.Device.OS != "WendyOS" {
		t.Fatalf("a sighting must not blank probe-owned fields: %+v", resighted.Device)
	}
	if !resighted.Probed {
		t.Fatalf("the address never moved, so the row stays probe-confirmed: %+v", resighted)
	}
}

func TestStreamAddressMoveClearsProbedUntilReconfirmed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")

	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))

	probed := make(chan string, 8)
	prober := func(_ context.Context, dev models.LANDevice) (models.LANDevice, error) {
		probed <- dev.IPAddress
		dev.AgentVersion = "1.0.0"
		return dev, nil
	}

	events, _ := startStream(t, StreamOptions{UseCache: true, Prober: prober})
	fb.emit(t, wendyService("dev-13", "orin", "orin.local", "10.0.0.13", 50051))

	got := collectEvents(t, events, 2, 5*time.Second)
	if got[0].Kind != LANFound || got[0].Probed {
		t.Fatalf("first sighting is unprobed: %+v", got[0])
	}
	if got[1].Kind != LANUpdated || !got[1].Probed {
		t.Fatalf("probe must confirm the row: %+v", got[1])
	}
	if first := <-probed; first != "10.0.0.13" {
		t.Fatalf("probe went to the wrong address: %s", first)
	}

	// The device moves. Its new address has been probed by nobody.
	fb.emit(t, wendyService("dev-13", "orin", "orin.local", "10.0.0.99", 50051))

	moved := collectEvents(t, events, 1, 5*time.Second)[0]
	if moved.Kind != LANUpdated || moved.Device.IPAddress != "10.0.0.99" {
		t.Fatalf("address move must surface as an update: %+v", moved)
	}
	if moved.Probed {
		t.Fatalf("an unverified address must not be reported as probe-confirmed: %+v", moved)
	}

	reconfirmed := collectEvents(t, events, 1, 5*time.Second)[0]
	if reconfirmed.Kind != LANUpdated || !reconfirmed.Probed || reconfirmed.Device.IPAddress != "10.0.0.99" {
		t.Fatalf("the retargeted probe must re-confirm the row: %+v", reconfirmed)
	}
	if second := <-probed; second != "10.0.0.99" {
		t.Fatalf("retargeted probe went to the wrong address: %s", second)
	}
}

func TestStreamNewDeviceFoundThenProbedUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json") // no file: empty cache

	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))

	prober := func(_ context.Context, dev models.LANDevice) (models.LANDevice, error) {
		dev.AgentVersion = "1.2.3"
		dev.OS = "WendyOS"
		return dev, nil
	}

	events, _ := startStream(t, StreamOptions{UseCache: true, Prober: prober})
	fb.emit(t, wendyService("dev-2", "pi", "pi.local", "10.0.0.7", 50051))

	got := collectEvents(t, events, 2, 5*time.Second)
	if got[0].Kind != LANFound || got[0].Probed || got[0].Device.ID != "dev-2" || got[0].Device.AgentVersion != "" {
		t.Fatalf("first event must be the unprobed mDNS sighting: %+v", got[0])
	}
	if got[1].Kind != LANUpdated || !got[1].Probed || got[1].Device.AgentVersion != "1.2.3" || got[1].Device.OS != "WendyOS" {
		t.Fatalf("second event must be the probe update: %+v", got[1])
	}
}

func TestStreamDedupAndAddressChange(t *testing.T) {
	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, nil)

	events, _ := startStream(t, StreamOptions{}) // no cache, no prober

	svc := wendyService("dev-3", "nano", "nano.local", "10.0.0.3", 50051)
	fb.emit(t, svc)
	fb.emit(t, svc) // identical re-announcement: must not produce an event

	found := collectEvents(t, events, 1, 5*time.Second)[0]
	if found.Kind != LANFound || found.Device.IPAddress != "10.0.0.3" {
		t.Fatalf("first event must be the initial sighting: %+v", found)
	}

	moved := svc
	moved.IPAddress = "10.0.0.44"
	fb.emit(t, moved)

	// If the duplicate had been emitted it would arrive here instead.
	next := collectEvents(t, events, 1, 5*time.Second)[0]
	if next.Kind != LANUpdated || next.Device.IPAddress != "10.0.0.44" || next.Device.ID != "dev-3" {
		t.Fatalf("address change must produce a single update: %+v", next)
	}
	expectQuiet(t, events, 100*time.Millisecond)
}

// TestStreamUnionsAddressesAcrossInterfaces proves a device seen on two
// interfaces with different IPs keeps BOTH in Device.Addresses rather than
// collapsing to the single primary preferStableTarget picks — the set the dial
// ladder needs so a device reachable only over its USB link is still tried when
// its WiFi IP is unreachable.
func TestStreamUnionsAddressesAcrossInterfaces(t *testing.T) {
	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, nil)

	events, _ := startStream(t, StreamOptions{}) // no cache, no prober

	wifi := wendyService("dev-7", "orin", "orin.local", "192.168.1.69", 50051)
	wifi.InterfaceName = "en0"
	fb.emit(t, wifi)
	found := collectEvents(t, events, 1, 5*time.Second)[0]
	if found.Device.IPAddress != "192.168.1.69" {
		t.Fatalf("first sighting must land at its address: %+v", found.Device)
	}
	if got := found.Device.Addresses; len(got) != 1 || got[0] != "192.168.1.69" {
		t.Fatalf("first sighting Addresses = %v, want [192.168.1.69]", got)
	}

	// Same device, different interface (USB link), different routable IPv4.
	usb := wendyService("dev-7", "orin", "orin.local", "169.254.1.2", 50051)
	usb.InterfaceName = "en5"
	fb.emit(t, usb)
	next := collectEvents(t, events, 1, 5*time.Second)[0]
	if next.Kind != LANUpdated || next.Device.ID != "dev-7" {
		t.Fatalf("second interface sighting must update the device: %+v", next)
	}
	want := map[string]bool{"192.168.1.69": true, "169.254.1.2": true}
	got := next.Device.Addresses
	if len(got) != len(want) {
		t.Fatalf("both interface addresses must survive: Addresses = %v", got)
	}
	for _, a := range got {
		if !want[a] {
			t.Fatalf("unexpected address %q in %v", a, got)
		}
	}
}

func TestStreamNoCacheNoCachedEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	seedCache(t, path, discoverycache.Entry{ID: "dev-1", DisplayName: "orin", Hostname: "orin.local", IP: "10.0.0.5", Port: 50051})

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seeded cache: %v", err)
	}
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat seeded cache: %v", err)
	}

	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))

	events, stop := startStream(t, StreamOptions{UseCache: false})
	fb.emit(t, wendyService("dev-9", "other", "other.local", "10.0.0.9", 50051))

	got := collectEvents(t, events, 1, 5*time.Second)[0]
	if got.Kind != LANFound || got.Device.ID != "dev-9" {
		t.Fatalf("without UseCache the first event must be a live sighting: %+v", got)
	}
	stop()

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cache after session: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("cache file was rewritten without UseCache:\nbefore: %s\nafter:  %s", before, after)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat cache after session: %v", err)
	}
	if !afterInfo.ModTime().Equal(beforeInfo.ModTime()) {
		t.Fatalf("cache file mtime changed without UseCache: %v → %v", beforeInfo.ModTime(), afterInfo.ModTime())
	}
}

func TestStreamConfirmedOnlyUpsertsPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")

	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))

	events, stop := startStream(t, StreamOptions{UseCache: true}) // browse-only, no prober
	fb.emit(t, wendyService("dev-4", "jetson", "jetson.local", "10.0.0.4", 50051))

	got := collectEvents(t, events, 1, 5*time.Second)[0]
	if got.Kind != LANFound {
		t.Fatalf("expected a live sighting: %+v", got)
	}
	stop() // session end flushes the cache before the channel closes

	reloaded, err := discoverycache.LoadFrom(path)
	if err != nil {
		t.Fatalf("reload cache: %v", err)
	}
	fresh := reloaded.Fresh(time.Now())
	if len(fresh) != 1 {
		t.Fatalf("expected exactly one persisted entry, got %+v", fresh)
	}
	entry := fresh[0]
	if entry.ID != "dev-4" || entry.Hostname != "jetson.local" || entry.IP != "10.0.0.4" || entry.Port != 50051 {
		t.Fatalf("browse-only upsert did not persist the device: %+v", entry)
	}
	if time.Since(entry.LastSeen) > time.Minute {
		t.Fatalf("LastSeen must be stamped at sighting time: %v", entry.LastSeen)
	}
}

func TestStreamPeriodicFlushPersistsMidSession(t *testing.T) {
	shrinkDuration(t, &cacheFlushDelay, 20*time.Millisecond)

	path := filepath.Join(t.TempDir(), "devices.json")

	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))

	events, _ := startStream(t, StreamOptions{UseCache: true})
	fb.emit(t, wendyService("dev-10", "orin", "orin.local", "10.0.0.10", 50051))
	if got := collectEvents(t, events, 1, 5*time.Second)[0]; got.Kind != LANFound {
		t.Fatalf("expected a live sighting: %+v", got)
	}

	// A long-lived session (picker, discover TUI) must not hold its findings
	// until shutdown — the debounced flush ticker writes them out as it goes.
	deadline := time.Now().Add(3 * time.Second)
	for {
		cache, err := discoverycache.LoadFrom(path)
		if err != nil {
			t.Fatalf("reload cache: %v", err)
		}
		if fresh := cache.Fresh(time.Now()); len(fresh) == 1 && fresh[0].ID == "dev-10" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("device never reached the cache file while the session was still running")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStreamBackendRestarts(t *testing.T) {
	shrinkDuration(t, &backendRetryDelay, 10*time.Millisecond)

	var calls atomic.Int32
	backend := func(ctx context.Context, _ string, emit func(MDNSService)) error {
		switch calls.Add(1) {
		case 1:
			emit(wendyService("dev-5", "first", "first.local", "10.0.0.5", 50051))
			return errors.New("mDNSResponder went away")
		default:
			emit(wendyService("dev-6", "second", "second.local", "10.0.0.6", 50051))
			<-ctx.Done()
			return nil
		}
	}
	useStreamSeams(t, backend, nil)

	events, _ := startStream(t, StreamOptions{})

	got := collectEvents(t, events, 2, 5*time.Second)
	if got[0].Kind != LANFound || got[0].Device.ID != "dev-5" {
		t.Fatalf("first event must come from the first backend run: %+v", got[0])
	}
	if got[1].Kind != LANFound || got[1].Device.ID != "dev-6" {
		t.Fatalf("second event must come from the restarted backend: %+v", got[1])
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("backend must be invoked exactly twice, got %d", n)
	}
}

func TestStreamCacheLoadErrorTreatedAsEmpty(t *testing.T) {
	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, func() (*discoverycache.Cache, error) {
		return nil, errors.New("looking up home directory: no $HOME")
	})

	events, stop := startStream(t, StreamOptions{UseCache: true, Prober: failingProber(errors.New("unreachable"))})
	fb.emit(t, wendyService("dev-7", "orin", "orin.local", "10.0.0.7", 50051))

	got := collectEvents(t, events, 1, 5*time.Second)[0]
	if got.Kind != LANFound || got.Device.ID != "dev-7" {
		t.Fatalf("a failed cache load must degrade to an empty cache, not to no scan: %+v", got)
	}
	stop() // the session-end flush must not panic without a cache
}

func TestRunLANStreamClosesProbesDone(t *testing.T) {
	shrinkDuration(t, &offlineGrace, 50*time.Millisecond)

	path := filepath.Join(t.TempDir(), "devices.json")
	seedCache(t, path, discoverycache.Entry{ID: "dev-8", DisplayName: "orin", Hostname: "orin.local", IP: "10.0.0.8", Port: 50051})

	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))

	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan LANEvent, 8)
	probesDone := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(out)
		runLANStream(ctx, StreamOptions{UseCache: true, Prober: failingProber(errors.New("unreachable"))}, out, probesDone)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	select {
	case <-probesDone:
	case <-time.After(5 * time.Second):
		t.Fatal("probesDone must close once every cached entry's initial probe concluded")
	}
}

func TestCollectLANSettlesEarly(t *testing.T) {
	shrinkDuration(t, &collectSettle, 50*time.Millisecond)

	path := filepath.Join(t.TempDir(), "devices.json") // no seed: empty cache
	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))

	go func() {
		fb.emit(t, wendyService("dev-20", "orin", "orin.local", "10.0.0.20", 50051))
		fb.emit(t, wendyService("dev-21", "nano", "nano.local", "10.0.0.21", 50051))
	}()

	start := time.Now()
	got, err := CollectLAN(context.Background(), StreamOptions{UseCache: true}, 5*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("CollectLAN error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 confirmed devices, got %+v", got)
	}
	if elapsed > time.Second {
		t.Fatalf("CollectLAN took %v to settle, want well under 1s", elapsed)
	}
}

func TestCollectLANConfirmedOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	seedCache(t, path, discoverycache.Entry{ID: "dev-22", DisplayName: "orin", Hostname: "orin.local", IP: "10.0.0.22", Port: 50051})

	fb := newFakeBackend() // stays silent: nothing on the network
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))

	got, err := CollectLAN(context.Background(), StreamOptions{UseCache: true, Prober: failingProber(errors.New("dial tcp: connection refused"))}, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("CollectLAN error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("cached-unconfirmed device leaked into batch output: %+v", got)
	}
}

func TestCollectLANWaitsForCachedProbes(t *testing.T) {
	shrinkDuration(t, &collectSettle, 20*time.Millisecond)

	path := filepath.Join(t.TempDir(), "devices.json")
	seedCache(t, path, discoverycache.Entry{ID: "dev-23", DisplayName: "orin", Hostname: "orin.local", IP: "10.0.0.23", Port: 50051})

	fb := newFakeBackend() // stays silent: the probed cache row must still surface
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))

	prober := func(ctx context.Context, dev models.LANDevice) (models.LANDevice, error) {
		select {
		case <-time.After(80 * time.Millisecond):
		case <-ctx.Done():
			return models.LANDevice{}, ctx.Err()
		}
		dev.AgentVersion = "9.9.9"
		return dev, nil
	}

	got, err := CollectLAN(context.Background(), StreamOptions{UseCache: true, Prober: prober}, 5*time.Second)
	if err != nil {
		t.Fatalf("CollectLAN error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "dev-23" || got[0].AgentVersion != "9.9.9" {
		t.Fatalf("settle must wait for the cached probe to conclude before returning: %+v", got)
	}
}

// useAnnotator swaps the platform annotator seam for the duration of a test.
func useAnnotator(t *testing.T, build func(context.Context) func(*models.LANDevice)) {
	t.Helper()
	orig := newLANAnnotator
	t.Cleanup(func() { newLANAnnotator = orig })
	newLANAnnotator = build
}

// TestStreamHostnamelessSightingNeverEmitsEmptyIdentity covers the darwin
// resolve-failure fallback's blast radius at the engine level: a sighting
// with no hostname must either arrive as a named, dialable row (the mapper
// falls back to the instance name) or be dropped entirely. What it must never
// do is produce a row with an empty identity — discoverycache.Key("", "") is
// "", so every such device would collapse onto one nameless, un-dialable
// cache entry that replays as a ghost row for an hour.
func TestStreamHostnamelessSightingNeverEmitsEmptyIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")

	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))

	events, stop := startStream(t, StreamOptions{UseCache: true})

	// Nothing to key a row by at all: no hostname, no instance name, no TXT.
	fb.emit(t, MDNSService{IPAddress: "10.0.0.60", Port: 50051})
	expectQuiet(t, events, 200*time.Millisecond)

	// Hostname-less but named (the darwin resolve-failure fallback's shape):
	// the row is synthesized, named, and dialable.
	fb.emit(t, MDNSService{InstanceName: "orin-nano", Hostname: "orin-nano.local", Port: defaultAgentPort})

	got := collectEvents(t, events, 1, 5*time.Second)[0]
	if got.Kind != LANFound || got.Device.ID != "orin-nano" || got.Device.DisplayName != "orin-nano" {
		t.Fatalf("hostname-less sighting must surface as a named row: %+v", got)
	}
	if got.Device.Hostname == "" || got.Device.Port == 0 {
		t.Fatalf("synthesized row must stay dialable: %+v", got.Device)
	}
	stop()

	reloaded, err := discoverycache.LoadFrom(path)
	if err != nil {
		t.Fatalf("reload cache: %v", err)
	}
	fresh := reloaded.Fresh(time.Now())
	if len(fresh) != 1 {
		t.Fatalf("expected exactly one persisted entry, got %+v", fresh)
	}
	if discoverycache.Key(fresh[0].ID, fresh[0].DisplayName) == "" {
		t.Fatalf("empty-key entry poisoned the cache: %+v", fresh[0])
	}
}

// TestCollectLANWaitsForFirstAnswerOnColdCache pins the batch scan's settle
// gate: with an empty cache every probe has concluded before the session even
// begins, so arming settle at that point would conclude an empty scan in
// collectSettle — `wendy discover --json` returning nothing on a first run
// while the answer was still in flight. Settle may only start once something
// has actually been confirmed.
func TestCollectLANWaitsForFirstAnswerOnColdCache(t *testing.T) {
	shrinkDuration(t, &collectSettle, 50*time.Millisecond)

	path := filepath.Join(t.TempDir(), "devices.json") // no seed: cold cache

	answerDelay := 800 * time.Millisecond
	backend := func(ctx context.Context, _ string, emit func(MDNSService)) error {
		select {
		case <-time.After(answerDelay):
		case <-ctx.Done():
			return nil
		}
		emit(wendyService("dev-24", "orin", "orin.local", "10.0.0.24", 50051))
		<-ctx.Done()
		return nil
	}
	useStreamSeams(t, backend, cacheLoaderFor(path))

	start := time.Now()
	got, err := CollectLAN(context.Background(), StreamOptions{UseCache: true}, 5*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("CollectLAN error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "dev-24" {
		t.Fatalf("a slow first answer must not be cut off by settle (returned after %v): %+v", elapsed, got)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("CollectLAN ran to the timeout cap (%v); settle should have concluded it after the answer", elapsed)
	}
}

// TestStreamLiveDeviceProbeFailureIsReported covers the probe black hole: a
// device mDNS can see but whose agent does not answer used to produce no
// event at all, leaving every surface spinning on "verifying" forever. The
// failure is reported once — repeats stay silent until something re-confirms
// the row.
func TestStreamLiveDeviceProbeFailureIsReported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")

	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))

	events, _ := startStream(t, StreamOptions{UseCache: true, Prober: failingProber(errors.New("connection refused"))})
	fb.emit(t, wendyService("dev-30", "orin", "orin.local", "10.0.0.30", 50051))

	got := collectEvents(t, events, 2, 5*time.Second)
	if got[0].Kind != LANFound || got[0].Probed || got[0].ProbeFailed {
		t.Fatalf("first event must be the unprobed mDNS sighting: %+v", got[0])
	}
	if got[1].Kind != LANUpdated || !got[1].ProbeFailed || got[1].Probed {
		t.Fatalf("a failed probe on a live device must be reported: %+v", got[1])
	}
	if got[1].Device.ID != "dev-30" {
		t.Fatalf("the failure must name the device it belongs to: %+v", got[1].Device)
	}
	// A live device never goes Offline — that marker is for cached rows the
	// network has not seen at all.
	expectQuiet(t, events, 200*time.Millisecond)
}

// TestStreamReSightingReProbesFailedDevice covers mid-boot recovery on every
// platform: a device whose probe failed is re-probed when mDNS sees it again
// (its agent may have finished starting), and that retry is event-driven —
// there is no polling loop behind it.
func TestStreamReSightingReProbesFailedDevice(t *testing.T) {
	shrinkDuration(t, &probeRetryInterval, 0)

	path := filepath.Join(t.TempDir(), "devices.json")
	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))

	var calls atomic.Int32
	prober := func(_ context.Context, dev models.LANDevice) (models.LANDevice, error) {
		if calls.Add(1) == 1 {
			return models.LANDevice{}, errors.New("connection refused")
		}
		dev.AgentVersion = "0.19.1"
		return dev, nil
	}

	events, _ := startStream(t, StreamOptions{UseCache: true, Prober: prober})

	svc := wendyService("dev-31", "orin", "orin.local", "10.0.0.31", 50051)
	fb.emit(t, svc)

	got := collectEvents(t, events, 2, 5*time.Second)
	if got[0].Kind != LANFound || !got[1].ProbeFailed {
		t.Fatalf("expected a sighting then a reported probe failure: %+v", got)
	}

	// The device announces itself again, unchanged: no row update is due, but
	// the failed probe is retried against it.
	fb.emit(t, svc)

	recovered := collectEvents(t, events, 1, 5*time.Second)[0]
	if recovered.Kind != LANUpdated || !recovered.Probed || recovered.Device.AgentVersion != "0.19.1" {
		t.Fatalf("the re-sighting's probe must confirm the row: %+v", recovered)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("expected exactly one re-probe off the re-sighting, prober ran %d times", n)
	}
}

// TestStreamCachedRowsPrecedeAnnotatorConstruction pins the whole point of
// the cache: building the platform annotator shells out (networksetup on
// darwin, Get-NetAdapter on windows — 0.5–2s), so it must not sit between
// session start and the cached rows. It is built lazily, on the first live
// sighting, which is the only thing that needs it.
func TestStreamCachedRowsPrecedeAnnotatorConstruction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	seedCache(t, path, discoverycache.Entry{ID: "dev-40", DisplayName: "orin", Hostname: "orin.local", IP: "10.0.0.40", Port: 50051})

	var built atomic.Bool
	buildStarted := make(chan struct{})
	useAnnotator(t, func(context.Context) func(*models.LANDevice) {
		close(buildStarted)
		time.Sleep(300 * time.Millisecond)
		built.Store(true)
		return func(*models.LANDevice) {}
	})

	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))

	start := time.Now()
	events, _ := startStream(t, StreamOptions{UseCache: true})

	cached := collectEvents(t, events, 1, 5*time.Second)[0]
	elapsed := time.Since(start)
	if cached.Kind != LANCached || cached.Device.ID != "dev-40" {
		t.Fatalf("first event must be the cached row: %+v", cached)
	}
	if built.Load() || elapsed >= 300*time.Millisecond {
		t.Fatalf("cached row waited on the annotator (%v elapsed, built=%v)", elapsed, built.Load())
	}

	// It is still built — a live sighting is what needs the refinement.
	fb.emit(t, wendyService("dev-41", "nano", "nano.local", "10.0.0.41", 50051))
	select {
	case <-buildStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("annotator was never built for a live sighting")
	}
}

// TestStreamSupersedesConnectMintedIdentity covers the duplicate row a plain
// `wendy run` leaves behind: cacheConnectSuccess mints an identity from the
// hostname when it finds no cache entry, and the same device's real TXT
// device id then arrives as a second identity. The engine retires the minted
// one — one row emitted, one cache entry left, keyed by the TXT id.
func TestStreamSupersedesConnectMintedIdentity(t *testing.T) {
	mintedEntry := func() discoverycache.Entry {
		return discoverycache.Entry{ID: "orin", DisplayName: "orin", Hostname: "orin.local", IP: "10.0.0.42", Port: 50051}
	}
	sighting := wendyService("uuid-1", "orin", "orin.local", "10.0.0.42", 50051)

	// liveRows replays what a surface keyed by cache identity would show.
	liveRows := func(events []LANEvent) map[string]bool {
		rows := make(map[string]bool)
		for _, ev := range events {
			if ev.Supersedes != "" {
				delete(rows, ev.Supersedes)
			}
			rows[discoverycache.Key(ev.Device.ID, ev.Device.DisplayName)] = true
		}
		return rows
	}

	assertOneEntry := func(t *testing.T, path string) {
		t.Helper()
		reloaded, err := discoverycache.LoadFrom(path)
		if err != nil {
			t.Fatalf("reload cache: %v", err)
		}
		fresh := reloaded.Fresh(time.Now())
		if len(fresh) != 1 || fresh[0].ID != "uuid-1" {
			t.Fatalf("cache must end with exactly one entry, keyed by the TXT id: %+v", fresh)
		}
	}

	t.Run("minted row still unconfirmed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "devices.json")
		seedCache(t, path, mintedEntry())

		fb := newFakeBackend()
		useStreamSeams(t, fb.fn, cacheLoaderFor(path))

		events, stop := startStream(t, StreamOptions{UseCache: true}) // browse-only
		cached := collectEvents(t, events, 1, 5*time.Second)[0]

		fb.emit(t, sighting)
		found := collectEvents(t, events, 1, 5*time.Second)[0]
		if found.Kind != LANFound || found.Device.ID != "uuid-1" {
			t.Fatalf("the TXT-id sighting must confirm the device: %+v", found)
		}
		if found.Supersedes != "orin" {
			t.Fatalf("Supersedes = %q; want the minted identity it replaced", found.Supersedes)
		}
		expectQuiet(t, events, 200*time.Millisecond)
		if rows := liveRows([]LANEvent{cached, found}); len(rows) != 1 || !rows["uuid-1"] {
			t.Fatalf("expected exactly one live row keyed by the TXT id, got %v", rows)
		}
		stop()
		assertOneEntry(t, path)
	})

	t.Run("minted row already emitted as confirmed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "devices.json")
		seedCache(t, path, mintedEntry())

		fb := newFakeBackend()
		useStreamSeams(t, fb.fn, cacheLoaderFor(path))

		prober := func(_ context.Context, dev models.LANDevice) (models.LANDevice, error) {
			dev.AgentVersion = "0.19.1"
			return dev, nil
		}
		events, stop := startStream(t, StreamOptions{UseCache: true, Prober: prober})

		got := collectEvents(t, events, 2, 5*time.Second)
		if got[1].Kind != LANFound || !got[1].Probed || got[1].Device.ID != "orin" {
			t.Fatalf("the minted row must be live-confirmed before the sighting: %+v", got[1])
		}

		fb.emit(t, sighting)
		replaced := collectEvents(t, events, 1, 5*time.Second)[0]
		if replaced.Supersedes != "orin" || replaced.Device.ID != "uuid-1" {
			t.Fatalf("an already-emitted minted row must be replaced, not duplicated: %+v", replaced)
		}
		// The retargeted probe re-confirms the row under its new identity.
		reconfirmed := collectEvents(t, events, 1, 5*time.Second)[0]
		if reconfirmed.Device.ID != "uuid-1" || !reconfirmed.Probed {
			t.Fatalf("superseding identity must be re-probed: %+v", reconfirmed)
		}
		if rows := liveRows(append(got, replaced, reconfirmed)); len(rows) != 1 || !rows["uuid-1"] {
			t.Fatalf("expected exactly one live row keyed by the TXT id, got %v", rows)
		}
		stop()
		assertOneEntry(t, path)
	})
}

// TestStreamIgnoresInterfaceChurn covers the multi-homed sighting churn every
// hashicorp-backed platform produces: each device is re-announced per
// interface per sweep, and Windows adds an interface-less default sweep on
// top. Those repeats must not flip the row's interface back and forth, and
// must not rewrite the cache file on every sweep.
func TestStreamIgnoresInterfaceChurn(t *testing.T) {
	shrinkDuration(t, &cacheFlushDelay, 20*time.Millisecond)

	path := filepath.Join(t.TempDir(), "devices.json")
	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))

	events, _ := startStream(t, StreamOptions{UseCache: true})

	withIface := wendyService("dev-51", "orin", "orin.local", "10.0.0.51", 50051)
	withIface.InterfaceName = "eth0"
	blankIface := withIface
	blankIface.InterfaceName = ""

	fb.emit(t, withIface)
	found := collectEvents(t, events, 1, 5*time.Second)[0]
	if found.Kind != LANFound || found.Device.NetworkInterface != "eth0" {
		t.Fatalf("first sighting must land with its interface: %+v", found)
	}

	// Let the first (real) flush land, then watch the file across the churn.
	var before os.FileInfo
	deadline := time.Now().Add(3 * time.Second)
	for {
		info, err := os.Stat(path)
		if err == nil {
			before = info
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sighting never reached the cache file: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	for i := 0; i < 3; i++ {
		fb.emit(t, blankIface)
		fb.emit(t, withIface)
	}

	// Not one further event: an interface that blanks and comes back changed
	// nothing about the device.
	expectQuiet(t, events, 200*time.Millisecond)

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat cache: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("unchanged re-announcements rewrote the cache file: %v → %v", before.ModTime(), after.ModTime())
	}
}

// TestStreamKeepsIPv4TargetOverLinkLocalIPv6 covers the other half of the
// churn: avahi and hashicorp both report a device once per protocol, and the
// IPv6 answer is typically a link-local address that needs a zone id. A row
// already answering on IPv4 must not retarget to it (which would clear
// probeConfirmed and flip the surface back to a spinner every sweep).
func TestStreamKeepsIPv4TargetOverLinkLocalIPv6(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))

	events, stop := startStream(t, StreamOptions{UseCache: true})

	fb.emit(t, wendyService("dev-52", "orin", "orin.local", "10.0.0.52", 50051))
	found := collectEvents(t, events, 1, 5*time.Second)[0]
	if found.Device.IPAddress != "10.0.0.52" {
		t.Fatalf("first sighting must land at its IPv4 address: %+v", found.Device)
	}

	v6 := wendyService("dev-52", "orin", "orin.local", "fe80::1%eth0", 50051)
	v6.InterfaceName = "eth0"
	fb.emit(t, v6)
	updated := collectEvents(t, events, 1, 5*time.Second)[0]
	if updated.Kind != LANUpdated || updated.Device.IPAddress != found.Device.IPAddress {
		t.Fatalf("address update changed primary: %+v", updated)
	}
	if len(updated.Device.Addresses) != 2 || updated.Device.Addresses[1] != "fe80::1%eth0" {
		t.Fatalf("sibling missing from event: %+v", updated.Device)
	}
	fb.emit(t, v6)
	expectQuiet(t, events, 200*time.Millisecond)
	stop()

	reloaded, err := discoverycache.LoadFrom(path)
	if err != nil {
		t.Fatalf("reload cache: %v", err)
	}
	fresh := reloaded.Fresh(time.Now())
	if len(fresh) != 1 || fresh[0].IP != "10.0.0.52" {
		t.Fatalf("stored dial target must stay the IPv4 one: %+v", fresh)
	}
}
