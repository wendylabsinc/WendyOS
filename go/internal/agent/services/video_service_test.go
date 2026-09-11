package services

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestV4L2FormatMatches64BitUAPI(t *testing.T) {
	var format v4l2Format
	if got := unsafe.Sizeof(format); got != 208 {
		t.Fatalf("sizeof(v4l2Format) = %d, want 208", got)
	}
	if got := unsafe.Offsetof(format.Width); got != 8 {
		t.Fatalf("offsetof(v4l2Format.Width) = %d, want 8", got)
	}
}

// mustBuildGStreamerArgs calls buildGStreamerArgs for the USB/v4l2src path and
// fails the test if it returns an error. CSI-specific behaviour is covered by
// the dedicated TestBuildGStreamerArgs_CSI_* tests, which call buildGStreamerArgs
// directly with a CSI transport.
func mustBuildGStreamerArgs(t *testing.T, gstPath, devicePath string, req *agentpb.StreamVideoRequest, encoder string, hasH264Parse bool) []string {
	t.Helper()
	args, err := buildGStreamerArgs(gstPath, devicePath, req, encoder, hasH264Parse, camera.TransportUSB, "", pipeWireSource{}, nil)
	if err != nil {
		t.Fatalf("unexpected error from buildGStreamerArgs: %v", err)
	}
	return args
}

// newTestVideoService creates a VideoService with injectable filesystem functions.
// hasVideoCapture defaults to always returning true so tests are not gated on real V4L2 devices.
// The CSI seams default to "Unknown transport / no libcamera / not Jetson" so existing
// V4L2 tests behave exactly as before unless they override them.
func newTestVideoService(glob func() ([]string, error), readName func(string) (string, error)) *VideoService {
	svc := NewVideoService(context.Background(), zap.NewNop())
	if glob != nil {
		svc.globDevices = glob
	}
	if readName != nil {
		svc.readDeviceName = readName
	}
	svc.hasVideoCapture = func(string) bool { return true }
	svc.classifyTransport = func(string) (camera.Transport, string) { return camera.TransportUnknown, "" }
	svc.enumerateLibcamera = func(context.Context) (map[string]string, error) { return nil, nil }
	svc.isJetson = func() bool { return false }
	svc.findCameraSource = func(context.Context, string) (uint64, bool) { return 0, false }
	// Keep enumeration off the host's real /dev/v4l; tests that care about
	// stable identity override this.
	svc.readStableNames = func() map[string]stableNames { return nil }
	return svc
}

func TestListV4L2Devices_TwoDevices(t *testing.T) {
	svc := newTestVideoService(
		func() ([]string, error) { return []string{"/dev/video0", "/dev/video1"}, nil },
		func(base string) (string, error) {
			names := map[string]string{"video0": "USB Camera", "video1": "Integrated Camera"}
			if name, ok := names[base]; ok {
				return name, nil
			}
			return base, nil
		},
	)

	devices, err := svc.listCameras(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(devices) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(devices))
	}
	if devices[0].GetId() != 0 || devices[0].GetName() != "USB Camera" || devices[0].GetPath() != "/dev/video0" {
		t.Errorf("device 0: got id=%d name=%q path=%q", devices[0].GetId(), devices[0].GetName(), devices[0].GetPath())
	}
	if devices[1].GetId() != 1 || devices[1].GetName() != "Integrated Camera" || devices[1].GetPath() != "/dev/video1" {
		t.Errorf("device 1: got id=%d name=%q path=%q", devices[1].GetId(), devices[1].GetName(), devices[1].GetPath())
	}
}

func TestListV4L2Devices_ExcludesNonCameraDrivers(t *testing.T) {
	// On Raspberry Pi 3/4 the bcm2835-isp and bcm2835-codec m2m nodes advertise
	// VIDEO_CAPTURE and would otherwise pollute `camera list` as fake cameras.
	// Only the real unicam capture node should be listed.
	svc := newTestVideoService(
		func() ([]string, error) {
			return []string{"/dev/video0", "/dev/video10", "/dev/video14", "/dev/video15"}, nil
		},
		func(base string) (string, error) {
			names := map[string]string{
				"video0":  "unicam",
				"video10": "bcm2835-codec-decode",
				"video14": "bcm2835-isp-capture0",
				"video15": "bcm2835-isp-capture1",
			}
			return names[base], nil
		},
	)
	svc.classifyTransport = func(base string) (camera.Transport, string) {
		drivers := map[string]string{
			"video0":  "bcm2835-unicam",
			"video10": "bcm2835-codec",
			"video14": "bcm2835-isp",
			"video15": "bcm2835-isp",
		}
		drv := drivers[base]
		if camera.IsNonCameraDriver(drv) {
			return camera.TransportUnknown, drv
		}
		return camera.TransportCSI, drv
	}

	devices, err := svc.listCameras(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("expected only the unicam node, got %d: %v", len(devices), devices)
	}
	if devices[0].GetPath() != "/dev/video0" || devices[0].GetName() != "unicam" {
		t.Errorf("expected /dev/video0 unicam, got path=%q name=%q", devices[0].GetPath(), devices[0].GetName())
	}
}

func TestListV4L2Devices_NoDevices(t *testing.T) {
	svc := newTestVideoService(
		func() ([]string, error) { return nil, nil },
		nil,
	)

	devices, err := svc.listCameras(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(devices) != 0 {
		t.Fatalf("expected 0 devices, got %d", len(devices))
	}
}

func TestListV4L2Devices_SysfsReadFailFallsBackToPath(t *testing.T) {
	svc := newTestVideoService(
		func() ([]string, error) { return []string{"/dev/video0"}, nil },
		func(base string) (string, error) { return "", fmt.Errorf("no sysfs") },
	)

	devices, err := svc.listCameras(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("expected 1 device, got %d", len(devices))
	}
	if devices[0].GetName() != "video0" {
		t.Errorf("expected fallback name 'video0', got %q", devices[0].GetName())
	}
}

func TestListV4L2Devices_GlobError(t *testing.T) {
	svc := newTestVideoService(
		func() ([]string, error) { return nil, fmt.Errorf("permission denied") },
		nil,
	)

	_, err := svc.listCameras(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestVideoService_ListVideoDevices(t *testing.T) {
	svc := newTestVideoService(
		func() ([]string, error) { return []string{"/dev/video0"}, nil },
		func(base string) (string, error) { return "Test Camera", nil },
	)

	resp, err := svc.ListVideoDevices(context.Background(), &agentpb.ListVideoDevicesRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.GetDevices()) != 1 {
		t.Fatalf("expected 1 device, got %d", len(resp.GetDevices()))
	}
	d := resp.GetDevices()[0]
	if d.GetId() != 0 || d.GetName() != "Test Camera" || d.GetPath() != "/dev/video0" {
		t.Errorf("unexpected device: id=%d name=%q path=%q", d.GetId(), d.GetName(), d.GetPath())
	}
}

func TestVideoService_ListVideoDevices_GlobError(t *testing.T) {
	svc := newTestVideoService(
		func() ([]string, error) { return nil, fmt.Errorf("permission denied") },
		nil,
	)

	_, err := svc.ListVideoDevices(context.Background(), &agentpb.ListVideoDevicesRequest{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.Internal {
		t.Errorf("expected codes.Internal, got %v", st.Code())
	}
}

func TestBuildGStreamerArgs_NoDimensions(t *testing.T) {
	req := &agentpb.StreamVideoRequest{}
	args := mustBuildGStreamerArgs(t, "/usr/bin/gst-launch-1.0", "/dev/video0", req, "x264enc", true)
	if len(args) == 0 || args[0] != "/usr/bin/gst-launch-1.0" {
		t.Errorf("expected first arg to be gst-launch-1.0 path, got %v", args)
	}
	if len(args) < 2 || args[1] != "-q" {
		t.Errorf("expected -q as second arg to suppress stdout noise, got %v", args)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "v4l2src") || !strings.Contains(joined, "x264enc") || !strings.Contains(joined, "fdsink") {
		t.Errorf("pipeline missing expected elements: %v", args)
	}
	if !strings.Contains(joined, "profile=high") {
		t.Errorf("x264enc pipeline must constrain profile=high for iOS compatibility: %v", args)
	}
	// The H.264 stream must be normalized to Annex B byte-stream with in-band
	// SPS/PPS; otherwise x264enc emits stream-format=avc and the client's
	// typefind cannot classify the raw piped stream.
	if !strings.Contains(joined, "h264parse config-interval=-1") {
		t.Errorf("server-side pipeline must repeat SPS/PPS via h264parse config-interval=-1: %v", args)
	}
	if !strings.Contains(joined, "video/x-h264,stream-format=byte-stream,alignment=au") {
		t.Errorf("server-side pipeline must force Annex B byte-stream output: %v", args)
	}
}

func TestBuildGStreamerArgs_WithoutH264Parse(t *testing.T) {
	req := &agentpb.StreamVideoRequest{}
	args := mustBuildGStreamerArgs(t, "/usr/bin/gst-launch-1.0", "/dev/video0", req, "x264enc", false)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "h264parse") {
		t.Errorf("pipeline must not use h264parse when unavailable: %v", args)
	}
	// No extra caps suffix: hardware encoders output byte-stream natively; x264enc
	// AVC output is an acceptable trade-off when h264parse is absent.
	if strings.Contains(joined, "stream-format") {
		t.Errorf("pipeline must not add stream-format caps when h264parse is unavailable: %v", args)
	}
}

func TestBuildGStreamerArgs_X264ProfileIsCapsFilter(t *testing.T) {
	req := &agentpb.StreamVideoRequest{}
	args := mustBuildGStreamerArgs(t, "/usr/bin/gst-launch-1.0", "/dev/video0", req, "x264enc", true)
	joined := strings.Join(args, " ")
	// profile=high must be an output capsfilter (,profile=high), not an x264enc
	// element property ( profile=high) — as a property x264enc may select a
	// 4:4:4 High profile that hardware decoders reject. The capsfilter only
	// constrains the negotiated output.
	if !strings.Contains(joined, "! video/x-h264,profile=high") {
		t.Fatalf("expected x264enc output capsfilter for high profile: %v", args)
	}
	if strings.Contains(joined, " profile=high") {
		t.Fatalf("profile=high must be a capsfilter, not an x264enc property: %v", args)
	}
}

func TestKeyframeIntervalFrames(t *testing.T) {
	cases := []struct {
		fps  uint32
		want int
	}{
		{0, 15},  // device default is treated as 30fps
		{30, 15}, // ~0.5s GOP
		{60, 30},
		{1, 1}, // never below 1
	}
	for _, c := range cases {
		if got := keyframeIntervalFrames(c.fps); got != c.want {
			t.Errorf("keyframeIntervalFrames(%d) = %d, want %d", c.fps, got, c.want)
		}
	}
}

func TestBuildGStreamerArgs_X264SetsShortKeyframeInterval(t *testing.T) {
	req := &agentpb.StreamVideoRequest{}
	args := mustBuildGStreamerArgs(t, "/usr/bin/gst-launch-1.0", "/dev/video0", req, "x264enc", true)
	joined := strings.Join(args, " ")
	// fps 0 -> default 30 -> a keyframe every ~0.5s -> 15 frames. A short GOP
	// lets a client resync within ~0.5s after a dropped or skipped frame.
	if !strings.Contains(joined, "key-int-max=15") {
		t.Errorf("x264enc pipeline must cap the GOP via key-int-max: %v", args)
	}
	if !strings.Contains(joined, "bframes=0") {
		t.Errorf("x264enc pipeline must disable B-frames so decoder reorder depth is 0: %v", args)
	}
}

func TestBuildGStreamerArgs_KeyframeIntervalScalesWithFramerate(t *testing.T) {
	req := &agentpb.StreamVideoRequest{Framerate: 60}
	args := mustBuildGStreamerArgs(t, "/usr/bin/gst-launch-1.0", "/dev/video0", req, "x264enc", true)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "key-int-max=30") {
		t.Errorf("at 60fps the GOP should be 30 frames (~0.5s): %v", args)
	}
}

func TestBuildGStreamerArgs_VP8SetsShortKeyframeInterval(t *testing.T) {
	req := &agentpb.StreamVideoRequest{}
	args := mustBuildGStreamerArgs(t, "/usr/bin/gst-launch-1.0", "/dev/video0", req, "vp8enc", false)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "keyframe-max-dist=15") {
		t.Errorf("vp8enc pipeline must cap the GOP via keyframe-max-dist: %v", args)
	}
}

func TestBuildGStreamerArgs_NVV4L2SetsKeyframeInterval(t *testing.T) {
	req := &agentpb.StreamVideoRequest{}
	args := mustBuildGStreamerArgs(t, "/usr/bin/gst-launch-1.0", "/dev/video0", req, "nvv4l2h264enc", true)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "iframeinterval=15") {
		t.Errorf("nvv4l2h264enc pipeline must set iframeinterval: %v", args)
	}
}

func TestBuildGStreamerArgs_V4L2SetsKeyframeInterval(t *testing.T) {
	req := &agentpb.StreamVideoRequest{}
	args := mustBuildGStreamerArgs(t, "/usr/bin/gst-launch-1.0", "/dev/video0", req, "v4l2h264enc", true)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "h264_i_frame_period=15") {
		t.Errorf("v4l2h264enc pipeline must set the I-frame period via extra-controls: %v", args)
	}
}

func TestBuildGStreamerArgs_WithDimensionsAndFramerate(t *testing.T) {
	req := &agentpb.StreamVideoRequest{Width: 1280, Height: 720, Framerate: 30}
	args := mustBuildGStreamerArgs(t, "/usr/bin/gst-launch-1.0", "/dev/video0", req, "x264enc", true)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "width=1280") || !strings.Contains(joined, "height=720") || !strings.Contains(joined, "framerate=30/1") {
		t.Errorf("expected dimension caps in args: %v", args)
	}
}

func TestBuildGStreamerArgs_V4L2HardwareEncoder(t *testing.T) {
	req := &agentpb.StreamVideoRequest{}
	args := mustBuildGStreamerArgs(t, "/usr/bin/gst-launch-1.0", "/dev/video0", req, "v4l2h264enc", true)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "v4l2h264enc") || !strings.Contains(joined, "video/x-h264") {
		t.Errorf("expected v4l2h264enc pipeline segment: %v", args)
	}
}

func TestBuildGStreamerArgs_V4L2PinsH264Level(t *testing.T) {
	// On the Raspberry Pi bcm2835-codec, a bare v4l2h264enc (even with a
	// profile-only capsfilter) negotiates an H.264 level the driver then rejects
	// at QBUF time ("Failed to process frame"). Pinning an explicit level in the
	// output caps is required for the encoder to run (verified on a Pi 4, WDY-1603).
	req := &agentpb.StreamVideoRequest{}
	args := mustBuildGStreamerArgs(t, "/usr/bin/gst-launch-1.0", "/dev/video0", req, "v4l2h264enc", true)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "level=(string)4") {
		t.Errorf("v4l2h264enc output caps must pin an H.264 level: %v", args)
	}
}

func TestBuildGStreamerArgs_NVV4L2HardwareEncoder(t *testing.T) {
	req := &agentpb.StreamVideoRequest{}
	args := mustBuildGStreamerArgs(t, "/usr/bin/gst-launch-1.0", "/dev/video0", req, "nvv4l2h264enc", true)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "nvv4l2h264enc") {
		t.Errorf("expected nvv4l2h264enc in pipeline: %v", args)
	}
	if !strings.Contains(joined, "video/x-raw,format=NV12") {
		t.Errorf("expected NV12 capsfilter for nvv4l2h264enc: %v", args)
	}
	// Without nvvidconv the encoder rejects system-memory NV12 and a USB camera on a
	// Jetson never streams.
	if !strings.Contains(joined, "nvvidconv ! video/x-raw(memory:NVMM),format=NV12 ! nvv4l2h264enc") {
		t.Errorf("nvv4l2h264enc must be fed NVMM NV12 via nvvidconv: %v", args)
	}
}

func TestBuildGStreamerArgs_VP8Encoder(t *testing.T) {
	req := &agentpb.StreamVideoRequest{}
	args := mustBuildGStreamerArgs(t, "/usr/bin/gst-launch-1.0", "/dev/video0", req, "vp8enc", false)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "vp8enc") || !strings.Contains(joined, "webmmux") {
		t.Errorf("expected vp8enc+webmmux pipeline segment: %v", args)
	}
	if strings.Contains(joined, "h264") {
		t.Errorf("VP8 pipeline should not mention h264: %v", args)
	}
}

func TestBuildGStreamerArgs_LeakyRawQueueBeforeEncoder(t *testing.T) {
	// No caps requested: the leaky raw queue must sit directly after v4l2src so
	// the encoder always works on the freshest raw frame and a capture backlog
	// drains by dropping raw frames rather than encoding stale ones.
	req := &agentpb.StreamVideoRequest{}
	args := mustBuildGStreamerArgs(t, "/usr/bin/gst-launch-1.0", "/dev/video0", req, "x264enc", true)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "queue max-size-buffers=2 max-size-bytes=0 max-size-time=0 leaky=downstream") {
		t.Fatalf("pipeline must insert a leaky raw queue before the encoder: %v", args)
	}
	srcIdx := strings.Index(joined, "v4l2src")
	queueIdx := strings.Index(joined, "queue ")
	encIdx := strings.Index(joined, "x264enc")
	if !(srcIdx < queueIdx && queueIdx < encIdx) {
		t.Errorf("leaky queue must sit between v4l2src and the encoder: %v", args)
	}
}

func TestBuildGStreamerArgs_LeakyRawQueueWithCaps(t *testing.T) {
	// With raw caps requested, the leaky queue must come after the caps filter
	// and before the encoder segment.
	req := &agentpb.StreamVideoRequest{Width: 1280, Height: 720, Framerate: 30}
	args := mustBuildGStreamerArgs(t, "/usr/bin/gst-launch-1.0", "/dev/video0", req, "x264enc", true)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "queue max-size-buffers=2 max-size-bytes=0 max-size-time=0 leaky=downstream") {
		t.Fatalf("pipeline must insert a leaky raw queue before the encoder: %v", args)
	}
	capsIdx := strings.Index(joined, "video/x-raw,width=1280")
	queueIdx := strings.Index(joined, "queue ")
	encIdx := strings.Index(joined, "x264enc")
	if !(capsIdx < queueIdx && queueIdx < encIdx) {
		t.Errorf("leaky queue must sit between the raw caps and the encoder: %v", args)
	}
}

func TestListGSTElements_ParsesElements(t *testing.T) {
	input := `
matroska:  matroskamux: Matroska muxer
matroska:  webmmux: WebM muxer
x264:  x264enc: H264 video encoder
vpx:  vp8enc: On2 VP8 Encoder
bad:  h264parse: H.264 parser
`
	// Inject a fake gst-inspect-1.0 that prints the above.
	tmpDir := t.TempDir()
	script := tmpDir + "/gst-inspect-1.0"
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '"+input+"'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	elements, err := listGSTElements(script)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{"matroskamux", "webmmux", "x264enc", "vp8enc", "h264parse"} {
		if !elements[want] {
			t.Errorf("expected %q in element list, got %v", want, elements)
		}
	}
}

// /dev/null is exactly what Thor ships as /dev/v4l2-nvenc: it opens, then rejects QUERYCAP.
func TestV4L2NodeUsable(t *testing.T) {
	if v4l2NodeUsable("/dev/null") {
		t.Error("/dev/null must not pass as a V4L2 device")
	}
	if v4l2NodeUsable(t.TempDir() + "/absent") {
		t.Error("a missing node must not pass")
	}
}

// stubV4L2NodeUsable forces the hardware-encoder device probe for the test's duration.
func stubV4L2NodeUsable(t *testing.T, usable bool) {
	t.Helper()
	prev := v4l2NodeUsable
	v4l2NodeUsable = func(string) bool { return usable }
	t.Cleanup(func() { v4l2NodeUsable = prev })
}

// Thor's exact element set: it registers nvv4l2h264enc but /dev/v4l2-nvenc is a /dev/null
// stub, so the pipeline cannot preroll. Selection has to fall through to the software encoder.
func TestFindGStreamerEncoder_SkipsNVENCWithUnusableNode(t *testing.T) {
	stubV4L2NodeUsable(t, false)
	available := map[string]bool{
		"nvv4l2h264enc": true, "x264enc": true, "h264parse": true,
		"vp8enc": true, "webmmux": true,
	}

	result, err := findGStreamerEncoderFromSet(available)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.element != "x264enc" {
		t.Errorf("expected fallback to x264enc, got %q", result.element)
	}
}

func TestFindGStreamerEncoder_UsesNVENCWithUsableNode(t *testing.T) {
	stubV4L2NodeUsable(t, true)
	available := map[string]bool{"nvv4l2h264enc": true, "x264enc": true, "h264parse": true}

	result, err := findGStreamerEncoderFromSet(available)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.element != "nvv4l2h264enc" {
		t.Errorf("expected nvv4l2h264enc, got %q", result.element)
	}
}

// With only a stubbed encoder there is nothing to fall back to. Reporting that names the
// remedy, where selecting it anyway produced an opaque preroll failure.
func TestFindGStreamerEncoder_GatedEncoderIsNeverAFallback(t *testing.T) {
	stubV4L2NodeUsable(t, false)
	available := map[string]bool{"nvv4l2h264enc": true, "h264parse": true}

	_, err := findGStreamerEncoderFromSet(available)
	if err == nil {
		t.Fatal("expected an error rather than selecting the gated encoder")
	}
	if !strings.Contains(err.Error(), "no supported GStreamer encoder found") {
		t.Errorf("error should name the remedy, got: %v", err)
	}
}

func TestFindGStreamerEncoder_PrefersX264(t *testing.T) {
	tmpDir := t.TempDir()
	script := tmpDir + "/gst-inspect-1.0"
	listing := "x264:  x264enc: H264 video encoder\nvpx:  vp8enc: VP8 encoder\n"
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '"+listing+"'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	result, err := findGStreamerEncoder(script)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.element != "x264enc" {
		t.Errorf("expected x264enc, got %q", result.element)
	}
	if result.codec != agentpb.VideoCodec_VIDEO_CODEC_H264 {
		t.Errorf("expected H264 codec, got %v", result.codec)
	}
}

func TestFindGStreamerEncoder_H264ParseDetection(t *testing.T) {
	tmpDir := t.TempDir()
	script := tmpDir + "/gst-inspect-1.0"

	withH264Parse := "x264:  x264enc: H264 video encoder\nbad:  h264parse: H.264 parser\n"
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '"+withH264Parse+"'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	result, err := findGStreamerEncoder(script)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.hasH264Parse {
		t.Errorf("expected hasH264Parse=true when h264parse is listed, got false")
	}

	withoutH264Parse := "x264:  x264enc: H264 video encoder\n"
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '"+withoutH264Parse+"'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	result, err = findGStreamerEncoder(script)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.hasH264Parse {
		t.Errorf("expected hasH264Parse=false when h264parse is not listed, got true")
	}
}

func TestFindGStreamerEncoder_PrefersNVV4L2OverOtherH264Encoders(t *testing.T) {
	stubV4L2NodeUsable(t, true) // no NVENC node on the build host
	tmpDir := t.TempDir()
	script := tmpDir + "/gst-inspect-1.0"
	listing := "x264:  x264enc: H264 video encoder\nvideo4linux2:  v4l2h264enc: V4L2 H264 encoder\nnvvideo4linux2:  nvv4l2h264enc: NVIDIA V4L2 H264 encoder\n"
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '"+listing+"'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	result, err := findGStreamerEncoder(script)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.element != "nvv4l2h264enc" {
		t.Errorf("expected nvv4l2h264enc, got %q", result.element)
	}
	if result.codec != agentpb.VideoCodec_VIDEO_CODEC_H264 {
		t.Errorf("expected H264 codec, got %v", result.codec)
	}
}

func TestFindGStreamerEncoder_PrefersVP8WhenH264ParseAbsent(t *testing.T) {
	tmpDir := t.TempDir()
	script := tmpDir + "/gst-inspect-1.0"
	// x264enc present but h264parse absent; vp8enc+webmmux also present
	listing := "x264:  x264enc: H264 video encoder\nvpx:  vp8enc: VP8 encoder\nmatroska:  webmmux: WebM muxer\n"
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '"+listing+"'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	result, err := findGStreamerEncoder(script)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.element != "vp8enc" {
		t.Errorf("expected vp8enc when h264parse absent but webmmux available, got %q", result.element)
	}
	if result.codec != agentpb.VideoCodec_VIDEO_CODEC_VP8 {
		t.Errorf("expected VP8 codec, got %v", result.codec)
	}
}

func TestFindGStreamerEncoder_FallsBackToVP8WhenNoH264Encoder(t *testing.T) {
	tmpDir := t.TempDir()
	script := tmpDir + "/gst-inspect-1.0"
	listing := "vpx:  vp8enc: VP8 encoder\nmatroska:  webmmux: WebM muxer\n"
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '"+listing+"'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	result, err := findGStreamerEncoder(script)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.element != "vp8enc" {
		t.Errorf("expected vp8enc fallback, got %q", result.element)
	}
	if result.codec != agentpb.VideoCodec_VIDEO_CODEC_VP8 {
		t.Errorf("expected VP8 codec, got %v", result.codec)
	}
}

func TestV4L2KeyframeControlIDs(t *testing.T) {
	// V4L2_CID_CODEC_BASE = V4L2_CTRL_CLASS_CODEC | 0x900. The keyframe control
	// IDs are fixed offsets from that base in the kernel uapi headers; pin them
	// here so a transcription slip cannot silently turn the ioctl into a no-op.
	const codecBase = 0x00990000 | 0x900
	if v4l2CIDGOPSize != codecBase+203 {
		t.Errorf("v4l2CIDGOPSize = %#x, want V4L2_CID_CODEC_BASE+203 = %#x", v4l2CIDGOPSize, codecBase+203)
	}
	if v4l2CIDH264IPeriod != codecBase+358 {
		t.Errorf("v4l2CIDH264IPeriod = %#x, want V4L2_CID_CODEC_BASE+358 = %#x", v4l2CIDH264IPeriod, codecBase+358)
	}
}

func TestV4L2ExtControlLayout(t *testing.T) {
	// struct v4l2_ext_control is __packed: id@0, value (union __s32)@12, 20 bytes.
	var ctrl v4l2ExtControl
	if len(ctrl) != 20 {
		t.Fatalf("v4l2_ext_control must be 20 bytes packed, got %d", len(ctrl))
	}
	ctrl.setID(0xAABBCCDD)
	ctrl.setValue(0x11223344)
	if got := binary.LittleEndian.Uint32(ctrl[0:4]); got != 0xAABBCCDD {
		t.Errorf("id must be at offset 0, got %#x", got)
	}
	if got := binary.LittleEndian.Uint32(ctrl[12:16]); got != 0x11223344 {
		t.Errorf("value must be at offset 12, got %#x", got)
	}
}

func TestV4L2ExtControlsLayout(t *testing.T) {
	// struct v4l2_ext_controls: which@0, count@4, controls pointer@24, 32 bytes.
	var ctrls v4l2ExtControls
	if len(ctrls) != 32 {
		t.Fatalf("v4l2_ext_controls must be 32 bytes, got %d", len(ctrls))
	}
	ctrls.setWhich(0x00990000)
	ctrls.setCount(1)
	ctrls.setControlsPtr(0xDEADBEEF)
	if got := binary.LittleEndian.Uint32(ctrls[0:4]); got != 0x00990000 {
		t.Errorf("which must be at offset 0, got %#x", got)
	}
	if got := binary.LittleEndian.Uint32(ctrls[4:8]); got != 1 {
		t.Errorf("count must be at offset 4, got %#x", got)
	}
	if got := binary.LittleEndian.Uint64(ctrls[24:32]); got != 0xDEADBEEF {
		t.Errorf("controls pointer must be at offset 24, got %#x", got)
	}
}

func TestStreamGStreamer_MissingGStreamer(t *testing.T) {
	t.Setenv("PATH", "") // ensure gst-launch-1.0 is not found regardless of host installation
	// Also neutralize the systemd-PATH fallback search so this test is deterministic
	// on hosts where gst-launch-1.0 happens to live in /usr/bin etc.
	prev := gstFallbackDirs
	gstFallbackDirs = nil
	t.Cleanup(func() { gstFallbackDirs = prev })
	svc := NewVideoService(context.Background(), zap.NewNop())
	err := svc.streamGStreamer(context.Background(), nil, "/dev/video0", &agentpb.StreamVideoRequest{}, camera.TransportUSB, "", pipeWireSource{}, noRawSink{})
	if err == nil {
		t.Fatal("expected error when gst-launch-1.0 not found")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("expected FailedPrecondition, got %v", st.Code())
	}
}

func newTestHub(t *testing.T) (*deviceHub, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := &deviceHub{
		subs:   make(map[int]*hubSubscriber),
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	return h, cancel
}

func TestDeviceHub_TwoSubscribersReceiveSameFrame(t *testing.T) {
	h, cancel := newTestHub(t)
	defer cancel()

	id1, ch1, _ := h.subscribe()
	id2, ch2, _ := h.subscribe()
	defer h.unsubscribe(id1)
	defer h.unsubscribe(id2)

	frame := &videoFrame{data: []byte{0xAB, 0xCD}, tsNs: 12345}
	if !h.broadcast(frame) {
		t.Fatal("broadcast returned false with two active subscribers")
	}

	for _, ch := range []chan *videoFrame{ch1, ch2} {
		select {
		case f := <-ch:
			if !bytes.Equal(f.data, frame.data) || f.tsNs != frame.tsNs {
				t.Errorf("subscriber received wrong frame: got data=%x tsNs=%d, want data=%x tsNs=%d",
					f.data, f.tsNs, frame.data, frame.tsNs)
			}
		case <-time.After(100 * time.Millisecond):
			t.Error("subscriber did not receive frame within timeout")
		}
	}
}

func TestDeviceHub_LastUnsubscribeCancelsProducer(t *testing.T) {
	h, _ := newTestHub(t)

	id1, _, _ := h.subscribe()
	id2, _, _ := h.subscribe()

	h.unsubscribe(id1)
	if h.ctx.Err() != nil {
		t.Error("context cancelled after first of two subscribers unsubscribed")
	}

	h.unsubscribe(id2)
	if h.ctx.Err() == nil {
		t.Error("context not cancelled after last subscriber unsubscribed")
	}
}

func TestDeviceHub_SlowSubscriberDropsFrames(t *testing.T) {
	h, cancel := newTestHub(t)
	defer cancel()

	_, ch, _ := h.subscribe()

	// Send more frames than the channel buffer (capacity 4).
	for i := 0; i < 10; i++ {
		h.broadcast(&videoFrame{data: []byte{byte(i)}})
	}

	if len(ch) > cap(ch) {
		t.Errorf("channel overfull: len=%d cap=%d", len(ch), cap(ch))
	}
	// Drain without blocking.
	for len(ch) > 0 {
		<-ch
	}
}

func TestDeviceHub_BroadcastReturnsFalseWithNoSubscribers(t *testing.T) {
	h, cancel := newTestHub(t)
	defer cancel()

	id, _, _ := h.subscribe()
	h.unsubscribe(id)

	if h.broadcast(&videoFrame{data: []byte{1}}) {
		t.Error("broadcast should return false when there are no subscribers")
	}
}

func TestDeviceHub_ProducerErrorPropagated(t *testing.T) {
	h, _ := newTestHub(t)

	_, ch, _ := h.subscribe()

	// Simulate producer recording an error and closing the channel.
	wantErr := status.Errorf(codes.Internal, "camera read failed: test error")
	h.mu.Lock()
	h.err = wantErr
	for _, c := range h.subs {
		close(c.ch)
	}
	h.mu.Unlock()

	// Drain the closed channel.
	for range ch {
	}

	if h.err != wantErr {
		t.Errorf("expected stored error %v, got %v", wantErr, h.err)
	}
}

func TestDeviceHub_GetOrCreateHub_RejectsParamMismatch(t *testing.T) {
	svc := newTestVideoService(nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req1 := &agentpb.StreamVideoRequest{Width: 1280, Height: 720, Framerate: 30}
	req2 := &agentpb.StreamVideoRequest{Width: 640, Height: 480, Framerate: 30}

	h, id, _, err := svc.getOrCreateHub(ctx, "/dev/video0", req1, hubHolderStreamClient)
	if err != nil {
		t.Fatalf("first getOrCreateHub failed: %v", err)
	}
	defer h.unsubscribe(id)

	_, _, _, err = svc.getOrCreateHub(ctx, "/dev/video0", req2, hubHolderStreamClient)
	if err == nil {
		t.Fatal("expected error for mismatched params, got nil")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", err)
	}
}

// --- CSI / ribbon-camera tests ---

// defaultElements is the element availability map used by the CSI pipeline tests:
// x264enc + h264parse + vp8enc/webmmux + libcamerasrc all present. Individual
// tests delete entries to exercise fallbacks.
func defaultElements() map[string]bool {
	return map[string]bool{
		"x264enc":      true,
		"h264parse":    true,
		"webmmux":      true,
		"vp8enc":       true,
		"libcamerasrc": true,
	}
}

// mustCSIArgs builds a pipeline with an explicit transport/libcameraID/available
// set and fails the test on a build error (buildGStreamerArgs returns an error
// for injection-token / invalid-parameter inputs).
func mustCSIArgs(t *testing.T, devicePath string, req *agentpb.StreamVideoRequest, encoder string, hasH264Parse bool, transport camera.Transport, libcameraID string, available map[string]bool) []string {
	t.Helper()
	args, err := buildGStreamerArgs("gst", devicePath, req, encoder, hasH264Parse, transport, libcameraID, pipeWireSource{}, available)
	if err != nil {
		t.Fatalf("unexpected error from buildGStreamerArgs: %v", err)
	}
	return args
}

func TestListV4L2Devices_UsbAndCsiMix(t *testing.T) {
	svc := newTestVideoService(
		func() ([]string, error) { return []string{"/dev/video0", "/dev/video1"}, nil },
		func(base string) (string, error) {
			return map[string]string{"video0": "USB Cam", "video1": "CSI Cam"}[base], nil
		},
	)
	svc.classifyTransport = func(base string) (camera.Transport, string) {
		switch base {
		case "video0":
			return camera.TransportUSB, "uvcvideo"
		case "video1":
			return camera.TransportCSI, "tegra-capture-vi"
		}
		return camera.TransportUnknown, ""
	}

	devices, err := svc.listCameras(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(devices) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(devices))
	}
	if devices[0].GetTransport() != agentpb.VideoTransport_VIDEO_TRANSPORT_USB || devices[0].GetDriver() != "uvcvideo" {
		t.Errorf("device 0 transport/driver wrong: %+v", devices[0])
	}
	if devices[1].GetTransport() != agentpb.VideoTransport_VIDEO_TRANSPORT_CSI || devices[1].GetDriver() != "tegra-capture-vi" {
		t.Errorf("device 1 transport/driver wrong: %+v", devices[1])
	}
}

func TestListV4L2Devices_CsiPopulatesLibcameraID(t *testing.T) {
	svc := newTestVideoService(
		func() ([]string, error) { return []string{"/dev/video0"}, nil },
		func(string) (string, error) { return "Ribbon", nil },
	)
	svc.classifyTransport = func(string) (camera.Transport, string) { return camera.TransportCSI, "tegra-capture-vi" }
	svc.enumerateLibcamera = func(context.Context) (map[string]string, error) {
		return map[string]string{"/base/soc/i2c/cam@1a": "Sensor"}, nil
	}

	devices, err := svc.listCameras(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if devices[0].GetLibcameraId() != "/base/soc/i2c/cam@1a" {
		t.Errorf("expected libcamera id to be set, got %q", devices[0].GetLibcameraId())
	}
}

func TestListV4L2Devices_AmbiguousLibcameraLeavesIDEmpty(t *testing.T) {
	svc := newTestVideoService(
		func() ([]string, error) { return []string{"/dev/video0", "/dev/video1"}, nil },
		func(string) (string, error) { return "Ribbon", nil },
	)
	svc.classifyTransport = func(string) (camera.Transport, string) { return camera.TransportCSI, "tegra-capture-vi" }
	svc.enumerateLibcamera = func(context.Context) (map[string]string, error) {
		return map[string]string{"/cam1": "A", "/cam2": "B"}, nil
	}

	devices, err := svc.listCameras(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, d := range devices {
		if d.GetLibcameraId() != "" {
			t.Errorf("expected empty libcamera id for ambiguous case, got %q on %s", d.GetLibcameraId(), d.GetPath())
		}
	}
}

func TestListV4L2Devices_LibcameraUnavailable_NoError(t *testing.T) {
	svc := newTestVideoService(
		func() ([]string, error) { return []string{"/dev/video0"}, nil },
		func(string) (string, error) { return "Cam", nil },
	)
	svc.classifyTransport = func(string) (camera.Transport, string) { return camera.TransportCSI, "tegra-capture-vi" }
	svc.enumerateLibcamera = func(context.Context) (map[string]string, error) { return nil, fmt.Errorf("no cam binary") }

	devices, err := svc.listCameras(context.Background())
	if err != nil {
		t.Fatalf("listCameras must not fail when libcamera enumeration errors: %v", err)
	}
	if devices[0].GetTransport() != agentpb.VideoTransport_VIDEO_TRANSPORT_CSI {
		t.Errorf("transport still must be classified: %+v", devices[0])
	}
	if devices[0].GetLibcameraId() != "" {
		t.Errorf("libcamera id must be empty when enumeration failed, got %q", devices[0].GetLibcameraId())
	}
}

func TestBuildGStreamerArgs_USB_UsesV4l2Src(t *testing.T) {
	args := mustCSIArgs(t, "/dev/video0", &agentpb.StreamVideoRequest{}, "x264enc", true, camera.TransportUSB, "", defaultElements())
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "v4l2src device=/dev/video0") {
		t.Errorf("USB pipeline must use v4l2src: %v", args)
	}
	if strings.Contains(joined, "libcamerasrc") {
		t.Errorf("USB pipeline must not use libcamerasrc: %v", args)
	}
}

func TestBuildGStreamerArgs_CSI_UsesLibcamerasrc(t *testing.T) {
	args := mustCSIArgs(t, "/dev/video0", &agentpb.StreamVideoRequest{}, "x264enc", true, camera.TransportCSI, "", defaultElements())
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "libcamerasrc") {
		t.Errorf("CSI pipeline must use libcamerasrc: %v", args)
	}
	if strings.Contains(joined, "v4l2src") {
		t.Errorf("CSI pipeline must not fall back to v4l2src when libcamerasrc is available: %v", args)
	}
}

func TestBuildGStreamerArgs_CSI_WithLibcameraID_AppendsCameraName(t *testing.T) {
	args := mustCSIArgs(t, "/dev/video0", &agentpb.StreamVideoRequest{}, "x264enc", true, camera.TransportCSI, "/base/cam@1a", defaultElements())
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "libcamerasrc camera-name=/base/cam@1a") {
		t.Errorf("CSI pipeline with id must pass camera-name=...: %v", args)
	}
}

func TestBuildGStreamerArgs_CSI_LibcamerasrcMissing_FallsBackToV4l2(t *testing.T) {
	elems := defaultElements()
	delete(elems, "libcamerasrc")
	args := mustCSIArgs(t, "/dev/video0", &agentpb.StreamVideoRequest{}, "x264enc", true, camera.TransportCSI, "/base/cam@1a", elems)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "v4l2src device=/dev/video0") {
		t.Errorf("CSI pipeline must fall back to v4l2src when libcamerasrc plugin is absent: %v", args)
	}
	if strings.Contains(joined, "libcamerasrc") {
		t.Errorf("CSI pipeline must not use libcamerasrc when plugin absent: %v", args)
	}
}

func TestBuildGStreamerArgs_CSI_PinsNV12SourceFormat(t *testing.T) {
	// libcamerasrc must be pinned to a processed format immediately after the
	// source. On a PiSP/CFE camera (Raspberry Pi 5) an unconstrained libcamerasrc
	// negotiates raw Bayer, which no videoconvert/encoder can consume
	// (Camera::configure() -22, not-negotiated). The NV12 caps must sit between
	// the source and the leaky queue.
	args := mustCSIArgs(t, "/dev/video0", &agentpb.StreamVideoRequest{}, "x264enc", true, camera.TransportCSI, "", defaultElements())
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "video/x-raw,format=NV12") {
		t.Fatalf("CSI/libcamerasrc pipeline must pin format=NV12 on the source caps: %v", args)
	}
	srcIdx := strings.Index(joined, "libcamerasrc")
	nv12Idx := strings.Index(joined, "video/x-raw,format=NV12")
	queueIdx := strings.Index(joined, "queue ")
	if !(srcIdx < nv12Idx && nv12Idx < queueIdx) {
		t.Errorf("NV12 source caps must sit between libcamerasrc and the leaky queue: %v", args)
	}
}

func TestBuildGStreamerArgs_CSI_NV12SourceFormatWithDimensions(t *testing.T) {
	// Requested dimensions must be folded into the SAME format-pinned source
	// capsfilter, not a separate formatless one — a formatless width/height
	// capsfilter still lets libcamerasrc select raw Bayer.
	req := &agentpb.StreamVideoRequest{Width: 1280, Height: 720, Framerate: 30}
	args := mustCSIArgs(t, "/dev/video0", req, "x264enc", true, camera.TransportCSI, "", defaultElements())
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "video/x-raw,format=NV12,width=1280,height=720,framerate=30/1") {
		t.Errorf("CSI source caps must combine NV12 with the requested dimensions: %v", args)
	}
}

func TestBuildGStreamerArgs_USB_DoesNotForceNV12SourceFormat(t *testing.T) {
	// The NV12 source pin is specific to libcamerasrc. A USB v4l2src must keep
	// negotiating its native format (YUYV/MJPEG/...); forcing NV12 on the source
	// would break cameras that don't emit it. With x264enc the only possible NV12
	// mention would be such a source pin, so none must appear.
	args := mustCSIArgs(t, "/dev/video0", &agentpb.StreamVideoRequest{}, "x264enc", true, camera.TransportUSB, "", defaultElements())
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "video/x-raw,format=NV12") {
		t.Errorf("USB/v4l2src pipeline must not pin NV12 on the source: %v", args)
	}
}

func TestBuildSourceElement_NilAvailableMapTreatedAsLibcamerasrcAbsent(t *testing.T) {
	src := buildSourceElement("/dev/video0", camera.TransportCSI, "/cam", pipeWireSource{}, nil)
	if src != "v4l2src device=/dev/video0" {
		t.Errorf("nil availability must degrade CSI to v4l2src, got %q", src)
	}
}

// A libcamera id that could inject extra pipeline elements (spaces, '!', '=')
// must not reach the pipeline: buildSourceElement falls back to plain
// libcamerasrc auto-select instead of interpolating camera-name=<hostile>.
func TestBuildSourceElement_RejectsInjectableLibcameraID(t *testing.T) {
	hostile := "/cam ! filesink location=/etc/passwd"
	src := buildSourceElement("/dev/video0", camera.TransportCSI, hostile, pipeWireSource{}, defaultElements())
	if src != "libcamerasrc" {
		t.Errorf("hostile libcamera id must degrade to plain libcamerasrc, got %q", src)
	}
}

func TestBuildArgusGStreamerArgs_NVV4L2DirectNVMM(t *testing.T) {
	args := buildArgusGStreamerArgs("gst", &agentpb.StreamVideoRequest{}, 0, "nvv4l2h264enc", true,
		map[string]bool{"nvarguscamerasrc": true, "nvv4l2h264enc": true})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "nvarguscamerasrc sensor-id=0") {
		t.Errorf("expected nvarguscamerasrc with sensor-id=0: %v", args)
	}
	if !strings.Contains(joined, "video/x-raw(memory:NVMM)") {
		t.Errorf("expected NVMM caps for the Argus path: %v", args)
	}
	if !strings.Contains(joined, "nvv4l2h264enc") {
		t.Errorf("expected nvv4l2h264enc encoder: %v", args)
	}
	if strings.Contains(joined, "nvvidconv") || strings.Contains(joined, "videoconvert") {
		t.Errorf("nvv4l2h264enc consumes NVMM directly; must not convert: %v", args)
	}
	if !strings.Contains(joined, "h264parse config-interval=-1") {
		t.Errorf("expected Annex B normalization when h264parse available: %v", args)
	}
	if !strings.Contains(joined, "fdsink fd=1") {
		t.Errorf("expected fdsink fd=1 output: %v", args)
	}
	if len(args) < 2 || args[0] != "gst" || args[1] != "-q" {
		t.Errorf("expected [gst -q ...]: %v", args)
	}
}

func TestBuildArgusGStreamerArgs_DefaultDimensions(t *testing.T) {
	args := buildArgusGStreamerArgs("gst", &agentpb.StreamVideoRequest{}, 0, "nvv4l2h264enc", true, nil)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "width=1920") || !strings.Contains(joined, "height=1080") || !strings.Contains(joined, "framerate=30/1") {
		t.Errorf("expected default 1920x1080@30 caps: %v", args)
	}
}

func TestBuildArgusGStreamerArgs_RequestDimensionsOverrideDefaults(t *testing.T) {
	req := &agentpb.StreamVideoRequest{Width: 1280, Height: 720, Framerate: 60}
	args := buildArgusGStreamerArgs("gst", req, 0, "nvv4l2h264enc", true, nil)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "width=1280") || !strings.Contains(joined, "height=720") || !strings.Contains(joined, "framerate=60/1") {
		t.Errorf("expected requested caps to override defaults: %v", args)
	}
}

func TestBuildArgusGStreamerArgs_SensorID(t *testing.T) {
	args := buildArgusGStreamerArgs("gst", &agentpb.StreamVideoRequest{}, 1, "nvv4l2h264enc", false, nil)
	if !strings.Contains(strings.Join(args, " "), "sensor-id=1") {
		t.Errorf("expected sensor-id=1: %v", args)
	}
}

func TestBuildArgusGStreamerArgs_NoH264ParseOmitsByteStream(t *testing.T) {
	args := buildArgusGStreamerArgs("gst", &agentpb.StreamVideoRequest{}, 0, "nvv4l2h264enc", false, nil)
	if strings.Contains(strings.Join(args, " "), "h264parse") {
		t.Errorf("must not add h264parse when unavailable: %v", args)
	}
}

func TestBuildArgusGStreamerArgs_NonNVEncoderUsesNvvidconv(t *testing.T) {
	args := buildArgusGStreamerArgs("gst", &agentpb.StreamVideoRequest{}, 0, "x264enc", true, nil)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "video/x-raw(memory:NVMM)") {
		t.Errorf("expected NVMM source caps: %v", args)
	}
	if !strings.Contains(joined, "nvvidconv ! video/x-raw,format=NV12") {
		t.Errorf("non-NV encoder must convert NVMM->system via nvvidconv: %v", args)
	}
	if !strings.Contains(joined, "x264enc") {
		t.Errorf("expected x264enc segment: %v", args)
	}
}

func TestUseArgusSource(t *testing.T) {
	withArgus := map[string]bool{"nvarguscamerasrc": true}
	cases := []struct {
		name      string
		transport camera.Transport
		isJetson  bool
		available map[string]bool
		want      bool
	}{
		{"csi jetson with plugin", camera.TransportCSI, true, withArgus, true},
		{"usb jetson with plugin", camera.TransportUSB, true, withArgus, false},
		{"csi non-jetson with plugin", camera.TransportCSI, false, withArgus, false},
		{"csi jetson no plugin", camera.TransportCSI, true, map[string]bool{}, false},
		{"csi jetson nil map", camera.TransportCSI, true, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := useArgusSource(tc.transport, tc.isJetson, tc.available); got != tc.want {
				t.Errorf("useArgusSource(%v, %v, %v) = %v, want %v", tc.transport, tc.isJetson, tc.available, got, tc.want)
			}
		})
	}
}

func TestHasVideoCapture(t *testing.T) {
	cases := []struct {
		name         string
		capabilities uint32
		deviceCaps   uint32
		want         bool
	}{
		{
			// Raspberry Pi 5 CSI capture node (rp1-cfe-csi2_ch0) advertises
			// VIDEO_CAPTURE *and* METADATA_CAPTURE on the same node (observed
			// device caps 0x24a00001). It must be treated as a capture device, or
			// `device camera list` hides the actual ribbon camera.
			name:         "csi node with video+metadata caps",
			capabilities: 0xaca00001, // DEVICE_CAPS bit set -> deviceCaps is authoritative
			deviceCaps:   0x24a00001,
			want:         true,
		},
		{
			// A UVC metadata companion (e.g. /dev/video1) is METADATA_CAPTURE only
			// and must be excluded.
			name:         "metadata-only companion excluded",
			capabilities: v4l2CapDeviceCaps | v4l2CapMetaCapture,
			deviceCaps:   v4l2CapMetaCapture,
			want:         false,
		},
		{
			name:         "plain video capture included",
			capabilities: v4l2CapDeviceCaps | v4l2CapVideoCapture,
			deviceCaps:   v4l2CapVideoCapture,
			want:         true,
		},
		{
			name:         "no capture caps excluded",
			capabilities: v4l2CapDeviceCaps,
			deviceCaps:   0,
			want:         false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := v4l2Capability{Capabilities: tc.capabilities, DeviceCaps: tc.deviceCaps}
			if got := c.hasVideoCapture(); got != tc.want {
				t.Errorf("hasVideoCapture() = %v, want %v (caps=%#x devcaps=%#x)", got, tc.want, tc.capabilities, tc.deviceCaps)
			}
		})
	}
}
