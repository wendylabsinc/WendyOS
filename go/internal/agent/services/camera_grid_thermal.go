package services

// Temperatures of a radiometric thermal camera, as a grid and as hot and cold
// markers for viewers (camera_grid.go publishes them).
//
// A thermal viewer wants the temperature of every spot, and the hottest and
// coldest thing in the picture. The picture cannot answer either: it is AGC'd,
// so its brightest pixel is merely the brightest after a per-frame stretch, and
// H.264 would blur it anyway. The answer is in the radiometric block the TOPDON
// TC001 (an InfiRay core) stacks on top of the picture in its 512x484 mode: the
// block the raw tap hands raw subscribers (video_raw_tap.go) and the encoded
// branch crops away (videocrop top=100). Ported from WendyOS#2434's
// thermal_stats.go, which sent the hot and cold pixel inside VideoFrame, and
// from wendy-data-app's tc001 decoder.
//
// The layout, measured on three physical TC001 units (wendy-box-theta, ccr1,
// ccr2) by the wendy-console team:
//
//	512x484 YUYV, 1024 bytes per line.
//	rows   0..95   a 256x192 array of little-endian uint16, one per sensor
//	               pixel, row-major (96 rows x 1024 B = 256*192*2 B)
//	row    96      header
//	row    97      zeros
//	rows  98..483  the AGC'd picture; viewers get rows 100..483 (512x384)
//
// The block and the 512x384 picture are both 4:3 and cover the same scene, so
// sensor pixel (col,row) covers the picture from col/256 to (col+1)/256 across
// and from row/192 to (row+1)/192 down, with its centre at ((col+0.5)/256,
// (row+0.5)/192).
//
// A count is a sixteenth of a kelvin in this mode, not the community's "/64":
// /64 reads a room as about -198 C. Only this layout is accepted. Any other
// size, format or line padding, or a picture that does not start at row 100,
// gets nothing rather than a guess. #2434 also scanned a 256-wide mode at /64;
// that mode cannot reach the raw tap as a stacked mode (its block needs 192
// rows, and a stacked mode has at most 128) and was never measured.
//
// A wrong scale is a factor of four, the difference between a room and a
// furnace, so a frame whose MEAN is not a plausible scene is refused too: one
// wrong assumption about a new firmware must produce no grid and no markers,
// never confident values on the wrong pixels. The ceiling is 600 C, not
// #2434's 150 C, so a camera looking at a large fire keeps its readings exactly
// when they matter. It still catches the factor of four both ways: any scene
// from -55 C to 660 C, read at the wrong scale, reads above 600 C or below
// -40 C. The TC001 is rated from -20 C to 550 C.

import (
	"encoding/binary"
	"time"

	"google.golang.org/protobuf/proto"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"github.com/wendylabsinc/wendy/go/proto/gen/datapb"
)

const (
	// The TC001's radiometric array: one 16-bit value per sensor pixel, stored
	// row-major from the first byte of the frame.
	thermalSensorWidth  = 256
	thermalSensorHeight = 192
	thermalSensorPixels = thermalSensorWidth * thermalSensorHeight
	thermalBlockBytes   = thermalSensorPixels * 2

	// The measured stacked mode, and the raw row the picture viewers get
	// starts at.
	tc001FrameWidth  = 512
	tc001FrameHeight = 484
	tc001PictureTop  = 100

	// thermalCountsPerKelvin is the scale of the stacked mode: sixteenths of a
	// kelvin, measured.
	thermalCountsPerKelvin = 16
	kelvinZeroCelsius      = 273.15

	// The plausibility guard on a frame's mean temperature. Wide enough for any
	// scene the camera is rated for; narrow enough that a scale off by 4x in
	// either direction always fails it.
	thermalMinMeanCelsius = -40.0
	thermalMaxMeanCelsius = 600.0

	// thermalMarkersValidFor is how long a viewer shows the hot and cold
	// markers without a newer frame. Past it the decoder has stalled or
	// stopped, and stale dots are worse than none.
	thermalMarkersValidFor = 2 * time.Second
)

// tc001Layout reports whether raw frames of this format, whose encoded picture
// starts at raw row pictureTop, are the measured TC001 stacked mode: the
// radiometric block at the top, and a picture of the sensor grid scaled by two
// that viewers get without the block. Every other layout is refused rather
// than adapted to.
func tc001Layout(format *agentpb.RawFormat, pictureTop uint32) bool {
	return format.GetWidth() == tc001FrameWidth && format.GetHeight() == tc001FrameHeight &&
		format.GetFourcc() == fourccYUYV && format.GetBytesPerLine() == tc001FrameWidth*yuyvBytesPerPixel &&
		pictureTop == tc001PictureTop
}

// thermalStats is one frame's reading. Positions are normalized against the
// picture viewers receive, at pixel centres; temperatures are degrees Celsius.
type thermalStats struct {
	maxX, maxY float32
	minX, minY float32
	maxCelsius float64
	minCelsius float64
	// meanCelsius is what the plausibility guard judged. It is set even when
	// the guard refuses the frame, so the refusal can be logged with the
	// number that caused it.
	meanCelsius float64
}

// computeThermalStats finds the hottest and coldest sensor pixels of one raw
// frame of the TC001 stacked mode, and checks that the frame is a plausible
// scene. Ties go to the first pixel in row-major order (top row first, left to
// right), so a dot does not wander across a flat region.
//
// One pass over 49,152 values and no allocation: it runs on every frame while
// a viewer asks for the camera's temperatures.
func computeThermalStats(frame []byte, format *agentpb.RawFormat, pictureTop uint32) (thermalStats, bool) {
	if !tc001Layout(format, pictureTop) || len(frame) < tc001FrameHeight*tc001FrameWidth*yuyvBytesPerPixel {
		return thermalStats{}, false
	}
	block := frame[:thermalBlockBytes]

	first := binary.LittleEndian.Uint16(block)
	minV, maxV := first, first
	minI, maxI := 0, 0
	var sum uint64
	for i := 0; i < thermalSensorPixels; i++ {
		v := binary.LittleEndian.Uint16(block[2*i:])
		sum += uint64(v)
		// Strict comparisons: an equal value later in the scan never displaces
		// the first one.
		if v > maxV {
			maxV, maxI = v, i
		}
		if v < minV {
			minV, minI = v, i
		}
	}

	st := thermalStats{meanCelsius: float64(sum)/thermalSensorPixels/thermalCountsPerKelvin - kelvinZeroCelsius}
	if st.meanCelsius < thermalMinMeanCelsius || st.meanCelsius > thermalMaxMeanCelsius {
		return st, false
	}
	st.maxX, st.maxY = thermalPixelCentre(maxI)
	st.minX, st.minY = thermalPixelCentre(minI)
	st.maxCelsius = thermalCelsius(maxV)
	st.minCelsius = thermalCelsius(minV)
	return st, true
}

// thermalCelsius converts one count of the stacked mode to degrees Celsius.
func thermalCelsius(count uint16) float64 {
	return float64(count)/thermalCountsPerKelvin - kelvinZeroCelsius
}

// thermalPixelCentre maps a row-major sensor index to the normalized centre of
// that pixel on the picture.
func thermalPixelCentre(i int) (x, y float32) {
	col, row := i%thermalSensorWidth, i/thermalSensorWidth
	return float32((float64(col) + 0.5) / thermalSensorWidth),
		float32((float64(row) + 0.5) / thermalSensorHeight)
}

// thermalGrid is a stacked frame's radiometric block as a grid of the camera's
// picture: the camera's own counts, byte for byte, with the scale and offset
// that make them degrees Celsius. Its data aliases frame, which a raw frame
// never changes; marshalling copies it.
func thermalGrid(frame []byte, camera string) *datapb.Grid {
	return &datapb.Grid{
		Width:        thermalSensorWidth,
		Height:       thermalSensorHeight,
		Unit:         "°C",
		Data:         frame[:thermalBlockBytes],
		BytesPerLine: thermalSensorWidth * 2,
		Scale:        1.0 / thermalCountsPerKelvin,
		Offset:       -kelvinZeroCelsius,
		Camera:       camera,
	}
}

// thermalMarkers points at the hottest and coldest sensor pixel on the
// camera's picture, labelled "max" in red and "min" in blue with their
// temperatures. A frame the plausibility guard refused has no reading, and
// its markers are empty, which clears the dots.
func thermalMarkers(st thermalStats, ok bool, camera string) *datapb.VideoMarkers {
	markers := &datapb.VideoMarkers{Camera: camera, ValidForNanos: int64(thermalMarkersValidFor)}
	if !ok {
		return markers
	}
	markers.Markers = []*datapb.Marker{
		{Kind: datapb.Marker_KIND_POINT, X: st.maxX, Y: st.maxY, Label: "max", Value: proto.Float64(st.maxCelsius), Unit: "°C", Color: "red"},
		{Kind: datapb.Marker_KIND_POINT, X: st.minX, Y: st.minY, Label: "min", Value: proto.Float64(st.minCelsius), Unit: "°C", Color: "blue"},
	}
	return markers
}
