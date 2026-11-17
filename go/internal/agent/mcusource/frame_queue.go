package mcusource

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
)

const sensorFIFOFrames = 8

// cameraRunFrames bounds the frames one camera may have pending at once.
const cameraRunFrames = 8

// cameraMaxWait is how long a pending camera frame may wait for the consumer
// before a newer frame of that camera replaces it. It is shorter than a frame
// interval at 30 fps, so a consumer that falls a frame behind resumes at the
// newest frame, while frames that only arrive together are all delivered.
const cameraMaxWait = 25 * time.Millisecond

// frameSink accepts complete frames without blocking the transport reader.
// offer reports how many frames it discarded, including superseded camera
// frames. The transport serializes offer with finish.
type frameSink interface {
	offer(*sensorlink.SensorFrame) int
	finish()
}

type channelFrameSink chan *sensorlink.SensorFrame

func (s channelFrameSink) offer(frame *sensorlink.SensorFrame) int {
	select {
	case s <- frame:
		return 0
	default:
		return 1
	}
}
func (s channelFrameSink) finish() { close(s) }

// liveFrameTransport lets the pairing supervisor choose a queue after it has
// validated the manifest and mounted its live sensors. Ordinary Stream callers
// retain their existing FIFO policy.
type liveFrameTransport interface {
	streamFrames(context.Context, []uint32, frameSink) (func() error, error)
}

// cameraFrameQueue gives each MJPEG camera one turn in the queue, alongside
// the existing eight-frame FIFO for audio and dependent video codecs. A turn
// holds a short run of frames, so frames that arrive together, as after a
// network stall, are all delivered. A frame that has waited longer than
// cameraMaxWait means the consumer is behind, and the camera's next frame
// replaces it. Replacing within the turn preserves fairness between cameras.
// Receiving under the same lock avoids holding a stale frame in a blocked
// channel send.
type cameraFrameQueue struct {
	mu      sync.Mutex
	latest  map[uint32]bool
	pending []*frameRun
	fifo    int
	closed  bool
	wake    chan struct{}
}

// frameRun is one channel's turn in the queue: a single FIFO frame, or the
// frames a camera has waiting, oldest first.
type frameRun struct {
	channel uint32
	frames  []pendingFrame
}

type pendingFrame struct {
	frame   *sensorlink.SensorFrame
	arrived time.Time
}

// makeRoom discards the frames that a frame arriving now supersedes: those
// that have waited too long, or the oldest one when the run is full.
func (r *frameRun) makeRoom(now time.Time) int {
	discarded := 0
	for discarded < len(r.frames) && now.Sub(r.frames[discarded].arrived) > cameraMaxWait {
		discarded++
	}
	if discarded == 0 && len(r.frames) == cameraRunFrames {
		discarded = 1
	}
	r.frames = slices.Delete(r.frames, 0, discarded)
	return discarded
}

func newCameraFrameQueue(channels []uint32) *cameraFrameQueue {
	q := &cameraFrameQueue{latest: make(map[uint32]bool), wake: make(chan struct{}, 1)}
	for _, id := range channels {
		q.latest[id] = true
	}
	return q
}

func (q *cameraFrameQueue) notify() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *cameraFrameQueue) offer(frame *sensorlink.SensorFrame) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return 0
	}
	arrival := pendingFrame{frame: frame, arrived: time.Now()}
	if q.latest[frame.ChannelID] {
		for _, run := range q.pending {
			if run.channel == frame.ChannelID {
				discarded := run.makeRoom(arrival.arrived)
				run.frames = append(run.frames, arrival)
				return discarded
			}
		}
	} else {
		if q.fifo == sensorFIFOFrames {
			return 1
		}
		q.fifo++
	}
	q.pending = append(q.pending, &frameRun{channel: frame.ChannelID, frames: []pendingFrame{arrival}})
	q.notify()
	return 0
}

func (q *cameraFrameQueue) next(ctx context.Context) (*sensorlink.SensorFrame, bool) {
	for {
		if ctx.Err() != nil {
			return nil, false
		}
		q.mu.Lock()
		if len(q.pending) > 0 {
			run := q.pending[0]
			frame := run.frames[0].frame
			run.frames = slices.Delete(run.frames, 0, 1)
			q.pending = slices.Delete(q.pending, 0, 1)
			if !q.latest[run.channel] {
				q.fifo--
			} else if len(run.frames) > 0 {
				// The rest of the run waits its turn behind the other channels.
				q.pending = append(q.pending, run)
			}
			q.mu.Unlock()
			return frame, true
		}
		closed := q.closed
		q.mu.Unlock()
		if closed {
			return nil, false
		}
		select {
		case <-q.wake:
		case <-ctx.Done():
			return nil, false
		}
	}
}

func (q *cameraFrameQueue) finish() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.notify()
}

// streamLiveFrames keeps the generic transport API's FIFO behavior available
// to other consumers. Pairing requests freshness only for its MJPEG channels.
func streamLiveFrames(ctx context.Context, tr SensorTransport, channels, latest []uint32) (func() (*sensorlink.SensorFrame, bool), func() error, error) {
	if live, ok := tr.(liveFrameTransport); ok && len(latest) > 0 {
		queue := newCameraFrameQueue(latest)
		closeStream, err := live.streamFrames(ctx, channels, queue)
		return func() (*sensorlink.SensorFrame, bool) { return queue.next(ctx) }, closeStream, err
	}
	frames, closeStream, err := tr.Stream(ctx, channels)
	return func() (*sensorlink.SensorFrame, bool) {
		frame, ok := <-frames
		return frame, ok
	}, closeStream, err
}
