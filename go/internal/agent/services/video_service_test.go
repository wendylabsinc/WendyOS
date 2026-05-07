package services

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newTestVideoService creates a VideoService with injectable filesystem functions.
func newTestVideoService(glob func() ([]string, error), readName func(string) (string, error)) *VideoService {
	svc := NewVideoService(zap.NewNop())
	if glob != nil {
		svc.globDevices = glob
	}
	if readName != nil {
		svc.readDeviceName = readName
	}
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

	devices, err := svc.listV4L2Devices()
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

func TestListV4L2Devices_NoDevices(t *testing.T) {
	svc := newTestVideoService(
		func() ([]string, error) { return nil, nil },
		nil,
	)

	devices, err := svc.listV4L2Devices()
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

	devices, err := svc.listV4L2Devices()
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

	_, err := svc.listV4L2Devices()
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
	args := buildGStreamerArgs("/usr/bin/gst-launch-1.0", "/dev/video0", req, "x264enc")
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
	if strings.Contains(joined, "h264parse") {
		t.Errorf("server-side pipeline should not include h264parse: %v", args)
	}
}

func TestBuildGStreamerArgs_WithDimensionsAndFramerate(t *testing.T) {
	req := &agentpb.StreamVideoRequest{Width: 1280, Height: 720, Framerate: 30}
	args := buildGStreamerArgs("/usr/bin/gst-launch-1.0", "/dev/video0", req, "x264enc")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "width=1280") || !strings.Contains(joined, "height=720") || !strings.Contains(joined, "framerate=30/1") {
		t.Errorf("expected dimension caps in args: %v", args)
	}
}

func TestBuildGStreamerArgs_V4L2HardwareEncoder(t *testing.T) {
	req := &agentpb.StreamVideoRequest{}
	args := buildGStreamerArgs("/usr/bin/gst-launch-1.0", "/dev/video0", req, "v4l2h264enc")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "v4l2h264enc") || !strings.Contains(joined, "video/x-h264") {
		t.Errorf("expected v4l2h264enc pipeline segment: %v", args)
	}
}

func TestBuildGStreamerArgs_NVV4L2HardwareEncoder(t *testing.T) {
	req := &agentpb.StreamVideoRequest{}
	args := buildGStreamerArgs("/usr/bin/gst-launch-1.0", "/dev/video0", req, "nvv4l2h264enc")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "nvv4l2h264enc") {
		t.Errorf("expected nvv4l2h264enc in pipeline: %v", args)
	}
	if !strings.Contains(joined, "video/x-raw,format=NV12") {
		t.Errorf("expected NV12 capsfilter for nvv4l2h264enc: %v", args)
	}
}

func TestBuildGStreamerArgs_VP8Encoder(t *testing.T) {
	req := &agentpb.StreamVideoRequest{}
	args := buildGStreamerArgs("/usr/bin/gst-launch-1.0", "/dev/video0", req, "vp8enc")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "vp8enc") || !strings.Contains(joined, "webmmux") {
		t.Errorf("expected vp8enc+webmmux pipeline segment: %v", args)
	}
	if strings.Contains(joined, "h264") {
		t.Errorf("VP8 pipeline should not mention h264: %v", args)
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

func TestStreamGStreamer_MissingGStreamer(t *testing.T) {
	t.Setenv("PATH", "") // ensure gst-launch-1.0 is not found regardless of host installation
	// Also neutralize the systemd-PATH fallback search so this test is deterministic
	// on hosts where gst-launch-1.0 happens to live in /usr/bin etc.
	prev := gstFallbackDirs
	gstFallbackDirs = nil
	t.Cleanup(func() { gstFallbackDirs = prev })
	svc := NewVideoService(zap.NewNop())
	err := svc.streamGStreamer(context.Background(), nil, "/dev/video0", &agentpb.StreamVideoRequest{})
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
