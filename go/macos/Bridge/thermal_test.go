package main

import (
	"testing"
	"unsafe"

	"github.com/wendylabsinc/wendy/go/internal/agent/data/thermal"
	"github.com/wendylabsinc/wendy/go/macos/backend"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestThermalFFIOwnsPixelsAndMeasurements(t *testing.T) {
	pixels := []byte{10, 20, 30, 255}
	p := events([]backend.Event{{Kind: backend.EventThermal, Session: 7, Stream: 42, Bytes: pixels, Thermal: &backend.ThermalFrame{Width: 80, Height: 60, Coldest: thermal.Point{X: 1, Y: 2, Raw: 28000}, Hottest: thermal.Point{X: 79, Y: 59, Raw: 32000}, TimestampNS: 123}}})
	defer WDEventsFree(p)
	pixels[0] = 99
	e := unsafe.Slice(p.items, int(p.count))[0]
	if e.thermal.cold_x != 1 || e.thermal.hot_y != 59 || e.thermal.hot_raw != 32000 || e.thermal.timestamp_ns != 123 || e.byte_count != 4 || e.stream != 42 {
		t.Fatal("thermal fields lost across FFI")
	}
	if unsafe.Slice(e.bytes, int(e.byte_count))[0] != 10 {
		t.Fatal("pixels borrowed instead of copied")
	}
	cameras := cameras([]*pb.VideoDevice{{Name: "PureThermal", PixelFormats: []string{"Y16 "}}, {Name: "Depth camera", PixelFormats: []string{"Y16 "}}}, nil)
	defer WDCamerasFree(cameras)
	list := unsafe.Slice(cameras.items, int(cameras.count))
	if list[0].thermal != 1 || list[1].thermal != 0 {
		t.Fatal("thermal capability lost across FFI")
	}
}
