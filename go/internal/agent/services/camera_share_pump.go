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

// sharePump turns one share node's reader count into capture on demand: it
// joins the camera's hub while an app reads the node and leaves when none does.
// All of its state is owned by Run's goroutine; mu guards only what State reads.
type sharePump struct {
	logger  *zap.Logger
	app     string
	camera  string
	node    shareNodeIO
	mode    sharedMode
	join    func(ctx context.Context) (shareSubscription, error)
	adopted bool

	// Clocks, replaceable in tests.
	nowMono func() int64
	nowWall func() int64

	mu    sync.Mutex
	state shareState

	// Owned by Run.
	reading bool // the node's latest count is 1
	sub     shareSubscription
	frames  <-chan *videoFrame
	wrote   bool // this session wrote a camera frame
	dropped bool // the size-mismatch line was logged this session
	attempt int  // consecutive failures, indexes the backoff
	retry   <-chan time.Time
	waiting <-chan struct{}
}

func newSharePump(logger *zap.Logger, app, camera string, node shareNodeIO, mode sharedMode, join func(ctx context.Context) (shareSubscription, error), adopted bool) *sharePump {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &sharePump{
		logger: logger, app: app, camera: camera, node: node, mode: mode, join: join, adopted: adopted,
		nowMono: monotonicNs,
		nowWall: func() int64 { return time.Now().UnixNano() },
		state:   shareState{Phase: shareIdle},
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
	first := true
	for {
		select {
		case <-ctx.Done():
			return
		case n, ok := <-counts:
			if !ok {
				return
			}
			want := n > 0
			if first {
				first = false
				if !want && p.adopted {
					p.blank()
				}
			}
			if want == p.reading {
				continue
			}
			p.reading = want
			if want {
				p.start(ctx)
			} else {
				p.stop()
			}
		case f, ok := <-p.frames:
			if !ok {
				p.captureEnded(ctx)
				continue
			}
			p.write(f)
		case <-p.retry:
			p.retry = nil
			p.start(ctx)
		case <-p.waiting:
			p.waiting = nil
			p.start(ctx)
		}
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
		p.joinFailed(err)
		return
	}
	p.sub, p.frames = sub, sub.Frames()
	p.dropped = false
	p.setState(shareReading, "")
}

func (p *sharePump) joinFailed(err error) {
	var wait *shareWaitError
	switch {
	case errors.As(err, &wait):
		p.logger.Info(fmt.Sprintf("camera share: %s waits: %s is %s; its node gets frames when the camera is free", p.app, p.camera, wait.Error()))
		p.setState(shareWaiting, wait.Error())
		p.waiting = wait.Done
		return
	case isCameraInUse(err):
		p.setState(shareNoFrames, "the real camera is held outside the agent")
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
	p.sub, p.frames = nil, nil
	if sub == nil {
		return
	}
	restarted, cerr := sub.Restarted(), sub.Err()
	sub.Leave()
	if restarted {
		p.start(ctx)
		return
	}
	d := p.nextDelay()
	detail := "capture stopped"
	if cerr != nil {
		detail = cerr.Error()
	}
	p.setState(shareNoFrames, detail)
	p.logger.Warn(fmt.Sprintf("camera share: %s: capture of %s stopped while the app reads: %v; retrying in %s", p.app, p.camera, cerr, d))
	p.retry = time.After(d)
}

// write puts one hub frame on the node.
func (p *sharePump) write(f *videoFrame) {
	if f == nil {
		return
	}
	rf := f.rawFmt
	if rf == nil || rf.GetWidth() != p.mode.Width || rf.GetHeight() != p.mode.Height ||
		strings.TrimSpace(rf.GetFourcc()) != strings.TrimSpace(p.mode.Fourcc) ||
		rf.GetBytesPerLine() != p.mode.BytesPerLine {
		if !p.dropped {
			p.dropped = true
			p.logger.Warn(fmt.Sprintf("camera share: %s: camera frames are %s, the node declares %s; not writing them",
				p.app, describeRawFormat(rf), p.mode))
		}
		return
	}
	mono := p.nowMono() - (p.nowWall() - int64(f.tsNs))
	if err := p.node.WriteFrame(f.data, mono); err != nil {
		if !errors.Is(err, errShareNodeClosed) {
			p.logger.Debug(fmt.Sprintf("camera share: %s: write to the node failed: %v", p.app, err))
		}
		return
	}
	p.wrote = true
	p.attempt = 0
}

// stop is the count going to 0: leave, discard what is queued, and replace a
// stale last frame with black.
func (p *sharePump) stop() {
	p.leave()
	if p.wrote {
		p.blank()
	}
	p.wrote = false
	p.attempt = 0
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

func (p *sharePump) blank() {
	if err := p.node.WriteBlank(p.nowMono()); err != nil && !errors.Is(err, errShareNodeClosed) {
		p.logger.Debug(fmt.Sprintf("camera share: %s: blank frame failed: %v", p.app, err))
	}
}
