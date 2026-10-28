package live

import (
	"sort"
	"time"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/protobuf/proto"
)

const (
	// CatalogPrefix starts the id of every catalog stream. The catalog stream
	// of stream X is CatalogPrefix + X. An empty selection leaves the catalog
	// out; a subscriber names it by id or prefix, or by CatalogSchema.
	CatalogPrefix = "_streams/"
	// CatalogSchema is the schema of catalog records. It is reserved: an app
	// stream may not declare it, and selecting it selects only catalog
	// streams.
	CatalogSchema = "wendy.agent.services.v2.DataStreamInfo"
)

const (
	// idleAfter is how long an active stream may go without a record before
	// the catalog calls it idle.
	idleAfter = 10 * time.Second
	// announceEvery is how often the catalog repeats an active stream's entry,
	// with its measured rate.
	announceEvery = 10 * time.Second
	// rateWindow is the span the measured rate is averaged over.
	rateWindow = 10 * time.Second
	// sweepInterval is how often, while anyone is subscribed, the hub looks
	// for active streams that went idle.
	sweepInterval = time.Second
)

// StreamInfo describes a stream its publisher advertises.
type StreamInfo struct {
	ID        string
	Schema    string
	MediaType string
	// Publisher is the app id of an application stream.
	Publisher string
}

// stream is what the hub knows about one advertised stream.
type stream struct {
	info      StreamInfo
	catalogID string
	// latest is the newest record, or nil when it was larger than
	// MaxLatestBytes. lastAt is when the newest record was offered and
	// lastTimestamp its time; both are zero before the first.
	latest        *Record
	lastAt        time.Time
	lastTimestamp int64
	// Rate measurement over the current active period: records offered since
	// windowStart, and the rate of the previous window. The period's first
	// record opens its first window.
	windowStart time.Time
	windowCount int
	prevRate    float64
	// What the catalog last said about the stream, and when.
	announced   agentpbv2.DataStreamInfo_State
	announcedAt time.Time
}

// Advertise adds a stream to the catalog, idle until its first record, and
// makes the hub keep its newest record. A publisher advertises a stream before
// it offers records and unadvertises it when it stops. Advertising a stream
// again updates its description.
func (h *Hub) Advertise(info StreamInfo) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	st := h.streams[info.ID]
	if st == nil {
		st = &stream{catalogID: CatalogPrefix + info.ID}
		h.streams[info.ID] = st
	}
	st.info = info
	h.announceLocked(st, h.stateLocked(st, now), now)
}

// Unadvertise tells catalog subscribers that the stream is gone, then forgets
// it and its newest record. Records offered afterwards are still delivered
// live, but are not kept.
func (h *Hub) Unadvertise(streamID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.streams[streamID]
	if st == nil {
		return
	}
	delete(h.streams, streamID)
	h.announceLocked(st, agentpbv2.DataStreamInfo_STATE_GONE, h.now())
}

// noteLocked keeps r as st's newest record, unless it is too large to keep,
// and counts it for the measured rate.
func (h *Hub) noteLocked(st *stream, r *Record, now time.Time) {
	st.latest = nil
	if len(r.Payload) <= h.limits.MaxLatestBytes {
		st.latest = r
	}
	if h.stateLocked(st, now) == agentpbv2.DataStreamInfo_STATE_IDLE {
		// A new active period: its rate is measured from this record, not
		// over the quiet before it.
		st.windowStart, st.windowCount, st.prevRate = now, 0, 0
	} else {
		st.windowCount++
		if elapsed := now.Sub(st.windowStart); elapsed >= rateWindow {
			st.prevRate = float64(st.windowCount) / elapsed.Seconds()
			st.windowStart, st.windowCount = now, 0
		}
	}
	st.lastAt, st.lastTimestamp = now, r.TimestampUnixNanos
}

// rate is an active stream's records per second over about the last
// rateWindow. It is 0 for the first second of an active period, before there
// is anything to measure.
func (st *stream) rate(now time.Time) float64 {
	if elapsed := now.Sub(st.windowStart); elapsed >= time.Second {
		return float64(st.windowCount) / elapsed.Seconds()
	}
	return st.prevRate
}

func (h *Hub) stateLocked(st *stream, now time.Time) agentpbv2.DataStreamInfo_State {
	if st.lastAt.IsZero() || now.Sub(st.lastAt) >= idleAfter {
		return agentpbv2.DataStreamInfo_STATE_IDLE
	}
	return agentpbv2.DataStreamInfo_STATE_ACTIVE
}

// announceLocked records what the catalog says about st and offers it to the
// subscribers that select st's catalog stream. The record is built only if
// one does.
func (h *Hub) announceLocked(st *stream, state agentpbv2.DataStreamInfo_State, now time.Time) {
	st.announced, st.announcedAt = state, now
	if h.active.Load() == 0 {
		return
	}
	var r *Record
	for s := range h.subs {
		if !s.selects(st.catalogID, CatalogSchema) {
			continue
		}
		if r == nil {
			r = h.catalogRecord(st, state, now)
		}
		s.offer(r)
	}
}

func (h *Hub) catalogRecord(st *stream, state agentpbv2.DataStreamInfo_State, now time.Time) *Record {
	info := &agentpbv2.DataStreamInfo{
		StreamId:               st.info.ID,
		Schema:                 st.info.Schema,
		MediaType:              st.info.MediaType,
		Publisher:              st.info.Publisher,
		State:                  state,
		LastTimestampUnixNanos: st.lastTimestamp,
	}
	// An idle stream had no record for idleAfter: its rate over the last
	// rateWindow is 0, whatever it was while active.
	if state == agentpbv2.DataStreamInfo_STATE_ACTIVE {
		info.RateHz = float32(st.rate(now))
	}
	// Marshalling a message of strings and numbers cannot fail.
	payload, _ := proto.Marshal(info)
	return &Record{StreamID: st.catalogID, Schema: CatalogSchema, TimestampUnixNanos: now.UnixNano(), Payload: payload}
}

// sweep marks active streams that stopped publishing idle. It runs every
// sweepEvery while anyone is subscribed, and stops with the last subscriber.
func (h *Hub) sweep() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) == 0 {
		h.sweeper = nil
		return
	}
	h.sweepLocked(h.now())
	h.sweeper.Reset(h.sweepEvery)
}

func (h *Hub) sweepLocked(now time.Time) {
	for _, st := range h.streams {
		if st.announced == agentpbv2.DataStreamInfo_STATE_ACTIVE && h.stateLocked(st, now) == agentpbv2.DataStreamInfo_STATE_IDLE {
			h.announceLocked(st, agentpbv2.DataStreamInfo_STATE_IDLE, now)
		}
	}
}

// sendLatestLocked queues the current catalog entries s selects, in stream id
// order, then the newest current records, oldest first. Live records offered
// later replace them in place, so s never sees an older record after a newer
// one.
func (h *Hub) sendLatestLocked(s *Subscriber) {
	now := h.now()
	var catalog, latest []*stream
	for _, st := range h.streams {
		if s.selects(st.catalogID, CatalogSchema) {
			catalog = append(catalog, st)
		}
		if st.latest != nil && now.Sub(st.lastAt) <= h.limits.MaxLatestAge && s.selects(st.info.ID, st.latest.Schema) {
			latest = append(latest, st)
		}
	}
	sort.Slice(catalog, func(i, j int) bool { return catalog[i].info.ID < catalog[j].info.ID })
	sort.Slice(latest, func(i, j int) bool { return latest[i].lastAt.Before(latest[j].lastAt) })
	for _, st := range catalog {
		s.offer(h.catalogRecord(st, h.stateLocked(st, now), now))
	}
	for _, st := range latest {
		s.offer(st.latest)
	}
}
