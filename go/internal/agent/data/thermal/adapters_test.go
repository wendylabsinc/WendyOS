package thermal

import (
	"encoding/binary"
	"math"
	"testing"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/protobuf/proto"
)

func TestTC001VariantsDecodeOnlySensorPlane(t *testing.T) {
	for _, tc := range []struct {
		adapter       string
		height, start uint32
		temperature   bool
	}{
		{TOPDONTC001, 384, 192, true}, {TOPDONTC001HIK, 392, 0, false},
	} {
		t.Run(tc.adapter, func(t *testing.T) {
			// Padded capture rows and poison preview/header bytes must not
			// affect sensor coordinates, extrema, or temperature conversion.
			layout := &pb.RawFormat{Width: 256, Height: tc.height, Fourcc: "YUYV", BytesPerLine: 520}
			payload := make([]byte, layout.BytesPerLine*layout.Height)
			for i := range payload {
				payload[i] = 255
			}
			for y := uint32(0); y < 192; y++ {
				for x := uint32(0); x < 256; x++ {
					binary.LittleEndian.PutUint16(payload[(tc.start+y)*520+x*2:], 19200)
				}
			}
			binary.LittleEndian.PutUint16(payload[tc.start*520:], 0)
			binary.LittleEndian.PutUint16(payload[(tc.start+191)*520+255*2:], 20000)
			var err error
			layout.ThermalPlane, err = Plane(tc.adapter, layout)
			if err != nil {
				t.Fatal(err)
			}
			f, err := Decode(payload, layout)
			if err != nil {
				t.Fatal(err)
			}
			if f.Width != 256 || f.Height != 192 || f.Coldest != (Point{1, 0, 19200}) || f.Hottest != (Point{255, 191, 20000}) || f.Valid[0] {
				t.Fatalf("wrong sensor plane: %dx%d, cold=%+v hot=%+v", f.Width, f.Height, f.Coldest, f.Hottest)
			}
			if (f.Temperature != nil) != tc.temperature {
				t.Fatal("temperature capability crossed camera variants")
			}
			if f.Temperature != nil && math.Abs(float64(f.Coldest.Raw)*f.Temperature.KelvinPerCount+f.Temperature.KelvinOffset-273.15-26.85) > 1e-9 {
				t.Fatal("incorrect TC001 temperature conversion")
			}
			if len(f.Image().Pix) != 256*192*4 {
				t.Fatal("preview geometry leaked into heatmap")
			}
		})
	}
}

func TestThermalPlaneRejectsMalformedAndUnknownContracts(t *testing.T) {
	base := &pb.RawFormat{Width: 2, Height: 2, BytesPerLine: 4, ThermalPlane: &pb.ThermalPlane{Version: 1, Width: 2, Height: 2, RowStrideBytes: 4}}
	payload := []byte{0, 0, 1, 0, 2, 0, 255, 255}
	// Sentinels belong to an adapter, not the general decoder.
	f, err := Decode(payload, base)
	if err != nil || f.Coldest.Raw != 0 || f.Hottest.Raw != 65535 {
		t.Fatalf("valid endpoint values rejected: %v", err)
	}
	for _, mutate := range []func(*pb.RawFormat){
		func(l *pb.RawFormat) { l.ThermalPlane = nil },
		func(l *pb.RawFormat) { l.ThermalPlane.Version = 2 },
		func(l *pb.RawFormat) { l.ThermalPlane.Width = math.MaxUint32 },
		func(l *pb.RawFormat) { l.ThermalPlane.Height = 0 },
		func(l *pb.RawFormat) { l.ThermalPlane.OffsetBytes = 8 },
		func(l *pb.RawFormat) { l.ThermalPlane.RowStrideBytes = 3 },
		func(l *pb.RawFormat) { l.BytesPerLine = 3 },
		func(l *pb.RawFormat) { l.ThermalPlane.InvalidValues = []uint32{65536} },
		func(l *pb.RawFormat) {
			l.ThermalPlane.Temperature = &pb.ThermalLinearTemperature{KelvinPerCount: math.NaN(), Provenance: "test"}
		},
		func(l *pb.RawFormat) { l.ThermalPlane.Temperature = &pb.ThermalLinearTemperature{KelvinPerCount: 0.01} },
	} {
		l := proto.Clone(base).(*pb.RawFormat)
		mutate(l)
		if _, err := Decode(payload, l); err == nil {
			t.Fatalf("accepted malformed descriptor %v", l)
		}
	}
	for _, adapter := range []string{TOPDONTC001, TOPDONTC001HIK, PureThermal, "unknown"} {
		if _, err := Plane(adapter, &pb.RawFormat{Width: 512, Height: 384, Fourcc: "YUYV", BytesPerLine: 1024}); err == nil {
			t.Fatal("accepted preview-only mode")
		}
	}
}

func TestCameraProfilesRequireIdentity(t *testing.T) {
	if Identify("TOPDON TC001", []string{"YUYV"}) != TOPDONTC001 {
		t.Fatal("TC001 not recognized")
	}
	for _, name := range []string{"USB Camera: UVC Camera", "depth camera", "TC001N", "TC0010"} {
		if Identify(name, []string{"YUYV", "Y16 "}) != "" {
			t.Fatalf("guessed profile for %q", name)
		}
	}
	p := &pb.ThermalCamera{Adapter: "future-camera-v1"}
	if CameraProfile(&pb.VideoDevice{Name: "USB Camera", Thermal: p}) != p {
		t.Fatal("agent capability ignored")
	}
}
