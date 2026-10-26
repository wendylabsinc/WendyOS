// Package udpmessage preserves complete unreliable IPv4 UDP messages across
// bounded transport chunks. It never retransmits an admitted fragment.
package udpmessage

import (
	"encoding/binary"
	"errors"
)

const (
	MaxMessageSize = 65507
	MaxChunkSize   = 1000
	MinChunkSize   = 256
	HeaderSize     = 20
	MaxFragments   = (MaxMessageSize + MinChunkSize - HeaderSize - 1) / (MinChunkSize - HeaderSize)
	WindowSize     = 4096
)

var (
	ErrMalformed = errors.New("invalid Wendy Net UDP message fragment")
	ErrCapacity  = errors.New("Wendy Net UDP message capacity exhausted")
	ErrClosed    = errors.New("Wendy Net UDP message flow closed")
	errExpired   = errors.New("Wendy Net UDP message lifetime ended")
	ErrQueueFull = errors.New("Wendy Net UDP transport queue full")
)

type ChunkTooLarge struct{ Maximum int }

func (*ChunkTooLarge) Error() string { return "Wendy Net UDP transport chunk too large" }

type fragment struct {
	id                          uint64
	total, stride, index, count int
	data                        []byte
}

func decode(b []byte, maximum int) (fragment, error) {
	if len(b) < HeaderSize || len(b) > maximum || b[0] != 'W' || b[1] != 'U' || b[2] != 2 || b[3] != 0 {
		return fragment{}, ErrMalformed
	}
	f := fragment{id: binary.BigEndian.Uint64(b[4:12]), total: int(binary.BigEndian.Uint16(b[12:14])), stride: int(binary.BigEndian.Uint16(b[14:16])), index: int(binary.BigEndian.Uint16(b[16:18])), count: int(binary.BigEndian.Uint16(b[18:20])), data: b[HeaderSize:]}
	if f.id == 0 || f.total > MaxMessageSize || f.stride < MinChunkSize-HeaderSize || f.stride > maximum-HeaderSize {
		return fragment{}, ErrMalformed
	}
	count := max(1, (f.total+f.stride-1)/f.stride)
	if f.count != count || f.count > MaxFragments || f.index >= count {
		return fragment{}, ErrMalformed
	}
	if len(f.data) != min(f.stride, f.total-f.index*f.stride) {
		return fragment{}, ErrMalformed
	}
	return f, nil
}
func encode(id uint64, payload []byte, stride, index int) []byte {
	offset := index * stride
	b := make([]byte, HeaderSize+min(stride, len(payload)-offset))
	b[0], b[1], b[2] = 'W', 'U', 2
	binary.BigEndian.PutUint64(b[4:12], id)
	binary.BigEndian.PutUint16(b[12:14], uint16(len(payload)))
	binary.BigEndian.PutUint16(b[14:16], uint16(stride))
	binary.BigEndian.PutUint16(b[16:18], uint16(index))
	binary.BigEndian.PutUint16(b[18:20], uint16(max(1, (len(payload)+stride-1)/stride)))
	copy(b[HeaderSize:], payload[offset:])
	return b
}
