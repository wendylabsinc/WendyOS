// Package live fans Wendy Data records out to live subscribers, the readers
// of DataService.StreamData.
//
// A Hub keeps no history and never blocks a publisher. Each subscriber holds
// at most one undelivered record per stream: a newer record of the same stream
// replaces the pending one, and the replaced record is counted as skipped. A
// slow subscriber therefore always receives the newest state of every stream
// it selected, and a fast stream cannot push a slow stream's record out, which
// is what a single queue shared across streams does.
//
// Records are shared, not copied. Every subscriber that selects a stream
// receives the same *Record, so a publisher must not change a record, or the
// payload it aliases, after offering it.
package live

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// Record is one record on a live stream.
type Record struct {
	// StreamID is opaque, stable and unique on the device, for example
	// "apps/<app-id>/<service>/<stream>".
	StreamID string
	// Schema names the payload type as its publisher declared it. It may be
	// empty.
	Schema string
	// TimestampUnixNanos is the time the data describes, on the agent's
	// CLOCK_REALTIME: the clock of VideoFrame.timestamp_ns.
	TimestampUnixNanos int64
	Payload            []byte
}

// Delivery is one record handed to one subscriber.
type Delivery struct {
	Record *Record
	// Skipped counts the records of the same stream this subscriber did not
	// receive since its previous delivery of that stream, because a newer
	// record replaced them or the pending-bytes cap dropped them. Skipping is
	// the delivery rule, not an error.
	Skipped uint64
}

// Limits bound what live subscribers can cost the agent.
type Limits struct {
	// MaxSubscribers caps concurrent subscriptions on one hub.
	MaxSubscribers int
	// MaxPendingBytes caps the payload bytes one subscriber holds undelivered.
	// Past it the oldest pending record is dropped and counted as skipped. A
	// single record larger than the cap is still delivered on its own.
	MaxPendingBytes int
}

// DefaultLimits keep the worst case predictable on a Jetson or IQ9-class
// device: 32 subscribers holding at most 8 MiB of payload each. Payloads are
// shared, so that is a bound on retained records, not on copies.
var DefaultLimits = Limits{MaxSubscribers: 32, MaxPendingBytes: 8 << 20}

var (
	// ErrTooManySubscribers is returned by Subscribe when the hub is full.
	ErrTooManySubscribers = errors.New("too many live data subscribers")
	// ErrClosed is returned by Next once the subscriber is closed.
	ErrClosed = errors.New("live data subscription closed")
)

// Selection says which streams a subscriber receives. The zero value selects
// every stream.
type Selection struct {
	// StreamIDs are exact stream ids. An id that has not published yet is not
	// an error; its records arrive once it does.
	StreamIDs []string
}

// Hub is the device-wide fan-out point. The zero value is not usable; call
// NewHub.
type Hub struct {
	limits Limits
	// active counts subscribers. Publishers read it without the lock, so a
	// publish with nobody listening costs one atomic load.
	active atomic.Int32
	mu     sync.Mutex
	subs   map[*Subscriber]struct{}
}

// NewHub returns an empty hub. A non-positive limit takes its DefaultLimits
// value.
func NewHub(limits Limits) *Hub {
	if limits.MaxSubscribers <= 0 {
		limits.MaxSubscribers = DefaultLimits.MaxSubscribers
	}
	if limits.MaxPendingBytes <= 0 {
		limits.MaxPendingBytes = DefaultLimits.MaxPendingBytes
	}
	return &Hub{limits: limits, subs: make(map[*Subscriber]struct{})}
}

// Subscribed reports whether anyone is subscribed. A publisher checks it
// before it builds a Record, so that work, timestamp conversion included, is
// skipped while nobody listens.
func (h *Hub) Subscribed() bool { return h.active.Load() > 0 }

// Offer hands r to every subscriber that selects its stream. It never blocks
// on a subscriber: it only replaces each one's pending slot for the stream.
// It is small enough to inline, so the no-subscriber case stays in the caller.
func (h *Hub) Offer(r *Record) {
	if h.active.Load() == 0 {
		return
	}
	h.offer(r)
}

func (h *Hub) offer(r *Record) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		if s.selects(r.StreamID) {
			s.offer(r)
		}
	}
}

// Subscribe registers a subscriber for sel. The caller must Close it.
func (h *Hub) Subscribe(sel Selection) (*Subscriber, error) {
	s := &Subscriber{hub: h, pending: make(map[string]*Record), skipped: make(map[string]uint64), wake: make(chan struct{}, 1)}
	if len(sel.StreamIDs) > 0 {
		s.ids = make(map[string]struct{}, len(sel.StreamIDs))
		for _, id := range sel.StreamIDs {
			s.ids[id] = struct{}{}
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) >= h.limits.MaxSubscribers {
		return nil, ErrTooManySubscribers
	}
	h.subs[s] = struct{}{}
	h.active.Add(1)
	return s, nil
}

// Subscriber receives the records of the streams it selected, newest first
// per stream. Next and Close may be called from different goroutines.
type Subscriber struct {
	hub *Hub
	ids map[string]struct{} // nil selects every stream

	mu      sync.Mutex
	closed  bool
	pending map[string]*Record // the newest undelivered record of each stream
	order   []string           // streams with a pending record, oldest first
	skipped map[string]uint64
	bytes   int           // payload bytes held in pending
	wake    chan struct{} // capacity 1: a record or Close is waiting
}

func (s *Subscriber) selects(streamID string) bool {
	if s.ids == nil {
		return true
	}
	_, ok := s.ids[streamID]
	return ok
}

func (s *Subscriber) offer(r *Record) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if old, ok := s.pending[r.StreamID]; ok {
		// The stream keeps its place in order, so a busy stream cannot starve
		// the others by being offered again and again.
		s.bytes -= len(old.Payload)
		s.skipped[r.StreamID]++
	} else {
		s.order = append(s.order, r.StreamID)
	}
	s.pending[r.StreamID] = r
	s.bytes += len(r.Payload)
	for i := 0; s.bytes > s.hub.limits.MaxPendingBytes && i < len(s.order); {
		id := s.order[i]
		if id == r.StreamID {
			i++
			continue
		}
		s.bytes -= len(s.pending[id].Payload)
		delete(s.pending, id)
		s.skipped[id]++
		s.order = append(s.order[:i], s.order[i+1:]...)
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Next returns the pending record of the stream that has waited longest,
// blocking until there is one, ctx is done, or the subscriber is closed.
func (s *Subscriber) Next(ctx context.Context) (Delivery, error) {
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return Delivery{}, ErrClosed
		}
		if len(s.order) > 0 {
			id := s.order[0]
			copy(s.order, s.order[1:])
			s.order[len(s.order)-1] = ""
			s.order = s.order[:len(s.order)-1]
			r := s.pending[id]
			delete(s.pending, id)
			s.bytes -= len(r.Payload)
			d := Delivery{Record: r, Skipped: s.skipped[id]}
			delete(s.skipped, id)
			s.mu.Unlock()
			return d, nil
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return Delivery{}, ctx.Err()
		case <-s.wake:
		}
	}
}

// Close unregisters the subscriber and releases its pending records. A
// blocked Next returns ErrClosed. Close is idempotent.
func (s *Subscriber) Close() {
	h := s.hub
	h.mu.Lock()
	if _, ok := h.subs[s]; ok {
		delete(h.subs, s)
		h.active.Add(-1)
	}
	h.mu.Unlock()
	s.mu.Lock()
	s.closed = true
	s.pending, s.order, s.skipped, s.bytes = nil, nil, nil, 0
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
