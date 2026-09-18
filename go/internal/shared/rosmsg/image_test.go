package rosmsg

import (
	"encoding/binary"
	"testing"
)

func imagePayload(frameID string, width, height uint32, encoding string) []byte {
	var buf []byte
	align := func(n int) {
		for len(buf)%n != 0 {
			buf = append(buf, 0)
		}
	}
	u32 := func(v uint32) { align(4); buf = binary.LittleEndian.AppendUint32(buf, v) }
	str := func(s string) {
		u32(uint32(len(s) + 1))
		buf = append(buf, s...)
		buf = append(buf, 0)
	}

	u32(1758207600) // header.stamp.sec
	u32(250000000)  // header.stamp.nanosec
	str(frameID)
	u32(height)
	u32(width)
	str(encoding)
	buf = append(buf, 0) // is_bigendian
	u32(width * 3)       // step
	// The pixel data follows and is deliberately never read.
	u32(width * height * 3)
	buf = append(buf, make([]byte, 64)...)
	return append([]byte{0x00, 0x01, 0x00, 0x00}, buf...)
}

func TestDecodeImageHeaderReadsTheFrameWithoutThePixels(t *testing.T) {
	header, err := DecodeImageHeader(imagePayload("camera_color_optical_frame", 848, 480, "rgb8"))
	if err != nil {
		t.Fatal(err)
	}
	if header.Width != 848 || header.Height != 480 {
		t.Errorf("resolution = %dx%d, want 848x480", header.Width, header.Height)
	}
	if header.Encoding != "rgb8" {
		t.Errorf("Encoding = %q, want rgb8", header.Encoding)
	}
	if header.Step != 848*3 {
		t.Errorf("Step = %d, want %d", header.Step, 848*3)
	}
	if header.FrameID != "camera_color_optical_frame" {
		t.Errorf("FrameID = %q", header.FrameID)
	}
	if got := header.Resolution(); got != "848x480" {
		t.Errorf("Resolution() = %q, want 848x480", got)
	}
	if header.StampSec != 1758207600 || header.StampNanosec != 250000000 {
		t.Errorf("stamp = %d.%d, want the publisher's", header.StampSec, header.StampNanosec)
	}
}

// The measured resolution is the counterpart to a CameraInfo's calibrated one. The two
// differing is what a report has to be able to show, so a frame claiming no resolution
// is rejected rather than passed along as zero.
func TestDecodeImageHeaderRejectsAFrameWithNoResolution(t *testing.T) {
	if _, err := DecodeImageHeader(imagePayload("cam", 0, 0, "rgb8")); err == nil {
		t.Error("a zero-resolution frame was accepted")
	}
}

func TestDecodeImageHeaderRejectsATruncatedFrame(t *testing.T) {
	full := imagePayload("cam", 640, 480, "rgb8")
	for _, cut := range []int{6, 16, 30} {
		if _, err := DecodeImageHeader(full[:cut]); err == nil {
			t.Errorf("payload truncated at %d decoded without error", cut)
		}
	}
}
