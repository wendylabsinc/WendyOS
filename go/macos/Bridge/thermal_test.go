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

func TestThermalFFIPreservesConversionAndAgentCapability(t *testing.T) {
	p := events([]backend.Event{{Kind: backend.EventThermal, Thermal: &backend.ThermalFrame{Width: 256, Height: 192,
		Temperature: &pb.ThermalLinearTemperature{KelvinPerCount: 1.0 / 64, KelvinOffset: 223.15, Provenance: "fixture"}}}})
	defer WDEventsFree(p)
	f := unsafe.Slice(p.items, int(p.count))[0].thermal
	if f.has_temperature != 1 || f.kelvin_per_count != 1.0/64 || f.kelvin_offset != 223.15 || f.allow_scale_override != 0 {
		t.Fatal("temperature capability lost across FFI")
	}
	listed := cameras([]*pb.VideoDevice{{Name: "USB Camera", Thermal: &pb.ThermalCamera{Adapter: thermal.TOPDONTC001HIK}}}, nil)
	defer WDCamerasFree(listed)
	if unsafe.Slice(listed.items, int(listed.count))[0].thermal != 1 {
		t.Fatal("agent thermal capability lost")
	}
}

func TestThermalFFIPreservesSeparateSummary(t *testing.T) {
	p := events([]backend.Event{{Kind: backend.EventThermal, Thermal: &backend.ThermalFrame{SummaryUpdatedNS: 123,
		Summary: &thermal.TemperatureSummary{Coldest: thermal.Point{X: 19, Y: 93}, Hottest: thermal.Point{X: 255, Y: 186}, ColdCelsius: 19.875, HotCelsius: 33.53125}}}})
	defer WDEventsFree(p)
	f := unsafe.Slice(p.items, int(p.count))[0].thermal
	if f.summary_updated_ns != 123 || f.summary_cold_x != 19 || f.summary_hot_y != 186 || f.summary_cold_celsius != 19.875 || f.summary_hot_celsius != 33.53125 || f.has_temperature != 0 {
		t.Fatal("summary confused with pixel calibration")
	}
}
