package thermal

import (
	"encoding/binary"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"testing"
)

func TestHIKTemperatureSummary(t *testing.T) {
	layout := &pb.RawFormat{Fourcc: "YUYV", Width: 256, Height: 392, BytesPerLine: 512, ThermalPlane: &pb.ThermalPlane{Adapter: TOPDONTC001HIK}}
	data := make([]byte, 200704)
	binary.LittleEndian.PutUint32(data[193*512:], 0xaabbccdd)
	// Metadata observed on mesh-1. These differ from the sensor pixel values.
	words := []uint16{65535, 61166, 5346, 1000, 973, 4472, 74, 486, 4591, 4557}
	for i, v := range words {
		binary.LittleEndian.PutUint16(data[388*512+i*2:], v)
	}
	s := DecodeTemperatureSummary(data, layout)
	if s == nil || s.HotCelsius != 33.53125 || s.ColdCelsius != 19.875 || s.Hottest.X != 255 || s.Hottest.Y != 186 || s.Coldest.X != 19 || s.Coldest.Y != 93 {
		t.Fatalf("unexpected summary: %+v", s)
	}
	for _, offset := range []int{193 * 512, 388 * 512, 388*512 + 6, 388*512 + 10} {
		changed := append([]byte(nil), data...)
		value := uint16(65535)
		if offset == 388*512 {
			value = 0
		}
		binary.LittleEndian.PutUint16(changed[offset:], value)
		if DecodeTemperatureSummary(changed, layout) != nil {
			t.Fatalf("accepted corrupt offset %d", offset)
		}
	}
	if DecodeTemperatureSummary(data[:100], layout) != nil {
		t.Fatal("accepted truncated frame")
	}
	layout.ThermalPlane.Adapter = TOPDONTC001
	if DecodeTemperatureSummary(data, layout) != nil {
		t.Fatal("accepted wrong camera family")
	}
}

func TestHIK96CameraReportedTemperatures(t *testing.T) {
	layout := &pb.RawFormat{Fourcc: "YUYV", Width: 96, Height: 200, BytesPerLine: 192, ThermalPlane: &pb.ThermalPlane{Adapter: TOPDONTC001HIK}}
	data := make([]byte, 38400)
	// Sensor counts are deliberately unrelated to the camera temperature codes.
	for i := 0; i < 96*192; i += 2 {
		binary.LittleEndian.PutUint16(data[i:], 5600)
	}
	binary.LittleEndian.PutUint32(data[97*192:], 0xaabbccdd)
	binary.LittleEndian.PutUint32(data[97*192+8:], 96)
	binary.LittleEndian.PutUint32(data[97*192+12:], 96)
	// A changed, live summary from SER9, with its sensor extrema at (95,85)/(13,2).
	words := []uint16{65535, 61166, 5228, 1000, 894, 4563, 136, 21, 4600, 4591}
	for i, v := range words {
		binary.LittleEndian.PutUint16(data[196*192+i*2:], v)
	}
	s := DecodeTemperatureSummary(data, layout)
	if s == nil || s.HotCelsius != 31.6875 || s.ColdCelsius != 21.296875 || s.Hottest.X != 95 || s.Hottest.Y != 85 || s.Coldest.X != 13 || s.Coldest.Y != 2 {
		t.Fatalf("incorrect 96x96 camera summary: %+v", s)
	}
	for _, offset := range []int{97 * 192, 97*192 + 8, 97*192 + 12, 196 * 192, 196*192 + 4, 196*192 + 10} {
		bad := append([]byte(nil), data...)
		binary.LittleEndian.PutUint16(bad[offset:], 0)
		if DecodeTemperatureSummary(bad, layout) != nil {
			t.Fatalf("accepted corrupt metadata at %d", offset)
		}
	}
	if DecodeTemperatureSummary(data[:len(data)-1], layout) != nil {
		t.Fatal("accepted incomplete 96x200 frame")
	}
	layout.BytesPerLine = 200
	if DecodeTemperatureSummary(data, layout) != nil {
		t.Fatal("accepted an unverified metadata stride")
	}
}
