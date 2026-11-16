package directstream

// ReliableLane connects a retained app socket to bounded flow records. All
// methods run on one session actor, after that actor has checked the attachment
// token/generation and OPEN result. The caller owns frame dispatch and fairness.
// App Read/Write may run concurrently through RetainedConn.
type ReliableLane struct {
	ID              uint32
	Conn            *RetainedConn
	ack             *ACKSchedule
	cursor          uint64
	finSent, finACK bool
	pending         bool
	pendingFrame    FlowFrame
}

func NewReliableLane(id uint32, c *RetainedConn, now int64) (*ReliableLane, error) {
	if id == 0 || c == nil {
		return nil, ErrBounds
	}
	l := &ReliableLane{ID: id, Conn: c}
	err := c.WithFlow(func(s *Sender, r *Receiver) (bool, error) {
		var err error
		l.ack, err = NewACKSchedule(receipt(r), now)
		l.cursor = s.Buffer.Start()
		return false, err
	})
	if err != nil {
		return nil, err
	}
	return l, nil
}
func receipt(r *Receiver) Receipt {
	return Receipt{Received: r.Buffer.End(), Limit: r.Buffer.Limit(), FINSeen: r.finished}
}

// Prepare returns at most one record. Reserve writer queue capacity first.
// DATA is copied into caller-owned scratch and offered before any wire write.
// Accepted completes ownership transfer to the writer. A failed or ambiguous
// submission requires freezing the attachment and Reattach, never blind retry.
func (l *ReliableLane) Prepare(alias uint32, now int64, scratch []byte) (FlowFrame, bool, error) {
	if alias == 0 || l.pending || len(scratch) < MaxDataPayload {
		return FlowFrame{}, false, ErrBounds
	}
	var f FlowFrame
	err := l.Conn.WithFlow(func(s *Sender, r *Receiver) (bool, error) {
		if err := l.ack.Update(receipt(r), now); err != nil {
			return false, err
		}
		if state, due := l.ack.Due(now); due {
			f = FlowFrame{Kind: FrameState, AttachmentID: alias, StateCount: 1}
			f.States[0] = StateEntry{FlowID: l.ID, Received: state.Received, Limit: state.Limit, FINSeen: state.FINSeen}
		} else if s.finished && !l.finSent && !l.finACK {
			f = FlowFrame{Kind: FrameFIN, AttachmentID: alias, FlowID: l.ID, Value: s.final}
		} else {
			n, err := s.Offer(scratch[:MaxDataPayload], l.cursor)
			if err != nil {
				return false, err
			}
			if n > 0 {
				f = FlowFrame{Kind: FrameData, AttachmentID: alias, FlowID: l.ID, Payload: scratch[:n]}
			}
		}
		return false, nil
	})
	if err != nil {
		return FlowFrame{}, false, err
	}
	if f.Kind == 0 {
		return FlowFrame{}, false, nil
	}
	l.pending = true
	l.pendingFrame = f
	return f, true, nil
}
func (l *ReliableLane) Accepted() error {
	if !l.pending {
		return ErrBounds
	}
	f := l.pendingFrame
	switch f.Kind {
	case FrameState:
		s := f.States[0]
		if err := l.ack.Written(Receipt{Received: s.Received, Limit: s.Limit, FINSeen: s.FINSeen}); err != nil {
			return err
		}
	case FrameData:
		l.cursor += uint64(len(f.Payload))
	case FrameFIN:
		l.finSent = true
	default:
		return ErrBounds
	}
	l.pending = false
	l.pendingFrame = FlowFrame{}
	return nil
}

// Receive accepts only complete DATA/FIN records. Dispatch must already have
// matched the active attachment alias. STATE batches are dispatched via State.
func (l *ReliableLane) Receive(f FlowFrame, now int64) error {
	if f.FlowID != l.ID || now < 0 || (l.ack.pending && now < l.ack.since) {
		return ErrBounds
	}
	return l.Conn.WithFlow(func(s *Sender, r *Receiver) (bool, error) {
		var err error
		switch f.Kind {
		case FrameData:
			if len(f.Payload) == 0 || len(f.Payload) > MaxDataPayload {
				return false, ErrBounds
			}
			err = r.Accept(f.Payload)
		case FrameFIN:
			err = r.Finish(f.Value)
		default:
			return false, ErrBounds
		}
		if err != nil {
			return false, err
		}
		return true, l.ack.Update(receipt(r), now)
	})
}
func (l *ReliableLane) State(state StateEntry) error {
	if state.FlowID != l.ID {
		return ErrBounds
	}
	return l.Conn.WithFlow(func(s *Sender, r *Receiver) (bool, error) {
		if (state.FINSeen && !s.finished) || (l.finACK && !state.FINSeen) {
			return false, ErrBounds
		}
		if err := s.State(state.Received, state.Limit); err != nil {
			return false, err
		}
		l.finACK = state.FINSeen
		return true, nil
	})
}

// Grant is an absolute byte limit chosen by shared-budget policy. The receive
// reservation must fit before it can become visible in a STATE record.
func (l *ReliableLane) Grant(limit uint64, now int64) error {
	return l.grant(limit, now, nil)
}

func (l *ReliableLane) grant(limit uint64, now int64, admit func(*Receiver) bool) error {
	if now < 0 || (l.ack.pending && now < l.ack.since) {
		return ErrBounds
	}
	return l.Conn.WithFlow(func(s *Sender, r *Receiver) (bool, error) {
		if limit < r.Buffer.Limit() {
			return false, ErrBounds
		}
		if admit != nil && !admit(r) {
			return false, ErrCapacity
		}
		if err := r.Buffer.Reserve(limit); err != nil {
			return false, err
		}
		return true, l.ack.Update(receipt(r), now)
	})
}

// Reattach is called only after both old carrier workers are joined and the
// session has authenticated and validated EVERY flow's reconciliation. App
// writes/reads may advance local tail/consumption while suspended; accepted
// receive prefix, previous ACKs and grants must still validate at commit.
// New attachment aliases/nonces are owned by the enclosing session transaction.
func (l *ReliableLane) Reattach(remote FlowSnapshot, now int64) error {
	return l.Conn.WithFlow(func(s *Sender, r *Receiver) (bool, error) {
		plan, err := Reconcile(Snapshot(s, r), remote)
		if err != nil {
			return false, err
		}
		if now < 0 || (l.finACK && !remote.ReceiveFinished) {
			return false, ErrBounds
		}
		if err = s.State(plan.Outgoing.Received, plan.Outgoing.Limit); err != nil {
			return false, err
		}
		if plan.IncomingFinished {
			if err = r.Finish(plan.IncomingFinal); err != nil {
				return false, err
			}
		}
		l.cursor = plan.Outgoing.Received
		l.finACK = plan.Outgoing.FINSeen
		l.finSent = false
		l.pending = false
		l.pendingFrame = FlowFrame{}
		l.ack, err = NewACKSchedule(receipt(r), now)
		return true, err
	})
}
func (l *ReliableLane) Snapshot() (FlowSnapshot, error) {
	var out FlowSnapshot
	err := l.Conn.WithFlow(func(s *Sender, r *Receiver) (bool, error) { out = Snapshot(s, r); return false, nil })
	return out, err
}
