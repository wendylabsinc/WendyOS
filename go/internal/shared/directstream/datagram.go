package directstream

// DatagramReceiver is an actor-owned, single-message reassembler. Storage is
// caller-reserved from the UDP memory budget; reassembly never allocates or ACKs.
// Ordered direct records need no out-of-order fragment queue. Gaps, overlap and
// interleaved newer messages discard incomplete datagrams, preserving UDP loss.
// Message IDs belong to the association, not the carrier, and never wrap.
type DatagramReceiver struct {
	storage            []byte
	flow, alias        uint32
	generation, lastID uint64
	total, received    int
	active             bool
}

func NewDatagramReceiver(flow uint32, storage []byte) (*DatagramReceiver, error) {
	if flow == 0 || len(storage) == 0 || len(storage) > 65535 {
		return nil, ErrBounds
	}
	return &DatagramReceiver{flow: flow, storage: storage}, nil
}

// Bind follows authenticated attachment commit. Generation is the local
// committed generation, never an unchecked value taken from an incoming frame.
// Retain the message high-water mark across attachment and promotion changes.
func (r *DatagramReceiver) Bind(generation uint64, alias uint32) error {
	if generation == 0 || generation <= r.generation || alias == 0 {
		return ErrBounds
	}
	r.generation, r.alias = generation, alias
	r.active = false
	return nil
}

func (r *DatagramReceiver) Suspend() { r.alias = 0; r.active = false }

// Receive returns a borrowed complete datagram, including a zero-length one.
// Consume/copy it before the next call. Stale incarnations cannot alter state.
// The returned bool distinguishes an empty datagram from a discarded fragment.
func (r *DatagramReceiver) Receive(generation uint64, f FlowFrame) ([]byte, bool, error) {
	if r.alias == 0 || generation != r.generation || f.AttachmentID != r.alias {
		return nil, false, nil
	}
	if f.Kind != FrameDatagram || f.FlowID != r.flow {
		return nil, false, ErrBounds
	}
	if _, err := f.Size(); err != nil {
		return nil, false, err
	}
	if f.Value < r.lastID || (f.Value == r.lastID && !r.active) {
		return nil, false, nil
	}
	if f.Value > r.lastID {
		r.lastID, r.active = f.Value, false
		if f.FragmentOffset != 0 || int(f.DatagramSize) > len(r.storage) {
			return nil, false, nil
		}
		r.total, r.received, r.active = int(f.DatagramSize), 0, true
	}
	if int(f.DatagramSize) != r.total || int(f.FragmentOffset) != r.received {
		r.active = false
		return nil, false, nil
	}
	copy(r.storage[r.received:], f.Payload)
	r.received += len(f.Payload)
	if r.received != r.total {
		return nil, false, nil
	}
	r.active = false
	return r.storage[:r.total], true, nil
}
