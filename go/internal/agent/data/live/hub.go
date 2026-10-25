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
//
// Publishers advertise their streams. The hub keeps the newest record of each
// advertised stream for subscribers that ask for it (send_latest), and
// publishes the catalog: one DataStreamInfo record per advertised stream, on
// stream CatalogPrefix + id.
package live

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// Record is one record on a live stream.
type Record struct {
	// StreamID is opaque, stable and unique on the device, for example
	// "apps/<app-id>/<service>/<stream>" or "agent/<stream>".
	StreamID string
	// Schema names the payload type as its publisher declared it. It may be
	// empty.
	Schema string
	// TimestampUnixNanos is the time the data describes, on the agent's
	// CLOCK_REALTIME: the clock of VideoFrame.timestamp_ns.
	TimestampUnixNanos int64
	Payload            []byte
	// SourceID and SampleID name the sensor sample the record describes, when
	// its publisher names one, for example "v4l2:/dev/video0" and the camera
	// sample's id. Empty otherwise.
	SourceID string
	SampleID uint64
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
	// MaxLatestBytes caps the payload of the record the hub keeps as a
	// stream's newest, for SendLatest. A larger record is delivered live but
	// not kept, so a stream of large records starts with nothing.
	MaxLatestBytes int
	// MaxLatestAge is how long a kept record stays current. SendLatest skips a
	// stream whose newest record is older, so a stale value is not shown as
	// the present.
	MaxLatestAge time.Duration
}

// DefaultLimits keep the worst case predictable on a Jetson or IQ9-class
// device: 32 subscribers holding at most 8 MiB of payload each, and at most
// 256 KiB kept per advertised stream for 10 seconds. Payloads are shared, so
// these bound retained records, not copies.
var DefaultLimits = Limits{MaxSubscribers: 32, MaxPendingBytes: 8 << 20, MaxLatestBytes: 256 << 10, MaxLatestAge: 10 * time.Second}

var (
	// ErrTooManySubscribers is returned by Subscribe when the hub is full.
	ErrTooManySubscribers = errors.New("too many live data subscribers")
	// ErrClosed is returned by Next once the subscriber is closed.
	ErrClosed = errors.New("live data subscription closed")
)

// Selection says which streams a subscriber receives and how. The zero value
// selects every stream except the catalog, uncapped, from the next record on.
//
// StreamIDs and StreamPrefixes name streams: a stream is selected when it
// matches any of them, or when both are empty. Schemas then keep only the
// streams whose schema is listed.
type Selection struct {
	// StreamIDs are exact stream ids. An id that has not published yet is not
	// an error; its records arrive once it does.
	StreamIDs []string
	// StreamPrefixes match stream ids by prefix.
	StreamPrefixes []string
	// Schemas are exact schema names.
	Schemas []string
	// MaxRateHz caps the records per second of each stream. A faster stream
	// is held back, and a newer record replaces the held one, so the cap never
	// queues. Zero or less means no cap.
	MaxRateHz float64
	// SendLatest starts the subscriber with the newest current record of each
	// selected advertised stream, and the current catalog entry of each
	// selected catalog stream.
	SendLatest bool
	// Allow, when not empty, is what the subscriber may read, whatever else
	// the selection names, catalog entries included. See Permits. An app's
	// data-read allowlist arrives here; operators have none.
	Allow []string
}

// Permits reports whether allow admits streamID. An entry admits the stream id
// it names and every stream id below it on a "/" boundary, so
// "apps/com.example" admits "apps/com.example/markers" but not
// "apps/com.example.other/markers". A catalog stream, CatalogPrefix + id, is
// admitted when the stream it describes is, so a subscriber never learns of a
// stream it may not read. An empty allow admits every stream.
func Permits(allow []string, streamID string) bool {
	if len(allow) == 0 {
		return true
	}
	streamID = strings.TrimPrefix(streamID, CatalogPrefix)
	for _, entry := range allow {
		if !strings.HasPrefix(streamID, entry) {
			continue
		}
		if len(streamID) == len(entry) || strings.HasSuffix(entry, "/") || streamID[len(entry)] == '/' {
			return true
		}
	}
	return false
}

// maxRateGap bounds the gap a tiny Selection.MaxRateHz asks for.
const maxRateGap = time.Hour

// Hub is the device-wide fan-out point. The zero value is not usable; call
// NewHub.
type Hub struct {
	limits Limits
	// active counts subscribers. Subscribed reads it without the lock.
	active  atomic.Int32
	mu      sync.Mutex
	subs    map[*Subscriber]struct{}
	streams map[string]*stream // advertised streams
	// sweeper runs every sweepEvery while anyone is subscribed; nil otherwise.
	// now is the clock. Tests replace both.
	sweeper    *time.Timer
	sweepEvery time.Duration
	now        func() time.Time
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
	if limits.MaxLatestBytes <= 0 {
		limits.MaxLatestBytes = DefaultLimits.MaxLatestBytes
	}
	if limits.MaxLatestAge <= 0 {
		limits.MaxLatestAge = DefaultLimits.MaxLatestAge
	}
	return &Hub{limits: limits, subs: make(map[*Subscriber]struct{}), streams: make(map[string]*stream), sweepEvery: sweepInterval, now: time.Now}
}

// Subscribed reports whether anyone is subscribed. A producer whose records
// are costly to make may skip them while nobody listens; its streams then
// have no newest record for SendLatest and show idle in the catalog.
func (h *Hub) Subscribed() bool { return h.active.Load() > 0 }

// Offer hands r to every subscriber that selects its stream and, for an
// advertised stream, keeps it as the stream's newest record. It never blocks
// on a subscriber: it only replaces each one's pending slot for the stream.
// With nobody subscribed it updates one table entry and allocates nothing.
func (h *Hub) Offer(r *Record) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if st := h.streams[r.StreamID]; st != nil {
		now := h.now()
		h.noteLocked(st, r, now)
		if st.announced != agentpbv2.DataStreamInfo_STATE_ACTIVE || now.Sub(st.announcedAt) >= announceEvery {
			h.announceLocked(st, agentpbv2.DataStreamInfo_STATE_ACTIVE, now)
		}
	}
	if h.active.Load() == 0 {
		return
	}
	for s := range h.subs {
		if s.selects(r.StreamID, r.Schema) {
			s.offer(r)
		}
	}
}

// Subscribe registers a subscriber for sel. The caller must Close it.
func (h *Hub) Subscribe(sel Selection) (*Subscriber, error) {
	s := &Subscriber{hub: h, allow: slices.Clone(sel.Allow), pending: make(map[string]*Record), skipped: make(map[string]uint64), wake: make(chan struct{}, 1)}
	if len(sel.StreamIDs) == 0 && len(sel.StreamPrefixes) == 0 && len(sel.Schemas) == 0 {
		s.all = true
	}
	if len(sel.StreamIDs) > 0 {
		s.ids = make(map[string]struct{}, len(sel.StreamIDs))
		for _, id := range sel.StreamIDs {
			s.ids[id] = struct{}{}
		}
	}
	if len(sel.StreamPrefixes) > 0 {
		s.prefixes = slices.Clone(sel.StreamPrefixes)
	}
	if len(sel.Schemas) > 0 {
		s.schemas = make(map[string]struct{}, len(sel.Schemas))
		for _, schema := range sel.Schemas {
			s.schemas[schema] = struct{}{}
		}
	}
	if sel.MaxRateHz > 0 {
		s.minGap = time.Duration(math.Min(float64(time.Second)/sel.MaxRateHz, float64(maxRateGap)))
		s.sent = make(map[string]time.Time)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) >= h.limits.MaxSubscribers {
		return nil, ErrTooManySubscribers
	}
	h.subs[s] = struct{}{}
	h.active.Add(1)
	if sel.SendLatest {
		h.sendLatestLocked(s)
	}
	if h.sweeper == nil {
		h.sweeper = time.AfterFunc(h.sweepEvery, h.sweep)
	}
	return s, nil
}

// Subscriber receives the records of the streams it selected, newest first
// per stream. Next and Close may be called from different goroutines.
type Subscriber struct {
	hub      *Hub
	all      bool                // an empty selection: every stream but the catalog
	ids      map[string]struct{} // nil: no ids named
	prefixes []string
	schemas  map[string]struct{} // nil: any schema
	minGap   time.Duration       // from MaxRateHz; 0 is no cap
	allow    []string            // empty admits every stream; see Permits

	mu      sync.Mutex
	closed  bool
	pending map[string]*Record // the newest undelivered record of each stream
	order   []string           // streams with a pending record, oldest first
	skipped map[string]uint64
	sent    map[string]time.Time // when each stream was last delivered, under a rate cap
	bytes   int                  // payload bytes held in pending
	wake    chan struct{}        // capacity 1: a record or Close is waiting
}

func (s *Subscriber) selects(streamID, schema string) bool {
	if !Permits(s.allow, streamID) {
		return false
	}
	if s.all {
		return !strings.HasPrefix(streamID, CatalogPrefix)
	}
	if s.ids != nil || s.prefixes != nil {
		if _, ok := s.ids[streamID]; !ok && !hasPrefix(streamID, s.prefixes) {
			return false
		}
	}
	if s.schemas != nil {
		_, ok := s.schemas[schema]
		return ok
	}
	return true
}

func hasPrefix(id string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(id, p) {
			return true
		}
	}
	return false
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
// blocking until there is one, ctx is done, or the subscriber is closed. Under
// a rate cap, a stream sent less than its gap ago waits, and the streams
// behind it go first.
func (s *Subscriber) Next(ctx context.Context) (Delivery, error) {
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return Delivery{}, ErrClosed
		}
		d, wait, ok := s.takeLocked()
		s.mu.Unlock()
		if ok {
			return d, nil
		}
		var held <-chan time.Time
		if wait > 0 {
			if timer == nil {
				timer = time.NewTimer(wait)
			} else {
				timer.Reset(wait)
			}
			held = timer.C
		}
		select {
		case <-ctx.Done():
			return Delivery{}, ctx.Err()
		case <-s.wake:
		case <-held:
		}
	}
}

// takeLocked removes and returns the first pending record the rate cap lets
// through. Otherwise it reports how long until a held stream may go, or zero
// when nothing is pending.
func (s *Subscriber) takeLocked() (Delivery, time.Duration, bool) {
	var now time.Time
	var wait time.Duration
	for i, id := range s.order {
		if s.minGap > 0 {
			if now.IsZero() {
				now = s.hub.now()
			}
			if left := s.minGap - now.Sub(s.sent[id]); left > 0 {
				if wait == 0 || left < wait {
					wait = left
				}
				continue
			}
			s.sent[id] = now
		}
		s.order = slices.Delete(s.order, i, i+1)
		r := s.pending[id]
		delete(s.pending, id)
		s.bytes -= len(r.Payload)
		d := Delivery{Record: r, Skipped: s.skipped[id]}
		delete(s.skipped, id)
		return d, 0, true
	}
	return Delivery{}, wait, false
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
	s.pending, s.order, s.skipped, s.sent, s.bytes = nil, nil, nil, nil, 0
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
