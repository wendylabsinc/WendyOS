package localmesh

import "errors"

const clockControlVersion = 1

// ClockControl relays a receiver's nonce to a pinned Roughtime authority before
// manifest admission. The authority signature, never the neighbour's clock,
// authenticates the response. Legacy peers ignore this optional control kind.
type ClockControl struct {
	Version uint32 `json:"v" cbor:"v"`
	Op      string `json:"op" cbor:"o"`
	ID      uint64 `json:"id,omitempty" cbor:"i,omitempty"`
	Server  uint8  `json:"server,omitempty" cbor:"s,omitempty"`
	Nonce   []byte `json:"nonce,omitempty" cbor:"n,omitempty"`
	Data    []byte `json:"data,omitempty" cbor:"d,omitempty"`
}

func knownClockOp(op string) bool {
	switch op {
	case "hello", "request", "reply", "error":
		return true
	}
	return false
}

func (m *ClockControl) supported() bool {
	return m != nil && m.Version == clockControlVersion && knownClockOp(m.Op)
}

func (m *ClockControl) validate() error {
	if !m.supported() {
		return errors.New("invalid clock control version or operation")
	}
	if m.Op == "hello" {
		if m.ID == 0 && m.Server == 0 && len(m.Nonce) == 0 && len(m.Data) == 0 {
			return nil
		}
	} else if m.ID != 0 {
		switch m.Op {
		case "request":
			if len(m.Nonce) == 32 && len(m.Data) == 0 {
				return nil
			}
		case "reply":
			if len(m.Nonce) == 0 && len(m.Data) > 0 && len(m.Data) <= 4096 {
				return nil
			}
		case "error":
			if len(m.Nonce) == 0 && len(m.Data) == 0 {
				return nil
			}
		}
	}
	return errors.New("invalid clock control frame")
}
