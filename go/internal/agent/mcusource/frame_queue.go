package mcusource

import (
	"context"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
)

const sensorFIFOFrames = 8

// frameSink accepts complete frames without blocking the transport reader.
// offer reports whether it discarded a frame, including a superseded camera
// frame. The transport serializes offer with finish.
type frameSink interface {
	offer(*sensorlink.SensorFrame) bool
	finish()
}

type channelFrameSink chan *sensorlink.SensorFrame

func (s channelFrameSink) offer(frame *sensorlink.SensorFrame) bool {
	select {
	case s <- frame:
		return false
	default:
		return true
	}
}
func (s channelFrameSink) finish() { close(s) }

// liveFrameTransport lets the pairing supervisor choose a queue after it has
// validated the manifest and mounted its live sensors. Ordinary Stream callers
// retain their existing FIFO policy.
type liveFrameTransport interface {
	streamFrames(context.Context, []uint32, frameSink) (func() error, error)
}

// cameraFrameQueue reserves one replaceable pending frame per MJPEG camera,
// alongside the existing eight-frame FIFO for audio and dependent video codecs.
// Replacing in place preserves fairness between cameras. Receiving under the
// same lock avoids holding a stale frame in a blocked channel send.
type cameraFrameQueue struct {
	mu      sync.Mutex
	latest  map[uint32]bool
	pending []*sensorlink.SensorFrame
	fifo    int
	closed  bool
	wake    chan struct{}
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

func (q *cameraFrameQueue) offer(frame *sensorlink.SensorFrame) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return false
	}
	if q.latest[frame.ChannelID] {
		for i, previous := range q.pending {
			if previous.ChannelID == frame.ChannelID {
				q.pending[i] = frame
				return true
			}
		}
	} else {
		if q.fifo == sensorFIFOFrames {
			return true
		}
		q.fifo++
	}
	q.pending = append(q.pending, frame)
	q.notify()
	return false
}

func (q *cameraFrameQueue) next(ctx context.Context) (*sensorlink.SensorFrame, bool) {
	for {
		if ctx.Err() != nil {
			return nil, false
		}
		q.mu.Lock()
		if len(q.pending) > 0 {
			frame := q.pending[0]
			copy(q.pending, q.pending[1:])
			q.pending[len(q.pending)-1] = nil
			q.pending = q.pending[:len(q.pending)-1]
			if !q.latest[frame.ChannelID] {
				q.fifo--
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
