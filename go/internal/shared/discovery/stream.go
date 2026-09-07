package discovery

import (
	"context"
	"log"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/discoverycache"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// LANEventKind classifies a LANEvent emitted by a streaming LAN discovery scan.
type LANEventKind int

const (
	LANCached    LANEventKind = iota // cache entry, not yet verified this run
	LANFound                         // live-confirmed (mDNS resolve or probe)
	LANUpdated                       // an already-emitted device changed
	LANOffline                       // cached entry failed verification
	LANRetracted                     // a listed device is excluded after all; drop its row
)

// LANEvent is a single update emitted while streaming LAN discovery results.
type LANEvent struct {
	Kind   LANEventKind
	Device models.LANDevice
	// Probed: Device's AgentVersion/OS/IsMTLS were confirmed by a live agent
	// probe (not just mDNS TXT records).
	Probed bool
	// ProbeFailed: the agent probe for this live-confirmed device concluded
	// with an error (refused, timed out, TLS rejected). The device is on the
	// network — mDNS just saw it — but nothing answered as an agent, so a
	// surface must stop showing a "verifying" spinner for it. Never set
	// together with Probed. A cached device that was never seen live goes
	// LANOffline instead.
	ProbeFailed bool
	// Supersedes names the cache key of an already-emitted identity this
	// device replaces: the connect-minted, hostname-derived row
	// (cacheConnectSuccess mints ID == DisplayName == hostname) for the same
	// physical device the live sighting just identified by its TXT device id.
	// A surface keyed by cache identity must drop that row; one keyed by
	// hostname (the picker) already merges the two.
	Supersedes string
}

// LANProber verifies a device by talking to its agent. On success the
// returned device carries refreshed AgentVersion/DeviceType/OS/OSVersion/
// CPUArchitecture and IsMTLS reflecting the actual connection.
type LANProber func(ctx context.Context, dev models.LANDevice) (models.LANDevice, error)

// StreamOptions configures a streaming LAN discovery scan.
type StreamOptions struct {
	UseCache bool      // emit cached entries and persist discoveries
	Prober   LANProber // nil = no probing (mDNS-only confirmation)
	Exclude  LANFilter // nil = nothing is excluded
}

// LANFilter keeps sightings a consumer never wants as device rows out of a
// session altogether. The CLI uses it for the VMs it runs itself: they belong
// in a Simulator list, not among LAN devices, yet a user-mode VM's mDNS
// announcement still escapes onto the host's LAN interface (SLIRP forwards
// guest multicast one way), so the engine has to know to drop it. An excluded
// identity is never emitted, probed or persisted, and one that was listed
// before the filter could tell is taken back with a LANRetracted event and
// deleted from the cache.
type LANFilter interface {
	// Exclude reports whether dev must not be listed. It runs on the engine
	// goroutine for every cached entry, live sighting and probe result, so it
	// has to be cheap.
	Exclude(dev models.LANDevice) bool
	// Changed delivers a value whenever Exclude's answer may have changed
	// for a device the session already listed (the CLI learns a VM's
	// hostname a moment after the session starts); the session then
	// re-checks everything it holds. May return nil, meaning never.
	Changed() <-chan struct{}
}

// Package seams. These are vars, not consts, so tests can swap the backend and
// the cache location and shrink the timings; production never reassigns them.
//
// A lanBackendFn implementation browses serviceType and calls emit for every
// resolved answer. It MUST return once ctx is done — the session waits for it
// before closing its event channel — and it may return a non-nil error at any
// time to have the session restart it (see runBackend).
var (
	lanBackendFn      = mdnsStreamBackend       // per-platform mDNS stream
	cacheLoadFn       = discoverycache.Load     // recently-seen device cache
	offlineGrace      = 4 * time.Second         // cached & silent → Offline
	offlineRetryDelay = 30 * time.Second        // one re-probe after Offline
	probeTimeout      = 1500 * time.Millisecond // per-probe ctx budget
	probeWorkers      = 4                       // concurrent probes/resolves
	cacheFlushDelay   = time.Second             // debounce for cache writes
	backendRetryDelay = 2 * time.Second         // backend died mid-session
	backendRetries    = 3                       // ...restart attempts before giving up
	// probeRetryInterval bounds how often a live device whose probe failed is
	// re-probed while it keeps announcing itself. The retry is driven by mDNS
	// re-sightings (a device mid-boot re-announces, and the hashicorp backend
	// re-sweeps every couple of seconds), never by a polling loop of our own,
	// so this only keeps a permanently-broken agent from being dialed on every
	// single sweep.
	probeRetryInterval = 5 * time.Second
	// cacheRefreshInterval is how often an unchanged, still-announcing device
	// re-stamps its cache entry's LastSeen. Repeat announcements otherwise
	// write nothing (see handleSighting), which is what keeps a long-lived
	// picker session from rewriting the cache file on every sweep; this bounds
	// how stale a still-present device's entry can get in such a session.
	cacheRefreshInterval = 5 * time.Minute
)

// streamEventBuffer decouples the engine loop from a consumer that renders
// between reads. Events are never dropped while the session is live; the loop
// simply blocks once the buffer fills.
const streamEventBuffer = 32

// newLANAnnotator returns a func applied to each live-sighted device before
// it is emitted, refining fields mDNS itself cannot supply (network interface
// display name, USB link speed). The default is a no-op; per-platform files
// override it in an init() — safe against init ordering because this default
// is a var initializer, which Go runs before any package's init() functions.
var newLANAnnotator = func(ctx context.Context) func(*models.LANDevice) {
	return func(*models.LANDevice) {}
}

// StreamLAN emits fresh cache entries immediately (when opts.UseCache), then
// live results and probe outcomes until ctx is cancelled. The channel closes
// when the session ends.
func StreamLAN(ctx context.Context, opts StreamOptions) <-chan LANEvent {
	out := make(chan LANEvent, streamEventBuffer)
	go func() {
		defer close(out)
		runLANStream(ctx, opts, out, nil)
	}()
	return out
}

// collectSettle is how long CollectLAN waits after the most recent confirmed
// device event before concluding a batch scan has settled. It is a var, not
// a const, so tests can shrink it; production never reassigns it.
var collectSettle = 500 * time.Millisecond

// CollectLAN runs one LAN discovery session to completion and returns the
// confirmed devices it found — LANFound/LANUpdated only. A cache entry whose
// probe never confirms it (offline, unreachable) never leaks into the
// result. Devices merge by cache key; when a device is reported more than
// once (e.g. a probe superseding an mDNS-only sighting), the later event
// wins.
//
// The scan concludes as soon as it safely can: once at least one device has
// been confirmed, every cached entry's initial probe has concluded, and
// collectSettle has passed with no further confirmation — or once timeout
// elapses, whichever comes first. Settle is deliberately gated on having
// confirmed *something*: on a cold cache every probe concludes before the
// session even starts, and arming settle there would conclude an empty scan
// in collectSettle, long before mDNS has had a chance to answer.
func CollectLAN(ctx context.Context, opts StreamOptions, timeout time.Duration) ([]models.LANDevice, error) {
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	out := make(chan LANEvent, streamEventBuffer)
	probesDone := make(chan struct{})
	go func() {
		defer close(out)
		runLANStream(sessionCtx, opts, out, probesDone)
	}()

	devices := make(map[string]models.LANDevice)
	probesDoneCh := probesDone
	probesDoneClosed := false

	var settleTimer *time.Timer
	var settleC <-chan time.Time
	armSettle := func() {
		if settleTimer != nil {
			settleTimer.Stop()
		}
		settleTimer = time.NewTimer(collectSettle)
		settleC = settleTimer.C
	}
	defer func() {
		if settleTimer != nil {
			settleTimer.Stop()
		}
	}()

	overall := time.NewTimer(timeout)
	defer overall.Stop()

	// conclude tears the session down and waits for it to fully finish before
	// collecting the result, so a caller never observes a session still
	// touching the cache or the seam vars it just used.
	conclude := func(err error) ([]models.LANDevice, error) {
		cancel()
		for range out {
		}
		result := make([]models.LANDevice, 0, len(devices))
		for _, dev := range devices {
			result = append(result, dev)
		}
		return result, err
	}

	for {
		select {
		case ev, ok := <-out:
			if !ok {
				return conclude(nil)
			}
			// A superseded identity is the same physical device under a stale,
			// hostname-derived key; keeping it would list the device twice.
			if ev.Supersedes != "" {
				delete(devices, ev.Supersedes)
			}
			key := discoverycache.Key(ev.Device.ID, ev.Device.DisplayName)
			switch ev.Kind {
			case LANRetracted:
				delete(devices, key)
				// A rejected sighting cannot justify ending an empty scan.
				// Wait for a real result (or the overall timeout) as we would
				// on a cold cache that had never confirmed anything.
				if len(devices) == 0 && settleTimer != nil {
					settleTimer.Stop()
					settleC = nil
				}
			case LANFound, LANUpdated:
				devices[key] = ev.Device
				if probesDoneClosed {
					armSettle()
				}
			}
		case <-probesDoneCh:
			probesDoneClosed = true
			probesDoneCh = nil
			// Nothing confirmed yet (the common cold-cache case): there is no
			// quiet period to measure, so hold out for the timeout cap rather
			// than concluding an empty scan collectSettle from now.
			if len(devices) > 0 {
				armSettle()
			}
		case <-settleC:
			return conclude(nil)
		case <-overall.C:
			return conclude(nil)
		case <-ctx.Done():
			return conclude(ctx.Err())
		}
	}
}

// lanDeviceState is the engine's bookkeeping for one device identity. It is
// owned exclusively by runLANStream's goroutine, so it carries no locks.
type lanDeviceState struct {
	dev models.LANDevice
	// fromCache: this identity entered the session as a LANCached row, so it
	// is the only kind that can go LANOffline.
	fromCache bool
	// confirmed: a live confirmation (mDNS sighting or probe success) has
	// been emitted for this identity.
	confirmed bool
	// probeConfirmed: a probe has succeeded at least once, so the device's
	// version fields are agent-verified rather than cache- or TXT-derived.
	probeConfirmed bool
	probing        bool
	// probeFailed: the most recent concluded probe failed.
	probeFailed bool
	// reportedFailure: consumers currently render this row as probe-failed, so
	// a repeat failure is not re-emitted. Cleared by any later confirmation
	// (a sighting-driven update or a successful probe), which is what makes a
	// device that comes back and fails again report the failure once more.
	reportedFailure bool
	// probeEnded is when the most recent probe concluded; it rate-limits the
	// re-sighting-driven retry (probeRetryInterval).
	probeEnded time.Time
	// persisted is when this identity was last written to the cache, bounding
	// the LastSeen refresh of an otherwise unchanged device.
	persisted time.Time
	// probeGen rises whenever a probe is (re)scheduled; a result carrying an
	// older generation lost its target and is discarded.
	probeGen       int
	offline        bool
	retryScheduled bool
}

// lanProbeResult is one prober outcome handed back to the engine loop.
type lanProbeResult struct {
	key string
	gen int
	dev models.LANDevice
	err error
}

// lanStream holds one session's state. Every field is read and written on the
// runLANStream goroutine except the channels and the WaitGroup.
type lanStream struct {
	ctx  context.Context
	opts StreamOptions
	out  chan<- LANEvent

	states map[string]*lanDeviceState
	cache  *discoverycache.Cache // nil when unavailable or not requested
	dirty  bool
	// annotator builds the platform refinement applied to live sightings. It
	// is lazy (sync.OnceValue) because building it shells out on some
	// platforms — networksetup on darwin, Get-NetAdapter on windows, 0.5–2s —
	// and cached rows must reach the consumer before any of that happens.
	// Built on the engine goroutine, on the first live sighting, so the
	// session's single-goroutine ownership of its state still holds.
	annotator func() func(*models.LANDevice)

	emissions chan MDNSService
	results   chan lanProbeResult
	retries   chan string
	sem       chan struct{}
	wg        sync.WaitGroup
	timers    []*time.Timer

	graceElapsed bool

	// pendingProbes tracks the cached identities whose initial probe has not
	// concluded yet; probesDone closes when the set empties.
	pendingProbes    map[string]bool
	probesDone       chan struct{}
	probesDoneClosed bool
}

// runLANStream drives one session. probesDone (may be nil) is closed once
// every cached entry's initial probe has concluded — Task 4's settle gate.
func runLANStream(ctx context.Context, opts StreamOptions, out chan<- LANEvent, probesDone chan struct{}) {
	s := &lanStream{
		ctx:           ctx,
		opts:          opts,
		out:           out,
		states:        make(map[string]*lanDeviceState),
		emissions:     make(chan MDNSService),
		results:       make(chan lanProbeResult),
		retries:       make(chan string, 1),
		sem:           make(chan struct{}, probeWorkers),
		pendingProbes: make(map[string]bool),
		probesDone:    probesDone,
		annotator:     sync.OnceValue(func() func(*models.LANDevice) { return newLANAnnotator(ctx) }),
	}
	defer s.finish()

	if opts.UseCache {
		s.emitCached()
	}
	s.closeProbesDoneIfIdle()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runBackend()
	}()

	grace := time.NewTimer(offlineGrace)
	defer grace.Stop()

	// The flush ticker exists only when the session persists what it learns.
	var flushC <-chan time.Time
	if opts.UseCache {
		flush := time.NewTicker(cacheFlushDelay)
		defer flush.Stop()
		flushC = flush.C
	}
	// Nil with no filter, or a filter that never changes its mind; a nil
	// channel simply never fires.
	var changedC <-chan struct{}
	if opts.Exclude != nil {
		changedC = opts.Exclude.Changed()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case svc := <-s.emissions:
			s.handleSighting(svc)
		case res := <-s.results:
			s.handleProbeResult(res)
		case <-grace.C:
			s.handleGrace()
		case key := <-s.retries:
			s.handleRetry(key)
		case <-flushC:
			s.flush()
		case <-changedC:
			s.retractExcluded()
		}
	}
}

// excluded asks the consumer's filter, when there is one, whether dev must
// stay out of the session.
func (s *lanStream) excluded(dev models.LANDevice) bool {
	return s.opts.Exclude != nil && s.opts.Exclude.Exclude(dev)
}

// retract takes back a listed identity the filter now rejects: its row, its
// cache entry and its place in the settle gate all go. Every identity in
// states has been emitted at least once, so the consumer always has a row to
// drop.
func (s *lanStream) retract(key string) {
	st, known := s.states[key]
	if !known {
		return
	}
	delete(s.states, key)
	if s.cache != nil {
		s.cache.Delete(key)
		s.dirty = true
	}
	// A probe still in flight lost its target: its result arrives under a key
	// nobody holds and handleProbeResult discards it. Retired from the settle
	// gate here as well, so a retraction between scheduling and result cannot
	// leave a batch scan waiting on it.
	s.probeConcluded(key)
	s.emit(LANEvent{Kind: LANRetracted, Device: st.dev})
}

// retractExcluded re-checks every listed identity after the filter reported a
// change of mind. Deleting from a map mid-range is safe in Go.
func (s *lanStream) retractExcluded() {
	for key, st := range s.states {
		if s.excluded(st.dev) {
			s.retract(key)
		}
	}
}

// finish tears the session down: pending retry timers are stopped, in-flight
// probes are awaited (they all abort on ctx, and waiting keeps the seam vars
// stable for the next session), and anything learned is persisted once.
func (s *lanStream) finish() {
	for _, t := range s.timers {
		t.Stop()
	}
	s.wg.Wait()
	s.flush()
	s.pendingProbes = nil
	s.closeProbesDoneIfIdle()
}

// emitCached loads the device cache and replays every fresh entry, scheduling
// each one's verification probe. A cache that cannot be loaded is treated as
// an empty one: the cache is an accelerator and must never fail a scan.
func (s *lanStream) emitCached() {
	cache, err := cacheLoadFn()
	if err != nil || cache == nil {
		return
	}
	s.cache = cache

	now := time.Now()
	for _, entry := range cache.Fresh(now) {
		key := discoverycache.Key(entry.ID, entry.DisplayName)
		// An identity-less entry (written by a build that emitted hostname-less
		// sightings) is un-dialable and collapses every such device onto the
		// same key. Skip it; the next save drops it.
		if key == "" {
			continue
		}
		if _, dup := s.states[key]; dup {
			continue
		}
		if s.excluded(entry.Device()) {
			// A leftover from before the consumer knew to exclude it (a VM
			// cached by an older build). Forgotten now, or it would re-seed
			// every session for the rest of its TTL.
			s.cache.Delete(key)
			s.dirty = true
			continue
		}
		st := &lanDeviceState{dev: entry.Device(), fromCache: true, persisted: entry.LastSeen}
		s.states[key] = st
		s.emit(LANEvent{Kind: LANCached, Device: st.dev})
		if s.opts.Prober != nil {
			s.pendingProbes[key] = true
			s.scheduleProbe(key, st)
		}
	}
}

// runBackend keeps the platform mDNS stream alive for the session. A non-nil
// error while ctx is still live (mDNSResponder restart, D-Bus disconnect) is
// retried with a fixed delay; after the last attempt the session keeps
// serving probe and timer events with whatever it already knows.
func (s *lanStream) runBackend() {
	emit := func(svc MDNSService) {
		select {
		case s.emissions <- svc:
		case <-s.ctx.Done():
		}
	}
	for attempt := 0; ; attempt++ {
		err := lanBackendFn(s.ctx, wendyServiceType, emit)
		if err == nil || s.ctx.Err() != nil {
			return
		}
		if attempt >= backendRetries {
			log.Printf("discovery: LAN stream backend stopped: %v", err)
			return
		}
		select {
		case <-time.After(backendRetryDelay):
		case <-s.ctx.Done():
			return
		}
	}
}

// handleSighting folds one live mDNS answer into the session.
func (s *lanStream) handleSighting(svc MDNSService) {
	dev := lanDeviceFromService(svc)
	key := discoverycache.Key(dev.ID, dev.DisplayName)
	if key == "" {
		// Neither a TXT device id, a display name, nor a hostname: there is no
		// identity to key a row (or a cache entry) by, and every such sighting
		// would collapse onto the same empty key as a nameless, un-dialable
		// row. Drop it rather than emit or persist it.
		return
	}
	if s.excluded(dev) {
		// Never a row: not listed, not probed, not persisted. Checked before
		// the annotator, whose first use may shell out. If this identity was
		// listed earlier (the filter has learned something since), that row
		// goes too.
		s.retract(key)
		// A prior connect or no-ID advertisement may have listed this same
		// host under a hostname-derived key. Retire that row too, without
		// migrating it onto the excluded identity or probing it again.
		if host := dev.HostKey(); host != "" && !hostDerivedIdentity(dev) {
			for oldKey, st := range s.states {
				if hostDerivedIdentity(st.dev) && st.dev.HostKey() == host {
					s.retract(oldKey)
				}
			}
		}
		return
	}
	s.annotate(&dev)
	now := time.Now()

	superseded := s.supersedeHostDerived(key, dev)

	st, known := s.states[key]
	if !known {
		st = &lanDeviceState{dev: dev, confirmed: true}
		s.states[key] = st
		s.persist(st, now)
		s.emit(LANEvent{Kind: LANFound, Device: st.dev, Supersedes: superseded})
		s.scheduleProbe(key, st)
		return
	}

	updated := applySighting(st.dev, dev)
	if !st.probeFailed {
		// Multi-homed churn guard: while the address the session already holds
		// still looks good, a re-sighting may refine it but never downgrade it.
		// A failed probe drops the guard — a target nothing answers at has
		// nothing left to protect.
		updated = preferStableTarget(st.dev, updated)
	}
	targetMoved := probeTargetChanged(st.dev, updated)
	changed := mdnsFieldsChanged(st.dev, updated)
	returning := !st.confirmed
	st.dev = updated
	if targetMoved {
		// Nothing has verified this address yet, so the row must stop claiming
		// probe-confirmed data until the retargeted probe below answers.
		st.probeConfirmed = false
	}

	switch {
	case returning:
		// A cached (or offlined) row just proved it is on the network.
		st.confirmed = true
		st.offline = false
		s.persist(st, now)
		s.emitConfirmation(LANEvent{Kind: LANFound, Device: updated, Probed: st.probeConfirmed, Supersedes: superseded}, st)
	case changed || superseded != "":
		s.persist(st, now)
		s.emitConfirmation(LANEvent{Kind: LANUpdated, Device: updated, Probed: st.probeConfirmed, Supersedes: superseded}, st)
	default:
		// An unchanged re-announcement: no event, and no cache write beyond
		// the occasional LastSeen refresh, so a long-lived session does not
		// flush the cache file on every sweep.
		if now.Sub(st.persisted) >= cacheRefreshInterval {
			s.persist(st, now)
		}
	}

	switch {
	case targetMoved || superseded != "":
		// Retarget: a probe aimed at the old address (or booked under the
		// superseded identity) can no longer speak for this device — whether
		// it is still in flight (its result is discarded by generation),
		// failed there, or succeeded there.
		s.scheduleProbe(key, st)
	case st.probing || !st.probeFailed:
		// Verified, or a verification is already under way.
	case returning || now.Sub(st.probeEnded) >= probeRetryInterval:
		// The device is announcing itself but its agent did not answer last
		// time (mid-boot, agent restarting). Re-probing off the re-sighting
		// keeps that recovery event-driven on every platform.
		s.scheduleProbe(key, st)
	}
}

// supersedeHostDerived retires an already-known identity that the sighting
// under key has just proven to be the same physical device: a connect-minted
// cache row (cacheConnectSuccess mints ID == DisplayName == hostname when it
// finds no existing entry) that the device's real TXT device id now replaces.
// The old row's probe bookkeeping migrates onto the new key, its cache entry
// is deleted, and the returned old key travels on the next event so consumers
// keyed by cache identity drop the stale row. "" when nothing was superseded.
func (s *lanStream) supersedeHostDerived(key string, dev models.LANDevice) string {
	host := dev.HostKey()
	if host == "" || hostDerivedIdentity(dev) {
		return "" // nothing better than a hostname to supersede *with*
	}
	for oldKey, st := range s.states {
		if oldKey == key || !hostDerivedIdentity(st.dev) || st.dev.HostKey() != host {
			continue
		}
		delete(s.states, oldKey)
		if s.cache != nil {
			s.cache.Delete(oldKey)
			s.dirty = true
		}
		if _, exists := s.states[key]; !exists {
			// Any probe still in flight was booked under oldKey and can no
			// longer be delivered (handleProbeResult retires it), so this
			// state is no longer probing; handleSighting schedules a fresh
			// probe under the new key.
			st.probing = false
			st.probeGen++
			s.states[key] = st
		}
		return oldKey
	}
	return ""
}

// hostDerivedIdentity reports whether dev's identity was minted from its
// hostname rather than read from a wendyosdevice/id TXT record — the shape a
// connect-success cache write leaves behind, and the shape a device that
// advertises no id TXT record resolves to.
func hostDerivedIdentity(dev models.LANDevice) bool {
	host := dev.HostKey()
	return host != "" && strings.EqualFold(dev.ID, host) && strings.EqualFold(dev.DisplayName, host)
}

// emitConfirmation emits an event that (re)states what a surface should render
// for a device, clearing the "already reported as probe-failed" latch so a
// probe that fails again after this is reported again.
func (s *lanStream) emitConfirmation(ev LANEvent, st *lanDeviceState) {
	st.reportedFailure = false
	s.emit(ev)
}

// annotate applies the platform refinement to a live sighting, building the
// annotator on first use (see lanStream.annotator).
func (s *lanStream) annotate(dev *models.LANDevice) {
	s.annotator()(dev)
}

// handleProbeResult folds one prober outcome into the session.
func (s *lanStream) handleProbeResult(res lanProbeResult) {
	st, known := s.states[res.key]
	if !known {
		// The identity is gone (superseded by the device's real TXT id), so
		// nothing will ever answer under this key: retire it from the settle
		// gate or a batch scan would wait out its whole timeout.
		s.probeConcluded(res.key)
		return
	}
	if res.gen != st.probeGen {
		// Stale: the probe target moved and a fresh probe is already running.
		return
	}
	st.probing = false
	st.probeEnded = time.Now()
	defer s.probeConcluded(res.key)

	if res.err != nil {
		st.probeFailed = true
		st.probeConfirmed = false
		switch {
		case st.fromCache && !st.confirmed:
			// Cached and never seen live: the grace window owns this row.
			if s.graceElapsed {
				s.markOffline(res.key, st)
			}
		case st.confirmed && !st.reportedFailure:
			// The device is on the network but its agent did not answer: say
			// so once, so a surface stops spinning on "verifying" and can show
			// the no-access hint. Repeat failures stay silent until something
			// else re-confirms the row.
			st.reportedFailure = true
			s.emit(LANEvent{Kind: LANUpdated, Device: st.dev, ProbeFailed: true})
		}
		return
	}

	st.probeFailed = false
	st.probeConfirmed = true
	st.offline = false
	st.dev = applyProbe(st.dev, res.dev)
	if s.excluded(st.dev) {
		// The probe is what revealed it -- an agent that reports a VM board
		// but advertises no devicetype record -- so the row shown on the
		// sighting is taken back here.
		s.retract(res.key)
		return
	}
	s.persist(st, time.Now())

	kind := LANUpdated
	if !st.confirmed {
		kind = LANFound
		st.confirmed = true
	}
	s.emitConfirmation(LANEvent{Kind: kind, Device: st.dev, Probed: true}, st)
}

// handleGrace runs once, offlineGrace after session start: every cached row
// that neither answered its probe nor showed up on the network is offline.
func (s *lanStream) handleGrace() {
	s.graceElapsed = true
	for key, st := range s.states {
		if st.fromCache && !st.confirmed && st.probeFailed {
			s.markOffline(key, st)
		}
	}
}

// markOffline emits the offline marker for a cached row and arms its single
// re-probe. The row stays listed and selectable.
func (s *lanStream) markOffline(key string, st *lanDeviceState) {
	if st.offline {
		return
	}
	st.offline = true
	s.emit(LANEvent{Kind: LANOffline, Device: st.dev})

	if st.retryScheduled || s.opts.Prober == nil {
		return
	}
	st.retryScheduled = true
	s.timers = append(s.timers, time.AfterFunc(offlineRetryDelay, func() {
		select {
		case s.retries <- key:
		case <-s.ctx.Done():
		}
	}))
}

// handleRetry runs the one post-offline re-probe (devices mid-boot).
func (s *lanStream) handleRetry(key string) {
	st, known := s.states[key]
	if !known || st.confirmed || st.probing {
		return
	}
	s.scheduleProbe(key, st)
}

// scheduleProbe verifies a device against its current address on the probe
// pool. The result is delivered to the engine loop, never applied here.
func (s *lanStream) scheduleProbe(key string, st *lanDeviceState) {
	prober := s.opts.Prober
	if prober == nil {
		return
	}
	st.probeGen++
	st.probing = true
	gen, dev := st.probeGen, st.dev

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		select {
		case s.sem <- struct{}{}:
		case <-s.ctx.Done():
			return
		}
		defer func() { <-s.sem }()

		// Marked so the prober's dial path cannot fall back to another mDNS
		// browse, which would start a fresh discovery session and probe from
		// there — see WithinProbe.
		probeCtx, cancel := context.WithTimeout(WithinProbe(s.ctx), probeTimeout)
		defer cancel()
		probed, err := prober(probeCtx, dev)

		select {
		case s.results <- lanProbeResult{key: key, gen: gen, dev: probed, err: err}:
		case <-s.ctx.Done():
		}
	}()
}

// probeConcluded retires a cached identity's initial probe from the settle
// gate. Re-probes and probes of newly discovered devices are not tracked:
// the gate is only about the cache rows the session started with.
func (s *lanStream) probeConcluded(key string) {
	if !s.pendingProbes[key] {
		return
	}
	delete(s.pendingProbes, key)
	s.closeProbesDoneIfIdle()
}

func (s *lanStream) closeProbesDoneIfIdle() {
	if s.probesDone == nil || s.probesDoneClosed || len(s.pendingProbes) > 0 {
		return
	}
	close(s.probesDone)
	s.probesDoneClosed = true
}

// emit hands one event to the consumer, abandoning it if the session is over.
func (s *lanStream) emit(ev LANEvent) {
	select {
	case s.out <- ev:
	case <-s.ctx.Done():
	}
}

// persist records a device in the cache; the write itself is debounced by the
// flush ticker. A session without a cache keeps everything in memory.
//
// Replace, not Upsert: dev is the session's complete current view of the
// device (a live sighting's fields plus whatever a probe confirmed), so
// Upsert's non-zero-wins merge would only ever resurrect a value the device
// has since dropped — an mTLS flag or an orgid TXT record that went away.
func (s *lanStream) persist(st *lanDeviceState, now time.Time) {
	if s.cache == nil {
		return
	}
	st.persisted = now
	s.cache.Replace(discoverycache.EntryFromDevice(st.dev), now)
	s.dirty = true
}

// flush persists what the session has learned, at most once per
// cacheFlushDelay plus once at session end.
func (s *lanStream) flush() {
	if s.cache == nil || !s.dirty {
		return
	}
	// Cleared up front: a failing write must not retry on every tick.
	s.dirty = false
	if err := s.cache.Flush(time.Now()); err != nil {
		log.Printf("discovery: persisting device cache: %v", err)
	}
}

// applySighting folds a live mDNS answer into what the session already knows.
// The answer is authoritative for everything mDNS carries — identity, address,
// and TXT-derived metadata — so a record the device stopped advertising (tls,
// orgid, name) is actually cleared rather than kept alive by a stale value.
// Only the fields no mDNS answer can speak for, the ones an agent probe fills
// in, survive from the previous view: a re-announcement must not blank a
// probed agent version until the next probe replaces it.
func applySighting(stored, sighted models.LANDevice) models.LANDevice {
	dev := sighted
	dev.AgentVersion = stored.AgentVersion
	// The advertisement's device type fills in only what nothing better has
	// supplied: a probe-verified (or earlier-cached) type is never downgraded
	// by a later announcement.
	if stored.DeviceType != "" {
		dev.DeviceType = stored.DeviceType
	}
	dev.OS = stored.OS
	dev.OSVersion = stored.OSVersion
	dev.CPUArchitecture = stored.CPUArchitecture
	return dev
}

// applyProbe folds an agent probe's answer into what the session already knows.
// The probe owns the version fields it read from the agent and the mTLS mode it
// actually negotiated (per the LANProber contract, that beats the tls TXT
// record); the address and mDNS metadata stay as the last sighting left them.
func applyProbe(stored, probed models.LANDevice) models.LANDevice {
	dev := stored
	dev.IsMTLS = probed.IsMTLS
	dev.AgentVersion = probed.AgentVersion
	dev.DeviceType = probed.DeviceType
	dev.OS = probed.OS
	dev.OSVersion = probed.OSVersion
	dev.CPUArchitecture = probed.CPUArchitecture
	return dev
}

// preferStableTarget keeps the dial target and interface the session already
// holds whenever a re-sighting would only make them worse. Every hashicorp
// platform re-announces each device once per interface per sweep (and Windows
// adds an interface-less default sweep on top), so without this a multi-homed
// device oscillates: NetworkInterface flips to "" and back, the probe target
// swings between a USB-C gadget link and Wi-Fi, and each swing clears
// probeConfirmed — an endless OK↔spinner flicker plus a cache write per sweep.
//
// Only downgrades are refused, so a device that genuinely moves still
// retargets (an equally-good address always wins): the preferences are the
// ones the pre-stream batch path applied when it collapsed per-interface
// duplicates — a USB gadget link over anything else, IPv4 over IPv6,
// routable over link-local, and a known value over a blank one.
func preferStableTarget(stored, sighted models.LANDevice) models.LANDevice {
	dev := sighted
	if (stored.USB != "" && sighted.USB == "") || addressDowngrade(stored.IPAddress, sighted.IPAddress) {
		dev.IPAddress, dev.Hostname, dev.Port = stored.IPAddress, stored.Hostname, stored.Port
		dev.NetworkInterface, dev.USB = stored.NetworkInterface, stored.USB
		return dev
	}
	if dev.Hostname == "" {
		dev.Hostname = stored.Hostname
	}
	if dev.Port == 0 {
		dev.Port = stored.Port
	}
	if dev.NetworkInterface == "" {
		dev.NetworkInterface, dev.USB = stored.NetworkInterface, stored.USB
	}
	return dev
}

// addressDowngrade reports whether candidate is a strictly worse dial target
// than the stored address: blank where one was known, IPv6 for a device
// already answering on IPv4 (an IPv6 set typically leads with an RFC 4941
// temporary address that rotates away), or link-local where a routable
// address was known.
func addressDowngrade(stored, candidate string) bool {
	if stored == "" {
		return false
	}
	if candidate == "" {
		return true
	}
	if isIPv4LANAddress(stored) && !isIPv4LANAddress(candidate) {
		return true
	}
	return isRoutableLANAddress(stored) && !isRoutableLANAddress(candidate)
}

// isIPv4LANAddress reports whether addr (optionally "%zone"-suffixed) is an
// IPv4 or IPv4-mapped address.
func isIPv4LANAddress(addr string) bool {
	a, err := netip.ParseAddr(stripZone(addr))
	return err == nil && (a.Is4() || a.Is4In6())
}

// isRoutableLANAddress reports whether addr is a directly dialable address —
// all IPv4 (including 169.254.0.0/16 link-local) or non-link-local IPv6 — as
// opposed to an IPv6 link-local unicast address (fe80::/10), which needs a
// zone id and is a poor default dial target. An empty or unparseable address
// is treated as non-routable.
func isRoutableLANAddress(addr string) bool {
	a, err := netip.ParseAddr(stripZone(addr))
	if err != nil {
		return false
	}
	return a.Is4() || a.Is4In6() || !a.IsLinkLocalUnicast()
}

func stripZone(addr string) string {
	if i := strings.IndexByte(addr, '%'); i >= 0 {
		return addr[:i]
	}
	return addr
}

// mdnsFieldsChanged reports whether anything a live sighting carries (address
// or TXT-derived metadata) differs between two views of the same device.
func mdnsFieldsChanged(old, updated models.LANDevice) bool {
	return probeTargetChanged(old, updated) ||
		old.DisplayName != updated.DisplayName ||
		old.IsMTLS != updated.IsMTLS ||
		old.AssetID != updated.AssetID ||
		old.OrgID != updated.OrgID ||
		old.MeshName != updated.MeshName ||
		old.DeviceType != updated.DeviceType ||
		old.NetworkInterface != updated.NetworkInterface
}

// probeTargetChanged reports whether a probe would now dial somewhere else.
func probeTargetChanged(old, updated models.LANDevice) bool {
	return old.IPAddress != updated.IPAddress ||
		old.Port != updated.Port ||
		old.Hostname != updated.Hostname
}
