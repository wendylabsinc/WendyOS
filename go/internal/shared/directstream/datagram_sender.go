package directstream

import "math"

// DatagramSender owns one caller-budgeted queued message. Carrier backpressure
// retains only this message; additional messages are dropped by the UDP owner.
// Suspend discards it, including any prepared fragment. Nothing is replayed on
// another carrier. Message IDs remain monotonic for the association lifetime.
type DatagramSender struct {
	storage                    []byte
	flow, alias                uint32
	generation, lastID         uint64
	total, offset, pendingSize int
	active, pending            bool
}

func NewDatagramSender(flow uint32, storage []byte) (*DatagramSender, error) {
	if flow == 0 || len(storage) == 0 || len(storage) > 65535 {
		return nil, ErrBounds
	}
	return &DatagramSender{flow: flow, storage: storage}, nil
}
func (s *DatagramSender) Bind(generation uint64, alias uint32) error {
	if generation == 0 || generation <= s.generation || alias == 0 {
		return ErrBounds
	}
	s.Suspend()
	s.generation, s.alias = generation, alias
	return nil
}
func (s *DatagramSender) Suspend() { s.alias = 0; s.active = false; s.pending = false }

// Queue copies a complete datagram. ErrCapacity means no queue space; no
// association state changes. The caller must not turn this into reliable replay.
func (s *DatagramSender) Queue(data []byte) error {
	if s.alias == 0 || len(data) > len(s.storage) || s.lastID == math.MaxUint64 {
		return ErrBounds
	}
	if s.active {
		return ErrCapacity
	}
	copy(s.storage, data)
	s.lastID++
	s.total = len(data)
	s.offset = 0
	s.active = true
	return nil
}

// Prepare borrows stable bytes until Accepted or Suspend. Repeated Prepare
// calls return the same fragment without advancing the message.
func (s *DatagramSender) Prepare() (FlowFrame, bool) {
	if !s.active || s.alias == 0 {
		return FlowFrame{}, false
	}
	n := s.total - s.offset
	if n > MaxDataPayload {
		n = MaxDataPayload
	}
	s.pending = true
	s.pendingSize = n
	return FlowFrame{Kind: FrameDatagram, AttachmentID: s.alias, FlowID: s.flow, Value: s.lastID, DatagramSize: uint16(s.total), FragmentOffset: uint16(s.offset), Payload: s.storage[s.offset : s.offset+n]}, true
}

// Accepted follows a successful COPY into the carrier queue, not delivery or
// peer acknowledgement. The token rejects delayed/double completion callbacks.
func (s *DatagramSender) Accepted(generation, id uint64, offset uint16) error {
	if !s.pending || s.alias == 0 || generation != s.generation || id != s.lastID || int(offset) != s.offset {
		return ErrBounds
	}
	s.offset += s.pendingSize
	s.pending = false
	if s.offset == s.total {
		s.active = false
	}
	return nil
}
