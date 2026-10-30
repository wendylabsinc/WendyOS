package usbthermal

import (
	"encoding/binary"
	"time"
)

// Frame is one picture a camera captured, in a shape an encoder can consume
// without knowing anything about thermal cameras.
//
// For a thermal camera it is the PICTURE half of a thermal frame: the camera's
// own grey image of the scene (hottest is brightest), 256x192 on the TC001 Max,
// sent as YUY2 whose chroma bytes are always 0x80. That is the format the bytes
// arrive in, so they are passed on untouched; a consumer that wants GRAY8 takes
// every even byte.
type Frame struct {
	Data      []byte    // one whole frame, tightly packed, no row padding
	Width     int       // pixels
	Height    int       // rows
	Format    string    // GStreamer raw format name: "GRAY8", "YUY2" or "GRAY16_LE"
	Timestamp time.Time // capture time (wall clock)
}

// GStreamer raw format names Frame.Format uses.
const (
	FormatGRAY8    = "GRAY8"
	FormatYUY2     = "YUY2"
	FormatGRAY16LE = "GRAY16_LE"
)

// RawFourCC is the V4L2 name of a RawFrame's pixel format, as the raw tap
// reports it. It names the picture half; the temperature half rides in the
// same rows as 16-bit words, exactly as on the TOPDON TC001's UVC mode.
const RawFourCC = "YUYV"

// RawFrame is one complete thermal frame in the stacked layout the agent's raw
// tap already describes for the TOPDON TC001's 256x384 UVC mode:
//
//	rows   0..H-1    picture, YUYV (grey, chroma 0x80)
//	rows   H..2H-1   temperatures, little-endian uint16, 1/64 kelvin
//
// On the TC001 Max that is 256 pixels by 384 rows of 512 bytes, 196,608 bytes.
// A consumer that already decodes a TC001 frame (the 1/64 K plane in the bottom
// half) reads a TC001 Max frame the same way.
type RawFrame struct {
	Data         []byte // Height rows of BytesPerLine bytes, as above
	Width        int    // pixels per row
	Height       int    // rows, picture and temperatures together (2 x sensor rows)
	BytesPerLine int
	// Sequence is the camera's own frame counter from the block's metadata row
	// (uint16 word 64). Observed to increase by exactly one per thermal frame,
	// so a jump means frames were lost between the camera and the agent.
	Sequence  uint16
	Timestamp time.Time // when the agent finished receiving the frame (wall clock)
}

// KelvinPerCount is the temperature plane's scale: one count is 1/64 K.
//
// Evidence: on exxon-01 the plane read 24.2-28.2 degC across a room (median
// 24.5 degC) and correlated 0.995 with the picture's grey level, the hottest
// pixel being the brightest. 1/64 K is also what the TOPDON TC001's 256x384 UVC
// mode and the Thermal Master P3 (same vendor, same protocol) are documented to
// use.
const KelvinPerCount = 1.0 / 64

// CountsToKelvin converts one temperature-plane value to kelvin.
func CountsToKelvin(raw uint16) float64 { return float64(raw) * KelvinPerCount }

// CountsToCelsius converts one temperature-plane value to degrees Celsius.
func CountsToCelsius(raw uint16) float64 { return CountsToKelvin(raw) - 273.15 }

// Temperature returns the raw temperature-plane value at sensor pixel (x, y),
// and false when the pixel is outside the frame.
func (f RawFrame) Temperature(x, y int) (uint16, bool) {
	sensorRows := f.Height / 2
	if x < 0 || y < 0 || x >= f.Width || y >= sensorRows {
		return 0, false
	}
	off := f.BytesPerLine*(sensorRows+y) + 2*x
	if off+2 > len(f.Data) {
		return 0, false
	}
	return binary.LittleEndian.Uint16(f.Data[off:]), true
}
