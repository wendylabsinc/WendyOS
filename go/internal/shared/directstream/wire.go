package directstream

import "encoding/binary"

// Flow records are carried only after complete-profile capability negotiation and
// authenticated attachment commit. The enclosing lane supplies record length.
// Integers are big endian. Attachment IDs are never reused on a TLS connection.
const (
	WireVersion     = 1
	MaxDataPayload  = 512
	MaxStateEntries = 4
	MaxFlowFrame    = 21 + MaxDataPayload
)

type FrameKind byte

const (
	FrameData FrameKind = 1 + iota
	FrameState
	FrameFIN
	FrameOpen
	FrameOpenResult
	FrameOpenCredit
	FrameDatagram
	FrameReset
	FrameCloseACK
	FrameDatagramLimit // Attachment-scoped maximum for new UDP OPENs; zero disables UDP.
	FrameEpochIdle     // Promoted epoch has drained.
)

const (
	ServiceTCP byte = 1 + iota
	ServiceUDP
	ServiceWendyCom
	ServiceGRPCAdmin
)

type StateEntry struct {
	FlowID          uint32
	Received, Limit uint64
	// FINSeen acknowledges the immutable final-offset promise, not its bytes.
	FINSeen bool
}

// FlowFrame is a bounded tagged record. Value is a FIN offset, cumulative OPEN
// credit, or datagram ID according to Kind. DatagramSize is the OPEN receive
// capacity or this datagram's complete length. Code is an OPEN result/reset code.
// Decoded Payload borrows the input: copy into reserved receive storage before
// acknowledging it, and never retain it after the lane reuses that storage.
type FlowFrame struct {
	Kind                                     FrameKind
	AttachmentID, FlowID                     uint32
	Payload                                  []byte
	States                                   [MaxStateEntries]StateEntry
	StateCount                               uint8
	Value                                    uint64
	Service                                  byte
	Port, DatagramSize, FragmentOffset, Code uint16
}

func (f FlowFrame) Size() (int, error) {
	if f.AttachmentID == 0 {
		return 0, ErrBounds
	}
	if f.Kind != FrameState && f.Kind != FrameOpenCredit && f.Kind != FrameDatagramLimit && f.Kind != FrameEpochIdle && f.FlowID == 0 {
		return 0, ErrBounds
	}
	switch f.Kind {
	case FrameEpochIdle:
		if f.FlowID != 0 {
			return 0, ErrBounds
		}
		return 5, nil
	case FrameDatagramLimit:
		if f.FlowID != 0 {
			return 0, ErrBounds
		}
		return 7, nil
	case FrameData:
		if len(f.Payload) == 0 || len(f.Payload) > MaxDataPayload {
			return 0, ErrBounds
		}
		return 9 + len(f.Payload), nil
	case FrameState:
		if f.StateCount == 0 || f.StateCount > MaxStateEntries {
			return 0, ErrBounds
		}
		for i, s := range f.States[:f.StateCount] {
			if s.FlowID == 0 || s.Received > s.Limit {
				return 0, ErrBounds
			}
			for j := 0; j < i; j++ {
				if f.States[j].FlowID == s.FlowID {
					return 0, ErrBounds
				}
			}
		}
		return 6 + 21*int(f.StateCount), nil
	case FrameFIN:
		return 17, nil
	case FrameOpen:
		switch f.Service {
		case ServiceTCP:
			if f.Port == 0 || f.DatagramSize != 0 {
				return 0, ErrBounds
			}
		case ServiceUDP:
			if f.Port == 0 || f.DatagramSize == 0 {
				return 0, ErrBounds
			}
		case ServiceWendyCom, ServiceGRPCAdmin:
			if f.Port != 0 || f.DatagramSize != 0 {
				return 0, ErrBounds
			}
		default:
			return 0, ErrBounds
		}
		return 14, nil
	case FrameOpenResult, FrameReset:
		return 11, nil
	case FrameOpenCredit:
		return 13, nil
	case FrameDatagram:
		if f.Value == 0 || len(f.Payload) > MaxDataPayload || int(f.FragmentOffset)+len(f.Payload) > int(f.DatagramSize) ||
			(f.DatagramSize != 0 && len(f.Payload) == 0) {
			return 0, ErrBounds
		}
		return 21 + len(f.Payload), nil
	case FrameCloseACK:
		return 9, nil
	default:
		return 0, ErrBounds
	}
}

// Encode writes no bytes on invalid input/insufficient space. dst must not
// alias Payload. Successful output owns its bytes independently of the frame.
func (f FlowFrame) Encode(dst []byte) (int, error) {
	n, err := f.Size()
	if err != nil || len(dst) < n {
		return 0, ErrBounds
	}
	dst[0] = byte(f.Kind)
	binary.BigEndian.PutUint32(dst[1:5], f.AttachmentID)
	if f.Kind != FrameState && f.Kind != FrameOpenCredit && f.Kind != FrameDatagramLimit && f.Kind != FrameEpochIdle {
		binary.BigEndian.PutUint32(dst[5:9], f.FlowID)
	}
	switch f.Kind {
	case FrameDatagramLimit:
		binary.BigEndian.PutUint16(dst[5:7], f.DatagramSize)
	case FrameData:
		copy(dst[9:n], f.Payload)
	case FrameState:
		dst[5] = f.StateCount
		for i, s := range f.States[:f.StateCount] {
			b := dst[6+21*i : 6+21*(i+1)]
			binary.BigEndian.PutUint32(b, s.FlowID)
			binary.BigEndian.PutUint64(b[4:], s.Received)
			binary.BigEndian.PutUint64(b[12:], s.Limit)
			b[20] = 0
			if s.FINSeen {
				b[20] = 1
			}
		}
	case FrameFIN:
		binary.BigEndian.PutUint64(dst[9:17], f.Value)
	case FrameOpen:
		dst[9] = f.Service
		binary.BigEndian.PutUint16(dst[10:12], f.Port)
		binary.BigEndian.PutUint16(dst[12:14], f.DatagramSize)
	case FrameOpenResult, FrameReset:
		binary.BigEndian.PutUint16(dst[9:11], f.Code)
	case FrameOpenCredit:
		binary.BigEndian.PutUint64(dst[5:13], f.Value)
	case FrameDatagram:
		binary.BigEndian.PutUint64(dst[9:17], f.Value)
		binary.BigEndian.PutUint16(dst[17:19], f.DatagramSize)
		binary.BigEndian.PutUint16(dst[19:21], f.FragmentOffset)
		copy(dst[21:n], f.Payload)
	}
	return n, nil
}

func DecodeFlowFrame(b []byte) (FlowFrame, error) {
	f, err := decodeFlowFrame(b)
	if err != nil {
		return FlowFrame{}, err
	}
	n, err := f.Size()
	if err != nil || n != len(b) {
		return FlowFrame{}, ErrBounds
	}
	return f, nil
}

func decodeFlowFrame(b []byte) (FlowFrame, error) {
	var f FlowFrame
	if len(b) < 5 || len(b) > MaxFlowFrame {
		return f, ErrBounds
	}
	f.Kind, f.AttachmentID = FrameKind(b[0]), binary.BigEndian.Uint32(b[1:5])
	if f.Kind != FrameState && f.Kind != FrameOpenCredit && f.Kind != FrameDatagramLimit && f.Kind != FrameEpochIdle {
		if len(b) < 9 {
			return f, ErrBounds
		}
		f.FlowID = binary.BigEndian.Uint32(b[5:9])
	}
	switch f.Kind {
	case FrameEpochIdle:
		if len(b) != 5 {
			return f, ErrBounds
		}
	case FrameDatagramLimit:
		if len(b) != 7 {
			return f, ErrBounds
		}
		f.DatagramSize = binary.BigEndian.Uint16(b[5:7])
	case FrameData:
		f.Payload = b[9:]
	case FrameState:
		if len(b) < 6 {
			return f, ErrBounds
		}
		f.StateCount = b[5]
		if f.StateCount == 0 || f.StateCount > MaxStateEntries || len(b) != 6+21*int(f.StateCount) {
			return f, ErrBounds
		}
		for i := 0; i < int(f.StateCount); i++ {
			s := b[6+21*i : 6+21*(i+1)]
			if s[20] > 1 {
				return f, ErrBounds
			}
			f.States[i] = StateEntry{binary.BigEndian.Uint32(s), binary.BigEndian.Uint64(s[4:]), binary.BigEndian.Uint64(s[12:]), s[20] != 0}
		}
	case FrameFIN:
		if len(b) != 17 {
			return f, ErrBounds
		}
		f.Value = binary.BigEndian.Uint64(b[9:])
	case FrameOpen:
		if len(b) != 14 {
			return f, ErrBounds
		}
		f.Service, f.Port, f.DatagramSize = b[9], binary.BigEndian.Uint16(b[10:]), binary.BigEndian.Uint16(b[12:])
	case FrameOpenResult, FrameReset:
		if len(b) != 11 {
			return f, ErrBounds
		}
		f.Code = binary.BigEndian.Uint16(b[9:])
	case FrameOpenCredit:
		if len(b) != 13 {
			return f, ErrBounds
		}
		f.Value = binary.BigEndian.Uint64(b[5:])
	case FrameDatagram:
		if len(b) < 21 {
			return f, ErrBounds
		}
		f.Value = binary.BigEndian.Uint64(b[9:])
		f.DatagramSize = binary.BigEndian.Uint16(b[17:])
		f.FragmentOffset = binary.BigEndian.Uint16(b[19:])
		f.Payload = b[21:]
	case FrameCloseACK:
		if len(b) != 9 {
			return f, ErrBounds
		}
	default:
		return f, ErrBounds
	}
	return f, nil
}
