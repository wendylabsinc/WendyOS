package carriers

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// The real manager owns Connect lifetimes. This fixture exposes authenticated
// attachment/readiness separately and can hold completion after cancellation,
// so requesting a close cannot be mistaken for an available physical slot.
type spareRun struct {
	t                *testing.T
	ctx              context.Context
	cancel           context.CancelFunc
	updates          chan []Candidate
	entered, closing chan Candidate
	done             chan error
	mu               sync.Mutex
	links            map[string]localmesh.PeerLink
	block            map[string]chan struct{}
	running, peak    int
}

func newSpareRun(t *testing.T, limits Limits, configure ...func(*Config)) *spareRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := &spareRun{t: t, ctx: ctx, cancel: cancel, updates: make(chan []Candidate, 8), entered: make(chan Candidate, 16), closing: make(chan Candidate, 16), done: make(chan error, 1), links: make(map[string]localmesh.PeerLink), block: make(map[string]chan struct{})}
	cfg := testConfig(func(ctx context.Context, c Candidate) error {
		c.Authenticated(c.Device)
		h.mu.Lock()
		h.running++
		if h.running > h.peak {
			h.peak = h.running
		}
		h.links[c.Address] = localmesh.PeerLink{Device: c.Device, Transport: c.Transport, Cost: c.Cost}
		h.mu.Unlock()
		h.entered <- c
		<-ctx.Done()
		h.closing <- c
		h.mu.Lock()
		blocked := h.block[c.Address]
		h.mu.Unlock()
		if blocked != nil {
			select {
			case <-blocked:
			case <-h.ctx.Done():
			}
		}
		h.mu.Lock()
		delete(h.links, c.Address)
		h.running--
		h.mu.Unlock()
		return ctx.Err()
	}, sourceChannel(h.updates))
	if limits.Connections != 0 {
		cfg.Limits.Connections = limits.Connections
	}
	if limits.Attempts != 0 {
		cfg.Limits.Attempts = limits.Attempts
	}
	if limits.BLEConnections != 0 {
		cfg.Limits.BLEConnections = limits.BLEConnections
	}
	if cfg.Limits.BLEConnections > cfg.Limits.Connections {
		cfg.Limits.BLEConnections = cfg.Limits.Connections
	}
	cfg.Snapshot = func() localmesh.NodeSnapshot {
		h.mu.Lock()
		defer h.mu.Unlock()
		v := localmesh.NodeSnapshot{}
		for _, l := range h.links {
			v.Links = append(v.Links, l)
		}
		return v
	}
	for _, apply := range configure {
		apply(&cfg)
	}
	go func() { h.done <- Run(ctx, cfg) }()
	t.Cleanup(func() { cancel(); wait(t, h.done) })
	return h
}

func (h *spareRun) ready(address string, ready bool) {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	l, ok := h.links[address]
	if !ok {
		h.t.Fatalf("no authenticated attachment %s", address)
	}
	l.ForwardingReady = ready
	h.links[address] = l
}
func (h *spareRun) quiet() {
	h.quietFor(35 * time.Millisecond)
}
func (h *spareRun) quietFor(duration time.Duration) {
	h.t.Helper()
	select {
	case c := <-h.entered:
		h.t.Fatalf("unexpected dial: %+v", c)
	case c := <-h.closing:
		h.t.Fatalf("unexpected carrier retirement: %+v", c)
	case <-time.After(duration):
	}
}
func (h *spareRun) start(candidates ...Candidate) { h.t.Helper(); h.updates <- candidates }
func (h *spareRun) expectStart(c Candidate) {
	h.t.Helper()
	if got := wait(h.t, h.entered); got.Address != c.Address {
		h.t.Fatalf("dialed %s, want %s", got.Address, c.Address)
	}
}
func spareTCP(n byte, cost uint16) Candidate {
	return Candidate{Device: testDevice(n), Transport: TCP, Address: netAddress(int(n)), Cost: cost}
}
func spareBLE(n byte) Candidate {
	return Candidate{Device: testDevice(n), Transport: BLE, Address: "radio-" + netAddress(int(n)), PSM: 129, Cost: 4096}
}

func TestRunNewPeerReclaimsStandbyOnlyAfterInstalledUpgradeAndCompletion(t *testing.T) {
	h := newSpareRun(t, Limits{Connections: 2})
	ble := spareBLE(2)
	lan := spareTCP(2, 128)
	newPeer := spareTCP(3, 128)
	h.start(ble)
	h.expectStart(ble)
	h.ready(ble.Address, true)
	h.start(ble, lan)
	h.expectStart(lan)
	// Authentication and a pending route proof are insufficient to make the
	// existing BLE path disposable, even though another peer needs the slot.
	h.start(ble, lan, newPeer)
	h.quiet()
	h.mu.Lock()
	release := make(chan struct{})
	h.block[ble.Address] = release
	h.mu.Unlock()
	h.ready(lan.Address, true)
	h.ready(ble.Address, false)
	if got := wait(t, h.closing); got.Address != ble.Address {
		t.Fatalf("retired %s, want slower backup", got.Address)
	}
	h.quiet() // No oversubscription, repeated close, or second victim while blocked.
	h.mu.Lock()
	running, peak := h.running, h.peak
	h.mu.Unlock()
	if running != 2 || peak != 2 {
		t.Fatalf("cancelled ownership released capacity early: running=%d peak=%d", running, peak)
	}
	close(release)
	h.expectStart(newPeer)
	h.ready(newPeer.Address, true)
	h.quiet()
	h.mu.Lock()
	_, lanRetained := h.links[lan.Address]
	_, newRetained := h.links[newPeer.Address]
	peak = h.peak
	h.mu.Unlock()
	if !lanRetained || !newRetained || peak != 2 {
		t.Fatalf("lost working peer or exceeded cap: LAN=%v new=%v peak=%d", lanRetained, newRetained, peak)
	}
}

func TestRunReclaimsOnlyBudgetThatNewPeerNeeds(t *testing.T) {
	h := newSpareRun(t, Limits{Connections: 4, BLEConnections: 1})
	ble := spareBLE(2)
	lan := spareTCP(2, 128)
	newBLE := spareBLE(3)
	h.start(ble)
	h.expectStart(ble)
	h.start(ble, lan)
	h.expectStart(lan)
	h.ready(lan.Address, true)
	h.start(ble, lan, newBLE)
	if got := wait(t, h.closing); got.Address != ble.Address {
		t.Fatalf("BLE limit reclaimed wrong carrier: %+v", got)
	}
	h.expectStart(newBLE)
	h.quiet()
	h.mu.Lock()
	_, retained := h.links[lan.Address]
	peak := h.peak
	h.mu.Unlock()
	if !retained || peak > 2 {
		t.Fatalf("unrelated total capacity changed reclamation: LAN=%v peak=%d", retained, peak)
	}
}

func TestRunUnknownBLECanProbeByReclaimingOnlyVerifiedRedundancy(t *testing.T) {
	unknown := spareBLE(3)
	unknown.Device = meshid.Device{} // An advertisement supplies no authenticated identity.
	actualPeer := testDevice(3)
	var probes atomic.Int32
	h := newSpareRun(t, Limits{Connections: 2, BLEConnections: 1}, func(cfg *Config) {
		connect := cfg.Connect
		cfg.Connect = func(ctx context.Context, c Candidate) error {
			if c.Address == unknown.Address {
				if c.Device != (meshid.Device{}) {
					return errors.New("unknown advertisement acquired an identity before authentication")
				}
				probes.Add(1)
				// Model the verified TLS boundary. The callback retains its exact
				// manager ownership token; only authentication supplies the peer.
				c.Device = actualPeer
			}
			return connect(ctx, c)
		}
	})
	ble, lan := spareBLE(2), spareTCP(2, 128)
	h.start(ble)
	h.expectStart(ble)
	h.ready(ble.Address, true)
	h.start(ble, lan)
	h.expectStart(lan)
	h.start(ble, lan, unknown)
	h.quiet() // Unknown demand and an unready LAN cannot authorize any close.
	h.mu.Lock()
	release := make(chan struct{})
	h.block[ble.Address] = release
	h.mu.Unlock()
	h.ready(lan.Address, true)
	h.ready(ble.Address, false)
	if got := wait(t, h.closing); got.Address != ble.Address {
		t.Fatalf("unknown probe retired the wrong owned carrier: %+v", got)
	}
	h.quiet() // Still full until the exact old lifetime owner actually completes.
	if n := probes.Load(); n != 0 {
		t.Fatalf("probe started before the owned slot was released: %d", n)
	}
	close(release)
	got := wait(t, h.entered)
	if got.Address != unknown.Address || got.Device != actualPeer {
		t.Fatalf("probe did not record the exact authenticated identity: %+v", got)
	}
	h.ready(unknown.Address, true)
	h.start(ble, lan, unknown)
	h.quiet() // No repeated close, duplicate probe, or hint-derived identity churn.
	h.mu.Lock()
	peer := h.links[unknown.Address].Device
	_, retained := h.links[lan.Address]
	peak := h.peak
	h.mu.Unlock()
	if probes.Load() != 1 || peer != actualPeer || !retained || peak != 2 {
		t.Fatalf("invalid bounded probe outcome: probes=%d peer=%s LAN=%v peak=%d", probes.Load(), peer, retained, peak)
	}
}

func TestRunFullLastPathIsNotEvictedForUpgradeOrNewPeer(t *testing.T) {
	h := newSpareRun(t, Limits{Connections: 1, Attempts: 1})
	ble := spareBLE(2)
	lan := spareTCP(2, 128)
	newPeer := spareTCP(3, 128)
	h.start(ble)
	h.expectStart(ble)
	h.ready(ble.Address, true)
	h.start(ble, lan, newPeer)
	h.quiet()
	// Discovery changes cannot manufacture a working alternate.
	changed := newPeer
	changed.Hint[0] = 7
	h.start(ble, lan, changed)
	h.start(ble, lan, newPeer)
	h.quiet()
}

func TestRunUnrelatedInstalledRouteCannotMakeCarrierRedundant(t *testing.T) {
	h := newSpareRun(t, Limits{Connections: 2})
	ble, unrelated, fresh := spareBLE(2), spareTCP(3, 64), spareTCP(4, 128)
	h.start(ble)
	h.expectStart(ble)
	h.ready(ble.Address, true)
	h.start(ble, unrelated)
	h.expectStart(unrelated)
	h.ready(unrelated.Address, true)
	h.start(ble, unrelated, fresh)
	h.quiet()
}

func TestRunRetryingAndExpiredDemandCannotEvictStandby(t *testing.T) {
	fresh := spareTCP(3, 128)
	failed := make(chan struct{}, 1)
	var attempts atomic.Int32
	h := newSpareRun(t, Limits{Connections: 2}, func(cfg *Config) {
		connect := cfg.Connect
		cfg.Limits.RetryDelay = 250 * time.Millisecond
		cfg.Limits.CandidateTTL = 70 * time.Millisecond
		cfg.Connect = func(ctx context.Context, c Candidate) error {
			if c.Address == fresh.Address {
				attempts.Add(1)
				failed <- struct{}{}
				return errors.New("bounded authenticated setup failed")
			}
			return connect(ctx, c)
		}
	})
	h.start(fresh)
	wait(t, failed)
	ble, lan := spareBLE(2), spareTCP(2, 128)
	h.start(ble)
	h.expectStart(ble)
	h.start(ble, lan)
	h.expectStart(lan)
	h.ready(lan.Address, true)
	// Advertisements refresh scheduling hints, never a failed dial's retry
	// deadline. Once withdrawn/expired the candidate must stop being demand.
	h.start(ble, lan, fresh)
	h.start(ble, lan, fresh)
	h.quietFor(90 * time.Millisecond)
	h.start(ble, lan)
	h.quietFor(200 * time.Millisecond)
	if n := attempts.Load(); n != 1 {
		t.Fatalf("retrying or stale demand launched %d probes", n)
	}
}

func TestRunUnsupportedBLECannotConsumeOrReclaimCapacity(t *testing.T) {
	h := newSpareRun(t, Limits{Connections: 2}, func(cfg *Config) {
		cfg.SupportedTransports = []string{TCP}
	})
	lan, ble := spareTCP(2, 128), spareBLE(2)
	h.start(lan)
	h.expectStart(lan)
	h.ready(lan.Address, true)
	h.start(lan, ble, spareBLE(3))
	h.quiet()
}

func TestRunFailedCandidateCannotRepeatedlyReclaimWarmStandby(t *testing.T) {
	fresh := spareTCP(3, 128)
	failed := make(chan struct{}, 8)
	var attempts atomic.Int32
	h := newSpareRun(t, Limits{Connections: 2}, func(cfg *Config) {
		connect := cfg.Connect
		cfg.Connect = func(ctx context.Context, c Candidate) error {
			if c.Address == fresh.Address {
				attempts.Add(1)
				failed <- struct{}{}
				return errors.New("new peer setup failed")
			}
			return connect(ctx, c)
		}
	})
	h.start(fresh)
	wait(t, failed)
	ble, lan := spareBLE(2), spareTCP(2, 128)
	h.start(ble)
	h.expectStart(ble)
	h.start(ble, lan)
	h.expectStart(lan)
	h.ready(lan.Address, true)
	// A failed endpoint's normal dial retry may run again after 20ms, but the
	// existing 30s failure penalty must protect established warm redundancy
	// from repeated reclamation. Fresh source updates cannot erase that history.
	h.quietFor(30 * time.Millisecond)
	for i := 0; i < 3; i++ {
		h.start(ble, lan, fresh)
		h.quiet()
	}
	if n := attempts.Load(); n != 1 {
		t.Fatalf("failed demand repeatedly displaced standby: probes=%d", n)
	}
}

func TestRunFailedNewPeerRecoversAfterExistingPenaltyUnderFullBudget(t *testing.T) {
	fresh := spareTCP(3, 128)
	failed := make(chan struct{}, 1)
	var attempts atomic.Int32
	var clock atomic.Int64
	epoch := time.Now()
	clock.Store(epoch.UnixNano())
	h := newSpareRun(t, Limits{Connections: 2}, func(cfg *Config) {
		cfg.now = func() time.Time { return time.Unix(0, clock.Load()) }
		connect := cfg.Connect
		cfg.Connect = func(ctx context.Context, c Candidate) error {
			if c.Address == fresh.Address && attempts.Add(1) == 1 {
				failed <- struct{}{}
				return errors.New("first probe failed")
			}
			return connect(ctx, c)
		}
	})
	h.start(fresh)
	wait(t, failed)
	ble, lan := spareBLE(2), spareTCP(2, 128)
	h.start(ble) // Remove the failed hint's observed entry, preserving queue history.
	h.expectStart(ble)
	h.start(ble, lan)
	h.expectStart(lan)
	h.ready(lan.Address, true)
	clock.Store(epoch.Add(localmesh.FailedConnectPenalty - time.Nanosecond).UnixNano())
	h.start(ble, lan, fresh) // Re-adding the same endpoint must retain its penalty.
	h.quiet()
	if n := attempts.Load(); n != 1 {
		t.Fatalf("penalized demand retried or reclaimed early: probes=%d", n)
	}
	h.mu.Lock()
	release := make(chan struct{})
	h.block[ble.Address] = release
	h.mu.Unlock()
	clock.Store(epoch.Add(localmesh.FailedConnectPenalty + time.Second).UnixNano())
	h.start(ble, lan, fresh) // Refresh legitimate current demand after penalty expiry.
	if got := wait(t, h.closing); got.Address != ble.Address {
		t.Fatalf("expired penalty reclaimed the wrong owned carrier: %+v", got)
	}
	h.quiet() // Cancellation cannot bypass the still-occupied two-link budget.
	close(release)
	h.expectStart(fresh)
	h.ready(fresh.Address, true)
	h.quiet()
	if n := attempts.Load(); n != 2 {
		t.Fatalf("new peer did not recover exactly once after penalty: probes=%d", n)
	}
	h.mu.Lock()
	_, retained := h.links[lan.Address]
	peak := h.peak
	h.mu.Unlock()
	if !retained || peak != 2 {
		t.Fatalf("recovery lost the working LAN or oversubscribed: LAN=%v peak=%d", retained, peak)
	}
}

func TestRunFullAttemptBudgetDoesNotEvictWorkingStandby(t *testing.T) {
	blocked := spareTCP(3, 128)
	entered := make(chan struct{}, 1)
	h := newSpareRun(t, Limits{Connections: 3, Attempts: 1}, func(cfg *Config) {
		connect := cfg.Connect
		cfg.Connect = func(ctx context.Context, c Candidate) error {
			if c.Address == blocked.Address {
				// The setup remains unverified and consumes the only attempt.
				entered <- struct{}{}
				<-ctx.Done()
				return ctx.Err()
			}
			return connect(ctx, c)
		}
	})
	ble, lan := spareBLE(2), spareTCP(2, 128)
	h.start(ble)
	h.expectStart(ble)
	h.start(ble, lan)
	h.expectStart(lan)
	h.ready(lan.Address, true)
	h.start(ble, lan, blocked)
	wait(t, entered)
	h.start(ble, lan, blocked, spareTCP(4, 128))
	h.quiet()
}

func TestRunPrefersNewPeerThenUpgradeThenRedundancy(t *testing.T) {
	for _, newPeerPresent := range []bool{false, true} {
		name := "upgrade-before-redundancy"
		if newPeerPresent {
			name = "new-peer-before-upgrade"
		}
		t.Run(name, func(t *testing.T) {
			h := newSpareRun(t, Limits{Connections: 3, Attempts: 1})
			ble := spareBLE(2)
			upgrade := spareTCP(2, 128)
			h.start(ble)
			h.expectStart(ble)
			// Another authenticated LAN peer makes its BLE candidate redundancy.
			lan := spareTCP(3, 128)
			h.start(ble, lan)
			h.expectStart(lan)
			h.ready(lan.Address, true)
			redundant := spareBLE(3)
			fresh := spareTCP(4, 128)
			candidates := []Candidate{ble, lan, upgrade, redundant}
			want := upgrade
			if newPeerPresent {
				candidates = append(candidates, fresh)
				want = fresh
			}
			h.start(candidates...)
			h.expectStart(want)
		})
	}
}

func TestRunUpgradeComparedWithWorkingCarrierNotStaleCheaperAttachment(t *testing.T) {
	h := newSpareRun(t, Limits{Connections: 3})
	ble, staleLAN := spareBLE(2), spareTCP(2, 128)
	h.start(ble)
	h.expectStart(ble)
	h.ready(ble.Address, true)
	h.start(ble, staleLAN)
	h.expectStart(staleLAN)
	// A verified but Babel-dead LAN attachment has no usable lower cost. The
	// middle-cost candidate is an upgrade over the working BLE route, while a
	// second medium to another directly connected peer is only redundancy.
	h.mu.Lock()
	h.links["incoming-peer-3"] = localmesh.PeerLink{Device: testDevice(3), Transport: TCP, Cost: 128, ForwardingReady: true}
	h.mu.Unlock()
	upgrade := spareTCP(2, 512)
	upgrade.Address = netAddress(22)
	h.start(ble, staleLAN, upgrade, spareBLE(3))
	h.expectStart(upgrade)
}
