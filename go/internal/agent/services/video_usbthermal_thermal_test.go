package services

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/binary"
	"io"
	"math"
	"os"
	"slices"
	"strings"
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

// readUSBThermalFixture reads one of usbthermal's real TC001 Max captures.
func readUSBThermalFixture(t *testing.T, name string) []byte {
	t.Helper()
	text, err := os.ReadFile("../usbthermal/testdata/" + name + ".gz.b64")
	if err != nil {
		t.Fatal(err)
	}
	z, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(text)), ""))
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(z))
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestUSBThermalPlaneOverTheRealStraddledFrame: the frame the raw tap served
// on exxon-01 on 2026-10-30 with a composite-frame header in its temperature
// rows, read through the thermal_plane this build declares. As served it reads
// 682.52 degC at (3, 126): that is what ThermalNotify alerted on, and the plane
// itself (offset, rows, 1/64 K) was right. Rebuilt into its stream and parsed
// by the fixed driver, the same plane reads the room.
func TestUSBThermalPlaneOverTheRealStraddledFrame(t *testing.T) {
	layout := usbthermal.Layout{Width: 256, Height: 192}
	format := describeThermalCapture(usbThermalProfile(layout), usbThermalRawFormat(layout))
	hottest := func(data []byte) (float64, uint32, uint32) {
		t.Helper()
		f, err := thermal.Decode(data, format)
		if err != nil {
			t.Fatal(err)
		}
		return float64(f.Hottest.Raw)*f.Temperature.KelvinPerCount + f.Temperature.KelvinOffset - 273.15, f.Hottest.X, f.Hottest.Y
	}

	served := readUSBThermalFixture(t, "tc001max-straddled-frame.bin")
	if c, x, y := hottest(served); math.Abs(c-682.52) > 0.01 || x != 3 || y != 126 {
		t.Fatalf("served frame reads %.2f degC at (%d, %d), want 682.52 at (3, 126)", c, x, y)
	}

	// The stream around it, as in usbthermal's TestParserRealStraddledFrame:
	// visible bytes, the served bytes, the six temperature pixels the header
	// pushed past the frame, then the fixture block's metadata and end rows.
	window := readUSBThermalFixture(t, "tc001max-block-window.bin")
	const margin = 4096
	lastSix := []byte{0xd8, 0x4b, 0xe0, 0x4b, 0xe4, 0x4b, 0xe0, 0x4b, 0xe4, 0x4b, 0xe0, 0x4b}
	stream := slices.Concat(window[:margin], served, lastSix, window[margin+layout.MetadataOffset():])
	var blocks [][]byte
	p := usbthermal.NewParser(layout)
	p.Feed(stream, func(b []byte) { blocks = append(blocks, b) })
	if len(blocks) != 1 || p.Stats().Spliced != 1 {
		t.Fatalf("parsed %d blocks, stats %+v; want one, spliced", len(blocks), p.Stats())
	}
	raw := blocks[0][:layout.RawBytes()]
	if c, x, y := hottest(raw); c > 32 || y > 10 {
		t.Fatalf("parsed frame reads %.2f degC at (%d, %d), want the room's warm corner, 31.2 degC", c, x, y)
	}
	if err := (usbthermal.RawFrame{Data: raw, Width: 256, Height: 384, BytesPerLine: 512}).CheckTemperatures(); err != nil {
		t.Fatal(err)
	}
	ext := rawFrameExtrema(&videoFrame{data: raw, codec: agentpb.VideoCodec_VIDEO_CODEC_RAW, rawFmt: format})
	if c := usbthermal.CountsToCelsius(uint16(ext.GetHotRaw())); c > 32 {
		t.Fatalf("frame extrema hottest %.2f degC", c)
	}
}
