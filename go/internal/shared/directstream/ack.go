package directstream

// Coalesce the common 512-byte BLE records into roughly one receipt per KiB.
// Exhausted advertised windows and FIN still bypass the bounded delay.
const (
	ACKBytes    = 1024
	CreditBytes = 1024
	ACKDelayUS  = 250000
)

// Receipt is an absolute receive prefix and a physically reserved byte limit.
// It must come from the receiver's owned state, not the last TLS read length.
type Receipt struct {
	Received, Limit uint64
	FINSeen         bool
}

// ACKSchedule coalesces STATE updates without allocating a queue per DATA frame.
// The session actor serializes calls and fences old attachment callbacks.
// Times are nonnegative monotonic microseconds, never wall-clock timestamps.
type ACKSchedule struct {
	current, sent    Receipt
	since            int64
	pending, initial bool
}

// NewACKSchedule immediately advertises the current prefix/credit on attachment.
func NewACKSchedule(state Receipt, now int64) (*ACKSchedule, error) {
	if state.Received > state.Limit || now < 0 {
		return nil, ErrBounds
	}
	return &ACKSchedule{current: state, since: now, pending: true, initial: true}, nil
}

func (a *ACKSchedule) Update(state Receipt, now int64) error {
	if now < 0 || (a.pending && now < a.since) || state.Received < a.current.Received || state.Limit < a.current.Limit || state.Received > state.Limit || (a.current.FINSeen && !state.FINSeen) {
		return ErrBounds
	}
	if state == a.current {
		return nil
	}
	a.current = state
	if !a.pending {
		a.pending = true
		a.since = now
	}
	return nil
}

// Due returns the newest coalesced state. Call Written only once the writer has
// accepted responsibility for this exact snapshot. Polling or a failed enqueue
// never consumes an ACK. Later progress never restarts the oldest ACK timer.
func (a *ACKSchedule) Due(now int64) (Receipt, bool) {
	// The sender can exhaust the advertised limit even after local delivery
	// has advanced current.Limit. Reopen that window without waiting for a timer.
	due := a.pending && (a.initial || (a.current.FINSeen && !a.sent.FINSeen) || a.current.Received-a.sent.Received >= ACKBytes || a.current.Limit-a.sent.Limit >= CreditBytes || (a.current.Received >= a.sent.Limit && (a.current.Received > a.sent.Received || a.current.Limit > a.sent.Limit)) || (now >= a.since && now-a.since >= ACKDelayUS))
	return a.current, due
}

func (a *ACKSchedule) Written(state Receipt) error {
	if state.Received > state.Limit || state.Received < a.sent.Received || state.Limit < a.sent.Limit || state.Received > a.current.Received || state.Limit > a.current.Limit || (a.sent.FINSeen && !state.FINSeen) || (state.FINSeen && !a.current.FINSeen) {
		return ErrBounds
	}
	a.sent = state
	a.initial = false
	a.pending = a.current != a.sent
	return nil
}
