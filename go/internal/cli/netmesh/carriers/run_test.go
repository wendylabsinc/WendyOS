package carriers

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

var testTenant, _ = meshid.ParseTenant("11111111-1111-4111-8111-111111111111")

func testDevice(n byte) meshid.Device {
	var id uuid.UUID
	id[0] = n
	id[6] = 0x40
	id[8] = 0x80
	return meshid.NewDevice(testTenant, id)
}
func testConfig(connect func(context.Context, Candidate) error, sources ...Source) Config {
	return Config{Credentials: &localmesh.Credentials{Device: testDevice(1)}, Connect: connect, Sources: sources, Limits: Limits{Candidates: 8, Connections: 3, Attempts: 2, BLEConnections: 2, PollInterval: 2 * time.Millisecond, RetryDelay: 20 * time.Millisecond, CandidateTTL: time.Second, AuthenticatedTTL: time.Second}}
}
func sourceChannel(ch <-chan []Candidate) Source {
	return func(ctx context.Context, emit func([]Candidate)) error {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case x := <-ch:
				emit(x)
			}
		}
	}
}
func wait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
		var zero T
		return zero
	}
}

func TestRunKeepsAttachedLinkAfterAdvertisementDisappears(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan []Candidate, 4)
	entered := make(chan Candidate, 4)
	ended := make(chan bool, 4)
	done := make(chan error, 1)
	cfg := testConfig(func(ctx context.Context, c Candidate) error {
		c.Authenticated(testDevice(2))
		entered <- c
		<-ctx.Done()
		ended <- true
		return ctx.Err()
	}, sourceChannel(updates))
	go func() { done <- Run(ctx, cfg) }()
	updates <- []Candidate{{Transport: BLE, Address: "radio", PSM: 129, Cost: 4096}}
	wait(t, entered)
	updates <- nil
	select {
	case <-ended:
		t.Fatal("disappearing advertisement closed authenticated link")
	case <-time.After(40 * time.Millisecond):
	}
	cancel()
	if !errors.Is(wait(t, done), context.Canceled) {
		t.Fatal("lost cancellation")
	}
	wait(t, ended)
}

func TestRunAuthenticatedBLECacheSuppressesLANRedialAndFallsBack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan []Candidate, 8)
	entered := make(chan string, 16)
	ended := make(chan string, 16)
	done := make(chan error, 1)
	var mu sync.Mutex
	view := localmesh.NodeSnapshot{}
	peer := testDevice(2)
	var dropLAN context.CancelFunc
	cfg := testConfig(func(ctx context.Context, c Candidate) error {
		c.Authenticated(peer)
		linkCtx := ctx
		if c.Transport == TCP {
			var close context.CancelFunc
			linkCtx, close = context.WithCancel(ctx)
			mu.Lock()
			dropLAN = close
			mu.Unlock()
		}
		mu.Lock()
		view.Links = append(view.Links, localmesh.PeerLink{Device: peer, Cost: c.Cost, Transport: c.Transport})
		mu.Unlock()
		entered <- c.Transport
		<-linkCtx.Done()
		mu.Lock()
		for i, l := range view.Links {
			if l.Transport == c.Transport {
				view.Links = append(view.Links[:i], view.Links[i+1:]...)
				break
			}
		}
		mu.Unlock()
		ended <- c.Transport
		return linkCtx.Err()
	}, sourceChannel(updates))
	cfg.Snapshot = func() localmesh.NodeSnapshot {
		mu.Lock()
		defer mu.Unlock()
		return localmesh.NodeSnapshot{Links: append([]localmesh.PeerLink(nil), view.Links...)}
	}
	go func() { done <- Run(ctx, cfg) }()
	ble := Candidate{Transport: BLE, Address: "radio", PSM: 129, Cost: 4096}
	lan := Candidate{Transport: TCP, Address: "192.0.2.2:43024", Device: peer, Cost: 128}
	updates <- []Candidate{ble}
	if wait(t, entered) != BLE {
		t.Fatal("missing BLE")
	}
	updates <- []Candidate{ble, lan}
	if wait(t, entered) != TCP {
		t.Fatal("missing LAN")
	}
	if wait(t, ended) != BLE {
		t.Fatal("BLE did not yield after authenticated LAN")
	}
	select {
	case <-entered:
		t.Fatal("known BLE reauthenticated while LAN healthy")
	case <-time.After(60 * time.Millisecond):
	}
	mu.Lock()
	close := dropLAN
	mu.Unlock()
	if close == nil {
		t.Fatal("LAN cancellation missing")
	}
	close()
	if wait(t, ended) != TCP {
		t.Fatal("LAN didn't end")
	}
	if wait(t, entered) != BLE {
		t.Fatal("known radio did not immediately recover after LAN loss")
	}
	cancel()
	wait(t, done)
}

func TestRunBoundsConnectionsAndStopsAllCallbacks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan []Candidate, 1)
	entered := make(chan bool, 20)
	done := make(chan error, 1)
	var mu sync.Mutex
	running, peak := 0, 0
	cfg := testConfig(func(ctx context.Context, c Candidate) error {
		mu.Lock()
		running++
		if running > peak {
			peak = running
		}
		mu.Unlock()
		entered <- true
		<-ctx.Done()
		mu.Lock()
		running--
		mu.Unlock()
		return ctx.Err()
	}, sourceChannel(updates))
	cfg.Limits.Candidates = 4
	cfg.Limits.Connections = 2
	cfg.Limits.Attempts = 2
	go func() { done <- Run(ctx, cfg) }()
	var candidates []Candidate
	for n := 2; n < 20; n++ {
		candidates = append(candidates, Candidate{Transport: TCP, Address: netAddress(n), Cost: 128})
	}
	updates <- candidates
	wait(t, entered)
	wait(t, entered)
	select {
	case <-entered:
		t.Fatal("connection budget exceeded")
	case <-time.After(40 * time.Millisecond):
	}
	cancel()
	wait(t, done)
	mu.Lock()
	defer mu.Unlock()
	if running != 0 || peak != 2 {
		t.Fatalf("running=%d peak=%d", running, peak)
	}
}
func netAddress(n int) string { return "192.0.2." + strconv.Itoa(n) + ":43024" }

func TestPeerCacheRequiresExactAdvertisementAndExpires(t *testing.T) {
	now := time.Now()
	peer := testDevice(2)
	c := Candidate{Transport: BLE, Address: "radio", Cost: 4096, PSM: 129}
	cache := make(peerCache)
	if cache.peer(c, now, time.Minute) != (meshid.Device{}) {
		t.Fatal("unverified radio has identity")
	}
	cache.remember(c, peer, now, time.Minute, 1)
	if cache.peer(c, now, time.Minute) != peer {
		t.Fatal("lost verified identity")
	}
	changed := c
	changed.PSM++
	if cache.peer(changed, now, time.Minute) != (meshid.Device{}) {
		t.Fatal("changed endpoint reused identity")
	}
	if cache.peer(c, now.Add(time.Minute), time.Minute) != (meshid.Device{}) {
		t.Fatal("stale identity retained")
	}
}

func TestRunSeedPersistsThroughDiscoveryLossAndCandidateExpiry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan []Candidate, 4)
	entered := make(chan Candidate, 4)
	release := make(chan struct{}, 4)
	done := make(chan error, 1)
	seed := Candidate{Transport: TCP, Address: netAddress(2), Cost: 128}
	cfg := testConfig(func(ctx context.Context, c Candidate) error {
		entered <- c
		select {
		case <-release:
			return errors.New("link lost")
		case <-ctx.Done():
			return ctx.Err()
		}
	}, sourceChannel(updates))
	cfg.Seeds = []Candidate{seed}
	cfg.Limits.CandidateTTL = 10 * time.Millisecond
	go func() { done <- Run(ctx, cfg) }()
	wait(t, entered)
	updated := seed
	updated.Cost = 256
	updates <- []Candidate{updated}
	// A complete empty snapshot cannot withdraw persistent reconnect intent.
	updates <- nil
	time.Sleep(30 * time.Millisecond)
	release <- struct{}{}
	if c := wait(t, entered); c.Cost != 256 {
		t.Fatalf("fresh metadata not retained: %+v", c)
	}
	cancel()
	wait(t, done)
}

func TestSeedLimitRejectsUnboundedInput(t *testing.T) {
	cfg := testConfig(func(context.Context, Candidate) error { return nil }, []Source{}...)
	cfg.Limits.Candidates = 1
	cfg.Seeds = []Candidate{{Transport: TCP, Address: netAddress(2), Cost: 128}, {Transport: TCP, Address: netAddress(3), Cost: 128}}
	if err := Run(context.Background(), cfg); err == nil {
		t.Fatal("accepted too many seeds")
	}
}

func TestRunFailedCandidateYieldsToOtherPeerAndWaitsForCooldown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type call struct {
		address string
		when    time.Time
	}
	entered := make(chan call, 8)
	done := make(chan error, 1)
	cfg := testConfig(func(_ context.Context, c Candidate) error {
		entered <- call{c.Address, time.Now()}
		return errors.New("peer unavailable")
	}, []Source{}...)
	cfg.Sources = []Source{} // no physical discovery in this fixture
	cfg.Seeds = []Candidate{{Transport: TCP, Address: netAddress(2), Cost: 128}, {Transport: TCP, Address: netAddress(3), Cost: 128}}
	cfg.Limits.Attempts = 1
	cfg.Limits.RetryDelay = 40 * time.Millisecond
	go func() { done <- Run(ctx, cfg) }()
	first, second := wait(t, entered), wait(t, entered)
	if first.address == second.address {
		t.Fatal("failed peer monopolized available attempt slot")
	}
	third := wait(t, entered)
	previous := first
	if third.address == second.address {
		previous = second
	}
	if third.when.Sub(previous.when) < 35*time.Millisecond {
		t.Fatal("failed peer retried before cooldown")
	}
	cancel()
	wait(t, done)
}

// Normal capability mismatch on a radio carrying another TLS role should
// release the attempt slot and wait for a changed advertisement, not churn TLS.
func TestRunUnsupportedRoleWaitsForChangedAdvertisement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan []Candidate, 8)
	entered := make(chan Candidate, 8)
	status := make(chan error, 8)
	done := make(chan error, 1)
	cfg := testConfig(func(_ context.Context, c Candidate) error { entered <- c; return ErrUnsupportedPeer }, sourceChannel(updates))
	cfg.Status = func(err error) { status <- err }
	go func() { done <- Run(ctx, cfg) }()
	c := Candidate{Transport: BLE, Address: "ordinary-other-role", PSM: 129, Cost: 4096}
	updates <- []Candidate{c}
	wait(t, entered)
	if !errors.Is(wait(t, status), ErrUnsupportedPeer) {
		t.Fatal("missing capability status")
	}
	for range 3 {
		updates <- []Candidate{c}
	}
	select {
	case <-entered:
		t.Fatal("repeated TLS attempt for unchanged unsupported role")
	case <-time.After(80 * time.Millisecond):
	}
	c.Capabilities = 3
	updates <- []Candidate{c}
	if got := wait(t, entered); got.Capabilities != 3 {
		t.Fatal("fresh changed role hint not reconsidered")
	}
	cancel()
	wait(t, done)
}

func TestRunCableReplacesLANWhenAllConnectionSlotsAreFull(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan []Candidate, 2)
	entered := make(chan Candidate, 8)
	done := make(chan error, 1)
	var mu sync.Mutex
	running, peak := 0, 0
	cfg := testConfig(func(ctx context.Context, c Candidate) error {
		mu.Lock()
		running++
		if running > peak {
			peak = running
		}
		mu.Unlock()
		c.Authenticated(c.Device)
		entered <- c
		<-ctx.Done()
		mu.Lock()
		running--
		mu.Unlock()
		return ctx.Err()
	}, sourceChannel(updates))
	cfg.Limits.Connections = 2
	lan := []Candidate{{Device: testDevice(2), Transport: TCP, Address: "192.0.2.2:43024", Cost: 128}, {Device: testDevice(3), Transport: TCP, Address: "192.0.2.3:43024", Cost: 128}}
	go func() { done <- Run(ctx, cfg) }()
	updates <- lan
	wait(t, entered)
	wait(t, entered)
	cable := Candidate{Device: testDevice(4), Transport: TCP, Address: "[fe80::5741:1%en77]:43024", Cost: 64}
	updates <- append(lan, cable)
	if got := wait(t, entered); got.Address != cable.Address {
		t.Fatalf("cable was starved: %+v", got)
	}
	cancel()
	wait(t, done)
	mu.Lock()
	defer mu.Unlock()
	if peak > cfg.Limits.Connections || running != 0 {
		t.Fatalf("connection bound or drain failed: peak=%d running=%d", peak, running)
	}
}
