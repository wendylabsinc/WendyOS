package rawvideo

import (
	"bytes"
	"sync"
	"time"
)

// Feed hands one in-process camera's frames to whoever is watching it, without
// ever making the driver wait.
//
// The driver calls Publish once per captured frame, from its capture loop. The
// video hub subscribes only while a viewer is streaming the camera, so a camera
// nobody watches costs one timestamp comparison per frame. A subscriber that
// falls behind (an encoder slower than the camera) loses its OLDEST queued frame
// to the newest one: a viewer that catches up sees what the camera sees now, and
// the driver's loop is never blocked by it.
//
// The zero value is ready to use.
type Feed struct {
	mu   sync.Mutex
	subs map[chan Frame]struct{}
	// last and interval measure the camera's frame rate from the frames'
	// own capture timestamps (see observe).
	last     time.Time
	interval time.Duration
}

// SubscriberBuffer is how many frames a subscriber may fall behind before its
// oldest is dropped. Kept small on purpose: every queued frame is latency in a
// live view, and the encoder pipeline has its own small queue after this one.
const SubscriberBuffer = 2

// maxRateGap is the longest gap between two frames that still counts as a
// frame interval. A longer one is a pause (a thermal camera's shutter
// calibration, a driver restart), not the camera's rate.
const maxRateGap = 2 * time.Second

// NewFeed returns an empty feed.
func NewFeed() *Feed { return &Feed{} }

// Publish offers frame to every current subscriber. It never blocks.
//
// Subscribers receive their own copy of frame.Data, made once per call and
// shared between them, so the driver may reuse its buffer as soon as Publish
// returns. With no subscriber nothing is copied.
func (f *Feed) Publish(frame Frame) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observe(frame.Timestamp)
	if len(f.subs) == 0 {
		return
	}
	frame.Data = bytes.Clone(frame.Data)
	for ch := range f.subs {
		select {
		case ch <- frame:
			continue
		default:
		}
		// Full: drop the oldest queued frame so this one fits.
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- frame:
		default:
		}
	}
}

// Subscribe returns a channel of the frames published from now on, and a
// function that ends the subscription. The channel is never closed; stop
// reading it after calling cancel. cancel may be called more than once.
func (f *Feed) Subscribe() (frames <-chan Frame, cancel func()) {
	ch := make(chan Frame, SubscriberBuffer)
	f.mu.Lock()
	if f.subs == nil {
		f.subs = make(map[chan Frame]struct{})
	}
	f.subs[ch] = struct{}{}
	f.mu.Unlock()
	return ch, func() {
		f.mu.Lock()
		delete(f.subs, ch)
		f.mu.Unlock()
	}
}

// Watched reports whether anyone is subscribed. A driver may use it to skip
// work, such as converting a frame, that only a subscriber would need.
func (f *Feed) Watched() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs) > 0
}

// FrameInterval is the camera's measured time between frames, or 0 until two
// frames with usable timestamps have been published.
func (f *Feed) FrameInterval() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.interval
}

// observe folds one capture timestamp into the frame interval: an exponential
// moving average (1/8 weight) of the gaps between consecutive frames, ignoring
// zero timestamps, gaps that go backwards and pauses longer than maxRateGap.
// Called with f.mu held.
func (f *Feed) observe(ts time.Time) {
	if ts.IsZero() {
		return
	}
	if !f.last.IsZero() {
		if gap := ts.Sub(f.last); gap > 0 && gap <= maxRateGap {
			if f.interval == 0 {
				f.interval = gap
			} else {
				f.interval += (gap - f.interval) / 8
			}
		}
	}
	f.last = ts
}
