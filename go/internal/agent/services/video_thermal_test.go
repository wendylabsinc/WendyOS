package services

import (
	"encoding/binary"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"testing"
)

func TestRawThermalExtremaUseSensorSamples(t *testing.T) {
	data := make([]byte, 80*60*2)
	for i := 0; i < len(data); i += 2 {
		binary.LittleEndian.PutUint16(data[i:], 30000)
	}
	binary.LittleEndian.PutUint16(data[0:], 0)
	binary.LittleEndian.PutUint16(data[22:], 32000)
	f := &videoFrame{codec: pb.VideoCodec_VIDEO_CODEC_RAW, data: data, rawFmt: &pb.RawFormat{Width: 80, Height: 60, BytesPerLine: 160, Fourcc: "Y16 "}}
	measured := rawFrameExtrema(f)
	if measured == nil || measured.ColdRaw != 30000 || measured.ColdX != 1 || measured.HotRaw != 32000 || measured.HotX != 11 {
		t.Fatalf("wrong measurements: %v", measured)
	}
	f.rawFmt.Fourcc = "UYVY"
	if rawFrameExtrema(f) != nil {
		t.Fatal("measured preview brightness")
	}
	f.rawFmt.Fourcc = "Y16 "
	f.rawFmt.Height = 63
	if rawFrameExtrema(f) != nil {
		t.Fatal("included telemetry rows")
	}
	if rawFrameExtrema(nil) != nil {
		t.Fatal("nil frame")
	}
}
