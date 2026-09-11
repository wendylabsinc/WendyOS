package ipcam

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
)

// ErrLoopbackUnavailable is returned when the running build has no compatible
// v4l2loopback module. Its text is user-facing: EnsureNodes swallows it into
// a one-time log so the rest of the agent degrades gracefully, but anything
// that surfaces it to a person (a CLI command, a status field) should show
// this text as-is rather than paraphrasing it.
var ErrLoopbackUnavailable = errors.New("running build lacks compatible v4l2loopback 0.15.x support")

// PumpFunc starts the in-process GStreamer helper that copies an RTSP
// camera's stream into its v4l2loopback node. Per branch-C-preamble.md, this
// is `wendy-agent utils ipcam-gstreamer` invoked through the purego helper
// (services/ipcam_gstreamer_process.go), not a shelled-out gst-launch-1.0, so
// camera credentials never touch argv.
//
// Task C2 only carries this through the constructor so its signature does not
// have to change later; Task C3 wires the pump and refcounting on top of it.
type PumpFunc func(ctx context.Context, args []string) error

// clock abstracts time.Now and time.After so the pump supervisor's backoff
// waits, idle-grace timer, and stable-run measurement are deterministic in
// tests instead of requiring real sleeps of up to a minute. Registry.now is
// the same idea applied to a single timestamp; this generalizes it to
// waiting, which the supervision loop needs and the registry does not.
type clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

const (
	// pumpIdleGrace is how long a pump keeps running after its last consumer
	// releases it (view ref drops to zero, or an entitled container stops)
	// before it is actually stopped. This absorbs a container restart or a
	// viewer window closing and reopening without paying the RTSP connect
	// cost again. A new consumer arriving within the grace period cancels the
	// pending stop rather than the pump ever going down.
	pumpIdleGrace = 10 * time.Second

	// pumpBackoffBase and pumpBackoffCap bound the delay between pump restart
	// attempts: 1s, 2s, 4s, ... doubling up to the 30s cap. The pump never
	// gives up retrying — a network camera coming back after a power cycle or
	// an address change is exactly the case this exists to recover from.
	pumpBackoffBase = 1 * time.Second
	pumpBackoffCap  = 30 * time.Second

	// pumpStableRun is how long a pump attempt must have been running before
	// its eventual failure is treated as a fresh problem rather than a
	// continuation of a crash loop: the backoff level resets to base after an
	// attempt that ran at least this long.
	pumpStableRun = 1 * time.Minute

	// removeCameraGrace bounds how long RemoveCamera waits for a running
	// pump to notice its supervisor was canceled and actually exit before
	// the node is removed anyway. It exists so a pump that hangs tearing
	// down cannot turn ForgetCamera into an unbounded RPC: removeNode is
	// always attempted afterward regardless of whether the wait succeeded or
	// timed out, and the ioctl's own EBUSY-on-a-held-open-node behavior —
	// logged, not retried — is the backstop for the timeout case.
	removeCameraGrace = 5 * time.Second
)

// pumpBackoffDelay returns the wait before a pump's level'th restart attempt
// (level counts restarts since the last reset), doubling from
// pumpBackoffBase and clamping at pumpBackoffCap:
//
//	level: 0   1    2    3    4     5     6+
//	delay: 0   1s   2s   4s   8s    16s   30s
//
// Level 0 — the very first attempt — starts immediately.
func pumpBackoffDelay(level int) time.Duration {
	if level <= 0 {
		return 0
	}
	d := pumpBackoffBase
	for i := 1; i < level; i++ {
		if d >= pumpBackoffCap {
			return pumpBackoffCap
		}
		d *= 2
	}
	if d > pumpBackoffCap {
		return pumpBackoffCap
	}
	return d
}

// camPump is the supervision state for one camera's demanded pump.
//
// It exists in Loopback.pumps only while a supervisor goroutine is running or
// an idle-grace timer is pending for that camera; reconcile is the only code
// that creates or removes entries.
type camPump struct {
	// cancel stops the supervisor goroutine: it is called to bounce a running
	// pump (CredentialsChanged), to actually stop one once idle grace expires,
	// and (transitively, since it derives from Loopback.ctx) by Shutdown.
	cancel context.CancelFunc

	// idleCancel is non-nil exactly while a pending idle-grace timer exists
	// for this camera (demand dropped to zero but the pump has not yet been
	// stopped). Canceling it — done by reconcile when demand returns before
	// the grace period elapses — prevents that timer from stopping the pump.
	idleCancel context.CancelFunc

	// idleGen is bumped each time reconcile arms a new idle-grace timer for
	// this camPump. presence checks alone (l.pumps[camID]==cp,
	// cp.idleCancel!=nil) cannot tell one arming episode from the next: a
	// drop→reacquire→drop sequence reuses the same cp, so a stale
	// awaitIdleGrace goroutine from the first drop and the current one from
	// the second both see a non-nil idleCancel and the same cp. Each
	// goroutine captures the generation it was armed with and compares it
	// here before acting, so only the most recent arming can ever stop the
	// pump.
	idleGen uint64

	// done is closed by finishSupervisor once this camPump's supervisor
	// goroutine has fully exited — meaning its current pump attempt, if any,
	// has already returned. RemoveCamera waits on it (bounded by
	// removeCameraGrace) before removing the camera's node, so removeNode is
	// never attempted while the pump's v4l2sink might still hold it open.
	done chan struct{}
}

// loopbackDeps seams every syscall and subprocess the loopback node manager
// touches, so the whole package builds and its tests run on macOS. Linux's
// real implementations live in loopback_linux.go; the non-Linux stubs that
// make Available degrade cleanly live in loopback_other.go.
type loopbackDeps struct {
	statControl func() error // stat /dev/v4l2loopback

	// modprobe loads the module. The ABI's "try devices=0/exclusive_caps=1,
	// retry a plain load on param rejection" policy is internal to a single
	// call here, so callers (and their tests) only ever see one attempt.
	modprobe func(ctx context.Context) error

	addNode    func(nr int, label string) error
	removeNode func(nr int) error
	nodeExists func(nr int) bool // stat /dev/video<nr>
}

// Loopback manages the v4l2loopback nodes that back container-visible camera
// streams: one output device per registered network camera, numbered by the
// camera's own ID so the node a user learns about never changes.
//
// It degrades rather than fails: a WendyOS build without the kernel module
// (or a macOS dev build, which never has it) still serves `camera view`
// directly from the camera — only container mirroring is unavailable, and
// Available reports that with ErrLoopbackUnavailable instead of panicking or
// wedging the agent.
type Loopback struct {
	ctx    context.Context
	cancel context.CancelFunc // stops ctx and everything derived from it; Shutdown's first step
	logger *zap.Logger
	reg    *Registry
	creds  *CredentialStore
	pump   PumpFunc

	deps  loopbackDeps
	clock clock

	detectMu sync.Mutex
	detected bool
	warnOnce sync.Once

	mu sync.Mutex
	// containerOwners is the current set of running entitled container names,
	// replaced wholesale by SetContainerConsumers. Only its size matters for
	// demand — the camera entitlement is all-cameras (spec :282-284) — but the
	// names are kept so a caller diffing its own state has something to log.
	containerOwners map[string]struct{}
	// viewRefs counts active camera-view consumers (AcquireView) per camera.
	viewRefs map[uint32]int
	// pumps holds supervision state for every camera with a running
	// supervisor goroutine or a pending idle-grace timer. reconcile is the
	// only code that adds or removes entries.
	pumps map[uint32]*camPump
	// wg tracks every supervisor and idle-grace goroutine, so Shutdown can
	// wait for all of them to actually exit rather than just signaling them.
	wg sync.WaitGroup
	// shuttingDown is set (under mu) before Shutdown cancels ctx or waits on
	// wg. Go's sync.WaitGroup documents concurrent Add(positive)/Wait as
	// misuse that can panic — a data race -race cannot catch, since it is a
	// contract violation rather than an unsynchronized memory access — so
	// every wg.Add call in reconcile happens inside the same mu-guarded
	// section that checks this flag, guaranteeing no Add can start after
	// Shutdown has begun (any Add either fully happens-before shuttingDown is
	// set, or sees it set and declines).
	shuttingDown bool
}

// NewLoopback returns a node manager. Detection of the v4l2loopback module is
// deferred to the first call that needs it (Available, EnsureNodes, ...): a
// device without the module still constructs a Loopback cleanly, since the
// whole point of this package is that the module's absence is not a startup
// failure.
func NewLoopback(ctx context.Context, logger *zap.Logger, reg *Registry, creds *CredentialStore, pump PumpFunc) *Loopback {
	ctx, cancel := context.WithCancel(ctx)
	return &Loopback{
		ctx:             ctx,
		cancel:          cancel,
		logger:          logger,
		reg:             reg,
		creds:           creds,
		pump:            pump,
		deps:            defaultLoopbackDeps(),
		clock:           realClock{},
		containerOwners: make(map[string]struct{}),
		viewRefs:        make(map[uint32]int),
		pumps:           make(map[uint32]*camPump),
	}
}

// Available reports whether the v4l2loopback module is usable, attempting to
// load it if it is not already present. Success is cached; failure is not, so
// installing or loading the module can recover without restarting the agent.
// A non-nil error always wraps ErrLoopbackUnavailable.
func (l *Loopback) Available() error {
	l.detectMu.Lock()
	defer l.detectMu.Unlock()
	if l.detected {
		return nil
	}
	if err := l.detect(); err != nil {
		return err
	}
	l.detected = true
	return nil
}

// detect runs one module-detection attempt while detectMu is held: stat the
// control device; if absent, try to load the module; re-stat; if it is still
// absent, degrade for this attempt and log the condition once. See
// loopback_linux.go's modprobe implementation for the params-then-plain
// fallback retry. From here each detection attempt makes one seam call; a
// later Available call may retry after the module has been installed.
func (l *Loopback) detect() error {
	if err := l.deps.statControl(); err == nil {
		return nil
	}

	if err := l.deps.modprobe(l.ctx); err != nil {
		l.warnUnavailable(err)
		return fmt.Errorf("%w (modprobe: %v)", ErrLoopbackUnavailable, err)
	}
	if err := l.deps.statControl(); err != nil {
		l.warnUnavailable(err)
		return fmt.Errorf("%w (control device still absent after modprobe: %v)", ErrLoopbackUnavailable, err)
	}

	// A plain-fallback load (see loopback_linux.go) does not honor devices=0, so
	// it can auto-create loopback nodes at the kernel's own low numbers before
	// our reserved camera-ID band. Those never belong to a camera; sweep them so
	// they can never collide with — or be mistaken for — one. A devices=0 load
	// creates nothing, so this is a no-op then.
	l.sweepAutoCreatedNodes()
	return nil
}

// sweepAutoCreatedNodes removes any loopback device numbered below Wendy's
// complete virtual-camera band. ROS 2 cameras occupy 128-199 and IP cameras
// occupy 200-255, so neither kind may be swept as an auto-created node.
func (l *Loopback) sweepAutoCreatedNodes() {
	for nr := 0; nr < LoopbackBandStart; nr++ {
		if !l.deps.nodeExists(nr) {
			continue
		}
		if err := l.deps.removeNode(nr); err != nil {
			l.logger.Warn("removing auto-created v4l2loopback node below the reserved camera-ID band",
				zap.Int("nr", nr), zap.Error(err))
		}
	}
}

// warnUnavailable logs the degradation once even though Available permits
// later detection attempts after an operator installs or loads the module.
func (l *Loopback) warnUnavailable(cause error) {
	l.warnOnce.Do(func() {
		l.logger.Warn("compatible v4l2loopback module unavailable; virtual camera streams are disabled",
			zap.Error(cause))
	})
}

// EnsureNodes creates a v4l2loopback node for every registered camera that
// does not already have one. It is idempotent — nodes that already exist are
// left alone — and it never fails the caller over a missing module: if the
// module is unavailable it returns nil, having already logged that once via
// Available.
func (l *Loopback) EnsureNodes(ctx context.Context) error {
	if err := l.Available(); err != nil {
		return nil
	}
	for _, cam := range l.reg.List() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := l.EnsureNode(ctx, cam.ID, fmt.Sprintf("Wendy IP camera %d", cam.ID)); err != nil {
			l.logger.Warn("creating v4l2loopback node", zap.Uint32("cameraId", cam.ID), zap.Error(err))
		}
	}
	return nil
}

// EnsureNode creates one Wendy-managed v4l2loopback device. It is shared by
// the IP-camera supervisor and the ROS 2 camera bridge so module detection and
// the control-device ABI have one owner.
func (l *Loopback) EnsureNode(ctx context.Context, id uint32, label string) error {
	if id < LoopbackBandStart || id > IDBandEnd {
		return fmt.Errorf("camera ID %d is outside Wendy's loopback band", id)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := l.Available(); err != nil {
		return err
	}
	nr := int(id)
	if l.deps.nodeExists(nr) {
		return nil
	}
	return l.deps.addNode(nr, label)
}

// NodePath returns the loopback device path for a camera and whether it
// currently exists. It always reflects live state, never a cache, so a node
// removed out from under the agent — or never created because the module is
// unavailable — correctly reports false.
func (l *Loopback) NodePath(camID uint32) (string, bool) {
	nr := int(camID)
	if !l.deps.nodeExists(nr) {
		return "", false
	}
	return fmt.Sprintf("/dev/video%d", nr), true
}

// RemoveCamera stops a camera's pump — if one is running, or about to be (a
// pending idle-grace timer) — and waits, bounded by removeCameraGrace, for
// it to actually exit before deleting the camera's v4l2loopback node. If
// nothing was running for this camera, or the module is unavailable, it goes
// straight to removeNode exactly as before.
//
// The ordering matters for two reasons. First, a still-running pump keeps an
// already-authenticated RTSP session alive on credentials the caller may
// have just deleted (see VideoService.ForgetCamera, which deletes
// credentials and forgets the camera from the registry before calling this —
// both of which make superviseCam's own re-resolution exit the supervisor on
// its next attempt regardless, but that is not a substitute for actively
// canceling the one already in flight). Second, V4L2LOOPBACK_CTL_REMOVE
// returns EBUSY while the pump's v4l2sink still holds the node open, which
// would leave the node orphaned: a forgotten camera drops out of
// reg.List(), so nothing ever retries removal for it again short of a
// reboot.
//
// It is best-effort and has no error to report, matching its existing
// contract: a camera being forgotten should not fail because its node was
// already gone, or because its pump outlived removeCameraGrace — removeNode
// is attempted regardless (logged if it fails, EBUSY-shaped or otherwise),
// and the underlying removeNode already treats "no such node" as success.
// There is deliberately no retry loop around removeNode beyond that single
// post-wait attempt: removeCameraGrace is the one wait this affords, so a
// pump that is still somehow holding the node past it becomes a logged
// anomaly rather than an unbounded RPC.
func (l *Loopback) RemoveCamera(camID uint32) {
	l.mu.Lock()
	cp, running := l.pumps[camID]
	var supCancel context.CancelFunc
	if running {
		supCancel = cp.cancel
		// Same bounce pattern CredentialsChanged uses: cancel any pending
		// idle-grace timer too, so it cannot independently act on a cp that
		// is already on its way out from under it.
		if cp.idleCancel != nil {
			cp.idleCancel()
			cp.idleCancel = nil
		}
	}
	l.mu.Unlock()

	if supCancel != nil {
		supCancel()
		select {
		case <-cp.done:
		case <-l.clock.After(removeCameraGrace):
			l.logger.Warn("camera's pump did not exit before its loopback node was removed",
				zap.Uint32("cameraId", camID), zap.Duration("waited", removeCameraGrace))
		}
	}

	if err := l.Available(); err != nil {
		return
	}
	nr := int(camID)
	if err := l.deps.removeNode(nr); err != nil {
		l.logger.Warn("removing v4l2loopback node", zap.Uint32("cameraId", camID), zap.Error(err))
	}
}

// AcquireView records a camera-view consumer for camID (`camera view`
// attaching, per the spec's "started when ... `camera view` attaches") and
// returns a release func the caller must call exactly once when it is done
// viewing. It is safe to call for a camera the loopback module cannot serve
// (module absent, camera unregistered, no credentials yet): the ref count is
// still tracked so bookkeeping stays consistent, but reconcile's own
// precondition checks mean no pump is ever started, and the returned release
// func is always safe to call.
func (l *Loopback) AcquireView(camID uint32) func() {
	l.mu.Lock()
	l.viewRefs[camID]++
	l.mu.Unlock()
	l.reconcile(camID)

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			if l.viewRefs[camID] > 0 {
				l.viewRefs[camID]--
				if l.viewRefs[camID] == 0 {
					delete(l.viewRefs, camID)
				}
			}
			l.mu.Unlock()
			l.reconcile(camID)
		})
	}
}

// SetContainerConsumers replaces the set of running entitled container names
// wholesale and re-evaluates demand for every registered camera. There is no
// acquire/release pairing to leak here — the camera entitlement is
// all-cameras (spec :282-284), so a container either counts toward demand for
// every camera or it does not — and calling this once with the current truth
// (e.g. after an agent restart) is exactly the right way to reconcile state,
// no diffing against a previous call required.
func (l *Loopback) SetContainerConsumers(names []string) {
	l.mu.Lock()
	owners := make(map[string]struct{}, len(names))
	for _, n := range names {
		owners[n] = struct{}{}
	}
	l.containerOwners = owners
	l.mu.Unlock()

	for _, cam := range l.reg.List() {
		l.reconcile(cam.ID)
	}
}

// CredentialsChanged notifies the supervisor that camID's stored credentials
// were set, updated, or deleted. A running pump is bounced — its supervisor
// is canceled so the next attempt (started by that supervisor's own exit
// reconciling, since demand is unchanged) re-resolves credentials fresh — and
// a camera that is demanded but has no running pump because it previously
// lacked credentials is started now.
func (l *Loopback) CredentialsChanged(camID uint32) {
	l.mu.Lock()
	cp, running := l.pumps[camID]
	var supCancel context.CancelFunc
	if running {
		supCancel = cp.cancel
		if cp.idleCancel != nil {
			cp.idleCancel()
			cp.idleCancel = nil
		}
	}
	l.mu.Unlock()

	if supCancel != nil {
		supCancel()
	}
	// Covers the pending case directly (no supervisor was running, so nothing
	// above will reconcile on its own). For the bounce case this is usually a
	// no-op that races the canceled supervisor's own exit-triggered reconcile
	// — harmless, since reconcile is idempotent — but costs nothing to call.
	l.reconcile(camID)
}

// Shutdown cancels every running pump and idle-grace timer and waits for
// their goroutines to actually exit. It leaves loopback nodes in place: only
// the pumps feeding them stop, not the nodes themselves, since a node
// disappearing out from under a container is a much more disruptive failure
// mode than a stream freezing.
//
// shuttingDown is set under l.mu before cancel/Wait, specifically so it can
// never observe wg's counter at zero concurrently with a reconcile call that
// is about to Add to it — see the field's doc comment.
func (l *Loopback) Shutdown() {
	l.mu.Lock()
	l.shuttingDown = true
	l.mu.Unlock()

	l.cancel()
	l.wg.Wait()
}

// reconcile recomputes demand for camID — len(containerOwners) > 0 ||
// viewRefs[camID] > 0 — and starts, stops, or leaves alone its supervisor
// accordingly. It is the sole place that starts or stops a pump: every other
// entry point (AcquireView/its release func, SetContainerConsumers,
// CredentialsChanged) and the supervisor's own self-heal after it exits all
// funnel through it, so there is exactly one place the start/stop decision is
// made — and the one place that must never call wg.Add once Shutdown has
// started, which is why both branches below check shuttingDown inside the
// same locked section that calls it.
func (l *Loopback) reconcile(camID uint32) {
	l.mu.Lock()
	if l.shuttingDown {
		l.mu.Unlock()
		return
	}
	demand := len(l.containerOwners) > 0 || l.viewRefs[camID] > 0
	cp, running := l.pumps[camID]
	if running {
		if demand {
			// Already running and still wanted: cancel any pending idle-grace
			// stop (a reacquire within the grace period) and do nothing else.
			if cp.idleCancel != nil {
				cp.idleCancel()
				cp.idleCancel = nil
			}
			l.mu.Unlock()
			return
		}
		// Demand just dropped to zero: start the idle-grace timer, unless one
		// is already pending (a second drop with no reacquire in between
		// should not restart the clock). Bumping idleGen — even though this
		// is a fresh camPump the first time, and idleCancel==nil implies no
		// previous timer's goroutine could still be running — keeps arming
		// uniform: every arming episode gets a generation strictly greater
		// than any that came before it, so a stale awaitIdleGrace from an
		// earlier drop→reacquire→drop cycle can never be mistaken for the
		// current one (see camPump.idleGen).
		if cp.idleCancel == nil {
			idleCtx, idleCancel := context.WithCancel(l.ctx)
			cp.idleCancel = idleCancel
			cp.idleGen++
			gen := cp.idleGen
			l.wg.Add(1)
			go l.awaitIdleGrace(idleCtx, camID, cp, gen)
		}
		l.mu.Unlock()
		return
	}
	l.mu.Unlock()

	if !demand || l.pump == nil {
		return
	}

	// Not running and demanded: check the pump preconditions — module
	// available, credentials stored — fresh. These all have their own
	// synchronization (Available's sync.Once, the registry's and credential
	// store's own mutexes), so this deliberately happens without l.mu held:
	// modprobe on a first-ever Available() call can be slow, and nothing here
	// should block every other camera's reconcile while it runs.
	if err := l.Available(); err != nil {
		return
	}
	cam, ok := l.reg.Get(camID)
	if !ok {
		return
	}
	if !l.creds.Has(cam.MAC) {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	// Re-check under the lock: another goroutine may have already started a
	// supervisor, demand may have dropped again, or Shutdown may have begun,
	// while the precondition checks above ran without l.mu held.
	if l.shuttingDown {
		return
	}
	if _, alreadyRunning := l.pumps[camID]; alreadyRunning {
		return
	}
	if len(l.containerOwners) == 0 && l.viewRefs[camID] == 0 {
		return
	}
	ctx, cancel := context.WithCancel(l.ctx)
	cp = &camPump{cancel: cancel, done: make(chan struct{})}
	l.pumps[camID] = cp
	l.wg.Add(1)
	go l.superviseCam(ctx, camID, cp)
}

// superviseCam is the supervision loop for one demanded camera. It re-resolves
// the camera and its credentials fresh on every attempt — an address move or
// a deleted login is picked up before the next connect, not cached from when
// the loop started — and runs the pump with exponential backoff between
// failed attempts, resetting the backoff once an attempt has stayed up for
// pumpStableRun.
//
// It exits (without restarting itself — that is finishSupervisor's job, via
// reconcile) when ctx is canceled (idle-grace expiry, a CredentialsChanged
// bounce, or Shutdown), when the camera is no longer registered, or when it
// has no stored credentials. The last two are not failures to back off and
// retry: they only change again through an explicit RemoveCamera/Forget or
// CredentialsChanged, so retrying on a timer would just spin uselessly until
// one of those happens anyway.
func (l *Loopback) superviseCam(ctx context.Context, camID uint32, cp *camPump) {
	defer l.wg.Done()

	level := 0
	loggedNodeNotReady := false
	loggedNoStreamURL := false
	for {
		if ctx.Err() != nil {
			l.finishSupervisor(camID, cp)
			return
		}

		cam, ok := l.reg.Get(camID)
		if !ok {
			l.finishSupervisor(camID, cp)
			return
		}
		cred, ok := l.creds.Get(cam.MAC)
		if !ok {
			l.finishSupervisor(camID, cp)
			return
		}

		var ran time.Duration
		devicePath, pathOK := l.NodePath(camID)
		switch {
		case !pathOK:
			// Once per supervisor lifetime, not every retry: a camera stuck
			// permanently node-less would otherwise log on every attempt of
			// an otherwise-infinite 1s..30s backoff ladder forever.
			if !loggedNodeNotReady {
				loggedNodeNotReady = true
				l.logger.Warn("camera has demand and stored credentials but no v4l2loopback node yet; pump waiting and retrying",
					zap.Uint32("cameraId", camID))
			}
		default:
			streamURL, err := StreamURL(cam, cred, StreamAuto)
			if err != nil {
				// Same once-per-lifetime discipline as the node log above: this
				// only fails for a camera with no stored address, which changes
				// through re-registration, not by retrying.
				if !loggedNoStreamURL {
					loggedNoStreamURL = true
					l.logger.Warn("cannot build a stream URL for demanded camera; pump waiting and retrying",
						zap.Uint32("cameraId", camID), zap.Error(err))
				}
				break
			}
			args := LoopbackPipelineArgs(streamURL, devicePath)
			start := l.clock.Now()
			runErr := l.pump(ctx, args)
			ran = l.clock.Now().Sub(start)
			if runErr != nil {
				l.logger.Warn("ip camera loopback pump exited",
					zap.Uint32("cameraId", camID),
					zap.String("error", RedactText(runErr.Error(), SecretsIn(args)...)),
				)
			}
		}

		if ctx.Err() != nil {
			// Stopped deliberately (idle grace, bounce, or Shutdown) rather
			// than failed: exit without treating it as a retry.
			l.finishSupervisor(camID, cp)
			return
		}

		if ran >= pumpStableRun {
			level = 0
		}
		level++

		if delay := pumpBackoffDelay(level); delay > 0 {
			select {
			case <-ctx.Done():
				l.finishSupervisor(camID, cp)
				return
			case <-l.clock.After(delay):
			}
		}
	}
}

// finishSupervisor removes cp from l.pumps, if it is still the current entry
// for camID (a fresh supervisor cannot yet exist — reconcile only starts one
// when none is registered — but the check keeps this safe even if that ever
// changes), and then re-reconciles.
//
// The re-reconcile is what makes idle-grace expiry and a credentials bounce
// self-healing: if a reacquire or a CredentialsChanged raced this
// supervisor's exit closely enough that reconcile did not see it, demand (or
// credentials) will still read correctly here, moments later, and a fresh
// supervisor starts immediately rather than staying stuck until some
// unrelated future call happens to reconcile this camera again. Calling
// reconcile unconditionally — even while Shutdown is in progress — is safe:
// reconcile's own shuttingDown check (taken under the same l.mu this delete
// used) makes that case a no-op, so there is only one place that decision is
// made.
func (l *Loopback) finishSupervisor(camID uint32, cp *camPump) {
	l.mu.Lock()
	if l.pumps[camID] == cp {
		delete(l.pumps, camID)
	}
	l.mu.Unlock()
	close(cp.done)

	l.reconcile(camID)
}

// awaitIdleGrace waits out a demanded-to-zero camera's idle grace period and
// then stops its pump, unless ctx is canceled first (a reacquire within the
// grace period, via reconcile, or Shutdown). gen is the idle-grace generation
// this goroutine was armed for (see camPump.idleGen); fireIdleGrace uses it
// to refuse to act if a later drop→reacquire→drop cycle has since armed a new
// one on the same camPump.
func (l *Loopback) awaitIdleGrace(ctx context.Context, camID uint32, cp *camPump, gen uint64) {
	defer l.wg.Done()

	select {
	case <-ctx.Done():
		return
	case <-l.clock.After(pumpIdleGrace):
	}

	l.fireIdleGrace(camID, cp, gen)
}

// fireIdleGrace is awaitIdleGrace's post-timer action, split out so it can be
// driven directly (bypassing the select, and so bypassing any dependence on
// goroutine scheduling) to test its guard in isolation.
//
// The guard checks three things, all under l.mu: that cp is still the
// current supervisor for camID (not removed, not replaced by a fresh one),
// that a pending idle-grace timer is still expected at all (idleCancel !=
// nil — a reacquire clears it), and — the fix for the case those two alone
// miss — that gen is still the current arming's generation. Presence and
// non-nilness alone cannot distinguish an earlier drop→reacquire→drop cycle's
// arming from the current one on the same camPump; idleGen can.
func (l *Loopback) fireIdleGrace(camID uint32, cp *camPump, gen uint64) {
	l.mu.Lock()
	if l.pumps[camID] != cp || cp.idleCancel == nil || cp.idleGen != gen {
		l.mu.Unlock()
		return
	}
	cp.idleCancel = nil
	supCancel := cp.cancel
	l.mu.Unlock()

	supCancel()
}

// Auxiliary nodes: v4l2loopback nodes that are NOT backed by a registered
// network camera.
//
// The two-plane camera path needs a node for a LOCAL camera, fed from the
// agent's own producer hub rather than from an RTSP pump. Everything above is
// keyed to the network camera registry (the node number IS the camera's
// registry ID), so those nodes have no registry entry to hang off. Rather than
// stand up a second node manager beside this one, they reuse the same module
// detection, the same create and remove primitives, and the same "module
// missing is not an error" posture. Only the numbering and the lifetime differ,
// and both belong to the caller.

// AllocateAuxNodeNumber returns a free v4l2loopback node number for a node that
// has no camera registry entry, or ErrBandExhausted if there is none.
//
// Numbers come from the TOP of the same reserved band the registry allocates
// from, downward, because the registry allocates the lowest free number upward.
// The two therefore only meet once the band is genuinely full, and a number
// already held by a registered camera or by an existing node is skipped
// outright, so the two allocators cannot hand out the same number.
//
// The band is capped by the kernel's VIDEO_NUM_DEVICES of 256, so there is no
// room above it to give auxiliary nodes a band of their own. Sharing with a
// skip check is the honest option; silently reusing a camera's number would
// point an app at the wrong camera's frames.
func (l *Loopback) AllocateAuxNodeNumber() (int, error) {
	taken := map[int]struct{}{}
	for _, cam := range l.reg.List() {
		taken[int(cam.ID)] = struct{}{}
	}
	for nr := IDBandEnd; nr >= IDBandStart; nr-- {
		if _, ok := taken[nr]; ok {
			continue
		}
		if l.deps.nodeExists(nr) {
			continue
		}
		return nr, nil
	}
	return 0, ErrBandExhausted
}

// EnsureAuxNode creates the node at nr if it does not already exist.
//
// Like EnsureNodes it is idempotent and treats a missing module as "nothing to
// do" rather than an error, so a build without v4l2loopback simply has no data
// plane instead of failing container start.
func (l *Loopback) EnsureAuxNode(_ context.Context, nr int, label string) error {
	if err := l.Available(); err != nil {
		return nil
	}
	if l.deps.nodeExists(nr) {
		return nil
	}
	return l.deps.addNode(nr, label)
}

// RemoveAuxNode deletes the node at nr. Unlike RemoveCamera there is no pump
// supervisor to stop first: an auxiliary node's pump is owned by whoever created
// it, and must already have been stopped before this is called.
func (l *Loopback) RemoveAuxNode(nr int) {
	if err := l.Available(); err != nil {
		return
	}
	if err := l.deps.removeNode(nr); err != nil {
		l.logger.Warn("removing auxiliary v4l2loopback node", zap.Int("nr", nr), zap.Error(err))
	}
}
