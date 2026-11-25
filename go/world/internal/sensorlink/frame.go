package sensorlink

import (
	"errors"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

const (
	FlagKeyframe  uint32 = 1 << 0
	FlagLastChunk uint32 = 1 << 1
)

type Frame struct {
	ChannelID uint32
	Seq       uint32
	TsUs      uint64
	Flags     uint32
	Payload   []byte
}
type SensorFrame = Frame

type partial struct {
	frame Frame
	next  uint32
}
type channelState struct {
	partial   *partial
	seen      bool
	sequence  uint32
	timestamp uint64
}

// Assembler is single-threaded. It keeps at most one partial frame per selected
// channel. The channel set and per-frame allocation are bounded at construction.
type Assembler struct {
	channels map[uint32]*channelState
	maxBytes int
	dropped  uint64
}

func NewAssembler(channels []uint32, maxBytes int) (*Assembler, error) {
	if maxBytes == 0 {
		maxBytes = MaxFrameBytes
	}
	if len(channels) < 1 || len(channels) > MaxChannels || maxBytes < 1 || maxBytes > MaxFrameBytes {
		return nil, errors.New("invalid sensorlink assembly bounds")
	}
	a := &Assembler{channels: map[uint32]*channelState{}, maxBytes: maxBytes}
	for _, id := range channels {
		if id == 0 || a.channels[id] != nil {
			return nil, errors.New("selected channel IDs must be nonzero and unique")
		}
		a.channels[id] = &channelState{}
	}
	return a, nil
}

func (a *Assembler) Dropped() uint64 { return a.dropped }

// Add discards an incomplete, duplicate, old or inconsistent frame as a whole.
// Unknown channels, unsupported flags and oversized chunks fail the session.
// Sequence comparison follows the 32-bit serial half-range rule, including wrap.
func (a *Assembler) Add(d *pb.SensorData) (*Frame, error) {
	if a == nil || d == nil {
		return nil, errors.New("nil sensorlink assembler or data")
	}
	s := a.channels[d.ChannelId]
	if s == nil {
		return nil, errors.New("data arrived on an unselected channel")
	}
	if d.Flags & ^uint32(FlagKeyframe|FlagLastChunk) != 0 || len(d.Payload) == 0 || len(d.Payload) > a.maxBytes {
		return nil, errors.New("invalid sensorlink data flags or size")
	}
	last := d.Flags&FlagLastChunk != 0
	flags := d.Flags &^ FlagLastChunk
	if d.ChunkSeq == 0 {
		if s.partial != nil {
			s.partial = nil
			a.dropped++
		}
		if s.seen {
			delta := d.FrameSeq - s.sequence
			if delta == 0 || delta >= 1<<31 || d.TsUs <= s.timestamp {
				a.dropped++
				return nil, nil
			}
		}
		// Fence this started version even if its later chunks never arrive.
		s.seen, s.sequence, s.timestamp = true, d.FrameSeq, d.TsUs
		f := Frame{ChannelID: d.ChannelId, Seq: d.FrameSeq, TsUs: d.TsUs, Flags: flags, Payload: append([]byte(nil), d.Payload...)}
		if last {
			return &f, nil
		}
		s.partial = &partial{frame: f, next: 1}
		return nil, nil
	}
	p := s.partial
	if p == nil {
		a.dropped++
		return nil, nil
	}
	if d.FrameSeq != p.frame.Seq || d.ChunkSeq != p.next || d.TsUs != p.frame.TsUs || (flags != 0 && flags != p.frame.Flags) || len(d.Payload) > a.maxBytes-len(p.frame.Payload) {
		s.partial = nil
		a.dropped++
		return nil, nil
	}
	p.frame.Payload = append(p.frame.Payload, d.Payload...)
	p.next++
	if last {
		s.partial = nil
		return &p.frame, nil
	}
	return nil, nil
}
