package thermal

import (
	"encoding/binary"
	"testing"
)

func TestMeasurementsHonorStrideAndInvalidPixels(t *testing.T) {
	const stride = 164
	payload := make([]byte, stride*60)
	for y := 0; y < 60; y++ {
		for x := 0; x < 80; x++ {
			binary.LittleEndian.PutUint16(payload[y*stride+x*2:], 30000)
		}
		for i := 160; i < stride; i++ {
			payload[y*stride+i] = 255
		}
	}
	binary.LittleEndian.PutUint16(payload[0:], 0)
	binary.LittleEndian.PutUint16(payload[2:], 65535)
	binary.LittleEndian.PutUint16(payload[5*stride+7*2:], 28000)
	binary.LittleEndian.PutUint16(payload[59*stride+79*2:], 32000)
	f, err := DecodePureThermal(payload, "Y16 ", 80, 60, stride)
	if err != nil {
		t.Fatal(err)
	}
	if f.Coldest != (Point{7, 5, 28000}) || f.Hottest != (Point{79, 59, 32000}) {
		t.Fatalf("wrong extrema: %+v %+v", f.Coldest, f.Hottest)
	}
	img := f.Image()
	if img.Rect.Dx() != 80 || img.Rect.Dy() != 60 || len(img.Pix) != 80*60*4 {
		t.Fatal("bad image geometry")
	}
	if img.RGBAAt(7, 5) == img.RGBAAt(79, 59) {
		t.Fatal("range lost")
	}
}

func TestRejectsAmbiguousOrMalformedFrames(t *testing.T) {
	for _, tc := range []struct {
		format       string
		w, h, stride uint32
		n            int
	}{
		{"UYVY", 80, 60, 160, 9600}, {"Y16 ", 160, 122, 320, 39040},
		{"Y16 ", 80, 60, 159, 9540}, {"Y16 ", 80, 60, 160, 9599},
		{"Y16 ", ^uint32(0), 60, ^uint32(0), 0}, {"Y16 ", 80, 60, 160, 9600},
	} {
		if _, err := DecodePureThermal(make([]byte, tc.n), tc.format, tc.w, tc.h, tc.stride); err == nil {
			t.Fatalf("accepted %+v", tc)
		}
	}
}

func TestUniformFrameAndCameraIdentification(t *testing.T) {
	payload := make([]byte, 9600)
	for i := 0; i < len(payload); i += 2 {
		binary.LittleEndian.PutUint16(payload[i:], 30000)
	}
	f, err := DecodePureThermal(payload, "Y16 ", 80, 60, 160)
	if err != nil {
		t.Fatal(err)
	}
	if f.Coldest != f.Hottest || f.Coldest.X != 0 || f.Coldest.Y != 0 {
		t.Fatal("unstable ties")
	}
	_ = f.Image()
	if !IsPureThermal("PureThermal 2", []string{"UYVY", "Y16 "}) || !IsPureThermal("PureThermal", nil) {
		t.Fatal("missing supported camera")
	}
	if IsPureThermal("Depth camera", []string{"Y16 "}) || IsPureThermal("PureThermal", []string{"UYVY"}) {
		t.Fatal("misidentified camera")
	}
}
