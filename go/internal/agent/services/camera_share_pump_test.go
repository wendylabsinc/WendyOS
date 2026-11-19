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

	mu  sync.Mutex
	log []spNodeEvent
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
	n.record(spNodeEvent{"frame", monoNs, len(data)})
	return nil
}
func (n *spFakeNode) WriteBlank(monoNs int64) error {
	n.record(spNodeEvent{"blank", monoNs, 0})
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
}

func newSpFakeSub() *spFakeSub {
	return &spFakeSub{frames: make(chan *videoFrame, 16), left: make(chan struct{})}
}
func (s *spFakeSub) Frames() <-chan *videoFrame { return s.frames }
func (s *spFakeSub) Restarted() bool            { return s.restarted }
func (s *spFakeSub) Err() error                 { return s.err }
func (s *spFakeSub) Leave()                     { s.once.Do(func() { close(s.left) }) }

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

func startSharePump(t *testing.T, j *spJoiner, adopted bool) *sharePumpHarness {
	t.Helper()
	old := shareRetryBackoff
	shareRetryBackoff = []time.Duration{2 * time.Millisecond, 4 * time.Millisecond}
	t.Cleanup(func() { shareRetryBackoff = old })
	core, logs := observer.New(zap.DebugLevel)
	node := newSpFakeNode()
	p := newSharePump(zap.New(core), "app1", "cam0", node, spMode, j.join, adopted)
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
	h := startSharePump(t, j, false)

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
	h := startSharePump(t, j, false)

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

func TestSharePump_WritesOneBlankAfterASessionThatWroteFrames(t *testing.T) {
	sub1, sub2 := newSpFakeSub(), newSpFakeSub()
	j := newSpJoiner(spSubResult(sub1), spSubResult(sub2))
	h := startSharePump(t, j, false)

	// A session that wrote a frame ends in exactly one blank.
	h.node.send(t, 1)
	j.waitJoin(t)
	sub1.frames <- spModeFrame(uint64(time.Now().UnixNano()))
	if e := h.node.next(t); e.kind != "frame" {
		t.Fatalf("first write = %s, want frame", e.kind)
	}
	// Frames still queued when the count drops must never reach the node.
	sub1.frames <- spModeFrame(uint64(time.Now().UnixNano()))
	sub1.frames <- spModeFrame(uint64(time.Now().UnixNano()))
	h.node.send(t, 0)
	spWaitClosed(t, sub1.left, "Leave")

	// An empty session leaves no blank.
	h.node.send(t, 1)
	j.waitJoin(t)
	h.node.send(t, 0)
	spWaitClosed(t, sub2.left, "Leave")
	h.waitState(t, shareIdle)
	h.stop()

	evs := h.node.all()
	blanks := 0
	blankAt := -1
	for i, e := range evs {
		if e.kind == "blank" {
			blanks++
			blankAt = i
		}
	}
	if blanks != 1 {
		t.Fatalf("blanks = %d in %v, want 1", blanks, evs)
	}
	if blankAt != len(evs)-1 {
		t.Fatalf("a write followed the blank: %v", evs)
	}
}

func TestSharePump_DropsFramesOfAnotherSize(t *testing.T) {
	sub := newSpFakeSub()
	j := newSpJoiner(spSubResult(sub))
	h := startSharePump(t, j, false)

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

func TestSharePump_RetriesCaptureWhileAReaderWaits(t *testing.T) {
	sub := newSpFakeSub()
	j := newSpJoiner(spErrResult(errors.New("boom")), spErrResult(errors.New("boom again")), spSubResult(sub))
	h := startSharePump(t, j, false)

	h.node.send(t, 1)
	for i := 0; i < 3; i++ {
		j.waitJoin(t)
	}
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
		name  string
		err   error
		phase sharePhase
	}{
		{"unplugged", fs.ErrNotExist, shareUnplugged},
		{"other", errors.New("boom"), shareNoFrames},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			j := newSpJoiner(spErrResult(c.err))
			h := startSharePump(t, j, false)
			h.node.send(t, 1)
			j.waitJoin(t)
			h.waitState(t, c.phase)
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
	h := startSharePump(t, j, false)

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

func TestSharePump_WaitsForARecordingAtOtherParameters(t *testing.T) {
	done := make(chan struct{})
	sub := newSpFakeSub()
	j := newSpJoiner(
		spErrResult(&shareWaitError{"episode capture", "1920x1080 at 30 fps", done}),
		spSubResult(sub),
	)
	h := startSharePump(t, j, false)

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
	h := startSharePump(t, j, false)
	const nowMono, nowWall = int64(5_000_000_000), int64(1_700_000_000_000_000_000)
	h.pump.nowMono = func() int64 { return nowMono }
	h.pump.nowWall = func() int64 { return nowWall }

	h.node.send(t, 1)
	j.waitJoin(t)
	sub.frames <- spModeFrame(uint64(nowWall - 40_000_000))
	e := h.node.next(t)
	want := nowMono - 40_000_000
	if d := e.monoNs - want; d < -2_000_000 || d > 2_000_000 {
		t.Fatalf("monoNs = %d, want %d +/- 2ms", e.monoNs, want)
	}
}

func TestSharePump_AdoptedIdleNodeGetsABlankFrame(t *testing.T) {
	j := newSpJoiner()
	h := startSharePump(t, j, true)
	h.node.send(t, 0)
	if e := h.node.next(t); e.kind != "blank" {
		t.Fatalf("write = %s, want blank", e.kind)
	}
	h.node.send(t, 0)
	h.stop()
	if got := len(h.node.all()); got != 1 {
		t.Fatalf("writes = %d, want exactly one blank", got)
	}
}

func TestSharePump_NotAdoptedIdleNodeGetsNoBlank(t *testing.T) {
	h := startSharePump(t, newSpJoiner(), false)
	h.node.send(t, 0)
	h.node.send(t, 0)
	h.stop()
	if got := len(h.node.all()); got != 0 {
		t.Fatalf("writes = %d, want none", got)
	}
}

func TestSharePump_RunReturnsWhenCountsClose(t *testing.T) {
	h := startSharePump(t, newSpJoiner(), false)
	close(h.node.counts)
	spWaitClosed(t, h.done, "Run to return")
}
