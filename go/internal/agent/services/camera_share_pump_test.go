package services

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

const spWait = 3 * time.Second

type spNodeEvent struct {
	kind   string // "frame" or "blank"
	monoNs int64
	size   int
}

type spFakeNode struct {
	counts chan uint32
	events chan spNodeEvent

	mu       sync.Mutex
	log      []spNodeEvent
	writeErr error
}

func (n *spFakeNode) setWriteErr(err error) {
	n.mu.Lock()
	n.writeErr = err
	n.mu.Unlock()
}

func newSpFakeNode() *spFakeNode {
	return &spFakeNode{counts: make(chan uint32), events: make(chan spNodeEvent, 256)}
}

func (n *spFakeNode) Counts() <-chan uint32 { return n.counts }
func (n *spFakeNode) Close() error          { return nil }
func (n *spFakeNode) record(e spNodeEvent) {
	n.mu.Lock()
	n.log = append(n.log, e)
	n.mu.Unlock()
	n.events <- e
}
func (n *spFakeNode) WriteFrame(data []byte, monoNs int64) error {
	n.mu.Lock()
	err := n.writeErr
	n.mu.Unlock()
	if err != nil {
		return err
	}
	n.record(spNodeEvent{"frame", monoNs, len(data)})
	return nil
}

func (n *spFakeNode) all() []spNodeEvent {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]spNodeEvent(nil), n.log...)
}
func (n *spFakeNode) next(t *testing.T) spNodeEvent {
	t.Helper()
	select {
	case e := <-n.events:
		return e
	case <-time.After(spWait):
		t.Fatal("timed out waiting for a node write")
		return spNodeEvent{}
	}
}
func (n *spFakeNode) send(t *testing.T, c uint32) {
	t.Helper()
	select {
	case n.counts <- c:
	case <-time.After(spWait):
		t.Fatalf("pump did not take count %d", c)
	}
}

type spFakeSub struct {
	frames    chan *videoFrame
	restarted bool
	err       error
	left      chan struct{}
	once      sync.Once
	// onLeave runs inside Leave, before it reports: it lets a test put frames
	// in flight at the instant the pump leaves.
	onLeave func()
}

func newSpFakeSub() *spFakeSub {
	return &spFakeSub{frames: make(chan *videoFrame, 16), left: make(chan struct{})}
}
func (s *spFakeSub) Frames() <-chan *videoFrame { return s.frames }
func (s *spFakeSub) Restarted() bool            { return s.restarted }
func (s *spFakeSub) Err() error                 { return s.err }
func (s *spFakeSub) Leave() {
	s.once.Do(func() {
		if s.onLeave != nil {
			s.onLeave()
		}
		close(s.left)
	})
}

// spJoiner hands out the scripted results of join in order and reports each
// call on joined.
type spJoiner struct {
	joined  chan struct{}
	mu      sync.Mutex
	results []func() (shareSubscription, error)
	calls   int
}

func newSpJoiner(results ...func() (shareSubscription, error)) *spJoiner {
	return &spJoiner{joined: make(chan struct{}, 64), results: results}
}
func spSubResult(s *spFakeSub) func() (shareSubscription, error) {
	return func() (shareSubscription, error) { return s, nil }
}
func spErrResult(err error) func() (shareSubscription, error) {
	return func() (shareSubscription, error) { return nil, err }
}
func (j *spJoiner) join(ctx context.Context) (shareSubscription, error) {
	j.mu.Lock()
	i := j.calls
	j.calls++
	var r func() (shareSubscription, error)
	if i < len(j.results) {
		r = j.results[i]
	}
	j.mu.Unlock()
	j.joined <- struct{}{}
	if r == nil {
		return nil, errors.New("no more scripted joins")
	}
	return r()
}
func (j *spJoiner) count() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.calls
}
func (j *spJoiner) waitJoin(t *testing.T) {
	t.Helper()
	select {
	case <-j.joined:
	case <-time.After(spWait):
		t.Fatal("timed out waiting for a join")
	}
}

func spWaitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(spWait):
		t.Fatalf("timed out waiting for %s", what)
	}
}

var spMode = sharedMode{Width: 1280, Height: 720, Fourcc: "YUYV", BytesPerLine: 2560, FPS: 30}

func spFrame(w, h, bpl uint32, ts uint64) *videoFrame {
	return &videoFrame{
		data: make([]byte, int(bpl)*int(h)),
		tsNs: ts,
		rawFmt: &agentpb.RawFormat{
			Width: w, Height: h, Fourcc: "YUYV", BytesPerLine: bpl,
		},
	}
}

func spModeFrame(ts uint64) *videoFrame { return spFrame(1280, 720, 2560, ts) }

type sharePumpHarness struct {
	pump   *sharePump
	node   *spFakeNode
	logs   *observer.ObservedLogs
	cancel context.CancelFunc
	done   chan struct{}
}

func startSharePump(t *testing.T, j *spJoiner, opts ...func(*sharePump)) *sharePumpHarness {
	t.Helper()
	return startSharePumpBackoff(t, j, []time.Duration{2 * time.Millisecond, 4 * time.Millisecond}, opts...)
}

func startSharePumpBackoff(t *testing.T, j *spJoiner, backoff []time.Duration, opts ...func(*sharePump)) *sharePumpHarness {
	t.Helper()
	old := shareRetryBackoff
	shareRetryBackoff = backoff
	t.Cleanup(func() { shareRetryBackoff = old })
	core, logs := observer.New(zap.DebugLevel)
	node := newSpFakeNode()
	p := newSharePump(zap.New(core), "app1", "cam0", node, spMode, j.join)
	for _, o := range opts {
		o(p)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &sharePumpHarness{pump: p, node: node, logs: logs, cancel: cancel, done: make(chan struct{})}
	go func() { defer close(h.done); p.Run(ctx) }()
	t.Cleanup(h.stop)
	return h
}

func (h *sharePumpHarness) stop() {
	h.cancel()
	<-h.done
}

// spWithin fails unless ch closes within d.
func spWithin(t *testing.T, ch <-chan struct{}, d time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(d):
		t.Fatalf("%s did not happen within %s", what, d)
	}
}

// spStateWithin fails unless the pump is in phase within d.
func (h *sharePumpHarness) stateWithin(t *testing.T, want sharePhase, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if h.pump.State().Phase == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("state is %+v, want %q within %s", h.pump.State(), want, d)
}

func (h *sharePumpHarness) waitState(t *testing.T, want sharePhase) shareState {
	t.Helper()
	deadline := time.Now().Add(spWait)
	for time.Now().Before(deadline) {
		if s := h.pump.State(); s.Phase == want {
			return s
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("state never became %q, is %+v", want, h.pump.State())
	return shareState{}
}

func (h *sharePumpHarness) logged(substr string) int {
	n := 0
	for _, e := range h.logs.All() {
		if strings.Contains(e.Message, substr) {
			n++
		}
	}
	return n
}

func TestSharePump_StartsCaptureOnFirstReaderAndStopsOnLast(t *testing.T) {
	sub := newSpFakeSub()
	j := newSpJoiner(spSubResult(sub))
	h := startSharePump(t, j)

	h.node.send(t, 0)
	h.node.send(t, 1) // the pump has taken the 0 by now
	j.waitJoin(t)
	h.waitState(t, shareReading)
	if got := j.count(); got != 1 {
		t.Fatalf("joins = %d, want 1", got)
	}
	h.node.send(t, 0)
	spWaitClosed(t, sub.left, "Leave")
	h.waitState(t, shareIdle)
}

func TestSharePump_IgnoresRepeatedCounts(t *testing.T) {
	sub := newSpFakeSub()
	j := newSpJoiner(spSubResult(sub))
	h := startSharePump(t, j)

	for i := 0; i < 3; i++ {
		h.node.send(t, 1)
	}
	j.waitJoin(t)
	h.node.send(t, 0)
	h.node.send(t, 0)
	spWaitClosed(t, sub.left, "Leave")
	h.node.send(t, 0)
	if got := j.count(); got != 1 {
		t.Fatalf("joins = %d, want 1", got)
	}
	h.stop()
	if evs := h.node.all(); len(evs) != 0 {
		t.Fatalf("node writes = %v, want none", evs)
	}
}

func TestSharePump_SessionEndDoesNotPublishSyntheticFrames(t *testing.T) {
	sub1, sub2 := newSpFakeSub(), newSpFakeSub()
	j := newSpJoiner(spSubResult(sub1), spSubResult(sub2))
	h := startSharePump(t, j)
	// Two frames are in flight at the instant the pump leaves; they must be
	// discarded, never written after the session ends.
	sub1.onLeave = func() {
		sub1.frames <- spModeFrame(uint64(time.Now().UnixNano()))
		sub1.frames <- spModeFrame(uint64(time.Now().UnixNano()))
	}

	// Ending a session writes no replacement image.
	h.node.send(t, 1)
	j.waitJoin(t)
	sub1.frames <- spModeFrame(uint64(time.Now().UnixNano()))
	if e := h.node.next(t); e.kind != "frame" {
		t.Fatalf("first write = %s, want frame", e.kind)
	}
	h.node.send(t, 0)
	spWaitClosed(t, sub1.left, "Leave")

	// An empty session also writes nothing.
	h.node.send(t, 1)
	j.waitJoin(t)
	h.node.send(t, 0)
	spWaitClosed(t, sub2.left, "Leave")
	h.waitState(t, shareIdle)
	h.stop()

	evs := h.node.all()
	if len(evs) != 1 || evs[0].kind != "frame" {
		t.Fatalf("node wrote synthetic or stale frames after capture stopped: %v", evs)
	}
}

func TestSharePump_DropsFramesOfAnotherSize(t *testing.T) {
	sub := newSpFakeSub()
	j := newSpJoiner(spSubResult(sub))
	h := startSharePump(t, j)

	h.node.send(t, 1)
	j.waitJoin(t)
	for i := 0; i < 3; i++ {
		sub.frames <- spFrame(1920, 1080, 3840, 1)
	}
	// A frame of the node's size marks the point where the others were seen.
	sub.frames <- spModeFrame(uint64(time.Now().UnixNano()))
	if e := h.node.next(t); e.kind != "frame" || e.size != spMode.frameBytes() {
		t.Fatalf("first write = %+v, want the one matching frame", e)
	}
	h.stop()
	if got := len(h.node.all()); got != 1 {
		t.Fatalf("writes = %d, want 1 (the 1920x1080 frames dropped)", got)
	}
	if n := h.logged("camera frames are 1920x1080"); n != 1 {
		t.Fatalf("mismatch log lines = %d, want 1", n)
	}
	if n := h.logged("the node declares 1280x720 YUYV, 30 fps; not writing them"); n != 1 {
		t.Fatalf("mismatch line does not name the node mode (%d)", n)
	}
}

// While every hub frame is dropped for being another mode, the app gets
// nothing, and the state says so with both formats; a matching frame puts it
// back to reading.
func TestSharePump_DroppedFramesShowAsNoFrames(t *testing.T) {
	sub := newSpFakeSub()
	j := newSpJoiner(spSubResult(sub))
	h := startSharePump(t, j)

	h.node.send(t, 1)
	j.waitJoin(t)
	h.waitState(t, shareReading)
	sub.frames <- spFrame(640, 480, 1280, 1)
	got := h.waitState(t, shareNoFrames)
	if want := "camera frames are 640x480 YUYV (1280 bytes/line, 614400 bytes/frame), the node declares 1280x720 YUYV, 30 fps"; got.Detail != want {
		t.Fatalf("state detail = %q, want %q", got.Detail, want)
	}
	sub.frames <- spFrame(640, 480, 1280, 2)
	sub.frames <- spModeFrame(uint64(time.Now().UnixNano()))
	if e := h.node.next(t); e.kind != "frame" {
		t.Fatalf("first write = %+v, want the matching frame", e)
	}
	h.waitState(t, shareReading)
	sub.frames <- spFrame(640, 480, 1280, 3)
	h.waitState(t, shareNoFrames)
	if n := h.logged("not writing them"); n != 1 {
		t.Fatalf("mismatch lines = %d, want 1 per subscription", n)
	}
}

// A nudge (the camera came back) ends a backoff at once; a pump that is
// reading or idle ignores it.
func TestSharePump_ANudgeEndsTheBackoffAtOnce(t *testing.T) {
	sub := newSpFakeSub()
	j := newSpJoiner(spErrResult(status.Error(codes.NotFound, "camera unplugged")), spSubResult(sub))
	h := startSharePumpBackoff(t, j, []time.Duration{time.Hour})

	h.node.send(t, 1)
	j.waitJoin(t)
	h.waitState(t, shareUnplugged)
	h.pump.Nudge()
	j.waitJoin(t)
	h.waitState(t, shareReading)
	h.pump.Nudge() // reading: nothing to retry
	h.pump.Nudge()
	time.Sleep(20 * time.Millisecond)
	h.node.send(t, 0)
	h.waitState(t, shareIdle)
	h.pump.Nudge() // idle: nothing to retry
	time.Sleep(20 * time.Millisecond)
	if got := j.count(); got != 2 {
		t.Fatalf("joins = %d, want 2: the failure, then the nudged retry", got)
	}
}

func TestSharePump_RetriesCaptureWhileAReaderWaits(t *testing.T) {
	sub := newSpFakeSub()
	gate := make(chan struct{})
	j := newSpJoiner(
		spErrResult(errors.New("boom")),
		func() (shareSubscription, error) { <-gate; return nil, errors.New("boom again") },
		spSubResult(sub),
	)
	h := startSharePump(t, j)

	h.node.send(t, 1)
	j.waitJoin(t)
	j.waitJoin(t) // the second join is held at the gate
	if s := h.pump.State(); s.Phase != shareNoFrames || s.Detail != "boom" {
		t.Fatalf("State between retries = %+v, want no frames: boom", s)
	}
	close(gate)
	j.waitJoin(t)
	h.waitState(t, shareReading)
	if got := j.count(); got != 3 {
		t.Fatalf("joins = %d, want 3", got)
	}
	if n := h.logged("retrying in"); n != 2 {
		t.Fatalf("retry log lines = %d, want 2", n)
	}
}

func TestSharePump_RetryStatesByError(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		phase  sharePhase
		detail string
	}{
		{"unplugged", fs.ErrNotExist, shareUnplugged, ""},
		{"not found", status.Error(codes.NotFound, "gone"), shareUnplugged, ""},
		{"in use", errCameraInUse("/dev/video0"), shareNoFrames, shareCameraHeldDetail},
		// A held camera that the agent read through PipeWire instead: the
		// refusal's own wording blames PipeWire, but the cause is the holder.
		{"held, read through PipeWire", pixelRefusal(fixedReason(pipeWireRawWhy), true), shareNoFrames, shareCameraHeldDetail},
		{"no tap for another reason", pixelRefusal(fixedReason("camera streams H.264 natively; raw frames are not offered"), false), shareNoFrames,
			"camera offers no uncompressed frames: camera streams H.264 natively; raw frames are not offered"},
		{"other", errors.New("boom"), shareNoFrames, "boom"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// The same failure every time, so whichever retry the state is
			// read after, it says the same.
			results := make([]func() (shareSubscription, error), 16)
			for i := range results {
				results[i] = spErrResult(c.err)
			}
			j := newSpJoiner(results...)
			h := startSharePump(t, j)
			h.node.send(t, 1)
			j.waitJoin(t)
			if s := h.waitState(t, c.phase); s.Detail != c.detail {
				t.Fatalf("Detail = %q, want %q", s.Detail, c.detail)
			}
		})
	}
}

func TestSharePump_CaptureStoppingRetriesOrRejoinsOnRestart(t *testing.T) {
	sub1 := newSpFakeSub()
	sub1.err = errors.New("device vanished")
	sub2 := newSpFakeSub()
	sub2.restarted = true
	sub3 := newSpFakeSub()
	j := newSpJoiner(spSubResult(sub1), spSubResult(sub2), spSubResult(sub3))
	h := startSharePump(t, j)

	h.node.send(t, 1)
	j.waitJoin(t)
	close(sub1.frames)
	j.waitJoin(t) // after backoff
	spWaitClosed(t, sub1.left, "Leave of the ended subscription")
	if n := h.logged("camera share: app1: capture of cam0 stopped while the app reads: device vanished; retrying in"); n != 1 {
		t.Fatalf("stop log lines = %d, want 1", n)
	}
	close(sub2.frames) // Restarted: joins again with no retry line
	j.waitJoin(t)
	if n := h.logged("retrying in"); n != 1 {
		t.Fatalf("retry lines = %d, want 1", n)
	}
	h.waitState(t, shareReading)
}

// A capture that ends because the camera is held (the hub fell back to
// PipeWire, which has no frames for the node) says the camera is held, while
// the log keeps the refusal as the hub gave it.
func TestSharePump_CaptureEndingOnAHeldCameraSaysItIsHeld(t *testing.T) {
	sub := newSpFakeSub()
	sub.err = pixelRefusal(fixedReason(pipeWireRawWhy), true)
	results := []func() (shareSubscription, error){spSubResult(sub)}
	for range 16 {
		results = append(results, spErrResult(sub.err))
	}
	j := newSpJoiner(results...)
	h := startSharePump(t, j)

	h.node.send(t, 1)
	j.waitJoin(t)
	h.waitState(t, shareReading)
	close(sub.frames)
	if s := h.waitState(t, shareNoFrames); s.Detail != shareCameraHeldDetail {
		t.Fatalf("Detail = %q, want %q", s.Detail, shareCameraHeldDetail)
	}
	if n := h.logged("capture of cam0 stopped while the app reads: " + sub.err.Error()); n != 1 {
		t.Fatalf("stop log lines with the hub's refusal = %d, want 1", n)
	}
}

func TestSharePump_WaitsForARecordingAtOtherParameters(t *testing.T) {
	done := make(chan struct{})
	sub := newSpFakeSub()
	j := newSpJoiner(
		spErrResult(&shareWaitError{"episode capture", "1920x1080 at 30 fps", done}),
		spSubResult(sub),
	)
	h := startSharePump(t, j)

	h.node.send(t, 1)
	j.waitJoin(t)
	s := h.waitState(t, shareWaiting)
	want := "held by episode capture at 1920x1080 at 30 fps"
	if s.Detail != want {
		t.Fatalf("Detail = %q, want %q", s.Detail, want)
	}
	if n := h.logged("camera share: app1 waits: cam0 is " + want + "; its node gets frames when the camera is free"); n != 1 {
		t.Fatalf("waiting log lines = %d, want 1", n)
	}
	if got := j.count(); got != 1 {
		t.Fatalf("joined again before the camera was free (%d joins)", got)
	}
	close(done)
	j.waitJoin(t)
	h.waitState(t, shareReading)
}

func TestSharePump_StampsCaptureTimeOnTheMonotonicClock(t *testing.T) {
	sub := newSpFakeSub()
	j := newSpJoiner(spSubResult(sub))
	const nowMono, nowWall = int64(5_000_000_000), int64(1_700_000_000_000_000_000)
	h := startSharePump(t, j, func(p *sharePump) {
		p.nowMono = func() int64 { return nowMono }
		p.nowWall = func() int64 { return nowWall }
	})

	h.node.send(t, 1)
	j.waitJoin(t)
	sub.frames <- spModeFrame(uint64(nowWall - 40_000_000))
	e := h.node.next(t)
	want := nowMono - 40_000_000
	if d := e.monoNs - want; d < -2_000_000 || d > 2_000_000 {
		t.Fatalf("monoNs = %d, want %d +/- 2ms", e.monoNs, want)
	}
}

func TestSharePump_IdleNodeNeverPublishesSyntheticFrames(t *testing.T) {
	h := startSharePump(t, newSpJoiner())
	h.node.send(t, 0)
	h.node.send(t, 0)
	h.stop()
	if got := len(h.node.all()); got != 0 {
		t.Fatalf("idle node writes = %d, want none", got)
	}
}

func TestSharePump_RunReturnsWhenCountsClose(t *testing.T) {
	h := startSharePump(t, newSpJoiner())
	close(h.node.counts)
	spWaitClosed(t, h.done, "Run to return")
}

func TestSharePump_ZeroDuringBackoffStartsNoFurtherJoin(t *testing.T) {
	j := newSpJoiner(spErrResult(errors.New("boom")))
	h := startSharePumpBackoff(t, j, []time.Duration{150 * time.Millisecond})
	h.node.send(t, 1)
	j.waitJoin(t)
	h.waitState(t, shareNoFrames)
	h.node.send(t, 0)
	h.stateWithin(t, shareIdle, 100*time.Millisecond)
	time.Sleep(300 * time.Millisecond) // two backoff periods
	if got := j.count(); got != 1 {
		t.Fatalf("joins = %d, want 1: the camera started for an app that left", got)
	}
}

func TestSharePump_ZeroDuringARecordingWaitStartsNoFurtherJoin(t *testing.T) {
	done := make(chan struct{})
	j := newSpJoiner(spErrResult(&shareWaitError{"episode capture", "1920x1080 at 30 fps", done}))
	h := startSharePump(t, j)
	h.node.send(t, 1)
	j.waitJoin(t)
	h.waitState(t, shareWaiting)
	h.node.send(t, 0)
	h.stateWithin(t, shareIdle, 100*time.Millisecond)
	close(done)
	time.Sleep(50 * time.Millisecond)
	if got := j.count(); got != 1 {
		t.Fatalf("joins = %d, want 1: the camera started for an app that left", got)
	}
}

func TestSharePump_ZeroDuringASubscriptionLeavesWithin100ms(t *testing.T) {
	sub := newSpFakeSub()
	j := newSpJoiner(spSubResult(sub))
	h := startSharePump(t, j)
	h.node.send(t, 1)
	j.waitJoin(t)
	h.waitState(t, shareReading)
	h.node.send(t, 0)
	spWithin(t, sub.left, 100*time.Millisecond, "Leave")
}

func TestSharePump_APendingZeroBeatsAFiredTimer(t *testing.T) {
	j := newSpJoiner()
	node := newSpFakeNode()
	node.counts = make(chan uint32, 1)
	p := newSharePump(zap.NewNop(), "app1", "cam0", node, spMode, j.join)
	p.reading = true
	node.counts <- 0
	if !p.leftMeanwhile(context.Background(), node.counts) {
		t.Fatal("a pending 0 did not stop the timer's start")
	}
	if p.reading || j.count() != 0 {
		t.Fatalf("reading = %v, joins = %d", p.reading, j.count())
	}
	node.counts <- 1
	if p.leftMeanwhile(context.Background(), node.counts) {
		t.Fatal("a pending 1 must not stop the start")
	}
}

func TestSharePump_ARepeatedlyClosedWaitFallsBackToBackoff(t *testing.T) {
	done := make(chan struct{})
	close(done)
	werr := &shareWaitError{"episode capture", "1920x1080 at 30 fps", done}
	var results []func() (shareSubscription, error)
	for i := 0; i < 20; i++ {
		results = append(results, spErrResult(werr))
	}
	j := newSpJoiner(results...)
	h := startSharePumpBackoff(t, j, []time.Duration{400 * time.Millisecond})
	h.node.send(t, 1)
	for i := 0; i < 1+shareQuickRejoins; i++ {
		j.waitJoin(t)
	}
	time.Sleep(100 * time.Millisecond)
	if got := j.count(); got != 1+shareQuickRejoins {
		t.Fatalf("joins = %d, want %d before the backoff", got, 1+shareQuickRejoins)
	}
}

func TestSharePump_ARestartThatNeverDeliversFallsBackToBackoff(t *testing.T) {
	var results []func() (shareSubscription, error)
	for i := 0; i < 20; i++ {
		results = append(results, func() (shareSubscription, error) {
			s := newSpFakeSub()
			s.restarted = true
			close(s.frames)
			return s, nil
		})
	}
	j := newSpJoiner(results...)
	h := startSharePumpBackoff(t, j, []time.Duration{400 * time.Millisecond})
	h.node.send(t, 1)
	for i := 0; i < 1+shareQuickRejoins; i++ {
		j.waitJoin(t)
	}
	time.Sleep(100 * time.Millisecond)
	if got := j.count(); got != 1+shareQuickRejoins {
		t.Fatalf("joins = %d, want %d before the backoff", got, 1+shareQuickRejoins)
	}
	if n := h.logged("retrying in"); n != 1 {
		t.Fatalf("retry lines = %d, want 1", n)
	}
}

func TestSharePump_ACancelledJoinIsQuiet(t *testing.T) {
	started := make(chan struct{})
	j := newSpJoiner(func() (shareSubscription, error) {
		close(started)
		time.Sleep(20 * time.Millisecond)
		return nil, context.Canceled
	})
	h := startSharePump(t, j)
	h.node.send(t, 1)
	spWaitClosed(t, started, "join to start")
	h.cancel()
	<-h.done
	if n := len(h.logs.FilterLevelExact(zap.WarnLevel).All()); n != 0 {
		t.Fatalf("warnings = %d, want none", n)
	}
	if s := h.pump.State(); s.Phase == shareNoFrames {
		t.Fatalf("state = %+v after cancel", s)
	}
}

func TestSharePump_NodeWriteFailuresAreLoggedOnceAndShownInState(t *testing.T) {
	sub := newSpFakeSub()
	j := newSpJoiner(spSubResult(sub))
	h := startSharePump(t, j)
	h.node.send(t, 1)
	j.waitJoin(t)
	h.node.setWriteErr(errors.New("QBUF: no buffers"))
	for i := 0; i < 3; i++ {
		sub.frames <- spModeFrame(uint64(time.Now().UnixNano()))
	}
	s := h.waitState(t, shareNoFrames)
	if s.Detail != "QBUF: no buffers" {
		t.Fatalf("Detail = %q", s.Detail)
	}
	h.node.setWriteErr(nil)
	sub.frames <- spModeFrame(uint64(time.Now().UnixNano()))
	h.node.next(t) // frames are handled in order: every failure came before this write
	h.waitState(t, shareReading)
	n := 0
	for _, e := range h.logs.FilterLevelExact(zap.WarnLevel).All() {
		if strings.Contains(e.Message, "writing cam0 frames to its node failed: QBUF: no buffers") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("write-failure warnings = %d, want 1", n)
	}
}

func TestSharePump_CaptureStoppedWithoutAnErrorSaysSo(t *testing.T) {
	sub := newSpFakeSub()
	j := newSpJoiner(spSubResult(sub))
	h := startSharePumpBackoff(t, j, []time.Duration{time.Hour})
	h.node.send(t, 1)
	j.waitJoin(t)
	close(sub.frames)
	s := h.waitState(t, shareNoFrames)
	if s.Detail != "capture stopped" {
		t.Fatalf("Detail = %q", s.Detail)
	}
	if n := h.logged("while the app reads: capture stopped; retrying in"); n != 1 {
		t.Fatalf("log lines = %d, want 1", n)
	}
}
