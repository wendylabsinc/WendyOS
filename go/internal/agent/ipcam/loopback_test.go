package ipcam

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// loopbackHarness wires a Loopback whose every seam is captured in memory, so
// module detection and node lifecycle are exercised with no real device and
// no ioctl — everything here runs on macOS as well as Linux.
type loopbackHarness struct {
	mu sync.Mutex

	controlExists bool
	modprobeCalls int
	modprobeErr   error

	nodes        map[int]bool // nr -> exists
	addNodeCalls []addNodeCall
	addNodeErr   error
	removeCalls  []int

	// removeSignal, if set by a test, receives the removed nr each time
	// removeNode is called — a proper synchronization primitive (rather than
	// a polling read of removeCalls) for tests that need to prove an exact
	// interleaving, e.g. "removeNode was not yet called at this point."
	removeSignal chan int
}

type addNodeCall struct {
	nr    int
	label string
}

func newLoopbackHarness() *loopbackHarness {
	return &loopbackHarness{nodes: make(map[int]bool)}
}

func (h *loopbackHarness) deps() loopbackDeps {
	return loopbackDeps{
		statControl: func() error {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.controlExists {
				return nil
			}
			return os.ErrNotExist
		},
		modprobe: func(ctx context.Context) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.modprobeCalls++
			if h.modprobeErr == nil {
				h.controlExists = true
			}
			return h.modprobeErr
		},
		addNode: func(nr int, label string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.addNodeErr != nil {
				return h.addNodeErr
			}
			h.addNodeCalls = append(h.addNodeCalls, addNodeCall{nr: nr, label: label})
			h.nodes[nr] = true
			return nil
		},
		removeNode: func(nr int) error {
			h.mu.Lock()
			h.removeCalls = append(h.removeCalls, nr)
			delete(h.nodes, nr)
			sig := h.removeSignal
			h.mu.Unlock()
			if sig != nil {
				sig <- nr
			}
			return nil
		},
		nodeExists: func(nr int) bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.nodes[nr]
		},
	}
}

func (h *loopbackHarness) modprobeCallCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.modprobeCalls
}

func (h *loopbackHarness) addNodeCallCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.addNodeCalls)
}

func (h *loopbackHarness) setNodeExists(nr int, exists bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nodes[nr] = exists
}

func newTestLoopback(t *testing.T, h *loopbackHarness) (*Loopback, *Registry) {
	t.Helper()
	reg := NewRegistry(filepath.Join(t.TempDir(), "cameras.json"))
	if err := reg.Load(); err != nil {
		t.Fatalf("registry load: %v", err)
	}
	creds := NewCredentialStore(filepath.Join(t.TempDir(), "credentials.json"))
	if err := creds.Load(); err != nil {
		t.Fatalf("credentials load: %v", err)
	}
	l := NewLoopback(context.Background(), zap.NewNop(), reg, creds, nil)
	l.deps = h.deps()
	return l, reg
}

// A control device that already exists means the module is already loaded:
// Available must report that without ever touching modprobe.
func TestLoopback_AvailableWhenControlDeviceExists(t *testing.T) {
	h := newLoopbackHarness()
	h.controlExists = true
	l, _ := newTestLoopback(t, h)

	if err := l.Available(); err != nil {
		t.Fatalf("Available() = %v, want nil", err)
	}
	if calls := h.modprobeCallCount(); calls != 0 {
		t.Fatalf("modprobe called %d times, want 0 when the control device already exists", calls)
	}
}

// A failed detection must not poison the Loopback for the life of the agent.
// Operators can install or load the compatible module and retry without
// restarting wendy-agent; a successful detection is cached afterward.
func TestLoopback_AvailableRetriesAfterModuleBecomesAvailable(t *testing.T) {
	h := newLoopbackHarness()
	h.modprobeErr = errors.New("modprobe: FATAL: Module v4l2loopback not found")
	l, _ := newTestLoopback(t, h)

	err1 := l.Available()
	if !errors.Is(err1, ErrLoopbackUnavailable) {
		t.Fatalf("Available() #1 = %v, want an error wrapping ErrLoopbackUnavailable", err1)
	}

	h.mu.Lock()
	h.modprobeErr = nil
	h.mu.Unlock()
	if err := l.Available(); err != nil {
		t.Fatalf("Available() #2 = %v after module install, want nil", err)
	}
	if err := l.Available(); err != nil {
		t.Fatalf("Available() #3 = %v, want cached success", err)
	}
	if calls := h.modprobeCallCount(); calls != 2 {
		t.Fatalf("modprobe called %d times, want one failed attempt and one successful retry", calls)
	}
}

// EnsureNodes must create a node for every registered camera that lacks one,
// and calling it again must not recreate nodes that already exist.
func TestLoopback_EnsureNodesCreatesMissingAndIsIdempotent(t *testing.T) {
	h := newLoopbackHarness()
	h.controlExists = true
	l, reg := newTestLoopback(t, h)

	cam1, err := reg.Upsert(Camera{MAC: "ec:71:db:2a:ae:7e", Address: "10.98.0.10"})
	if err != nil {
		t.Fatalf("upsert cam1: %v", err)
	}
	cam2, err := reg.Upsert(Camera{MAC: "ec:71:db:2a:ae:7f", Address: "10.98.0.11"})
	if err != nil {
		t.Fatalf("upsert cam2: %v", err)
	}

	if err := l.EnsureNodes(context.Background()); err != nil {
		t.Fatalf("EnsureNodes() #1 = %v", err)
	}
	if calls := h.addNodeCallCount(); calls != 2 {
		t.Fatalf("addNode called %d times, want 2 (one per registered camera)", calls)
	}
	wantNrs := map[int]bool{int(cam1.ID): true, int(cam2.ID): true}
	h.mu.Lock()
	for _, c := range h.addNodeCalls {
		if !wantNrs[c.nr] {
			t.Errorf("addNode called with nr=%d, which is not a registered camera ID", c.nr)
		}
	}
	h.mu.Unlock()

	if err := l.EnsureNodes(context.Background()); err != nil {
		t.Fatalf("EnsureNodes() #2 = %v", err)
	}
	if calls := h.addNodeCallCount(); calls != 2 {
		t.Fatalf("addNode called %d times total after a second EnsureNodes, want still 2 (idempotent)", calls)
	}
}

// Without the module, EnsureNodes must not fail the caller: it degrades to a
// nil error (the degradation itself was already logged once by Available),
// and it must not attempt to create any nodes.
func TestLoopback_EnsureNodesDegradesToNilErrorWithoutModule(t *testing.T) {
	h := newLoopbackHarness()
	h.modprobeErr = errors.New("modprobe: FATAL: Module v4l2loopback not found")
	l, reg := newTestLoopback(t, h)
	if _, err := reg.Upsert(Camera{MAC: "ec:71:db:2a:ae:7e", Address: "10.98.0.10"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if err := l.EnsureNodes(context.Background()); err != nil {
		t.Fatalf("EnsureNodes() = %v, want nil even though the module is unavailable", err)
	}
	if calls := h.addNodeCallCount(); calls != 0 {
		t.Fatalf("addNode called %d times, want 0 when the module is unavailable", calls)
	}
}

// NodePath must reflect live existence, not a guess from the registry: a
// camera ID with no node reports false, and one with a node reports its path.
func TestLoopback_NodePathReportsOnlyExistingNodes(t *testing.T) {
	h := newLoopbackHarness()
	h.controlExists = true
	l, _ := newTestLoopback(t, h)

	if path, ok := l.NodePath(203); ok {
		t.Fatalf("NodePath(203) = (%q, true), want false: no node was ever created", path)
	}

	h.setNodeExists(203, true)

	path, ok := l.NodePath(203)
	if !ok {
		t.Fatal("NodePath(203) reported false for a node that exists")
	}
	if path != "/dev/video203" {
		t.Fatalf("NodePath(203) = %q, want /dev/video203", path)
	}
}

// RemoveCamera must issue the REMOVE ioctl for the camera's node and leave
// NodePath reporting it gone afterward.
func TestLoopback_RemoveCameraRemovesNode(t *testing.T) {
	h := newLoopbackHarness()
	h.controlExists = true
	l, reg := newTestLoopback(t, h)
	cam, err := reg.Upsert(Camera{MAC: "ec:71:db:2a:ae:7e", Address: "10.98.0.10"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := l.EnsureNodes(context.Background()); err != nil {
		t.Fatalf("EnsureNodes() = %v", err)
	}
	if _, ok := l.NodePath(cam.ID); !ok {
		t.Fatal("precondition failed: EnsureNodes did not create the camera's node")
	}

	l.RemoveCamera(cam.ID)

	h.mu.Lock()
	removed := append([]int(nil), h.removeCalls...)
	h.mu.Unlock()
	if len(removed) != 1 || removed[0] != int(cam.ID) {
		t.Fatalf("removeNode calls = %v, want exactly [%d]", removed, cam.ID)
	}
	if path, ok := l.NodePath(cam.ID); ok {
		t.Fatalf("NodePath(%d) = (%q, true) after RemoveCamera, want false", cam.ID, path)
	}
}

// ---------------------------------------------------------------------------
// Task C3: reference-counted pump supervision with backoff.
//
// Every test below drives time through fakeClock and pump attempts through
// fakePump, so none of them sleeps through a real backoff wait, idle-grace
// period, or stable-run window — the longest of which (pumpStableRun) is a
// full minute. See fakeClock's doc comment for the one deliberate exception:
// a short bounded real-time guard used purely to detect a negative result
// (nothing happened) that no amount of clock-advancing can otherwise prove.
// ---------------------------------------------------------------------------

// fakeClock is a controllable clock implementing the ipcam package's clock
// seam. After registers a waiter that Advance fires once the clock reaches
// its deadline; nothing here ever waits on real time except the test-only
// synchronization helpers below, which poll briefly to let a goroutine reach
// its select before the test acts on it — a mechanic, not a simulation of
// the business timing under test.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeWaiter
}

type fakeWaiter struct {
	deadline time.Time
	ch       chan time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	deadline := c.now.Add(d)
	if !deadline.After(c.now) {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, fakeWaiter{deadline: deadline, ch: ch})
	return ch
}

// Advance moves the clock forward by d, delivering to every pending waiter
// whose deadline has now been reached. A waiter that was superseded (its
// goroutine took a different select branch, e.g. a canceled idle-grace timer)
// and so is never read again still gets a value written to it here: its
// channel is buffered at 1, so the write never blocks, and the abandoned
// value is simply never observed.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	var remaining []fakeWaiter
	for _, w := range c.waiters {
		if !w.deadline.After(c.now) {
			w.ch <- c.now
		} else {
			remaining = append(remaining, w)
		}
	}
	c.waiters = remaining
}

func (c *fakeClock) waiterCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// blockUntilWaiters polls in short real-time increments — synchronization
// only, never a stand-in for the business duration under test — until n
// goroutines are parked in After, so a test can Advance knowing exactly who
// is listening rather than racing a goroutine that has not reached its
// select yet.
func (c *fakeClock) blockUntilWaiters(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c.waiterCount() >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d clock waiter(s), have %d", n, c.waiterCount())
		}
		time.Sleep(time.Millisecond)
	}
}

// pendingDelay returns the delay from now until the single pending waiter's
// deadline, letting a test assert the supervisor requested exactly the
// backoff or idle-grace duration it should have, rather than inferring that
// indirectly from which Advance sizes do or don't unblock it.
func (c *fakeClock) pendingDelay(t *testing.T) time.Duration {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.waiters) != 1 {
		t.Fatalf("pendingDelay: want exactly 1 pending waiter, have %d", len(c.waiters))
	}
	return c.waiters[0].deadline.Sub(c.now)
}

// pumpAttempt is what fakePump reports to a test each time its PumpFunc is
// invoked.
type pumpAttempt struct {
	n    int
	args []string
}

// pumpBehavior configures how a single numbered pump attempt resolves. A
// zero value (no release channel) returns err immediately; a non-nil release
// channel makes the attempt block — simulating a healthy running pump — until
// the test closes or sends on it (or ctx is canceled first, unless
// ignoreCancel is set).
type pumpBehavior struct {
	release <-chan struct{}
	err     error
	// ignoreCancel, when true, makes this attempt block on release alone,
	// deaf to ctx cancellation — modeling a pump that takes real, observable
	// time (or hangs outright) tearing down after being asked to stop. Tests
	// that need to see the gap between "canceled" and "actually exited" (a
	// pump the default ctx-respecting behavior would close instantly) use
	// this; whoever sets it is responsible for eventually closing/sending on
	// release so the goroutine does not leak past the test.
	ignoreCancel bool
}

// fakePump is a controllable PumpFunc for exercising Loopback's supervisor
// without a real GStreamer process or RTSP server. Attempts not explicitly
// configured via on block on ctx.Done() and return ctx.Err(), the default
// "healthy, running" behavior a demanded camera with nothing else configured
// should get.
type fakePump struct {
	mu        sync.Mutex
	attempts  int
	behaviors map[int]pumpBehavior

	started chan pumpAttempt
	// finished receives an attempt the moment its call actually returns —
	// distinct from started, and needed wherever a test must prove something
	// did NOT happen before the pump exited, not just that it eventually did.
	finished chan pumpAttempt
}

func newFakePump() *fakePump {
	return &fakePump{
		behaviors: make(map[int]pumpBehavior),
		started:   make(chan pumpAttempt, 64),
		finished:  make(chan pumpAttempt, 64),
	}
}

// on configures attempt n's (1-indexed) behavior. Set these up before
// triggering demand: attempt numbers are deterministic given the fake clock
// drives every retry, so a test always knows which attempt it is about to
// observe.
func (f *fakePump) on(n int, b pumpBehavior) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.behaviors[n] = b
}

func (f *fakePump) Func() PumpFunc {
	return func(ctx context.Context, args []string) error {
		f.mu.Lock()
		f.attempts++
		n := f.attempts
		b, configured := f.behaviors[n]
		f.mu.Unlock()

		f.started <- pumpAttempt{n: n, args: args}
		err := f.run(ctx, b, configured)
		f.finished <- pumpAttempt{n: n, args: args}
		return err
	}
}

func (f *fakePump) run(ctx context.Context, b pumpBehavior, configured bool) error {
	if !configured {
		<-ctx.Done()
		return ctx.Err()
	}
	if b.ignoreCancel {
		<-b.release
		return b.err
	}
	if b.release != nil {
		select {
		case <-b.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return b.err
}

func (f *fakePump) attemptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

// next blocks until the pump's next attempt has started (i.e. is at least as
// far as having been invoked and reported itself), or fails the test after a
// generous real-time bound — a safety net against a hang, not a simulation of
// business timing.
func (f *fakePump) next(t *testing.T) pumpAttempt {
	t.Helper()
	select {
	case a := <-f.started:
		return a
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for a pump attempt to start")
		return pumpAttempt{}
	}
}

// assertNoFurtherAttemptWithin is the one place these tests wait on real
// time for its own sake: proving a negative ("the pump was not restarted")
// has no clock-driven equivalent, since there is no future deadline to
// advance to that would prove silence forever. The bound is short and used
// only to catch a regression that would otherwise show up as flakiness, not
// to pace the scenario under test.
func assertNoFurtherAttemptWithin(t *testing.T, pump *fakePump, d time.Duration) {
	t.Helper()
	select {
	case a := <-pump.started:
		t.Fatalf("unexpected pump attempt %d started", a.n)
	case <-time.After(d):
	}
}

// waitDrained blocks until l's supervisor/idle-grace goroutines have all
// exited (l.wg reaches zero), or fails the test after a generous bound. It
// is only safe to call once a test knows no further goroutine will be
// added — e.g. right after triggering a stop with nothing left demanded.
func waitDrained(t *testing.T, l *Loopback) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		l.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for Loopback's supervisor goroutines to exit")
	}
}

// newSupervisedLoopback returns a Loopback wired to a fake clock and the
// given pump, plus the registry and credential store backing it, for Task
// C3's tests. It registers l.Shutdown as test cleanup so no test leaks a
// blocked supervisor goroutine into the next one.
func newSupervisedLoopback(t *testing.T, h *loopbackHarness, fc *fakeClock, pump PumpFunc) (*Loopback, *Registry, *CredentialStore) {
	t.Helper()
	reg := NewRegistry(filepath.Join(t.TempDir(), "cameras.json"))
	if err := reg.Load(); err != nil {
		t.Fatalf("registry load: %v", err)
	}
	creds := NewCredentialStore(filepath.Join(t.TempDir(), "credentials.json"))
	if err := creds.Load(); err != nil {
		t.Fatalf("credentials load: %v", err)
	}
	l := NewLoopback(context.Background(), zap.NewNop(), reg, creds, pump)
	l.deps = h.deps()
	l.clock = fc
	t.Cleanup(l.Shutdown)
	return l, reg, creds
}

// registerReadyCamera upserts a camera, stores a credential for it, and
// ensures its loopback node exists — the full precondition set reconcile
// checks before starting a pump.
func registerReadyCamera(t *testing.T, l *Loopback, reg *Registry, creds *CredentialStore, mac, address string) Camera {
	t.Helper()
	cam, err := reg.Upsert(Camera{MAC: mac, Address: address})
	if err != nil {
		t.Fatalf("upsert %s: %v", mac, err)
	}
	if err := creds.Set(mac, Credential{Username: "admin", Password: "hunter2"}); err != nil {
		t.Fatalf("set credentials for %s: %v", mac, err)
	}
	if err := l.EnsureNodes(context.Background()); err != nil {
		t.Fatalf("EnsureNodes: %v", err)
	}
	if _, ok := l.NodePath(cam.ID); !ok {
		t.Fatalf("precondition failed: no node for camera %d", cam.ID)
	}
	return cam
}

// A container consumer starting is the "started when an entitled container
// starts" half of the reference-counting model: demand goes from nothing to
// non-empty, and the pump for every registered (ready) camera starts.
func TestLoopback_PumpStartsOnFirstContainerConsumer(t *testing.T) {
	h := newLoopbackHarness()
	h.controlExists = true
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	pump := newFakePump()
	l, reg, creds := newSupervisedLoopback(t, h, fc, pump.Func())
	cam := registerReadyCamera(t, l, reg, creds, "ec:71:db:2a:ae:7e", "10.98.0.10")

	if n := pump.attemptCount(); n != 0 {
		t.Fatalf("pump called %d times before any consumer, want 0", n)
	}

	l.SetContainerConsumers([]string{"com.example.app"})

	a := pump.next(t)
	joined := strings.Join(a.args, " ")
	wantDevice := "device=/dev/video" + strconv.FormatUint(uint64(cam.ID), 10)
	if !strings.Contains(joined, wantDevice) {
		t.Fatalf("pump args = %q, want to contain %q", joined, wantDevice)
	}
	if !strings.Contains(joined, "location=rtsp://") {
		t.Fatalf("pump args = %q, want an rtsp location", joined)
	}
}

// After the last consumer goes away, the pump keeps running through the
// idle-grace period and only actually stops once it elapses.
func TestLoopback_PumpStopsAfterIdleGrace(t *testing.T) {
	h := newLoopbackHarness()
	h.controlExists = true
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	pump := newFakePump()
	l, reg, creds := newSupervisedLoopback(t, h, fc, pump.Func())
	registerReadyCamera(t, l, reg, creds, "ec:71:db:2a:ae:7e", "10.98.0.10")

	l.SetContainerConsumers([]string{"com.example.app"})
	pump.next(t) // attempt 1 starts and blocks (default behavior).

	l.SetContainerConsumers(nil) // demand drops to zero.

	// The pump must still be running immediately after demand drops: only
	// the idle-grace timer has started, nothing has stopped yet.
	fc.blockUntilWaiters(t, 1)
	if got := fc.pendingDelay(t); got != pumpIdleGrace {
		t.Fatalf("idle-grace wait = %v, want %v", got, pumpIdleGrace)
	}
	if n := pump.attemptCount(); n != 1 {
		t.Fatalf("pump attempts = %d after demand dropped but before grace elapsed, want still 1", n)
	}

	fc.Advance(pumpIdleGrace)
	waitDrained(t, l)

	if n := pump.attemptCount(); n != 1 {
		t.Fatalf("pump attempts = %d after idle grace elapsed and no reacquire, want still 1 (no restart)", n)
	}
}

// A new consumer arriving before the idle-grace period elapses cancels the
// pending stop: the pump must never actually go down, and a stale timer that
// was already in flight when the reacquire happened must not stop it later
// either.
func TestLoopback_ReacquireWithinGraceKeepsPump(t *testing.T) {
	h := newLoopbackHarness()
	h.controlExists = true
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	pump := newFakePump()
	l, reg, creds := newSupervisedLoopback(t, h, fc, pump.Func())
	cam := registerReadyCamera(t, l, reg, creds, "ec:71:db:2a:ae:7e", "10.98.0.10")

	l.SetContainerConsumers([]string{"com.example.app"})
	pump.next(t) // attempt 1 starts and blocks.

	l.SetContainerConsumers(nil)
	fc.blockUntilWaiters(t, 1) // idle-grace timer is genuinely pending.

	l.SetContainerConsumers([]string{"com.example.app"}) // reacquire.

	// Deterministic, no clock advance needed: reconcile clears idleCancel
	// synchronously before SetContainerConsumers returns.
	l.mu.Lock()
	cp, running := l.pumps[cam.ID]
	stillIdlePending := running && cp.idleCancel != nil
	l.mu.Unlock()
	if !running {
		t.Fatal("supervisor no longer registered after a reacquire within the grace period")
	}
	if stillIdlePending {
		t.Fatal("idle-grace timer still pending after a reacquire; it should have been canceled")
	}
	if n := pump.attemptCount(); n != 1 {
		t.Fatalf("pump attempts = %d after reacquire, want still 1 (never restarted)", n)
	}

	// The original timer's fake-clock waiter is still sitting in fc (its
	// goroutine took the ctx.Done() branch and never consumed it — see
	// Advance's doc comment) so advancing past its deadline must still be a
	// no-op rather than a second, stale stop.
	fc.Advance(pumpIdleGrace)
	assertNoFurtherAttemptWithin(t, pump, 200*time.Millisecond)
	if n := pump.attemptCount(); n != 1 {
		t.Fatalf("pump attempts = %d after the stale timer's deadline passed, want still 1", n)
	}
}

// AcquireView is the `camera view`-attach half of the reference-counting
// model: its release func is the other half, and releasing the last view ref
// must behave exactly like the last container consumer leaving.
func TestLoopback_ViewRefStartsAndReleasesPump(t *testing.T) {
	h := newLoopbackHarness()
	h.controlExists = true
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	pump := newFakePump()
	l, reg, creds := newSupervisedLoopback(t, h, fc, pump.Func())
	cam := registerReadyCamera(t, l, reg, creds, "ec:71:db:2a:ae:7e", "10.98.0.10")

	release := l.AcquireView(cam.ID)
	pump.next(t) // attempt 1 starts.

	release()

	fc.blockUntilWaiters(t, 1)
	if got := fc.pendingDelay(t); got != pumpIdleGrace {
		t.Fatalf("idle-grace wait = %v, want %v", got, pumpIdleGrace)
	}
	fc.Advance(pumpIdleGrace)
	waitDrained(t, l)

	if n := pump.attemptCount(); n != 1 {
		t.Fatalf("pump attempts = %d after the sole view ref released and grace elapsed, want still 1", n)
	}

	// Releasing a second time must not panic or double-decrement.
	release()
}

// SetContainerConsumers replaces the consumer set wholesale rather than
// diffing against the previous call: swapping in an entirely different,
// still-non-empty set of names must not bounce a pump that stayed demanded
// throughout, and it must apply to every registered camera at once.
func TestLoopback_SetContainerConsumersDiffIsWholesale(t *testing.T) {
	h := newLoopbackHarness()
	h.controlExists = true
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	pump := newFakePump()
	l, reg, creds := newSupervisedLoopback(t, h, fc, pump.Func())
	cam1 := registerReadyCamera(t, l, reg, creds, "ec:71:db:2a:ae:7e", "10.98.0.10")
	cam2 := registerReadyCamera(t, l, reg, creds, "ec:71:db:2a:ae:7f", "10.98.0.11")

	l.SetContainerConsumers([]string{"com.example.appA"})
	seen := map[int]bool{}
	for range 2 {
		a := pump.next(t)
		seen[a.n] = true
	}
	if !seen[1] || !seen[2] {
		t.Fatalf("did not observe both cameras' first pump attempt: %v", seen)
	}

	// A wholesale replacement with a different, still non-empty set must not
	// restart anything: demand (len > 0) never dropped.
	l.SetContainerConsumers([]string{"com.example.appB"})
	assertNoFurtherAttemptWithin(t, pump, 200*time.Millisecond)
	if n := pump.attemptCount(); n != 2 {
		t.Fatalf("pump attempts = %d after swapping consumer names with demand still non-empty, want still 2", n)
	}

	// Dropping to no consumers at all now stops both.
	l.SetContainerConsumers(nil)
	fc.blockUntilWaiters(t, 2)
	fc.Advance(pumpIdleGrace)
	waitDrained(t, l)

	if n := pump.attemptCount(); n != 2 {
		t.Fatalf("pump attempts = %d after both cameras went idle, want still 2 (no restart)", n)
	}
	_, _ = cam1, cam2
}

// The backoff between failed pump attempts doubles from 1s and caps at 30s,
// and resets to 1s once an attempt has run stably (>1 minute) before it
// eventually fails.
func TestLoopback_PumpRestartsWithExponentialBackoffAndResetsAfterStableRun(t *testing.T) {
	h := newLoopbackHarness()
	h.controlExists = true
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	pump := newFakePump()
	l, reg, creds := newSupervisedLoopback(t, h, fc, pump.Func())
	registerReadyCamera(t, l, reg, creds, "ec:71:db:2a:ae:7e", "10.98.0.10")

	release3 := make(chan struct{})
	pump.on(1, pumpBehavior{err: errors.New("boom1")})
	pump.on(2, pumpBehavior{err: errors.New("boom2")})
	pump.on(3, pumpBehavior{release: release3, err: errors.New("boom3")})
	pump.on(4, pumpBehavior{err: errors.New("boom4")})

	l.SetContainerConsumers([]string{"com.example.app"})

	pump.next(t) // attempt 1: fails immediately, no wait beforehand (level 0).
	fc.blockUntilWaiters(t, 1)
	if got := fc.pendingDelay(t); got != 1*time.Second {
		t.Fatalf("backoff after attempt 1 = %v, want 1s", got)
	}
	fc.Advance(1 * time.Second)

	pump.next(t) // attempt 2: fails immediately, backoff doubles.
	fc.blockUntilWaiters(t, 1)
	if got := fc.pendingDelay(t); got != 2*time.Second {
		t.Fatalf("backoff after attempt 2 = %v, want 2s", got)
	}
	fc.Advance(2 * time.Second)

	pump.next(t) // attempt 3: stays up (blocks on release3).
	// Simulate a stable run of well over a minute before it fails.
	fc.Advance(90 * time.Second)
	close(release3)

	fc.blockUntilWaiters(t, 1)
	if got := fc.pendingDelay(t); got != 1*time.Second {
		t.Fatalf("backoff after a stable run = %v, want reset to 1s, not a continued doubling", got)
	}
	fc.Advance(1 * time.Second)

	a4 := pump.next(t) // attempt 4: confirms the loop continues correctly post-reset.
	if a4.n != 4 {
		t.Fatalf("attempt number = %d, want 4", a4.n)
	}
}

// A camera with demand but no stored credentials never starts a pump: the
// node may exist, but nothing feeds it.
func TestLoopback_NoPumpWithoutCredentials(t *testing.T) {
	h := newLoopbackHarness()
	h.controlExists = true
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	pump := newFakePump()
	l, reg, _ := newSupervisedLoopback(t, h, fc, pump.Func())
	cam, err := reg.Upsert(Camera{MAC: "ec:71:db:2a:ae:7e", Address: "10.98.0.10"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := l.EnsureNodes(context.Background()); err != nil {
		t.Fatalf("EnsureNodes: %v", err)
	}

	l.SetContainerConsumers([]string{"com.example.app"})

	if n := pump.attemptCount(); n != 0 {
		t.Fatalf("pump attempts = %d for a camera with no stored credentials, want 0", n)
	}
	l.mu.Lock()
	_, running := l.pumps[cam.ID]
	l.mu.Unlock()
	if running {
		t.Fatal("a supervisor is registered for a camera with no credentials")
	}
}

// Once credentials are stored for a camera that is already demanded, the
// pump that could not start before starts now.
func TestLoopback_CredentialsChangedStartsPendingPump(t *testing.T) {
	h := newLoopbackHarness()
	h.controlExists = true
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	pump := newFakePump()
	l, reg, creds := newSupervisedLoopback(t, h, fc, pump.Func())
	cam, err := reg.Upsert(Camera{MAC: "ec:71:db:2a:ae:7e", Address: "10.98.0.10"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := l.EnsureNodes(context.Background()); err != nil {
		t.Fatalf("EnsureNodes: %v", err)
	}

	l.SetContainerConsumers([]string{"com.example.app"})
	if n := pump.attemptCount(); n != 0 {
		t.Fatalf("pump attempts = %d before credentials exist, want 0", n)
	}

	if err := creds.Set(cam.MAC, Credential{Username: "admin", Password: "hunter2"}); err != nil {
		t.Fatalf("set credentials: %v", err)
	}
	l.CredentialsChanged(cam.ID)

	pump.next(t) // the previously pending pump now starts.
}

// AcquireView must be safe to call, and safe to release, when the
// v4l2loopback module is unavailable: no pump can start, and nothing panics.
func TestLoopback_AcquireViewNoopWhenModuleAbsent(t *testing.T) {
	h := newLoopbackHarness()
	h.modprobeErr = errors.New("modprobe: FATAL: Module v4l2loopback not found")
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	pump := newFakePump()
	l, reg, creds := newSupervisedLoopback(t, h, fc, pump.Func())
	cam, err := reg.Upsert(Camera{MAC: "ec:71:db:2a:ae:7e", Address: "10.98.0.10"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := creds.Set(cam.MAC, Credential{Username: "admin", Password: "hunter2"}); err != nil {
		t.Fatalf("set credentials: %v", err)
	}

	release := l.AcquireView(cam.ID)
	if n := pump.attemptCount(); n != 0 {
		t.Fatalf("pump attempts = %d with the module unavailable, want 0", n)
	}
	release() // must not panic.

	l.mu.Lock()
	_, running := l.pumps[cam.ID]
	l.mu.Unlock()
	if running {
		t.Fatal("a supervisor is registered despite the module being unavailable")
	}
}

// Shutdown cancels every running pump and waits for their goroutines to
// exit, but leaves the loopback nodes themselves in place.
func TestLoopback_ShutdownStopsAllPumps(t *testing.T) {
	h := newLoopbackHarness()
	h.controlExists = true
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	pump := newFakePump()
	l, reg, creds := newSupervisedLoopback(t, h, fc, pump.Func())
	cam1 := registerReadyCamera(t, l, reg, creds, "ec:71:db:2a:ae:7e", "10.98.0.10")
	cam2 := registerReadyCamera(t, l, reg, creds, "ec:71:db:2a:ae:7f", "10.98.0.11")

	l.SetContainerConsumers([]string{"com.example.app"})
	pump.next(t)
	pump.next(t)

	l.Shutdown() // cancels + waits; safe to call again via t.Cleanup.

	l.mu.Lock()
	remaining := len(l.pumps)
	l.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("pumps map has %d entries after Shutdown, want 0", remaining)
	}

	for _, cam := range []Camera{cam1, cam2} {
		if _, ok := l.NodePath(cam.ID); !ok {
			t.Fatalf("camera %d's loopback node is gone after Shutdown; Shutdown must leave nodes in place", cam.ID)
		}
	}
}

// ---------------------------------------------------------------------------
// Fix-up round: closing the Shutdown/wg.Add race and giving idle-grace
// timers per-cycle identity.
// ---------------------------------------------------------------------------

// Once Shutdown has set shuttingDown, a later reconcile — here, a
// demand-raising SetContainerConsumers — must be a no-op: no supervisor
// starts, and in particular no wg.Add happens. Go's WaitGroup documents
// concurrent Add(positive)/Wait as misuse that can panic — not a data race,
// so -race cannot flag it — which is exactly why this needs its own
// deterministic test rather than relying on -race to catch a regression.
func TestLoopback_ShutdownDeclinesConcurrentReconcileStart(t *testing.T) {
	h := newLoopbackHarness()
	h.controlExists = true
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	pump := newFakePump()
	l, reg, creds := newSupervisedLoopback(t, h, fc, pump.Func())
	registerReadyCamera(t, l, reg, creds, "ec:71:db:2a:ae:7e", "10.98.0.10")

	l.Shutdown() // nothing running yet; sets shuttingDown and returns immediately.

	l.SetContainerConsumers([]string{"com.example.app"}) // must be a no-op now.

	assertNoFurtherAttemptWithin(t, pump, 200*time.Millisecond)
	l.mu.Lock()
	remaining := len(l.pumps)
	shuttingDown := l.shuttingDown
	l.mu.Unlock()
	if !shuttingDown {
		t.Fatal("shuttingDown was not set by Shutdown")
	}
	if remaining != 0 {
		t.Fatalf("pumps map has %d entries after a post-Shutdown reconcile, want 0", remaining)
	}
}

// Stress the actual race the fix closes, rather than only the deterministic
// half above: Shutdown and demand-raising reconcile calls firing
// concurrently and repeatedly. A regression that dropped the shuttingDown
// check has many chances here — amplified further by -race and -count=N at
// the go test invocation — to surface as either a WaitGroup misuse panic or
// a supervisor that outlives Shutdown.
func TestLoopback_ShutdownConcurrentWithReconcileNeverPanics(t *testing.T) {
	h := newLoopbackHarness()
	h.controlExists = true
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	pump := newFakePump()
	l, reg, creds := newSupervisedLoopback(t, h, fc, pump.Func())
	registerReadyCamera(t, l, reg, creds, "ec:71:db:2a:ae:7e", "10.98.0.10")

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if i%2 == 0 {
				l.SetContainerConsumers([]string{"com.example.app"})
			} else {
				l.SetContainerConsumers(nil)
			}
		}
	}()

	go func() {
		defer wg.Done()
		l.Shutdown() // must never panic, no matter how it interleaves with the loop above.
	}()

	wg.Wait()

	// Once both goroutines have returned, Shutdown has definitely completed
	// (its own goroutine cannot have returned otherwise) and shuttingDown is
	// permanently true, so nothing the loop goroutine did — including any
	// call still in flight at the moment Shutdown finished — can have left a
	// supervisor behind.
	l.mu.Lock()
	remaining := len(l.pumps)
	l.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("pumps map has %d entries after a concurrent Shutdown drained, want 0", remaining)
	}
}

// fireIdleGrace's guard must key off the specific arming episode (idleGen),
// not just cp identity and idleCancel presence: across a drop→reacquire→drop
// cycle on the same camPump, a call carrying the FIRST arming's generation —
// standing in for a stale awaitIdleGrace goroutine from that first drop,
// finally reaching its post-timer action late — must not be able to stop a
// pump that the SECOND arming is now the one watching. Driving fireIdleGrace
// directly (rather than via awaitIdleGrace's real select) tests the guard
// deterministically instead of depending on how the Go scheduler happens to
// interleave two goroutines.
func TestLoopback_IdleGraceStaleGenerationDoesNotStopPump(t *testing.T) {
	h := newLoopbackHarness()
	h.controlExists = true
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	pump := newFakePump()
	l, reg, creds := newSupervisedLoopback(t, h, fc, pump.Func())
	cam := registerReadyCamera(t, l, reg, creds, "ec:71:db:2a:ae:7e", "10.98.0.10")

	l.SetContainerConsumers([]string{"com.example.app"})
	pump.next(t) // attempt 1 starts and blocks.

	l.SetContainerConsumers(nil)                         // drop 1: arms idleGen 1.
	l.SetContainerConsumers([]string{"com.example.app"}) // reacquire: cancels it.
	l.SetContainerConsumers(nil)                         // drop 2: arms idleGen 2 on the same camPump.

	l.mu.Lock()
	cp, running := l.pumps[cam.ID]
	currentGen := cp.idleGen
	l.mu.Unlock()
	if !running {
		t.Fatal("supervisor no longer registered after drop->reacquire->drop")
	}
	if currentGen != 2 {
		t.Fatalf("idleGen = %d after a second arming on the same camPump, want 2", currentGen)
	}

	// A stale, first-cycle generation must not touch cycle 2's pending timer.
	l.fireIdleGrace(cam.ID, cp, currentGen-1)

	l.mu.Lock()
	stillPendingCurrent := l.pumps[cam.ID] == cp && cp.idleCancel != nil
	l.mu.Unlock()
	if !stillPendingCurrent {
		t.Fatal("a stale-generation fireIdleGrace cleared the current (gen 2) idle-grace timer")
	}
	if n := pump.attemptCount(); n != 1 {
		t.Fatalf("pump attempts = %d after a stale-generation fireIdleGrace, want still 1 (must not have stopped)", n)
	}

	// The current generation, by contrast, must still be able to stop it —
	// proving the guard discriminates rather than refusing unconditionally.
	l.fireIdleGrace(cam.ID, cp, currentGen)
	l.mu.Lock()
	cleared := cp.idleCancel == nil
	l.mu.Unlock()
	if !cleared {
		t.Fatal("fireIdleGrace with the current generation did not clear idleCancel")
	}
}

// End-to-end companion to the white-box test above, driven only through the
// public API and the fake clock: a drop→reacquire→drop cycle leaves cycle
// 1's stale waiter dangling in fc (see fakeClock's doc comment — a canceled
// idle-grace goroutine never consumes the channel it registered), so
// advancing to cycle 1's original deadline must not stop the pump; only
// advancing to cycle 2's own full grace duration, measured from when it was
// actually armed, does.
func TestLoopback_DropReacquireDropRunsSecondGraceFully(t *testing.T) {
	h := newLoopbackHarness()
	h.controlExists = true
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	pump := newFakePump()
	l, reg, creds := newSupervisedLoopback(t, h, fc, pump.Func())
	registerReadyCamera(t, l, reg, creds, "ec:71:db:2a:ae:7e", "10.98.0.10")

	l.SetContainerConsumers([]string{"com.example.app"})
	pump.next(t) // attempt 1 starts and blocks.

	l.SetContainerConsumers(nil) // drop 1, at t+0: arms a grace ending at t+10s.
	fc.blockUntilWaiters(t, 1)

	fc.Advance(4 * time.Second)                          // some idle time passes first.
	l.SetContainerConsumers([]string{"com.example.app"}) // reacquire at t+4s.

	l.SetContainerConsumers(nil) // drop 2, at t+4s: arms a grace ending at t+14s.
	fc.blockUntilWaiters(t, 2)   // cycle 1's dangling waiter, plus cycle 2's fresh one.

	fc.Advance(6 * time.Second) // now at t+10s: cycle 1's stale deadline, well short of cycle 2's.
	assertNoFurtherAttemptWithin(t, pump, 200*time.Millisecond)
	if n := pump.attemptCount(); n != 1 {
		t.Fatalf("pump attempts = %d at cycle 1's stale deadline, want still 1 (not stopped)", n)
	}

	fc.Advance(4 * time.Second) // now at t+14s: cycle 2's real deadline.
	waitDrained(t, l)
	if n := pump.attemptCount(); n != 1 {
		t.Fatalf("pump attempts = %d after cycle 2's full grace elapsed, want still 1 (stopped once, not restarted)", n)
	}
}

// A demanded, credentialed camera whose loopback node does not exist yet
// must not block forever or give up: the supervisor backs off and retries
// exactly like a failed attempt would (it just never invokes the pump func,
// since there is nothing to feed), and starts for real the moment the node
// appears — e.g. because a container's entitlement hook ran EnsureNodes.
func TestLoopback_PumpRetriesUntilNodeAppears(t *testing.T) {
	h := newLoopbackHarness()
	h.controlExists = true
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	pump := newFakePump()
	l, reg, creds := newSupervisedLoopback(t, h, fc, pump.Func())
	cam, err := reg.Upsert(Camera{MAC: "ec:71:db:2a:ae:7e", Address: "10.98.0.10"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := creds.Set(cam.MAC, Credential{Username: "admin", Password: "hunter2"}); err != nil {
		t.Fatalf("set credentials: %v", err)
	}
	// Deliberately no EnsureNodes yet: demand and credentials are both
	// present, but the node itself does not exist.

	l.SetContainerConsumers([]string{"com.example.app"})

	fc.blockUntilWaiters(t, 1)
	if got := fc.pendingDelay(t); got != 1*time.Second {
		t.Fatalf("backoff before the node exists = %v, want 1s", got)
	}
	if n := pump.attemptCount(); n != 0 {
		t.Fatalf("pump invoked %d times before its node exists, want 0", n)
	}
	fc.Advance(1 * time.Second)

	fc.blockUntilWaiters(t, 1)
	if got := fc.pendingDelay(t); got != 2*time.Second {
		t.Fatalf("backoff on the second still-missing-node retry = %v, want 2s", got)
	}
	if n := pump.attemptCount(); n != 0 {
		t.Fatalf("pump invoked %d times before its node exists, want 0", n)
	}

	// The node appears.
	if err := l.EnsureNodes(context.Background()); err != nil {
		t.Fatalf("EnsureNodes: %v", err)
	}
	fc.Advance(2 * time.Second)

	a := pump.next(t) // the pump starts for real now that the node exists.
	joined := strings.Join(a.args, " ")
	wantDevice := "device=/dev/video" + strconv.FormatUint(uint64(cam.ID), 10)
	if !strings.Contains(joined, wantDevice) {
		t.Fatalf("pump args = %q, want to contain %q", joined, wantDevice)
	}
}

// ---------------------------------------------------------------------------
// Final-review fix wave: RemoveCamera must stop the pump before removing the
// node, and a permanently node-less camera must not retry in total silence.
// ---------------------------------------------------------------------------

// RemoveCamera must stop a camera's running pump and wait for it to actually
// exit before removing its loopback node — not remove the node first (or
// concurrently), which would return EBUSY while the pump's v4l2sink still
// holds it open, and would leave an already-authenticated RTSP session
// streaming with credentials the caller may have just deleted.
//
// This is proven via a genuine race between "removeNode fires" and "the pump
// is released," not by inspecting state after the fact: the fake pump is
// configured to ignore its context cancellation until explicitly released,
// so if RemoveCamera's implementation ever regressed to removing the node
// without waiting, this test would observe removeNode (or RemoveCamera
// itself returning) before the release.
func TestLoopback_RemoveCameraStopsPumpThenRemovesNode(t *testing.T) {
	h := newLoopbackHarness()
	h.controlExists = true
	h.removeSignal = make(chan int, 4)
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	pump := newFakePump()
	release := make(chan struct{})
	pump.on(1, pumpBehavior{ignoreCancel: true, release: release})
	l, reg, creds := newSupervisedLoopback(t, h, fc, pump.Func())
	cam := registerReadyCamera(t, l, reg, creds, "ec:71:db:2a:ae:7e", "10.98.0.10")

	l.SetContainerConsumers([]string{"com.example.app"})
	pump.next(t) // attempt 1 starts; it will ignore cancellation until released.

	// Mirrors VideoService.ForgetCamera's real ordering: the camera is gone
	// from the registry before RemoveCamera is ever called.
	if !reg.Forget(cam.ID) {
		t.Fatalf("Forget(%d) = false", cam.ID)
	}

	removeDone := make(chan struct{})
	go func() {
		l.RemoveCamera(cam.ID)
		close(removeDone)
	}()

	select {
	case <-h.removeSignal:
		t.Fatal("removeNode was called before the pump exited")
	case <-removeDone:
		t.Fatal("RemoveCamera returned before its pump exited")
	case <-pump.finished:
		t.Fatal("the fake pump reported finishing before being released")
	case <-time.After(100 * time.Millisecond):
		// Expected: RemoveCamera has had time to reach its wait, and none of
		// the above fired — this bound is a synchronization mechanic (giving
		// the RemoveCamera goroutine a chance to run), not a stand-in for
		// removeCameraGrace or any other business duration.
	}

	close(release) // let the pump actually return now.

	select {
	case <-removeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("RemoveCamera did not return after its pump exited")
	}

	select {
	case nr := <-h.removeSignal:
		if nr != int(cam.ID) {
			t.Fatalf("removeNode called for nr=%d, want %d", nr, cam.ID)
		}
	default:
		t.Fatal("removeNode was never called")
	}
	if path, ok := l.NodePath(cam.ID); ok {
		t.Fatalf("NodePath(%d) = (%q, true) after RemoveCamera, want false", cam.ID, path)
	}
	l.mu.Lock()
	_, stillRunning := l.pumps[cam.ID]
	l.mu.Unlock()
	if stillRunning {
		t.Fatal("supervisor still registered after RemoveCamera")
	}
}

// If a pump never notices its cancellation (hung, or simply slower than
// removeCameraGrace), RemoveCamera must not hang its caller — a gRPC
// ForgetCamera handler — waiting on it forever. It gives up on the wait,
// logs it, and still attempts removeNode: the ioctl's own EBUSY-on-a-held-
// open-node behavior is the backstop for a pump that is still holding it,
// not an unbounded wait here. This is the documented contract for the
// "removeNode initially errors" case: RemoveCamera does not retry — it
// already waited once, bounded, which is the one retry this design affords —
// it just doesn't let a stuck pump turn into a stuck RPC.
func TestLoopback_RemoveCameraProceedsAfterGraceIfPumpNeverExits(t *testing.T) {
	h := newLoopbackHarness()
	h.controlExists = true
	h.removeSignal = make(chan int, 4)
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	pump := newFakePump()
	release := make(chan struct{})
	pump.on(1, pumpBehavior{ignoreCancel: true, release: release})
	l, reg, creds := newSupervisedLoopback(t, h, fc, pump.Func())
	cam := registerReadyCamera(t, l, reg, creds, "ec:71:db:2a:ae:7e", "10.98.0.10")
	// Let the stuck attempt actually exit once the test is done, so
	// t.Cleanup(l.Shutdown) — registered first, so it runs after this one —
	// does not hang waiting on a goroutine that would otherwise never notice
	// cancellation at all.
	t.Cleanup(func() { close(release) })

	l.SetContainerConsumers([]string{"com.example.app"})
	pump.next(t)
	if !reg.Forget(cam.ID) {
		t.Fatalf("Forget(%d) = false", cam.ID)
	}

	removeDone := make(chan struct{})
	go func() {
		l.RemoveCamera(cam.ID)
		close(removeDone)
	}()

	fc.blockUntilWaiters(t, 1) // RemoveCamera is now parked on its bounded timer.
	fc.Advance(removeCameraGrace)

	select {
	case <-removeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("RemoveCamera did not return after its grace period elapsed")
	}

	select {
	case nr := <-h.removeSignal:
		if nr != int(cam.ID) {
			t.Fatalf("removeNode called for nr=%d, want %d", nr, cam.ID)
		}
	default:
		t.Fatal("removeNode was never attempted after the grace period elapsed")
	}
}

// A camera stuck permanently node-less (demand and credentials present, but
// no node ever appears) must not spin in total silence — but it also must
// not spam a warning on every 1s/2s/4s.../30s-capped retry. superviseCam logs
// it exactly once per supervisor lifetime.
func TestLoopback_LogsNodeNotReadyOnceThenStaysQuiet(t *testing.T) {
	h := newLoopbackHarness()
	h.controlExists = true
	fc := newFakeClock(time.Unix(1_700_000_000, 0))
	pump := newFakePump()

	core, logs := observer.New(zapcore.WarnLevel)
	logger := zap.New(core)

	reg := NewRegistry(filepath.Join(t.TempDir(), "cameras.json"))
	if err := reg.Load(); err != nil {
		t.Fatalf("registry load: %v", err)
	}
	creds := NewCredentialStore(filepath.Join(t.TempDir(), "credentials.json"))
	if err := creds.Load(); err != nil {
		t.Fatalf("credentials load: %v", err)
	}
	l := NewLoopback(context.Background(), logger, reg, creds, pump.Func())
	l.deps = h.deps()
	l.clock = fc
	t.Cleanup(l.Shutdown)

	cam, err := reg.Upsert(Camera{MAC: "ec:71:db:2a:ae:7e", Address: "10.98.0.10"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := creds.Set(cam.MAC, Credential{Username: "admin", Password: "hunter2"}); err != nil {
		t.Fatalf("set credentials: %v", err)
	}
	// Deliberately no EnsureNodes: demand and credentials are both present,
	// but the node itself never appears for the life of this test.

	l.SetContainerConsumers([]string{"com.example.app"})

	fc.blockUntilWaiters(t, 1)
	fc.Advance(1 * time.Second) // retry 1 -> 2
	fc.blockUntilWaiters(t, 1)
	fc.Advance(2 * time.Second) // retry 2 -> 3
	fc.blockUntilWaiters(t, 1)
	fc.Advance(4 * time.Second) // retry 3 -> 4: several retries in, still nothing logs again.

	if n := pump.attemptCount(); n != 0 {
		t.Fatalf("pump invoked %d times for a camera with no node, want 0", n)
	}
	if entries := logs.All(); len(entries) != 1 {
		t.Fatalf("warnings logged = %d across multiple node-not-ready retries, want exactly 1: %v", len(entries), entries)
	}
}

// TestAllocateAuxNodeNumber_TopDownAndSkipsTaken pins the auxiliary node
// allocator's collision avoidance.
//
// Auxiliary nodes (the two-plane camera data path) and registered network
// cameras share one reserved band, because the kernel's VIDEO_NUM_DEVICES of 256
// leaves no room for a second one. They must never be handed the same number: an
// auxiliary node reusing a camera's number would point an app at the wrong
// camera's frames. The allocator therefore works DOWN from the top of the band
// while the registry allocates UP from the bottom, and skips anything already
// taken by a camera or by an existing node.
func TestAllocateAuxNodeNumber_TopDownAndSkipsTaken(t *testing.T) {
	h := newLoopbackHarness()
	h.controlExists = true
	l, reg := newTestLoopback(t, h)

	// A fresh device allocates from the top of the band.
	nr, err := l.AllocateAuxNodeNumber()
	if err != nil {
		t.Fatalf("AllocateAuxNodeNumber: %v", err)
	}
	if nr != IDBandEnd {
		t.Errorf("first allocation = %d, want %d (top of the band)", nr, IDBandEnd)
	}

	// A registered camera's number must be skipped even though nothing has
	// created its node yet: the registry owns that number already.
	cam, err := reg.Upsert(Camera{MAC: "aa:bb:cc:dd:ee:ff", Address: "10.0.0.5"})
	if err != nil {
		t.Fatalf("registry upsert: %v", err)
	}
	h.setNodeExists(IDBandEnd, true) // the node we just allocated now exists
	// Force the camera to the top of the band so the two allocators collide.
	reg.mu.Lock()
	for mac, c := range reg.by {
		c.ID = uint32(IDBandEnd - 1)
		reg.by[mac] = c
	}
	reg.mu.Unlock()
	_ = cam

	nr, err = l.AllocateAuxNodeNumber()
	if err != nil {
		t.Fatalf("AllocateAuxNodeNumber: %v", err)
	}
	if nr == IDBandEnd {
		t.Error("allocator reused a number whose node already exists")
	}
	if nr == IDBandEnd-1 {
		t.Error("allocator handed out a number already held by a registered camera")
	}
	if nr != IDBandEnd-2 {
		t.Errorf("allocation = %d, want %d", nr, IDBandEnd-2)
	}
}

// TestAllocateAuxNodeNumber_BandExhausted pins that a full band is a clean
// refusal rather than a number outside it. The band's ceiling is the kernel's,
// so there is nothing above it to fall back to.
func TestAllocateAuxNodeNumber_BandExhausted(t *testing.T) {
	h := newLoopbackHarness()
	h.controlExists = true
	l, _ := newTestLoopback(t, h)
	for nr := IDBandStart; nr <= IDBandEnd; nr++ {
		h.setNodeExists(nr, true)
	}
	if _, err := l.AllocateAuxNodeNumber(); !errors.Is(err, ErrBandExhausted) {
		t.Errorf("AllocateAuxNodeNumber() error = %v, want ErrBandExhausted", err)
	}
}
