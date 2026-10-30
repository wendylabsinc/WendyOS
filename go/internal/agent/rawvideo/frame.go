// Package rawvideo is the contract between an in-process camera driver and the
// agent's video hub.
//
// Most cameras reach the agent as a /dev/videoN node, and the hub encodes them
// by pointing GStreamer at that node. Some cameras have no node: a userspace USB
// driver (the TOPDON TC001 Max, for one) produces whole frames inside the agent
// process and nowhere else. Such a driver publishes those frames to a Feed, and
// registers the Feed with the video service (services.VideoService.
// RegisterInProcessCamera) under the device ID it reports for the camera. From
// then on StreamVideo's default, encoded path serves that ID from the Feed,
// encoded exactly as a V4L2 camera would be.
//
// This package deliberately depends on nothing in the agent, so a driver can
// import it without importing the video service.
package rawvideo

import (
	"errors"
	"fmt"
	"time"
)

// Frame is one whole uncompressed picture from an in-process camera.
type Frame struct {
	// Data is the picture, tightly packed: Height rows of Width pixels each,
	// with no padding at the end of a row.
	Data   []byte
	Width  int
	Height int
	// Format is the GStreamer raw video format name of Data: FormatGRAY8,
	// FormatYUY2 or FormatGRAY16LE.
	Format string
	// Timestamp is when the frame was captured, on the wall clock. The feed
	// uses it to measure the camera's frame rate.
	Timestamp time.Time
}

// The formats a Frame may carry, spelled as GStreamer names them.
const (
	// FormatGRAY8 is one byte of luminance per pixel.
	FormatGRAY8 = "GRAY8"
	// FormatYUY2 is packed 4:2:2, Y0 U Y1 V, two bytes per pixel. A grey camera
	// that sends luminance in the low byte and 0x80 in the high byte is YUY2
	// with neutral chroma, and is published as such.
	FormatYUY2 = "YUY2"
	// FormatGRAY16LE is 16 bits of intensity per pixel, little endian. It is
	// stretched to 8 bits for viewing; see StretchGray16.
	FormatGRAY16LE = "GRAY16_LE"
)

// MaxDimension bounds a frame's width and height. It keeps one frame, and the
// encoder work it asks for, bounded no matter what a driver publishes.
const MaxDimension = 4096

// BytesPerPixel returns the size of one pixel of format, and false for a
// format this package does not support.
func BytesPerPixel(format string) (int, bool) {
	switch format {
	case FormatGRAY8:
		return 1, true
	case FormatYUY2, FormatGRAY16LE:
		return 2, true
	default:
		return 0, false
	}
}

// ErrUnsupportedFormat is returned for a frame whose Format this package does
// not know how to encode.
var ErrUnsupportedFormat = errors.New("unsupported raw video format")

// Validate reports whether f is a whole frame of a supported format: known
// format, dimensions in range, and exactly Width*Height pixels of data.
func (f Frame) Validate() error {
	bpp, ok := BytesPerPixel(f.Format)
	if !ok {
		return fmt.Errorf("%w %q (want %s, %s or %s)", ErrUnsupportedFormat, f.Format,
			FormatGRAY8, FormatYUY2, FormatGRAY16LE)
	}
	if f.Width < 1 || f.Height < 1 || f.Width > MaxDimension || f.Height > MaxDimension {
		return fmt.Errorf("frame size %dx%d out of range (1..%d per axis)", f.Width, f.Height, MaxDimension)
	}
	if f.Format == FormatYUY2 && f.Width%2 != 0 {
		// YUY2 stores two pixels per four bytes; an odd width cannot be laid out.
		return fmt.Errorf("YUY2 frame width %d is odd", f.Width)
	}
	if want := f.Width * f.Height * bpp; len(f.Data) != want {
		return fmt.Errorf("%dx%d %s frame has %d bytes, want %d", f.Width, f.Height, f.Format, len(f.Data), want)
	}
	return nil
}

// Describe names a frame's shape, for logs and errors: "256x192 YUY2".
func (f Frame) Describe() string {
	return fmt.Sprintf("%dx%d %s", f.Width, f.Height, f.Format)
}
