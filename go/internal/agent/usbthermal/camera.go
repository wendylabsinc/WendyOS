package usbthermal

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
)

const (
	// subscriberBuffer is how many frames wait for a slow subscriber before
	// the oldest is dropped. Latest wins: a live view or an analytic reader
	// wants the newest frame, and capture must never wait for a reader.
	subscriberBuffer = 2

	// maxReadTimeouts consecutive empty one-second reads end a session: the
	// stream has stopped and the camera needs its start sequence again.
	maxReadTimeouts = 3
	// noFrameTimeout ends a session whose bytes never form a valid frame. The
	// stream is ~28 frames a second, so five seconds without one is a camera
	// in a state the start sequence has to reset, not a slow frame.
	noFrameTimeout = 5 * time.Second

	// Reconnect backoff after a session ends: unplugged, in use by another
	// process, or stopped streaming.
	minBackoff = time.Second
	maxBackoff = 10 * time.Second
)

// Camera is one thermal camera, identified by its stable id. It captures only
// while someone is subscribed: the first subscription runs the start sequence,
// the last one leaving puts the camera back to idle (interface 1 at alternate
// setting 0) and releases it. While anyone is subscribed it reconnects by
// itself after an unplug, a stall or a refused claim, with backoff, re-reading
// sysfs each time because the device number changes on every plug.
//
// A Camera outlives an unplug, so a subscriber that was waiting keeps waiting
// and resumes when the camera comes back. Deciding how long that is worth
// waiting is the subscriber's business (the video service gives up after a
// while and says why, using LastError).
type Camera struct {
	stableID string
	model    Model
	locate   func() (attached, bool)
	devRoot  string
	open     OpenFunc
	sleep    sleepFunc
	now      func() time.Time
	logger   *zap.Logger
	base     context.Context // the manager's lifetime

	mu      sync.Mutex
	subs    map[int]*subscriber
	nextID  int
	cancel  context.CancelFunc // the running capture; nil while idle
	done    chan struct{}      // closed once the latest capture has released the device
	lastErr error
}

type subscriber struct {
	raw chan RawFrame
	pic chan Frame
}

func (s *subscriber) close() {
	if s.raw != nil {
		close(s.raw)
	}
	if s.pic != nil {
		close(s.pic)
	}
}

// StableID is the camera's identity, as ListVideoDevices reports it.
func (c *Camera) StableID() string { return c.stableID }

// Model is the camera's model.
func (c *Camera) Model() Model { return c.model }

// SubscribePictures delivers the picture half of every thermal frame (see
// Frame) on the returned channel until ctx ends, then closes the channel. It
// starts capture if nothing else is subscribed.
//
// The channel holds two frames; when the reader falls behind, the oldest
// waiting frame is dropped. Frame.Data is shared with every other subscriber
// and must be treated as read-only.
//
// This is the hook the encoded live view consumes: one subscription per
// encoder, each frame a whole YUY2 picture with its capture time.
func (c *Camera) SubscribePictures(ctx context.Context) (<-chan Frame, error) {
	ch := make(chan Frame, subscriberBuffer)
	if err := c.subscribe(ctx, &subscriber{pic: ch}); err != nil {
		return nil, err
	}
	return ch, nil
}

// SubscribeRaw delivers every complete thermal frame (picture and temperatures,
// see RawFrame) on the returned channel until ctx ends, then closes it. Same
// buffering and sharing rules as SubscribePictures.
func (c *Camera) SubscribeRaw(ctx context.Context) (<-chan RawFrame, error) {
	ch := make(chan RawFrame, subscriberBuffer)
	if err := c.subscribe(ctx, &subscriber{raw: ch}); err != nil {
		return nil, err
	}
	return ch, nil
}

// LastError is why the most recent capture attempt ended, or nil while frames
// are arriving (and before any attempt).
func (c *Camera) LastError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastErr
}

func (c *Camera) subscribe(ctx context.Context, s *subscriber) error {
	c.mu.Lock()
	if c.base.Err() != nil {
		c.mu.Unlock()
		return ErrClosed
	}
	id := c.nextID
	c.nextID++
	c.subs[id] = s
	if c.cancel == nil {
		c.startLocked()
	}
	c.mu.Unlock()
	go func() {
		select {
		case <-ctx.Done():
		case <-c.base.Done():
		}
		c.unsubscribe(id)
	}()
	return nil
}

// unsubscribe removes a subscriber and closes its channel. Both happen under
// c.mu, which deliver also holds while it sends, so a channel is never sent to
// after it is closed.
func (c *Camera) unsubscribe(id int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.subs[id]
	if !ok {
		return
	}
	delete(c.subs, id)
	s.close()
	if len(c.subs) == 0 && c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
}

// startLocked starts a capture goroutine. A previous capture may still be
// stopping (its last read can take up to readTimeout to return); only one
// process -- and one session -- can own the interface, so the new capture
// waits for it.
func (c *Camera) startLocked() {
	ctx, cancel := context.WithCancel(c.base)
	prev := c.done
	done := make(chan struct{})
	c.cancel, c.done = cancel, done
	go c.capture(ctx, prev, done)
}

// wait blocks until the latest capture has released the device.
func (c *Camera) wait() {
	c.mu.Lock()
	done := c.done
	c.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (c *Camera) capture(ctx context.Context, prev <-chan struct{}, done chan struct{}) {
	defer close(done)
	if prev != nil {
		// Unconditionally, even if ctx has ended meanwhile: whoever starts
		// after this goroutine waits on done, and must find the device free.
		<-prev
	}
	backoff := minBackoff
	for ctx.Err() == nil {
		delivered, err := c.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("thermal stream ended")
		}
		c.mu.Lock()
		c.lastErr = err
		c.mu.Unlock()
		if delivered > 0 {
			backoff = minBackoff
		}
		c.logger.Warn("thermal camera stream stopped; retrying",
			zap.String("camera", c.stableID), zap.Uint64("frames", delivered),
			zap.Duration("retry_in", backoff), zap.Error(err))
		if c.sleep(ctx, backoff) != nil {
			return
		}
		backoff = min(2*backoff, maxBackoff)
	}
}

// session opens the camera, streams until ctx ends or the stream fails, and
// always leaves the camera idle and released. It returns how many frames it
// delivered.
func (c *Camera) session(ctx context.Context) (uint64, error) {
	a, ok := c.locate()
	if !ok {
		return 0, fmt.Errorf("%s is not connected", c.model.Name)
	}
	path := a.devNode(c.devRoot)
	dev, err := c.open(path)
	if err != nil {
		return 0, err
	}
	defer dev.Close() //nolint:errcheck
	s := &session{dev: dev, sleep: c.sleep}
	defer func() {
		if err := s.stop(); err != nil {
			c.logger.Debug("thermal camera stop was not clean",
				zap.String("camera", c.stableID), zap.Error(err))
		}
	}()
	model, err := s.start(ctx)
	if err != nil {
		if errors.Is(err, ErrBusy) {
			return 0, fmt.Errorf("%s at %s is held by another process; only the agent may stream it: %w", c.model.Name, path, err)
		}
		return 0, fmt.Errorf("starting %s at %s: %w", c.model.Name, path, err)
	}
	if model != c.model.Name {
		// The USB id is what selected the layout, so say so loudly; frames are
		// still validated block by block, and a camera whose blocks do not match
		// the layout delivers nothing rather than something wrong.
		c.logger.Warn("thermal camera reports an unexpected model",
			zap.String("camera", c.stableID), zap.String("expected", c.model.Name), zap.String("reported", model))
	}
	c.logger.Info("thermal camera streaming",
		zap.String("camera", c.stableID), zap.String("model", model), zap.String("device", path))
	return c.read(ctx, dev)
}

// read pumps the bulk endpoint through a parser until ctx ends or the stream
// fails.
func (c *Camera) read(ctx context.Context, dev Device) (uint64, error) {
	parser := NewParser(c.model.Layout)
	buf := make([]byte, bulkReadBytes)
	var delivered uint64
	lastFrame := c.now()
	timeouts := 0
	defer func() {
		st := parser.Stats()
		c.logger.Info("thermal camera stream ended",
			zap.String("camera", c.stableID), zap.Uint64("bytes", st.Bytes),
			zap.Uint64("frames", st.Frames), zap.Uint64("rejected", st.Rejected),
			zap.Uint64("lost", st.Lost), zap.Uint64("composite_frames", st.Headers))
	}()
	for ctx.Err() == nil {
		n, err := dev.Bulk(streamEndpoint, buf, readTimeout)
		if err != nil {
			if !errors.Is(err, ErrTimeout) {
				return delivered, fmt.Errorf("reading the thermal stream: %w", err)
			}
			if timeouts++; timeouts >= maxReadTimeouts {
				return delivered, fmt.Errorf("the camera sent nothing for %s", time.Duration(timeouts)*readTimeout)
			}
			continue
		}
		timeouts = 0
		parser.Feed(buf[:n], func(block []byte) {
			delivered++
			lastFrame = c.now()
			c.deliver(block, lastFrame)
		})
		if c.now().Sub(lastFrame) > noFrameTimeout {
			return delivered, noFramesError(parser.Stats())
		}
	}
	return delivered, nil
}

// noFramesError explains a stream that flows but yields no frame.
func noFramesError(st Stats) error {
	if st.Rejected > 0 {
		return fmt.Errorf("no valid thermal frame in %s: %d bytes received, %d blocks failed validation "+
			"(the picture must be neutral grey; a camera in another picture mode is not supported yet)",
			noFrameTimeout, st.Bytes, st.Rejected)
	}
	return fmt.Errorf("no thermal frame in %s: %d bytes received and none of them formed a frame", noFrameTimeout, st.Bytes)
}

// deliver hands one block to every subscriber. The picture and the raw frame
// are both views of the block's single allocation.
func (c *Camera) deliver(block []byte, at time.Time) {
	l := c.model.Layout
	raw := RawFrame{
		Data:         block[:l.RawBytes():l.RawBytes()],
		Width:        l.Width,
		Height:       2 * l.Height,
		BytesPerLine: l.RowBytes(),
		Sequence:     blockSequence(l, block),
		Timestamp:    at,
	}
	pic := Frame{
		Data:      block[:l.PictureBytes():l.PictureBytes()],
		Width:     l.Width,
		Height:    l.Height,
		Format:    FormatYUY2,
		Timestamp: at,
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastErr = nil
	for _, s := range c.subs {
		if s.raw != nil {
			offer(s.raw, raw)
		} else {
			offer(s.pic, pic)
		}
	}
}

// offer sends v without blocking, dropping the oldest queued value to make
// room. Only deliver sends, under c.mu, so the second send always has room.
func offer[T any](ch chan T, v T) {
	select {
	case ch <- v:
		return
	default:
	}
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- v:
	default:
	}
}
