package services

import (
	"strings"
	"testing"
	"unsafe"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// The VIDIOC_ENUM_FRAMESIZES request code encodes the struct's size, so the
// Go struct and the constant must agree with the kernel's layout or the ioctl
// silently misbehaves (ENOTTY, or worse, a partial read into the wrong fields).
// Both numbers are hand-derived, so pin them.
//
//	struct v4l2_frmsizeenum {
//	    __u32 index;                  // 4
//	    __u32 pixel_format;           // 4
//	    __u32 type;                   // 4
//	    union { discrete(8); stepwise(24); };  // 24 — the larger arm sizes it
//	    __u32 reserved[2];            // 8
//	};                                // 44
func TestV4L2FrmSizeEnumLayout(t *testing.T) {
	if got := unsafe.Sizeof(v4l2FrmSizeEnum{}); got != 44 {
		t.Fatalf("v4l2FrmSizeEnum is %d bytes, kernel expects 44", got)
	}

	// Discrete width/height are the first two members of the union, i.e. at
	// offset 12 and 16 — right after index/pixel_format/type.
	var fse v4l2FrmSizeEnum
	if off := unsafe.Offsetof(fse.Width); off != 12 {
		t.Errorf("discrete.width at offset %d, want 12", off)
	}
	if off := unsafe.Offsetof(fse.Height); off != 16 {
		t.Errorf("discrete.height at offset %d, want 16", off)
	}
}

// _IOWR('V', 74, struct v4l2_frmsizeenum):
//
//	dir  = _IOC_READ|_IOC_WRITE = 3 -> bits 30..31
//	size = 44                       -> bits 16..29
//	type = 'V' = 0x56               -> bits 8..15
//	nr   = 74  = 0x4a               -> bits 0..7
func TestEnumFramesizesRequestCode(t *testing.T) {
	const (
		dirReadWrite = 3
		size         = 44
		typ          = 'V'
		nr           = 74
	)
	want := uint32(dirReadWrite)<<30 | uint32(size)<<16 | uint32(typ)<<8 | uint32(nr)
	if vidiocEnumFramesizes != want {
		t.Fatalf("vidiocEnumFramesizes = %#x, want %#x", uint32(vidiocEnumFramesizes), want)
	}
	if want != 0xC02C564A {
		t.Fatalf("derivation drifted: %#x", want)
	}
}

// The cap exists so a 4K webcam does not become the default for every
// subscriber that asked for "no preference". It sits at 720p, not 1080p,
// because both codec halves of the default path can land on a CPU: encode
// (x264enc fallback for cameras without onboard H.264: ~5 fps at 1080p on an
// AGX-class Jetson) and decode (Orin Nano has no NVDEC: ~5.7 fps at 1080p).
func TestDefaultFrameSizeCapIs720p(t *testing.T) {
	if defaultMaxDefaultWidth != 1280 || defaultMaxDefaultHeight != 720 {
		t.Fatalf("default cap is %dx%d, want 1280x720",
			defaultMaxDefaultWidth, defaultMaxDefaultHeight)
	}
}

// withMJPEGProbe overrides the MJPEG capability probe for the duration of a test.
func withMJPEGProbe(t *testing.T, fn func(string, uint32, uint32) bool) {
	t.Helper()
	orig := deviceSupportsMJPEGSize
	deviceSupportsMJPEGSize = fn
	t.Cleanup(func() { deviceSupportsMJPEGSize = orig })
}

// A USB camera that advertises MJPEG at the negotiated size should be captured
// compressed (image/jpeg + jpegdec) — raw capture is USB-bandwidth-capped to
// single-digit frame rates at 720p+ on common webcams.
func TestBuildGStreamerArgs_USB_PrefersMJPEG(t *testing.T) {
	withMJPEGProbe(t, func(path string, w, h uint32) bool { return w == 1280 && h == 720 })
	req := &agentpb.StreamVideoRequest{Width: 1280, Height: 720}
	args, err := buildGStreamerArgs("/usr/bin/gst-launch-1.0", "/dev/video9", req,
		"x264enc", true, camera.TransportUSB, "", pipeWireSource{}, map[string]bool{"jpegdec": true})
	if err != nil {
		t.Fatalf("buildGStreamerArgs: %v", err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "image/jpeg,width=1280,height=720") {
		t.Fatalf("expected MJPEG capture caps, got: %s", joined)
	}
	if !strings.Contains(joined, "jpegdec") {
		t.Fatalf("expected jpegdec in pipeline, got: %s", joined)
	}
	// The leaky queue must sit on the compressed side, before the decode.
	if strings.Index(joined, "jpegdec") < strings.Index(joined, "leaky=downstream") {
		t.Fatalf("leaky queue must precede jpegdec, got: %s", joined)
	}
}

// Without MJPEG support at the negotiated size, capture stays raw.
func TestBuildGStreamerArgs_USB_RawWhenNoMJPEG(t *testing.T) {
	withMJPEGProbe(t, func(string, uint32, uint32) bool { return false })
	req := &agentpb.StreamVideoRequest{Width: 640, Height: 480}
	args, err := buildGStreamerArgs("/usr/bin/gst-launch-1.0", "/dev/video9", req,
		"x264enc", true, camera.TransportUSB, "", pipeWireSource{}, map[string]bool{"jpegdec": true})
	if err != nil {
		t.Fatalf("buildGStreamerArgs: %v", err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "jpegdec") || strings.Contains(joined, "image/jpeg") {
		t.Fatalf("expected raw capture, got: %s", joined)
	}
	if !strings.Contains(joined, "video/x-raw,width=640,height=480") {
		t.Fatalf("expected raw caps, got: %s", joined)
	}
}

// MJPEG capture also needs jpegdec present on the device; otherwise stay raw
// even when the camera offers MJPEG.
func TestBuildGStreamerArgs_USB_RawWhenNoJpegdec(t *testing.T) {
	withMJPEGProbe(t, func(string, uint32, uint32) bool { return true })
	req := &agentpb.StreamVideoRequest{Width: 1280, Height: 720}
	args, err := buildGStreamerArgs("/usr/bin/gst-launch-1.0", "/dev/video9", req,
		"x264enc", true, camera.TransportUSB, "", pipeWireSource{}, map[string]bool{"x264enc": true})
	if err != nil {
		t.Fatalf("buildGStreamerArgs: %v", err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "jpegdec") || strings.Contains(joined, "image/jpeg") {
		t.Fatalf("expected raw capture without jpegdec available, got: %s", joined)
	}
}

// A thermal module offers none of the formats a webcam does. The PureThermal
// carrying a FLIR Lepton advertises Y16, UYVY and GREY — so probing only
// {YUYV, MJPEG} found nothing, the agent concluded the camera had no frame
// sizes, and zero dimensions took out the whole capture path: no caps on the
// pipeline, no frame ever negotiated, and the raw tap declined for "no discrete
// frame size". Observed on enmax01 with the agent holding /dev/video2 open in a
// pipeline that could never produce a frame.
func TestFrameSizeProbeFormatsCoversThermalPixelFormats(t *testing.T) {
	probed := frameSizeProbeFormats()

	seen := make(map[uint32]bool, len(probed))
	for _, f := range probed {
		seen[f] = true
	}

	for _, want := range []struct {
		name   string
		pixfmt uint32
	}{
		{"YUYV", v4l2PixFmtYUYV},
		{"UYVY", v4l2PixFmtUYVY},
		{"Y16", v4l2PixFmtY16},
		{"GREY", v4l2PixFmtGrey},
		{"MJPEG", v4l2PixFmtMJPEG},
	} {
		if !seen[want.pixfmt] {
			t.Errorf("frame size probe omits %s (0x%08x); a camera offering only "+
				"that format reports no sizes and becomes unstreamable", want.name, want.pixfmt)
		}
	}

	// Every format the raw tap can capture must be probed, or the tap can be
	// offered a format whose sizes were never enumerated.
	for _, f := range rawPixelFormats {
		if !seen[f.v4l2] {
			t.Errorf("raw tap handles %q but the frame size probe does not ask about it",
				strings.TrimSpace(f.fourcc))
		}
	}

	// The common webcam path must be unchanged: YUYV is still tried first.
	if len(probed) == 0 || probed[0] != v4l2PixFmtYUYV {
		t.Errorf("probe order changed: want YUYV first, got %v", probed)
	}
}
