package models

import (
	"slices"
	"time"
)

// Detection event types.
const (
	EventEntered = "entered"
	EventLeft    = "left"
)

// Event is one detection event, numbered per instance from 1.
type Event struct {
	Sequence   uint64
	Type       string
	Class      string
	Confidence float32
	TrackID    uint64
	Box        Box
	SourceID   string
	SampleID   uint64
	Time       time.Time
}

// Box is normalized to the frame: 0..1 on both axes.
type Box struct{ X, Y, Width, Height float32 }

// Gap reports events a watch did not receive.
type Gap struct{ FirstMissing, LastMissing uint64 }

// Filter selects the events one watch receives.
type Filter struct {
	Classes       []string // empty: every class
	MinConfidence float32  // 0: everything the host reports
	Types         []string // empty: entered and left
}

// Match reports whether e passes the filter.
func (f Filter) Match(e Event) bool {
	if len(f.Classes) > 0 && !slices.Contains(f.Classes, e.Class) {
		return false
	}
	if e.Confidence < f.MinConfidence {
		return false
	}
	return len(f.Types) == 0 || slices.Contains(f.Types, e.Type)
}

// ring keeps an instance's most recent events and numbers them.
type ring struct {
	buf   []Event // oldest first
	limit int
	last  uint64 // sequence of the newest event; 0 before the first
}

func newRing(limit int) *ring { return &ring{limit: limit} }

// append numbers e and keeps it, evicting the oldest event when full.
func (r *ring) append(e Event) Event {
	r.last++
	e.Sequence = r.last
	if len(r.buf) == r.limit {
		r.buf = append(r.buf[:0], r.buf[1:]...)
	}
	r.buf = append(r.buf, e)
	return e
}

// since returns the retained events numbered after `after`, oldest first, and
// the requested range that was already evicted.
func (r *ring) since(after uint64) ([]Event, *Gap) {
	if after >= r.last {
		return nil, nil
	}
	first := r.last + 1 - uint64(len(r.buf)) // sequence of buf[0]
	var gap *Gap
	if after+1 < first {
		gap = &Gap{FirstMissing: after + 1, LastMissing: first - 1}
	}
	var out []Event
	for _, e := range r.buf {
		if e.Sequence > after {
			out = append(out, e)
		}
	}
	return out, gap
}
