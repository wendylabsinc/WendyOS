package sensorlink

import (
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

// SensorData.flags bits.
const (
	FlagKeyframe  uint32 = 1 << 0
	FlagLastChunk uint32 = 1 << 1
)

// SensorFrame is one whole frame, reassembled from the SensorData chunks a
// source sends for it.
type SensorFrame struct {
	ChannelID uint32
	Seq       uint32 // SensorData.frame_seq
	TsUs      uint64 // from the frame's first chunk
	Flags     uint32 // the first chunk's flags, without FlagLastChunk
	Payload   []byte
}

// Assembler reassembles SensorData chunks into SensorFrames, one frame in
// progress per channel so channels may interleave. A frame missing any chunk
// is discarded whole. An Assembler is not safe for concurrent use.
type Assembler struct {
	partial map[uint32]*partialFrame
}

type partialFrame struct {
	frame     SensorFrame
	nextChunk uint32
}

// Add feeds one chunk and returns the frame it completes, or nil. A frame
// sent as a single chunk is returned without copying its payload; a
// multi-chunk frame's payload is a fresh buffer.
func (a *Assembler) Add(d *sensorlinkpb.SensorData) *SensorFrame {
	ch := d.GetChannelId()
	last := d.GetFlags()&FlagLastChunk != 0
	if d.GetChunkSeq() == 0 {
		// A new frame starts: whatever was in progress lost its tail.
		delete(a.partial, ch)
		f := SensorFrame{
			ChannelID: ch,
			Seq:       d.GetFrameSeq(),
			TsUs:      d.GetTsUs(),
			Flags:     d.GetFlags() &^ FlagLastChunk,
		}
		if last {
			f.Payload = d.GetPayload()
			return &f
		}
		f.Payload = append([]byte(nil), d.GetPayload()...)
		if a.partial == nil {
			a.partial = make(map[uint32]*partialFrame)
		}
		a.partial[ch] = &partialFrame{frame: f, nextChunk: 1}
		return nil
	}
	p := a.partial[ch]
	if p == nil {
		return nil // the frame's first chunk was lost; wait for the next frame
	}
	if d.GetFrameSeq() != p.frame.Seq || d.GetChunkSeq() != p.nextChunk ||
		len(p.frame.Payload)+len(d.GetPayload()) > MaxFrameBytes {
		delete(a.partial, ch)
		return nil
	}
	p.frame.Payload = append(p.frame.Payload, d.GetPayload()...)
	p.nextChunk++
	if !last {
		return nil
	}
	delete(a.partial, ch)
	return &p.frame
}
