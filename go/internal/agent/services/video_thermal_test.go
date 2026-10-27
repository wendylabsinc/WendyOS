package services

import (
	"encoding/binary"
	"github.com/wendylabsinc/wendy/go/internal/agent/data/thermal"
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
	f.rawFmt = describeThermalCapture(&pb.ThermalCamera{Adapter: thermal.PureThermal}, f.rawFmt)
	measured := rawFrameExtrema(f)
	if measured == nil || measured.ColdRaw != 30000 || measured.ColdX != 1 || measured.HotRaw != 32000 || measured.HotX != 11 {
		t.Fatalf("wrong measurements: %v", measured)
	}
	f.rawFmt.Fourcc = "UYVY"
	f.rawFmt.ThermalPlane = nil
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

func TestThermalProbeRequiresUSBIdentityAndMode(t *testing.T) {
	previous := thermalUSBIdentity
	thermalUSBIdentity = func(string) (string, string) { return "2bdf", "0102" }
	t.Cleanup(func() { thermalUSBIdentity = previous })
	withYUYVModes(t, [][2]uint32{{256, 392}, {512, 384}})
	p := probeThermalCamera("/dev/video2", "USB Camera: UVC Camera")
	if p.GetAdapter() != thermal.TOPDONTC001HIK || p.GetCaptureHeight() != 392 {
		t.Fatalf("wrong profile: %v", p)
	}
	thermalUSBIdentity = func(string) (string, string) { return "046d", "08e5" }
	if p := probeThermalCamera("/dev/video2", "USB Camera"); p != nil {
		t.Fatalf("webcam classified as thermal: %v", p)
	}
	thermalUSBIdentity = func(string) (string, string) { return "0bda", "5830" }
	if p := probeThermalCamera("/dev/video2", "USB Camera"); p != nil {
		t.Fatalf("accepted unrecognized bridge layout: %v", p)
	}
}

func TestDescribeThermalCapturePreservesRawContract(t *testing.T) {
	l := &pb.RawFormat{Width: 256, Height: 392, Fourcc: "YUYV", BytesPerLine: 512}
	described := describeThermalCapture(&pb.ThermalCamera{Adapter: thermal.TOPDONTC001HIK}, l)
	if l.ThermalPlane != nil || described.ThermalPlane == nil || described.Width != l.Width || described.Height != l.Height || described.BytesPerLine != l.BytesPerLine {
		t.Fatal("capture description mutated or missing")
	}
	data := make([]byte, 512*392)
	for i := 0; i < 256*192*2; i += 2 {
		binary.LittleEndian.PutUint16(data[i:], 5000)
	}
	for i := 256 * 192 * 2; i < len(data); i++ {
		data[i] = 255
	}
	binary.LittleEndian.PutUint16(data[10:], 6000)
	f := &videoFrame{codec: pb.VideoCodec_VIDEO_CODEC_RAW, data: data, rawFmt: described}
	summary := rawFrameExtrema(f)
	if summary.GetHotRaw() != 6000 || summary.GetHotX() != 5 {
		t.Fatalf("metadata became sensor values: %v", summary)
	}
	f.rawFmt = l
	if rawFrameExtrema(f) != nil {
		t.Fatal("unidentified YUYV frame became thermal")
	}
}
