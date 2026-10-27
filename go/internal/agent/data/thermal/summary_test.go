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
