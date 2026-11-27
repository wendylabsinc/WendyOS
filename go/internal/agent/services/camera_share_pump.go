package services

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// shareSubscription is the pump's view of its place in a camera's hub.
type shareSubscription interface {
	// Frames delivers decoded pixel frames; it closes when the capture ends.
	Frames() <-chan *videoFrame
	// Restarted reports that the capture ended because the hub restarted it
	// (so a join at once will succeed), not because it failed.
	Restarted() bool
	// Err is why the capture ended, once Frames is closed.
	Err() error
	// Leave gives up the subscription; it is idempotent.
	Leave()
}

// shareWaitError is what a join returns when the camera is held at other
// parameters (a recording). Done closes when the camera is free.
type shareWaitError struct {
	Holder, Params string
	Done           <-chan struct{}
}

func (e *shareWaitError) Error() string { return "held by " + e.Holder + " at " + e.Params }

type sharePhase string

const (
	shareIdle      sharePhase = "idle"
	shareReading   sharePhase = "reading"
	shareWaiting   sharePhase = "waiting"
	shareNoFrames  sharePhase = "no frames"
	shareUnplugged sharePhase = "camera unplugged"
)

type shareState struct {
	Phase  sharePhase
	Detail string
}

// shareRetryBackoff is how long the pump waits before joining again after a
// failure; the last value repeats. A variable so tests can shorten it.
var shareRetryBackoff = []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second}

// shareQuickRejoins is how many consecutive rejoins without a written frame
// happen at once (a restart, a wait that ends) before the pump backs off.
const shareQuickRejoins = 3

// sharePump turns one share node's reader count into capture on demand: it
// joins the camera's hub while an app reads the node and leaves when none does.
// All of its state is owned by Run's goroutine; mu guards only what State reads.
type sharePump struct {
	logger *zap.Logger
	app    string
	camera string
	node   shareNodeIO
	mode   sharedMode
	join   func(ctx context.Context) (shareSubscription, error)
	// onReading, when set, runs each time the pump starts reading for the app
	// (camera_share.go clears a "took the real camera" note then). It is set
	// before Run starts and called without mu.
	onReading func()

	// Clocks, replaceable in tests.
	nowMono func() int64
	nowWall func() int64

	mu    sync.Mutex
	state shareState

	// nudge asks Run to end a backoff at once (Nudge); buffered, so a nudge
	// never blocks the caller and several in a row count once.
	nudge chan struct{}

	// Owned by Run.
	reading bool // the node's latest count is 1
	sub     shareSubscription
	frames  <-chan *videoFrame
	dropped bool // the size-mismatch line was logged for this subscription
	// mismatch is the state detail while the hub's frames are not the node's
	// mode and are dropped ("" while they match), so the state is set once per
	// change rather than once per frame.
	mismatch string
	attempt  int // consecutive failures, indexes the backoff
	// writeLogged: a node write failure was logged for this subscription.
	// writeFailed: the latest write failed (state is "no frames").
	writeLogged, writeFailed bool
	// quick counts rejoins since the last written frame that did not wait for
	// a backoff; past shareQuickRejoins the pump backs off instead.
	quick   int
	retry   <-chan time.Time
	waiting <-chan struct{}
}

func newSharePump(logger *zap.Logger, app, camera string, node shareNodeIO, mode sharedMode, join func(ctx context.Context) (shareSubscription, error)) *sharePump {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &sharePump{
		logger: logger, app: app, camera: camera, node: node, mode: mode, join: join,
		nowMono: monotonicNs,
		nowWall: func() int64 { return time.Now().UnixNano() },
		state:   shareState{Phase: shareIdle},
		nudge:   make(chan struct{}, 1),
	}
}

// Nudge tells the pump its camera may be back (a replug, or a /dev watch that
// lost events): a pump waiting out a backoff while an app reads joins at once
// instead of after up to the last backoff step. It never blocks, and does
// nothing to a pump that is reading, idle, or waiting for a recording.
func (p *sharePump) Nudge() {
	select {
	case p.nudge <- struct{}{}:
	default:
	}
}

func monotonicNs() int64 {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0
	}
	return ts.Nano()
}

// State is the pump's phase, for the camera listing.
func (p *sharePump) State() shareState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

func (p *sharePump) setState(phase sharePhase, detail string) {
	p.mu.Lock()
	p.state = shareState{Phase: phase, Detail: detail}
	p.mu.Unlock()
}

// Run drives the pump until ctx ends or the node's counts channel closes.
func (p *sharePump) Run(ctx context.Context) {
	defer p.leave()
	counts := p.node.Counts()
	for {
		select {
		case <-ctx.Done():
			return
		case n, ok := <-counts:
			if !ok {
				return
			}
			p.handleCount(ctx, n)
		case f, ok := <-p.frames:
			if !ok {
				p.captureEnded(ctx)
				continue
			}
			p.write(f)
		case <-p.retry:
			p.retry = nil
			if p.leftMeanwhile(ctx, counts) {
				continue
			}
			p.start(ctx)
		case <-p.nudge:
			// Only a backoff is cut short: a recording's wait ends on its
			// own, and a pump that is reading or idle has nothing to retry.
			if p.retry == nil || !p.reading {
				continue
			}
			p.retry = nil
			if p.leftMeanwhile(ctx, counts) {
				continue
			}
			p.start(ctx)
		case <-p.waiting:
			p.waiting = nil
			if p.leftMeanwhile(ctx, counts) {
				continue
			}
			if p.quick >= shareQuickRejoins {
				p.retry = time.After(p.nextDelay())
				continue
			}
			p.quick++
			p.start(ctx)
		}
	}
}

// leftMeanwhile reports whether a 0 was already pending when a timer fired, in
// which case it has been handled and the camera must not start. A pending 1
// changes nothing: the caller starts.
func (p *sharePump) leftMeanwhile(ctx context.Context, counts <-chan uint32) bool {
	select {
	case n, ok := <-counts:
		if !ok {
			p.leave()
			// Run's next receive on the closed channel ends it.
			return true
		}
		if n == 0 {
			p.handleCount(ctx, n)
			return true
		}
	default:
	}
	return false
}

func (p *sharePump) handleCount(ctx context.Context, n uint32) {
	want := n > 0
	if want == p.reading {
		return
	}
	p.reading = want
	if want {
		p.start(ctx)
	} else {
		p.stop()
	}
}

// start joins the hub; it is called with the count at 1.
func (p *sharePump) start(ctx context.Context) {
	p.retry, p.waiting = nil, nil
	if ctx.Err() != nil {
		return
	}
	sub, err := p.join(ctx)
	if err != nil {
		p.joinFailed(ctx, err)
		return
	}
	p.sub, p.frames = sub, sub.Frames()
	p.dropped, p.writeLogged, p.writeFailed, p.mismatch = false, false, false, ""
	p.setState(shareReading, "")
	if p.onReading != nil {
		p.onReading()
	}
}

func (p *sharePump) joinFailed(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	var wait *shareWaitError
	switch {
	case errors.As(err, &wait):
		p.logger.Info(fmt.Sprintf("camera share: %s waits: %s is %s; its node gets frames when the camera is free", p.app, p.camera, wait.Error()))
		p.setState(shareWaiting, wait.Error())
		p.waiting = wait.Done
		return
	case cameraHeldElsewhere(err):
		p.setState(shareNoFrames, shareCameraHeldDetail)
	case status.Code(err) == codes.NotFound || errors.Is(err, fs.ErrNotExist):
		p.setState(shareUnplugged, "")
	default:
		p.setState(shareNoFrames, err.Error())
	}
	d := p.nextDelay()
	p.logger.Warn(fmt.Sprintf("camera share: %s: joining %s failed while the app reads: %v; retrying in %s", p.app, p.camera, err, d))
	p.retry = time.After(d)
}

func (p *sharePump) nextDelay() time.Duration {
	b := shareRetryBackoff
	if len(b) == 0 {
		return time.Second
	}
	i := p.attempt
	if i >= len(b) {
		i = len(b) - 1
	}
	p.attempt++
	return b[i]
}

// captureEnded handles the frames channel closing while the count is 1.
func (p *sharePump) captureEnded(ctx context.Context) {
	sub := p.sub
	p.sub, p.frames, p.mismatch = nil, nil, ""
	if sub == nil {
		return
	}
	restarted, cerr := sub.Restarted(), sub.Err()
	sub.Leave()
	if restarted && p.quick < shareQuickRejoins {
		p.quick++
		p.start(ctx)
		return
	}
	d := p.nextDelay()
	detail := "capture stopped"
	if cerr != nil {
		detail = cerr.Error()
	}
	p.logger.Warn(fmt.Sprintf("camera share: %s: capture of %s stopped while the app reads: %s; retrying in %s", p.app, p.camera, detail, d))
	if cameraHeldElsewhere(cerr) {
		detail = shareCameraHeldDetail
	}
	p.setState(shareNoFrames, detail)
	p.retry = time.After(d)
}

// shareCameraHeldDetail is the state detail while something other than the
// agent holds the real camera, so the agent cannot read it for the node.
const shareCameraHeldDetail = `the real camera is held by another app ("mode": "direct") or process`

// cameraHeldElsewhere reports whether a join or a capture failed because
// something else holds the real camera: the agent could not open it at all, or
// read it through PipeWire, which has no frames for a node. The second reaches
// the pump as a pixel refusal that names PipeWire, not the holder.
func cameraHeldElsewhere(err error) bool {
	return err != nil && (isCameraInUse(err) || errors.Is(err, errPixelsCameraHeld))
}

// write puts one hub frame on the node. A frame that is not the node's mode is
// dropped, and while frames are dropped the state says so ("no frames", with
// both formats), not "reading": the app gets nothing. A matching frame puts it
// back to "reading".
func (p *sharePump) write(f *videoFrame) {
	if f == nil {
		return
	}
	rf := f.rawFmt
	if rf == nil || rf.GetWidth() != p.mode.Width || rf.GetHeight() != p.mode.Height ||
		strings.TrimSpace(rf.GetFourcc()) != strings.TrimSpace(p.mode.Fourcc) ||
		rf.GetBytesPerLine() != p.mode.BytesPerLine {
		detail := fmt.Sprintf("camera frames are %s, the node declares %s", describeRawFormat(rf), p.mode)
		if !p.dropped {
			p.dropped = true
			p.logger.Warn(fmt.Sprintf("camera share: %s: %s; not writing them", p.app, detail))
		}
		if p.mismatch != detail {
			p.mismatch = detail
			p.setState(shareNoFrames, detail)
		}
		return
	}
	mono := p.nowMono() - (p.nowWall() - int64(f.tsNs))
	if err := p.node.WriteFrame(f.data, mono); err != nil {
		if !errors.Is(err, errShareNodeClosed) {
			if !p.writeLogged {
				p.writeLogged = true
				p.logger.Warn(fmt.Sprintf("camera share: %s: writing %s frames to its node failed: %v; frames are not reaching the app", p.app, p.camera, err))
			}
			p.writeFailed, p.mismatch = true, ""
			p.setState(shareNoFrames, err.Error())
		}
		return
	}
	if p.writeFailed || p.mismatch != "" {
		p.writeFailed, p.mismatch = false, ""
		p.setState(shareReading, "")
	}
	p.attempt = 0
	p.quick = 0
}

// stop leaves the capture and discards queued frames. The node starts its next
// reader at a fresh producer frame, so no synthetic placeholder is published.
func (p *sharePump) stop() {
	p.leave()
	p.attempt = 0
	p.quick = 0
	p.writeFailed, p.mismatch = false, ""
	p.setState(shareIdle, "")
}

// leave drops the subscription and its queued frames.
func (p *sharePump) leave() {
	p.retry, p.waiting = nil, nil
	sub, frames := p.sub, p.frames
	p.sub, p.frames = nil, nil
	if sub == nil {
		return
	}
	sub.Leave()
	for frames != nil {
		select {
		case _, ok := <-frames:
			if !ok {
				return
			}
		default:
			return
		}
	}
}
