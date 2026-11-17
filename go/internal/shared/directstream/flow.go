package directstream

import "math"

// Sender separates accepted bytes, offered bytes and acknowledged ownership.
// It remains alive when a carrier closes. A session generation fences callers.
type Sender struct {
	Buffer      *Buffer
	Offered     uint64
	RemoteLimit uint64
	final       uint64
	finished    bool
}

func (s *Sender) Accept(data []byte, window uint64) error {
	if s.finished || uint64(len(data)) > math.MaxUint64-s.Buffer.End() {
		return ErrBounds
	}
	end := s.Buffer.End() + uint64(len(data))
	if end-s.Buffer.Start() > window {
		return ErrCapacity
	}
	if err := s.Buffer.Reserve(end); err != nil {
		return err
	}
	return s.Buffer.Append(data)
}

// Offer copies a retained suffix to writer-owned storage and publishes its
// high-water mark BEFORE any carrier write. A remote ACK may precede local
// write completion. A failed/ambiguous write requires attachment replacement.
func (s *Sender) Offer(dst []byte, offset uint64) (int, error) {
	if offset > s.Offered || offset > s.RemoteLimit {
		return 0, ErrBounds
	}
	n := min(uint64(len(dst)), s.RemoteLimit-offset)
	got, err := s.Buffer.CopyTo(dst[:int(n)], offset)
	if err == nil {
		s.Offered = max(s.Offered, offset+uint64(got))
	}
	return got, err
}

// State applies a cumulative ACK and absolute receive credit. Attachment
// reconciliation uses this same validation: a lost ACK may advance ownership,
// but a receiver cannot forget previously acknowledged data or retract credit.
func (s *Sender) State(received, limit uint64) error {
	if received < s.Buffer.Start() || received > s.Offered || limit < received || limit < s.RemoteLimit {
		return ErrBounds
	}
	if err := s.Buffer.Release(received); err != nil {
		return err
	}
	s.RemoteLimit = limit
	return nil
}

func (s *Sender) Finish() uint64 {
	if !s.finished {
		s.finished, s.final = true, s.Buffer.End()
	}
	return s.final
}

// Receiver acknowledges only complete, locally owned payloads. Application
// consumption advances Buffer.Start independently from acknowledged Buffer.End.
type Receiver struct {
	Buffer   *Buffer
	final    uint64
	finished bool
}

func (r *Receiver) Accept(data []byte) error {
	if r.finished && uint64(len(data)) > r.final-r.Buffer.End() {
		return ErrBounds
	}
	return r.Buffer.Append(data)
}

// Finish can overtake queued DATA on the priority control lane. It records a
// final-offset promise; EOF is visible only once preceding bytes are consumed.
func (r *Receiver) Finish(final uint64) error {
	if final < r.Buffer.End() || (r.finished && r.final != final) {
		return ErrBounds
	}
	r.final, r.finished = final, true
	return nil
}

func (r *Receiver) EOF() bool { return r.finished && r.Buffer.Start() == r.final }
