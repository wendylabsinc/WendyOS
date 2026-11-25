package rpctrace

import (
	"context"
	"math"
)

type RefusalPoint uint8

const (
	PointAuthorize RefusalPoint = iota + 1
	PointPeerBeforeScope
	PointPeerAfterScope
	PointAuthority
	PointResponse
	PointPeerWatcher
	PointAuthorityWatcher
)

var pointNames = [...]string{"", "authorize", "peer_before_scope", "peer_after_scope", "authority", "response", "peer_watcher", "authority_watcher"}

type RefusalReason uint8

const (
	ReasonContextCanceled RefusalReason = iota + 1
	ReasonContextDeadline
	ReasonServiceClosed
	ReasonInvalidBinding
	ReasonPinClosed
	ReasonPinDead
	ReasonPinEvent
	ReasonPinSyscall
	ReasonPinInterruptLimit
	ReasonPinUnknown
	ReasonCredentialLookup
	ReasonScopedPeerRefused
	ReasonPeerChanged
	ReasonPublisherDone
	ReasonPublisherRefused
)

var reasonNames = [...]string{"", "context_canceled", "context_deadline", "service_closed", "invalid_binding", "pin_closed", "pin_dead", "pin_event", "pin_syscall", "pin_interrupt_limit", "pin_unknown", "credential_lookup", "scoped_peer_refused", "peer_changed", "publisher_done", "publisher_refused"}

type Refusal struct {
	Point         string `json:"point"`
	Reason        string `json:"reason"`
	WatcherReason string `json:"watcher_reason,omitempty"`
}

type PinInterruption struct {
	Point string `json:"point"`
	Count uint64 `json:"count"`
}

var interruptionPoints = [...]RefusalPoint{PointPeerBeforeScope, PointPeerAfterScope, PointResponse, PointPeerWatcher}

// Enabled reports only whether this context has an admitted diagnostic
// collector. It makes no authority observation and allocates nothing.
func Enabled(ctx context.Context) bool { return fromContext(ctx) != nil }

// Refuse retains the first observed fixed predicate. Later generic refusals
// cannot overwrite a more specific failed check. Watcher state is diagnostic
// history only and never establishes which predicate currently failed.
func Refuse(ctx context.Context, point RefusalPoint, reason, watcher RefusalReason) {
	c := fromContext(ctx)
	if c == nil || point == 0 || int(point) >= len(pointNames) || reason == 0 || int(reason) >= len(reasonNames) || int(watcher) >= len(reasonNames) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished || c.refusalReason != 0 {
		return
	}
	c.refusalPoint, c.refusalReason, c.watcherReason = point, reason, watcher
}

// PinInterrupted records the interruptions already observed by one bounded
// peer poll, without another poll or clock read. Each call permits at most
// eight; aggregate counters saturate. Invalid points/counts are ignored.
func PinInterrupted(ctx context.Context, point RefusalPoint, count uint8) {
	c := fromContext(ctx)
	if c == nil || count == 0 || count > 8 {
		return
	}
	for index, known := range interruptionPoints {
		if point != known {
			continue
		}
		c.mu.Lock()
		if !c.finished {
			old := c.pinInterruptions[index]
			if math.MaxUint64-old < uint64(count) {
				c.pinInterruptions[index] = math.MaxUint64
			} else {
				c.pinInterruptions[index] += uint64(count)
			}
		}
		c.mu.Unlock()
		return
	}
}
