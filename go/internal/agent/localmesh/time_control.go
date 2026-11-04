package localmesh

import "errors"

const timestampControlVersion = 1

// TimestampControl exchanges public nonce-bearing TSA bytes inside the already
// authenticated router stream. Replies are pulled one 1KiB piece at a time.
// Version 0 (including an absent field) retains the original version 1 meaning.
type TimestampControl struct {
	Version uint32 `json:"v" cbor:"v"`
	Op      string `json:"op" cbor:"o"`
	ID      uint64 `json:"id" cbor:"i"`
	Offset  uint32 `json:"offset" cbor:"f"`
	Total   uint32 `json:"total,omitempty" cbor:"l,omitempty"`
	Data    []byte `json:"data,omitempty" cbor:"d,omitempty"`
}

func knownTimestampOp(op string) bool {
	switch op {
	case "request", "next", "reply", "error":
		return true
	}
	return false
}

func (m *TimestampControl) supported() bool {
	return m != nil && m.Version <= timestampControlVersion && knownTimestampOp(m.Op)
}

func (m *TimestampControl) validate() error {
	if !m.supported() || m.ID == 0 {
		return errors.New("missing timestamp request")
	}
	switch m.Op {
	case "request":
		if len(m.Data) > 0 && len(m.Data) <= 128 && m.Offset == 0 && m.Total == 0 {
			return nil
		}
	case "next":
		if len(m.Data) == 0 && m.Offset > 0 && m.Offset < 65536 && m.Total == 0 {
			return nil
		}
	case "reply":
		if len(m.Data) > 0 && len(m.Data) <= 1024 && m.Total > 0 && m.Total <= 65536 && uint64(m.Offset)+uint64(len(m.Data)) <= uint64(m.Total) {
			return nil
		}
	case "error":
		if len(m.Data) == 0 && m.Offset == 0 && m.Total == 0 {
			return nil
		}
	}
	return errors.New("invalid timestamp control frame")
}
