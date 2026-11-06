package realsense

import (
	"encoding/binary"
	"testing"
)

func depthPlane(w, h int, fill func(x, y int) uint16) []byte {
	b := make([]byte, w*h*2)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			binary.LittleEndian.PutUint16(b[(y*w+x)*2:], fill(x, y))
		}
	}
	return b
}

func depthAt(b []byte, w, x, y int) uint16 {
	return binary.LittleEndian.Uint16(b[(y*w+x)*2:])
}

var testIntrin = Intrinsics{Width: 64, Height: 48, Fx: 60, Fy: 60, Ppx: 32, Ppy: 24}

// A uniform plane through identity extrinsics must come out exactly as it
// went in: the corner dilation writes each value into neighbours that hold
// the same value, so any deviation is a real projection bug.
func TestAlignIdentityIsExact(t *testing.T) {
	in := depthPlane(64, 48, func(x, y int) uint16 { return 1000 })
	out, err := AlignDepthToColour(in, testIntrin, testIntrin, Extrinsics{Rotation: [9]float64{1, 0, 0, 0, 1, 0, 0, 0, 1}})
	if err != nil {
		t.Fatal(err)
	}
	for i := range out {
		if out[i] != in[i] {
			t.Fatalf("byte %d differs: %d != %d", i, out[i], in[i])
		}
	}
}

// A fronto-parallel plane seen by a colour camera translated along X shifts
// by fx*tx/z pixels — the parallax the alignment exists to model.
func TestAlignTranslationParallax(t *testing.T) {
	const z = 2000 // device units
	in := depthPlane(64, 48, func(x, y int) uint16 { return z })
	// Translation is +6000 units along X: shift = fx * tx / z = 60*6000/2000 = 180? No:
	// projected x = (X + tx)/Z * fx + ppx, so pixel shift = fx*tx/z = 60*100/2000 = 3
	// with tx = 100 units.
	e := Extrinsics{Rotation: [9]float64{1, 0, 0, 0, 1, 0, 0, 0, 1}, Translation: [3]float64{100, 0, 0}}
	out, err := AlignDepthToColour(in, testIntrin, testIntrin, e)
	if err != nil {
		t.Fatal(err)
	}
	// Source pixel 10 lands at 13; target pixels below the shift stay empty.
	if got := depthAt(out, 64, 13, 24); got != z {
		t.Errorf("pixel (13,24) = %d, want %d", got, z)
	}
	if got := depthAt(out, 64, 1, 24); got != 0 {
		t.Errorf("pixel (1,24) = %d, want 0 (nothing projects below the shift)", got)
	}
}

// Two depth pixels landing on the same colour pixel keep the NEARER one:
// foreground occludes background, never averages with it.
func TestAlignOcclusionKeepsNearest(t *testing.T) {
	in := depthPlane(64, 48, func(x, y int) uint16 {
		if x == 20 {
			return 500 // near column
		}
		if x == 21 {
			return 3000 // far column right beside it
		}
		return 0
	})
	out, err := AlignDepthToColour(in, testIntrin, testIntrin, Extrinsics{Rotation: [9]float64{1, 0, 0, 0, 1, 0, 0, 0, 1}})
	if err != nil {
		t.Fatal(err)
	}
	// The dilated near column covers x=20±1; where it overlaps the far
	// column's dilation (x=20,21), near must win.
	if got := depthAt(out, 64, 20, 24); got != 500 {
		t.Errorf("overlap pixel = %d, want the nearer 500", got)
	}
	if got := depthAt(out, 64, 22, 24); got != 3000 {
		t.Errorf("far-only pixel = %d, want 3000", got)
	}
}

// Points projecting outside the colour plane are dropped, not clamped into
// fake border measurements, and zero depth never spreads.
func TestAlignBoundsAndZeros(t *testing.T) {
	in := depthPlane(64, 48, func(x, y int) uint16 {
		if x == 0 && y == 0 {
			return 800
		}
		return 0
	})
	// A colour model half the size: most of the depth frame projects outside.
	small := Intrinsics{Width: 16, Height: 12, Fx: 60, Fy: 60, Ppx: -40, Ppy: -30}
	out, err := AlignDepthToColour(in, testIntrin, small, Extrinsics{Rotation: [9]float64{1, 0, 0, 0, 1, 0, 0, 0, 1}})
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for i := 0; i < len(out); i += 2 {
		if binary.LittleEndian.Uint16(out[i:]) != 0 {
			total++
		}
	}
	if total != 0 {
		t.Errorf("%d pixels written from a source that projects off-plane", total)
	}
}

func TestAlignRefusesWrongSizeAndDistortedDepth(t *testing.T) {
	if _, err := AlignDepthToColour(make([]byte, 10), testIntrin, testIntrin, Extrinsics{}); err == nil {
		t.Error("wrong-size plane accepted")
	}
	di := testIntrin
	di.Distortion = [5]float64{0.1}
	if _, err := AlignDepthToColour(depthPlane(64, 48, func(int, int) uint16 { return 1 }), di, testIntrin, Extrinsics{}); err == nil {
		t.Error("distorted depth model accepted")
	}
}

// AlignFrame expresses the metre-denominated factory translation in device
// units before aligning — the 15 mm RGB offset is 15 units at 1 mm scale, and
// a missing scale is a refusal.
func TestAlignFrameScalesTranslation(t *testing.T) {
	cal := &Calibration{
		DepthScaleM:   0.001,
		DepthToColour: Extrinsics{Rotation: [9]float64{1, 0, 0, 0, 1, 0, 0, 0, 1}, Translation: [3]float64{0.1, 0, 0}}, // 100 mm
	}
	const z = 2000
	in := depthPlane(64, 48, func(x, y int) uint16 { return z })
	out, err := cal.AlignFrame(in, testIntrin, testIntrin)
	if err != nil {
		t.Fatal(err)
	}
	// 0.1 m at 0.001 m/unit = 100 units → shift fx*tx/z = 3 pixels, as above.
	if got := depthAt(out, 64, 13, 24); got != z {
		t.Errorf("pixel (13,24) = %d, want %d", got, z)
	}

	cal.DepthScaleM = 0
	if _, err := cal.AlignFrame(in, testIntrin, testIntrin); err == nil {
		t.Error("unknown scale accepted")
	}
}

func BenchmarkAlign848x480(b *testing.B) {
	di := Intrinsics{Width: 848, Height: 480, Fx: 430.3, Fy: 430.3, Ppx: 421.5, Ppy: 244.2}
	ci := Intrinsics{Width: 1280, Height: 720, Fx: 905.9, Fy: 905.1, Ppx: 653.5, Ppy: 388.4}
	e := Extrinsics{Rotation: [9]float64{1, 0, 0, 0, 1, 0, 0, 0, 1}, Translation: [3]float64{15, 0, 0}}
	in := depthPlane(848, 480, func(x, y int) uint16 { return uint16(500 + x + y) })
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := AlignDepthToColour(in, di, ci, e); err != nil {
			b.Fatal(err)
		}
	}
}
