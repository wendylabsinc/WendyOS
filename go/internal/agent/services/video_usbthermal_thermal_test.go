package services

import (
	"context"
	"encoding/binary"
	"math"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data/thermal"
	"github.com/wendylabsinc/wendy/go/internal/agent/usbthermal"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// TestUSBThermalDeclaredAsThermalCamera: the TC001 Max is listed with the
// thermal capability ThermalNotify and the desktop viewer look for, naming the
// TC001 Max adapter and its one raw mode.
func TestUSBThermalDeclaredAsThermalCamera(t *testing.T) {
	svc, _ := newUSBThermalTestService(t)
	resp, err := svc.ListVideoDevices(context.Background(), &agentpb.ListVideoDevicesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var cam *agentpb.VideoDevice
	for _, d := range resp.GetDevices() {
		if d.GetId() == usbthermal.IDBandStart {
			cam = d
		}
	}
	if cam == nil {
		t.Fatal("the TC001 Max is not listed")
	}
	p := cam.GetThermal()
	if p.GetAdapter() != thermal.TOPDONTC001Max || p.GetCaptureWidth() != 256 || p.GetCaptureHeight() != 384 {
		t.Fatalf("thermal capability %v, want %s 256x384", p, thermal.TOPDONTC001Max)
	}
	if thermal.CameraProfile(cam) != p {
		t.Fatal("clients would not use the declared capability")
	}
}

// TestUSBThermalRawFramesCarryThermalPlane: every raw frame names the
// temperature plane (bottom 192 rows, 1/64 K) and carries the extrema the
// agent measured from it, so a client converts it without knowing the camera.
func TestUSBThermalRawFramesCarryThermalPlane(t *testing.T) {
	svc, fake := newUSBThermalTestService(t)
	stream, cancel, done := startStream(svc, &agentpb.StreamVideoRequest{
		DeviceId: usbthermal.IDBandStart, StableId: testThermalStableID, Codec: agentpb.VideoCodec_VIDEO_CODEC_RAW,
	})
	defer cancel()

	frame := testRawFrame(0x80)
	// Room temperature everywhere in the plane, one hot pixel at (10, 20).
	room := uint16(math.Round((22 + 273.15) * 64))
	hot := uint16(math.Round((61.5 + 273.15) * 64))
	for i := 192 * 512; i < 384*512; i += 2 {
		binary.LittleEndian.PutUint16(frame.Data[i:], room)
	}
	binary.LittleEndian.PutUint16(frame.Data[(192+20)*512+10*2:], hot)
	select {
	case fake.feed <- frame:
	case err := <-done:
		t.Fatalf("StreamVideo ended: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the raw request did not reach the raw tap")
	}
	var got *agentpb.VideoFrame
	select {
	case got = <-stream.frames:
	case <-time.After(5 * time.Second):
		t.Fatal("no frame reached the client")
	}
	plane := got.GetRawFormat().GetThermalPlane()
	if plane.GetAdapter() != thermal.TOPDONTC001Max || plane.GetWidth() != 256 || plane.GetHeight() != 192 ||
		plane.GetOffsetBytes() != 192*512 || plane.GetRowStrideBytes() != 512 ||
		plane.GetTemperature().GetKelvinPerCount() != 1.0/64 || plane.GetTemperature().GetProvenance() == "" {
		t.Fatalf("thermal plane %v", plane)
	}
	ext := got.GetRawExtrema()
	if ext.GetHotX() != 10 || ext.GetHotY() != 20 || ext.GetHotRaw() != uint32(hot) || ext.GetColdRaw() != uint32(room) {
		t.Fatalf("extrema %v", ext)
	}
	f, err := thermal.Decode(got.GetData(), got.GetRawFormat())
	if err != nil {
		t.Fatal(err)
	}
	celsius := float64(f.Hottest.Raw)*f.Temperature.KelvinPerCount + f.Temperature.KelvinOffset - 273.15
	if math.Abs(celsius-61.5) > 0.02 {
		t.Fatalf("hottest pixel reads %.3f degC, want 61.5", celsius)
	}
}
