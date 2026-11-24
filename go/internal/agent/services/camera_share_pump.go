package services

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// shareSubscription is the pump's view of its place in a camera's hub.
type shareSubscription interface {
	// Frames closes when the capture ends.
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
	// In this phase the pump is not in the hub.
	shareCannotServe sharePhase = "cannot serve"
)

type shareState struct {
	Phase  sharePhase
	Detail string
}

// shareRetryBackoff is how long the pump waits before joining again after a
// failure; the last value repeats. A variable so tests can shorten it.
var shareRetryBackoff = []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second}

// shareCannotServeRecheck is how long a pump whose camera cannot serve its node
// waits before it looks again. Looking can mean joining, which opens the real
// camera, so this is slow where shareRetryBackoff is fast. A variable so tests
// can shorten it.
var shareCannotServeRecheck = time.Minute

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
	stamp  string
	join   func(ctx context.Context) (shareSubscription, error)
	// onReading, when set, runs each time the pump starts reading for the app
	// (camera_share.go clears a "took the real camera" note then). It is set
	// before Run starts and called without mu.
	onReading func()
	// unserved, when set, must not capture. It is set before Run starts and
	// called without mu.
	unserved func() string

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
	// awaitRandomAccess: the next frame written must be decodable without the
	// frames before it, because the app has none of them (a new subscription)
	// or misses one (a frame that was not written, or one the pump never got).
	awaitRandomAccess bool
	lastSampleID      uint64 // of the subscription's latest frame
	// blocked is why the camera cannot serve the node (cannotServe), or "".
	// While it is set the state is "cannot serve" with it, whether or not an
	// app reads, until a frame is written or another failure takes its place.
	blocked string
	// quick counts rejoins since the last written frame that did not wait for
	// a backoff; past shareQuickRejoins the pump backs off instead.
	quick   int
	retry   <-chan time.Time
	waiting <-chan struct{}
	recheck <-chan time.Time
}

func newSharePump(logger *zap.Logger, app, camera string, node shareNodeIO, mode sharedMode, stamp string, join func(ctx context.Context) (shareSubscription, error)) *sharePump {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &sharePump{
		logger: logger, app: app, camera: camera, node: node, mode: mode, stamp: stamp, join: join,
		nowMono: monotonicNs,
		nowWall: func() int64 { return time.Now().UnixNano() },
		state:   shareState{Phase: shareIdle},
		nudge:   make(chan struct{}, 1),
	}
}

// Nudge tells the pump its camera may be back (a replug, or a /dev watch that
// lost events): a pump waiting out a backoff or a recheck while an app reads
// looks at once instead of when its timer fires. It never blocks, and does
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
	// Before any app reads: the state says why from the start.
	if reason := p.unservedNow(); reason != "" {
		p.cannotServe(ctx, reason)
	}
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
			p.write(ctx, f)
		case <-p.retry:
			p.retry = nil
			if p.leftMeanwhile(ctx, counts) {
				continue
			}
			p.start(ctx)
		case <-p.recheck:
			p.recheck = nil
			if p.reading && p.leftMeanwhile(ctx, counts) {
				continue
			}
			p.lookAgain(ctx)
		case <-p.nudge:
			// Only a backoff or a recheck is cut short: a recording's wait
			// ends on its own, and a pump that is reading or idle with a
			// camera that serves it has nothing to retry.
			if (p.retry == nil || !p.reading) && p.recheck == nil {
				continue
			}
			p.retry, p.recheck = nil, nil
			if p.reading && p.leftMeanwhile(ctx, counts) {
				continue
			}
			p.lookAgain(ctx)
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
// changes nothing: the caller starts. Only for a pump with a reader: it takes
// a pending 1 off the channel.
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

func (p *sharePump) unservedNow() string {
	if p.unserved == nil {
		return ""
	}
	return p.unserved()
}

func (p *sharePump) lookAgain(ctx context.Context) {
	if p.reading {
		p.start(ctx)
		return
	}
	if reason := p.unservedNow(); reason != "" {
		p.cannotServe(ctx, reason)
		return
	}
	p.blocked = ""
	p.setState(shareIdle, "")
}

// start joins the hub; it is called with the count at 1. A camera that cannot
// serve the node is not joined at all. After a capture the pump left because
// it could not serve the node, the state stays "cannot serve" until a frame is
// written (blocked).
func (p *sharePump) start(ctx context.Context) {
	p.retry, p.waiting, p.recheck = nil, nil, nil
	if ctx.Err() != nil {
		return
	}
	if reason := p.unservedNow(); reason != "" {
		p.cannotServe(ctx, reason)
		return
	}
	sub, err := p.join(ctx)
	if err != nil {
		p.joinFailed(ctx, err)
		return
	}
	p.sub, p.frames = sub, sub.Frames()
	p.dropped, p.writeLogged, p.writeFailed, p.mismatch = false, false, false, ""
	p.awaitRandomAccess = true
	if p.blocked == "" {
		p.setState(shareReading, "")
	}
	if p.onReading != nil {
		p.onReading()
	}
}

func (p *sharePump) joinFailed(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	if isNativeRefusal(err) {
		p.cannotServe(ctx, refusalText(err))
		return
	}
	p.blocked = ""
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
		p.setState(shareNoFrames, refusalText(err))
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
	if isNativeRefusal(cerr) {
		p.cannotServe(ctx, refusalText(cerr))
		return
	}
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
	switch {
	case cameraHeldElsewhere(cerr):
		detail = shareCameraHeldDetail
	case cerr != nil:
		detail = refusalText(cerr)
	}
	p.blocked = ""
	p.setState(shareNoFrames, detail)
	p.retry = time.After(d)
}

// refusalText is why a join or a capture failed as the hub worded it, without
// the status code a refusal travels with.
func refusalText(err error) string {
	if refusal, ok := status.FromError(err); ok {
		return refusal.Message()
	}
	return err.Error()
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

// v4l2MonotonicClock is the nativeClock of a frame whose nativeNs the kernel
// took on CLOCK_MONOTONIC at capture, the clock a node's readers compare a
// buffer timestamp with.
const v4l2MonotonicClock = "CLOCK_MONOTONIC_V4L2"

// captureMonoNs is a frame's capture time on the monotonic clock: the kernel's
// when the frame carries one, and otherwise its wall-clock time rebased.
func (p *sharePump) captureMonoNs(f *videoFrame) int64 {
	if f.nativeClock == v4l2MonotonicClock && f.nativeNs > 0 {
		return f.nativeNs
	}
	return p.nowMono() - (p.nowWall() - int64(f.tsNs))
}

func (p *sharePump) stampOf(f *videoFrame) shareFrameStamp {
	if p.stamp == appconfig.CameraStampFrame {
		return stampWithSampleID(f.sampleID)
	}
	return stampWithCaptureTime(p.captureMonoNs(f))
}

// decodableAlone reports whether an app can decode f without the frames
// before it.
func decodableAlone(f *videoFrame) bool {
	if f.rawFmt != nil {
		return true
	}
	_, randomAccess := frameRandomAccess(f)
	return randomAccess
}

// Why a capture that delivers frames of the node's mode still cannot serve it.
const (
	shareNotWholeFramesDetail = "the camera's %s is not arriving as whole frames, so there is none to put in a buffer"
	shareNoSampleIDDetail     = `the camera's frames carry no frame number for the "frame" stamp`
)

// unservable is why f, a frame of the node's mode, shows that this capture
// cannot serve the node, or "".
func (p *sharePump) unservable(f *videoFrame) string {
	switch {
	case p.mode.compressed() && !f.auAligned:
		return fmt.Sprintf(shareNotWholeFramesDetail, describeNodeFrame(f))
	case p.stamp == appconfig.CameraStampFrame && f.sampleID == 0:
		return shareNoSampleIDDetail
	}
	return ""
}

// cannotServe is for a camera that, as it is captured, has no frames for the
// node: the pump leaves the hub if it is in it, holds "cannot serve" with the
// reason, and looks again every shareCannotServeRecheck. The fast backoff is
// for failures that pass.
func (p *sharePump) cannotServe(ctx context.Context, reason string) {
	p.leave()
	if ctx.Err() != nil {
		return
	}
	if p.blocked != reason {
		p.blocked = reason
		p.logger.Warn(fmt.Sprintf("camera share: %s: %s cannot serve its node (%s): %s; it gets no frames until the camera can, checked every %s",
			p.app, p.camera, describeShareSettings(p.mode.Format, p.stamp), reason, shareCannotServeRecheck))
	}
	p.setState(shareCannotServe, reason)
	p.recheck = time.After(shareCannotServeRecheck)
}

// write puts one hub frame on the node. A picture that is not the node's mode
// is dropped, and while pictures are dropped the state says so ("no frames",
// with both formats), not "reading": the app gets nothing. A matching one puts
// it back to "reading". A frame that shows the capture cannot serve the node
// (another codec than the node declares, part of a frame, no frame number for
// a "frame" stamp) ends the subscription (cannotServe).
func (p *sharePump) write(ctx context.Context, f *videoFrame) {
	if f == nil {
		return
	}
	if !p.mode.carries(f) {
		detail := fmt.Sprintf("camera frames are %s, the node declares %s", describeNodeFrame(f), p.mode)
		if p.mode.compressed() {
			p.cannotServe(ctx, detail)
			return
		}
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
	if reason := p.unservable(f); reason != "" {
		p.cannotServe(ctx, reason)
		return
	}
	if sampleGap(p.lastSampleID, f.sampleID) {
		p.awaitRandomAccess = true
	}
	if f.sampleID != 0 {
		p.lastSampleID = f.sampleID
	}
	if p.awaitRandomAccess {
		if !decodableAlone(f) {
			return
		}
		p.awaitRandomAccess = false
	}
	if err := p.node.WriteFrame(f.data, p.stampOf(f)); err != nil {
		if !errors.Is(err, errShareNodeClosed) {
			if !p.writeLogged {
				p.writeLogged = true
				p.logger.Warn(fmt.Sprintf("camera share: %s: writing %s frames to its node failed: %v; frames are not reaching the app", p.app, p.camera, err))
			}
			p.writeFailed, p.mismatch = true, ""
			p.awaitRandomAccess = true
			p.setState(shareNoFrames, err.Error())
		}
		return
	}
	if p.writeFailed || p.mismatch != "" || p.blocked != "" {
		p.writeFailed, p.mismatch, p.blocked = false, "", ""
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
	if p.blocked == "" {
		p.setState(shareIdle, "")
		return
	}
	p.setState(shareCannotServe, p.blocked)
	p.recheck = time.After(shareCannotServeRecheck)
}

// leave drops the subscription and its queued frames.
func (p *sharePump) leave() {
	p.retry, p.waiting, p.recheck = nil, nil, nil
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
