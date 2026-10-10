package models

import (
	"fmt"
	"slices"
)

// watchBuffer holds a full ring replay plus room for statuses.
const watchBuffer = 128

// WatchRequest opens a watch on a live instance.
type WatchRequest struct {
	InstanceID    string
	Label         string // what the watcher calls this watch, e.g. "front door"
	Filter        Filter
	AfterSequence uint64 // replay retained events after this; 0 replays nothing
}

// WatchMessage is one item on a watch; exactly one field is set.
type WatchMessage struct {
	Status *InstanceInfo
	Event  *Event
	Gap    *Gap
}

// Watch is one client's subscription to an instance, and holds the instance
// alive. C closes when the watch ends. Once it has closed, Final returns the
// instance's end state if the instance itself ended.
type Watch struct {
	ID         string
	InstanceID string
	Label      string
	C          <-chan WatchMessage

	// Guarded by the instance's mutex.
	c      chan WatchMessage
	filter Filter
	gap    *Gap // events dropped because C was full, not yet reported
	final  *InstanceInfo
	closed bool
}

// Final returns the instance's end state after C has closed, or nil when only
// the watch ended. Read it only after observing C closed.
func (w *Watch) Final() *InstanceInfo { return w.final }

// offer delivers msg without blocking. A full buffer drops the message; for
// events, the dropped range is reported as a Gap ahead of the next event.
// Caller holds the instance's mutex.
func (w *Watch) offer(msg WatchMessage) {
	if w.closed {
		return
	}
	if msg.Event != nil && w.gap != nil {
		select {
		case w.c <- WatchMessage{Gap: w.gap}:
			w.gap = nil
		default:
			w.gap.LastMissing = msg.Event.Sequence
			return
		}
	}
	select {
	case w.c <- msg:
	default:
		if msg.Event != nil {
			if w.gap == nil {
				w.gap = &Gap{FirstMissing: msg.Event.Sequence}
			}
			w.gap.LastMissing = msg.Event.Sequence
		}
		// A dropped status is superseded by the next one.
	}
}

// end closes the watch. final is the instance's end state, or nil when only
// the watch ended. Caller holds the instance's mutex.
func (w *Watch) end(final *InstanceInfo) {
	if w.closed {
		return
	}
	w.closed = true
	w.final = final
	close(w.c)
}

// checkFilter rejects classes the model cannot report, unknown event types
// and confidences outside 0..1.
func (m Model) checkFilter(f Filter) error {
	for _, c := range f.Classes {
		if !slices.Contains(m.Labels, c) {
			return fmt.Errorf("%w: %s does not report %q", ErrInvalidFilter, m.ID, c)
		}
	}
	for _, t := range f.Types {
		if t != EventEntered && t != EventLeft {
			return fmt.Errorf("%w: unknown event type %q", ErrInvalidFilter, t)
		}
	}
	if f.MinConfidence < 0 || f.MinConfidence > 1 {
		return fmt.Errorf("%w: min confidence %v is outside 0..1", ErrInvalidFilter, f.MinConfidence)
	}
	return nil
}
