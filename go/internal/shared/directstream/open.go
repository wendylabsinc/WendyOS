package directstream

import "errors"

var ErrRetired = errors.New("direct flow ID retired")

// OpenEntry lives inside its flow until bilateral retirement. It is never a
// separately allocated registry entry or an independently retired resource.
type OpenEntry struct {
	Digest                    [32]byte
	Result                    uint16
	Complete                  bool
	LocalClosed, RemoteClosed bool
}

// OpenNamespace owns cumulative admission promises, not flow records. IDs are
// monotonic within each direction, so a missing ID below highWater is retired.
type OpenNamespace struct {
	parity, highWater uint32
	admitted, grant   uint64
	slots, free       int
}

func NewOpenNamespace(slots int, coordinator bool) (*OpenNamespace, error) {
	if slots < 1 || slots > 65535 {
		return nil, ErrBounds
	}
	parity := uint32(1)
	if coordinator {
		parity = 0
	}
	return &OpenNamespace{parity: parity, slots: slots, free: slots}, nil
}
func (r *OpenNamespace) Available() int     { return r.free }
func (r *OpenNamespace) GrantLimit() uint64 { return r.grant }
func (r *OpenNamespace) Admitted() uint64   { return r.admitted }
func (r *OpenNamespace) HighWater() uint32  { return r.highWater }
func (r *OpenNamespace) Grant(through uint64) error {
	if through < r.grant || through < r.admitted || through-r.admitted > uint64(r.free) {
		return ErrBounds
	}
	r.grant = through
	return nil
}
func (r *OpenNamespace) GrantPeer(through uint64) error {
	if through < r.grant || through < r.admitted {
		return ErrBounds
	}
	r.grant = through
	return nil
}

// Admit is called once, after the flow owner has excluded an existing record.
func (r *OpenNamespace) Admit(id uint32) error {
	if id == 0 || id&1 != r.parity {
		return ErrBounds
	}
	if id <= r.highWater {
		return ErrRetired
	}
	if r.admitted >= r.grant || r.free == 0 {
		return ErrCapacity
	}
	r.highWater = id
	r.admitted++
	r.free--
	return nil
}
