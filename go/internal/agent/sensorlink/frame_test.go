package sensorlink_test

import (
	"bytes"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

func chunk(ch, frame, seq uint32, last bool, payload string) *sensorlinkpb.SensorData {
	d := &sensorlinkpb.SensorData{ChannelId: ch, FrameSeq: frame, ChunkSeq: seq, TsUs: uint64(frame) * 1000, Payload: []byte(payload)}
	if last {
		d.Flags |= sensorlink.FlagLastChunk
	}
	return d
}

func TestAssemblerSingleChunkFrame(t *testing.T) {
	var a sensorlink.Assembler
	d := chunk(4, 9, 0, true, "jpeg")
	d.Flags |= sensorlink.FlagKeyframe
	f := a.Add(d)
	if f == nil {
		t.Fatal("single-chunk frame not returned")
	}
	if f.ChannelID != 4 || f.Seq != 9 || f.TsUs != 9000 || string(f.Payload) != "jpeg" {
		t.Fatalf("frame = %+v", f)
	}
	if f.Flags != sensorlink.FlagKeyframe {
		t.Fatalf("flags = %#x, want keyframe only", f.Flags)
	}
	if &f.Payload[0] != &d.Payload[0] {
		t.Error("single-chunk payload was copied")
	}
}

func TestAssemblerMultiChunkFrame(t *testing.T) {
	var a sensorlink.Assembler
	first := chunk(1, 5, 0, false, "ab")
	first.Flags |= sensorlink.FlagKeyframe
	if f := a.Add(first); f != nil {
		t.Fatalf("frame returned early: %+v", f)
	}
	// The source may reuse its buffer once a chunk is handed over.
	copy(first.Payload, "xx")
	if f := a.Add(chunk(1, 5, 1, false, "cd")); f != nil {
		t.Fatalf("frame returned early: %+v", f)
	}
	f := a.Add(chunk(1, 5, 2, true, "ef"))
	if f == nil {
		t.Fatal("frame not completed by its last chunk")
	}
	if f.ChannelID != 1 || f.Seq != 5 || f.TsUs != 5000 || f.Flags != sensorlink.FlagKeyframe || string(f.Payload) != "abcdef" {
		t.Fatalf("frame = %+v", f)
	}
}

func TestAssemblerInterleavedChannels(t *testing.T) {
	var a sensorlink.Assembler
	a.Add(chunk(1, 7, 0, false, "cam-"))
	a.Add(chunk(2, 3, 0, false, "mic-"))
	if f := a.Add(chunk(1, 7, 1, true, "a")); f == nil || f.ChannelID != 1 || string(f.Payload) != "cam-a" {
		t.Fatalf("channel 1 frame = %+v", f)
	}
	if f := a.Add(chunk(2, 3, 1, true, "b")); f == nil || f.ChannelID != 2 || string(f.Payload) != "mic-b" {
		t.Fatalf("channel 2 frame = %+v", f)
	}
}

func TestAssemblerDiscardsIncompleteFrames(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks []*sensorlinkpb.SensorData
	}{
		{"missing middle chunk", []*sensorlinkpb.SensorData{
			chunk(1, 5, 0, false, "a"), chunk(1, 5, 2, true, "c"),
		}},
		{"missing first chunk", []*sensorlinkpb.SensorData{
			chunk(1, 5, 1, false, "b"), chunk(1, 5, 2, true, "c"),
		}},
		{"chunk from another frame", []*sensorlinkpb.SensorData{
			chunk(1, 5, 0, false, "a"), chunk(1, 6, 1, true, "b"),
		}},
		{"duplicate chunk", []*sensorlinkpb.SensorData{
			chunk(1, 5, 0, false, "a"), chunk(1, 5, 1, false, "b"), chunk(1, 5, 1, true, "b"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var a sensorlink.Assembler
			for _, d := range tc.chunks {
				if f := a.Add(d); f != nil {
					t.Fatalf("incomplete frame returned: %+v", f)
				}
			}
			// The channel recovers on the next frame.
			if f := a.Add(chunk(1, 8, 0, true, "ok")); f == nil || f.Seq != 8 {
				t.Fatalf("next frame = %+v", f)
			}
		})
	}
}

func TestAssemblerNewFrameDropsUnfinishedOne(t *testing.T) {
	var a sensorlink.Assembler
	a.Add(chunk(1, 5, 0, false, "old"))
	a.Add(chunk(1, 6, 0, false, "new-"))
	f := a.Add(chunk(1, 6, 1, true, "tail"))
	if f == nil || f.Seq != 6 || string(f.Payload) != "new-tail" {
		t.Fatalf("frame = %+v", f)
	}
	if f := a.Add(chunk(1, 5, 1, true, "late")); f != nil {
		t.Fatalf("abandoned frame completed: %+v", f)
	}
}

func TestAssemblerCapsFrameSize(t *testing.T) {
	var a sensorlink.Assembler
	half := string(bytes.Repeat([]byte{'x'}, sensorlink.MaxFrameBytes/2))
	a.Add(chunk(1, 5, 0, false, half))
	a.Add(chunk(1, 5, 1, false, half))
	if f := a.Add(chunk(1, 5, 2, true, "y")); f != nil {
		t.Fatalf("oversized frame returned (%d bytes)", len(f.Payload))
	}
}
