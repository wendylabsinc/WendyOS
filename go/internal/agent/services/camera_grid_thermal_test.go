package services

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"github.com/wendylabsinc/wendy/go/proto/gen/datapb"
)

// The TC001's stacked mode as the raw tap describes it: 512x484 YUYV, the
// picture viewers get from raw row 100. No real frame is checked in anywhere;
// these tests build frames in the layout measured on three units (see
// camera_grid_thermal.go), as #2434 and wendy-data-app did.
var tc001Format = &agentpb.RawFormat{Width: 512, Height: 484, Fourcc: fourccYUYV, BytesPerLine: 1024}

// thermalCounts encodes a temperature as the stacked mode does.
func thermalCounts(celsius float64) uint16 {
	return uint16(math.Round((celsius + kelvinZeroCelsius) * thermalCountsPerKelvin))
}

// stackedThermalFrame builds one raw tap frame: the 256x192 radiometric block,
// little-endian, from the first byte, then picture rows of mid-grey that must
// never be read. temp gives each sensor pixel's temperature.
func stackedThermalFrame(temp func(col, row int) float64) []byte {
	frame := make([]byte, 1024*484)
	for i := thermalBlockBytes; i < len(frame); i++ {
		frame[i] = 0x80
	}
	for row := 0; row < thermalSensorHeight; row++ {
		for col := 0; col < thermalSensorWidth; col++ {
			binary.LittleEndian.PutUint16(frame[2*(row*thermalSensorWidth+col):], thermalCounts(temp(col, row)))
		}
	}
	return frame
}

// roomWith is a 22 C room with one hot and one cold sensor pixel.
func roomWith(hotCol, hotRow int, hotC float64, coldCol, coldRow int, coldC float64) func(int, int) float64 {
	return func(col, row int) float64 {
		switch {
		case col == hotCol && row == hotRow:
			return hotC
		case col == coldCol && row == coldRow:
			return coldC
		}
		return 22
	}
}

func uniform(c float64) func(int, int) float64 { return func(int, int) float64 { return c } }

func within(got float32, want, tol float64) bool { return math.Abs(float64(got)-want) <= tol }

// One count of the stacked mode is 1/16 K.
const sixteenthK = 1.0 / 16

func TestComputeThermalStats_FindsTheHotAndColdPixels(t *testing.T) {
	for _, tc := range []struct {
		name               string
		hotCol, hotRow     int
		coldCol, coldRow   int
		hotC, coldC        float64
		wantMaxX, wantMaxY float64
		wantMinX, wantMinY float64
	}{
		{
			name:   "lighter mid-frame, cold window bottom-left",
			hotCol: 128, hotRow: 96, coldCol: 10, coldRow: 180,
			hotC: 110, coldC: 4,
			wantMaxX: 128.5 / 256, wantMaxY: 96.5 / 192,
			wantMinX: 10.5 / 256, wantMinY: 180.5 / 192,
		},
		{
			// The sensor's corners land on the picture's corners, at pixel
			// centres: never exactly 0 or 1.
			name:   "hot top-left corner, cold bottom-right corner",
			hotCol: 0, hotRow: 0, coldCol: 255, coldRow: 191,
			hotC: 60, coldC: -10,
			wantMaxX: 0.5 / 256, wantMaxY: 0.5 / 192,
			wantMinX: 255.5 / 256, wantMinY: 191.5 / 192,
		},
		{
			name:   "hot bottom-right corner, cold top-left corner",
			hotCol: 255, hotRow: 191, coldCol: 0, coldRow: 0,
			hotC: 45, coldC: 0,
			wantMaxX: 255.5 / 256, wantMaxY: 191.5 / 192,
			wantMinX: 0.5 / 256, wantMinY: 0.5 / 192,
		},
		{
			name:   "hot top-right corner, cold bottom-left corner",
			hotCol: 255, hotRow: 0, coldCol: 0, coldRow: 191,
			hotC: 35, coldC: 15,
			wantMaxX: 255.5 / 256, wantMaxY: 0.5 / 192,
			wantMinX: 0.5 / 256, wantMinY: 191.5 / 192,
		},
		{
			// A fire: far above #2434's old 150 C ceiling on the hottest pixel,
			// and still a plausible scene.
			name:   "a 540 C flame in a room",
			hotCol: 40, hotRow: 20, coldCol: 41, coldRow: 20,
			hotC: 540, coldC: 18,
			wantMaxX: 40.5 / 256, wantMaxY: 20.5 / 192,
			wantMinX: 41.5 / 256, wantMinY: 20.5 / 192,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame := stackedThermalFrame(roomWith(tc.hotCol, tc.hotRow, tc.hotC, tc.coldCol, tc.coldRow, tc.coldC))
			st, ok := computeThermalStats(frame, tc001Format, tc001PictureTop)
			if !ok {
				t.Fatalf("stats refused for a plausible frame (mean %.2f C)", st.meanCelsius)
			}
			const posTol = 1e-6
			if !within(st.maxX, tc.wantMaxX, posTol) || !within(st.maxY, tc.wantMaxY, posTol) {
				t.Errorf("hottest at (%v, %v), want (%v, %v)", st.maxX, st.maxY, tc.wantMaxX, tc.wantMaxY)
			}
			if !within(st.minX, tc.wantMinX, posTol) || !within(st.minY, tc.wantMinY, posTol) {
				t.Errorf("coldest at (%v, %v), want (%v, %v)", st.minX, st.minY, tc.wantMinX, tc.wantMinY)
			}
			if math.Abs(st.maxCelsius-tc.hotC) > sixteenthK || math.Abs(st.minCelsius-tc.coldC) > sixteenthK {
				t.Errorf("max %v C, min %v C, want %v and %v", st.maxCelsius, st.minCelsius, tc.hotC, tc.coldC)
			}
			if math.Abs(st.meanCelsius-22) > 0.1 { // one hot pixel moves the mean a little
				t.Errorf("mean %v C, want about 22 C", st.meanCelsius)
			}
		})
	}
}

// The plausibility guard judges the mean, from -40 C to 600 C. A scale off by
// four in either direction fails it for any scene the camera is rated for.
func TestComputeThermalStats_RefusesAnImplausibleMean(t *testing.T) {
	sixtyFourths := func(celsius float64) []byte {
		f := stackedThermalFrame(uniform(22))
		v := uint16(math.Round((celsius + kelvinZeroCelsius) * 64))
		for i := 0; i < thermalSensorPixels; i++ {
			binary.LittleEndian.PutUint16(f[2*i:], v)
		}
		return f
	}
	for _, tc := range []struct {
		name  string
		frame []byte
		ok    bool
	}{
		{"room", stackedThermalFrame(uniform(22)), true},
		{"just above the floor", stackedThermalFrame(uniform(-39.5)), true},
		{"just below the ceiling", stackedThermalFrame(uniform(599.5)), true},
		{"below the floor", stackedThermalFrame(uniform(-41)), false},
		{"above the ceiling", stackedThermalFrame(uniform(601)), false},
		// The failure the guard exists for: sixty-fourths written into the
		// stacked mode. Read at /16 a 22 C room is about 907 C, and the
		// coldest scene the camera is rated for, -20 C, about 739 C.
		{"sixty-fourths of a room", sixtyFourths(22), false},
		{"sixty-fourths of -20 C", sixtyFourths(-20), false},
		{"an all-zero block, before the camera writes temperatures", func() []byte {
			f := stackedThermalFrame(uniform(22))
			clear(f[:thermalBlockBytes])
			return f
		}(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, ok := computeThermalStats(tc.frame, tc001Format, tc001PictureTop)
			if ok != tc.ok {
				t.Errorf("ok = %v (mean %.2f C), want %v", ok, st.meanCelsius, tc.ok)
			}
		})
	}
}

// Equal values never displace the first one in row-major order, so the dot
// stays put on a flat region instead of drifting to its last pixel.
func TestComputeThermalStats_TiesGoToTheFirstPixel(t *testing.T) {
	temp := func(col, row int) float64 {
		switch {
		case (col == 7 && row == 3) || (col == 2 && row == 150) || (col == 200 && row == 3):
			return 90 // three equally hot pixels; (7,3) comes first
		case (col == 250 && row == 0) || (col == 0 && row == 1):
			return 1 // two equally cold pixels; (250,0) comes first
		}
		return 22
	}
	st, ok := computeThermalStats(stackedThermalFrame(temp), tc001Format, tc001PictureTop)
	if !ok {
		t.Fatal("stats refused")
	}
	if !within(st.maxX, 7.5/256, 1e-6) || !within(st.maxY, 3.5/192, 1e-6) {
		t.Errorf("hottest tie went to (%v, %v), want the first at col 7 row 3", st.maxX, st.maxY)
	}
	if !within(st.minX, 250.5/256, 1e-6) || !within(st.minY, 0.5/192, 1e-6) {
		t.Errorf("coldest tie went to (%v, %v), want the first at col 250 row 0", st.minX, st.minY)
	}

	// A uniform scene: every pixel ties, so both dots sit on the first.
	st, ok = computeThermalStats(stackedThermalFrame(uniform(22)), tc001Format, tc001PictureTop)
	if !ok {
		t.Fatal("uniform frame refused")
	}
	if st.maxX != st.minX || st.maxY != st.minY || !within(st.maxX, 0.5/256, 1e-6) {
		t.Errorf("uniform frame: max (%v,%v) min (%v,%v), want both at the first pixel", st.maxX, st.maxY, st.minX, st.minY)
	}
}

// Anything that is not the measured layout gets nothing: the block's position
// and scale are known for one geometry, and a guess draws dots on wrong pixels.
func TestComputeThermalStats_RefusesUnmeasuredLayouts(t *testing.T) {
	good := stackedThermalFrame(roomWith(1, 1, 80, 2, 2, 5))
	for _, tc := range []struct {
		name       string
		format     *agentpb.RawFormat
		pictureTop uint32
		frame      []byte
	}{
		{"viewers see the whole frame (no videocrop)", tc001Format, 0, good},
		{"the picture starts elsewhere", tc001Format, 98, good},
		{"the picture mode, without the block", &agentpb.RawFormat{Width: 512, Height: 384, Fourcc: fourccYUYV, BytesPerLine: 1024}, 0, good[:1024*384]},
		// #2434 scanned this half-size mode at /64; it cannot be a stacked
		// mode of the raw tap and was never measured.
		{"the 256-wide mode", &agentpb.RawFormat{Width: 256, Height: 392, Fourcc: fourccYUYV, BytesPerLine: 512}, 200, make([]byte, 512*392)},
		{"a width with no known scale", &agentpb.RawFormat{Width: 640, Height: 580, Fourcc: fourccYUYV, BytesPerLine: 1280}, 100, make([]byte, 1280*580)},
		{"not YUYV", &agentpb.RawFormat{Width: 512, Height: 484, Fourcc: "UYVY", BytesPerLine: 1024}, 100, good},
		{"padded lines", &agentpb.RawFormat{Width: 512, Height: 484, Fourcc: fourccYUYV, BytesPerLine: 1088}, 100, make([]byte, 1088*484)},
		{"a taller stacked mode", &agentpb.RawFormat{Width: 512, Height: 488, Fourcc: fourccYUYV, BytesPerLine: 1024}, 104, make([]byte, 1024*488)},
		{"short frame", tc001Format, tc001PictureTop, good[:len(good)-1]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := computeThermalStats(tc.frame, tc.format, tc.pictureTop); ok {
				t.Error("stats computed for a layout that was never measured")
			}
		})
	}
	if !tc001Layout(tc001Format, tc001PictureTop) {
		t.Error("the measured layout is refused")
	}
}

// It runs on every frame while a viewer asks, so it must cost one scan and
// nothing on the heap.
func TestComputeThermalStats_DoesNotAllocate(t *testing.T) {
	frame := stackedThermalFrame(roomWith(5, 5, 60, 6, 6, 10))
	if allocs := testing.AllocsPerRun(20, func() {
		if _, ok := computeThermalStats(frame, tc001Format, tc001PictureTop); !ok {
			t.Fatal("stats refused")
		}
	}); allocs != 0 {
		t.Errorf("computeThermalStats allocated %v times per frame, want 0", allocs)
	}
}

// The grid is the camera's own counts, byte for byte, with the scale and
// offset that make them degrees Celsius, of the picture viewers get.
func TestThermalGridCarriesTheBlockAndItsScale(t *testing.T) {
	frame := stackedThermalFrame(roomWith(200, 50, 61.5, 10, 180, 4.25))
	grid := thermalGrid(frame, "/dev/video4")
	if grid.Width != 256 || grid.Height != 192 || grid.BytesPerLine != 512 || grid.Unit != "°C" || grid.Camera != "/dev/video4" {
		t.Fatalf("grid %+v", grid)
	}
	if !bytes.Equal(grid.Data, frame[:thermalBlockBytes]) {
		t.Fatal("grid data is not the radiometric block")
	}
	at := func(col, row int) float64 {
		count := binary.LittleEndian.Uint16(grid.Data[int(grid.BytesPerLine)*row+2*col:])
		return float64(count)*grid.Scale + grid.Offset
	}
	// One count is 1/16 K, so values are exact to within half a count.
	for _, c := range []struct {
		col, row int
		want     float64
	}{{200, 50, 61.5}, {10, 180, 4.25}, {0, 0, 22}, {255, 191, 22}} {
		if got := at(c.col, c.row); math.Abs(got-c.want) > sixteenthK/2 {
			t.Errorf("cell (%d,%d) = %.4f C, want %.2f", c.col, c.row, got, c.want)
		}
	}
}

// Hot is red and cold is blue, as points labelled with their temperatures; a
// refused frame clears them.
func TestThermalMarkers(t *testing.T) {
	st, ok := computeThermalStats(stackedThermalFrame(roomWith(128, 96, 110, 10, 180, 4)), tc001Format, tc001PictureTop)
	markers := thermalMarkers(st, ok, "/dev/video4")
	if markers.Camera != "/dev/video4" || markers.ValidForNanos != int64(thermalMarkersValidFor) || len(markers.Markers) != 2 {
		t.Fatalf("markers %v", markers)
	}
	hot, cold := markers.Markers[0], markers.Markers[1]
	if hot.Kind != datapb.Marker_KIND_POINT || hot.Label != "max" || hot.Color != "red" || hot.Unit != "°C" ||
		!within(hot.X, 128.5/256, 1e-6) || !within(hot.Y, 96.5/192, 1e-6) || math.Abs(hot.GetValue()-110) > sixteenthK {
		t.Errorf("hot %v", hot)
	}
	if cold.Kind != datapb.Marker_KIND_POINT || cold.Label != "min" || cold.Color != "blue" ||
		!within(cold.X, 10.5/256, 1e-6) || !within(cold.Y, 180.5/192, 1e-6) || math.Abs(cold.GetValue()-4) > sixteenthK {
		t.Errorf("cold %v", cold)
	}
	if cleared := thermalMarkers(thermalStats{}, false, "/dev/video4"); len(cleared.Markers) != 0 || cleared.Camera != "/dev/video4" {
		t.Errorf("refused frame: %v", cleared)
	}
}
