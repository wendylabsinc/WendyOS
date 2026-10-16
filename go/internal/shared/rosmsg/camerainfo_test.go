package rosmsg

import (
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/rtps/cdr"
)

// cdrBody builds a little-endian CDR payload, paying the alignment the format requires.
// Writing one by hand is how the decoder gets exercised without a camera on the bench.
type cdrBody struct{ buf []byte }

func (w *cdrBody) align(n int) {
	for len(w.buf)%n != 0 {
		w.buf = append(w.buf, 0)
	}
}

func (w *cdrBody) uint32(v uint32) {
	w.align(4)
	w.buf = binary.LittleEndian.AppendUint32(w.buf, v)
}

func (w *cdrBody) int32(v int32) { w.uint32(uint32(v)) }

func (w *cdrBody) float64(v float64) {
	w.align(8)
	w.buf = binary.LittleEndian.AppendUint64(w.buf, math.Float64bits(v))
}

// str writes a ROS string: a length that counts the trailing NUL, then the bytes.
func (w *cdrBody) str(s string) {
	w.uint32(uint32(len(s) + 1))
	w.buf = append(w.buf, s...)
	w.buf = append(w.buf, 0)
}

func (w *cdrBody) float64Seq(values ...float64) {
	w.uint32(uint32(len(values)))
	for _, v := range values {
		w.float64(v)
	}
}

// payload prefixes the CDR_LE encapsulation header the decoder expects.
func (w *cdrBody) payload() []byte {
	return append([]byte{0x00, 0x01, 0x00, 0x00}, w.buf...)
}

// cameraInfoPayload encodes a CameraInfo up to and including the K matrix, which is all
// the decoder reads.
func cameraInfoPayload(frameID string, width, height uint32, fx, fy, cx, cy float64) []byte {
	var w cdrBody
	w.int32(1758207600) // header.stamp.sec
	w.uint32(250000000) // header.stamp.nanosec
	w.str(frameID)
	w.uint32(height)
	w.uint32(width)
	w.str("plumb_bob")
	w.float64Seq(0, 0, 0, 0, 0)
	for _, v := range []float64{fx, 0, cx, 0, fy, cy, 0, 0, 1} {
		w.float64(v)
	}
	return w.payload()
}

// The intrinsics here are the ones measured on unitree-g1-nx-2: 43.4 degrees vertical
// and 55.9 horizontal at 640x480. Deriving both axes from K is what makes the axis
// explicit at the source, so nothing downstream has to remember which one it holds.
func TestDecodeCameraInfoDerivesBothAxesFromTheIntrinsics(t *testing.T) {
	payload := cameraInfoPayload("camera_color_optical_frame", 640, 480, 603.33, 603.67, 320.5, 240.5)

	info, err := DecodeCameraInfo(payload)
	if err != nil {
		t.Fatal(err)
	}
	if info.FrameID != "camera_color_optical_frame" {
		t.Errorf("FrameID = %q", info.FrameID)
	}
	if info.Width != 640 || info.Height != 480 {
		t.Errorf("resolution = %dx%d, want 640x480", info.Width, info.Height)
	}
	if info.DistortionModel != "plumb_bob" {
		t.Errorf("DistortionModel = %q", info.DistortionModel)
	}
	if info.CX != 320.5 || info.CY != 240.5 {
		t.Errorf("principal point = (%g, %g), want (320.5, 240.5)", info.CX, info.CY)
	}

	horizontal, vertical, err := info.FieldOfView()
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(vertical-43.4) > 0.05 {
		t.Errorf("vertical FOV = %.2f, want 43.4 (the figure measured on the robot)", vertical)
	}
	if math.Abs(horizontal-55.9) > 0.05 {
		t.Errorf("horizontal FOV = %.2f, want 55.9", horizontal)
	}
	if got := info.Resolution(); got != "640x480" {
		t.Errorf("Resolution() = %q, want 640x480; a field of view is only true at its resolution", got)
	}
}

// An uncalibrated RealSense publishes an all-zero K. Deriving a field of view from it
// yields exactly 180 degrees, which looks like a measurement and is not one.
func TestFieldOfViewRefusesAnUncalibratedCamera(t *testing.T) {
	payload := cameraInfoPayload("camera_color_optical_frame", 848, 480, 0, 0, 0, 0)
	info, err := DecodeCameraInfo(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := info.FieldOfView(); err == nil {
		t.Fatal("an all-zero K yielded a field of view; 180 degrees would have been reported as fact")
	} else if !strings.Contains(err.Error(), "uncalibrated") {
		t.Errorf("error should name the cause: %v", err)
	}
}

func TestFieldOfViewRefusesAMissingResolution(t *testing.T) {
	info := CameraInfo{FrameID: "cam", FX: 600, FY: 600}
	if _, _, err := info.FieldOfView(); err == nil {
		t.Error("intrinsics without a resolution yielded a field of view")
	}
}

func TestDecodeCameraInfoReportsWhereATruncatedPayloadEnded(t *testing.T) {
	full := cameraInfoPayload("cam", 640, 480, 600, 600, 320, 240)
	for _, cut := range []int{8, 20, 40, len(full) - 8} {
		if _, err := DecodeCameraInfo(full[:cut]); err == nil {
			t.Errorf("payload truncated at %d decoded without error", cut)
		} else if !errors.Is(err, cdr.ErrShort) {
			t.Errorf("truncation at %d gave %v, want ErrShort", cut, err)
		}
	}
}

func TestDecodeCameraInfoRejectsAnUnknownEncapsulation(t *testing.T) {
	if _, err := DecodeCameraInfo([]byte{0xff, 0xff, 0x00, 0x00, 0x01}); err == nil {
		t.Error("an unknown representation identifier was accepted")
	}
}
