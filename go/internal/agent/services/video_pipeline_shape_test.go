package services

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func withMJPEGDevice(t *testing.T) {
	t.Helper()
	withYUYVModes(t, [][2]uint32{{1280, 720}})
	prev := deviceSupportsMJPEGSize
	deviceSupportsMJPEGSize = func(string, uint32, uint32) bool { return true }
	t.Cleanup(func() { deviceSupportsMJPEGSize = prev })
}

func shapedPlan(shape pipelineShape, req *agentpb.StreamVideoRequest, transport camera.Transport, pw pipeWireSource, available map[string]bool) (gstPipelinePlan, error) {
	return planGStreamerPipelineShaped(shape, "gst", "/dev/video4", req, "x264enc", true, transport, "", pw, available)
}

func TestPlan_MJPEGCaptureTeesDecodedFramesForPixels(t *testing.T) {
	withMJPEGDevice(t)
	plan := mustPlan(t, &agentpb.StreamVideoRequest{Width: 1280, Height: 720}, map[string]bool{"x264enc": true, "jpegdec": true})
	if plan.pixels == nil || plan.pixels.GetWidth() != 1280 || plan.pixels.GetHeight() != 720 ||
		plan.pixels.GetFourcc() != fourccYUYV || plan.pixels.GetBytesPerLine() != 2560 {
		t.Errorf("pixels = %v, want 1280x720 YUYV 2560", plan.pixels)
	}
	if plan.raw != nil {
		t.Errorf("raw offered on an MJPEG capture: %v", plan.raw)
	}
	if got, w := plan.rawReason(), "camera is captured as MJPEG at 1280x720; raw frames are not offered"; got != w {
		t.Errorf("rawReason = %q, want %q", got, w)
	}
	joined := strings.Join(plan.args, " ")
	for _, s := range []string{
		"jpegdec ! tee name=rawtap",
		"rawtap. ! " + rawTapQueue + " ! videoconvert ! video/x-raw,format=YUY2 ! fdsink fd=3",
	} {
		if !strings.Contains(joined, s) {
			t.Errorf("pipeline missing %q: %s", s, joined)
		}
	}
	if plan.tapOnly {
		t.Error("an encoding pipeline must not be tapOnly")
	}
}

func TestPlan_WithoutEncoderWritesOnlyTheTap(t *testing.T) {
	check := func(t *testing.T, plan gstPipelinePlan, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		joined := strings.Join(plan.args, " ")
		for _, bad := range []string{"x264enc", "fd=1"} {
			if strings.Contains(joined, bad) {
				t.Errorf("pipeline contains %q: %s", bad, joined)
			}
		}
		for _, want := range []string{"fdsink fd=3", "format=YUY2"} {
			if !strings.Contains(joined, want) {
				t.Errorf("pipeline missing %q: %s", want, joined)
			}
		}
		if !plan.tapOnly || plan.pixels == nil {
			t.Errorf("tapOnly=%v pixels=%v", plan.tapOnly, plan.pixels)
		}
	}
	t.Run("mjpeg", func(t *testing.T) {
		withMJPEGDevice(t)
		plan, err := shapedPlan(pipelineShape{}, &agentpb.StreamVideoRequest{Width: 1280, Height: 720}, camera.TransportUSB, pipeWireSource{}, map[string]bool{"x264enc": true, "jpegdec": true})
		check(t, plan, err)
	})
	t.Run("raw yuyv", func(t *testing.T) {
		withYUYVModes(t, [][2]uint32{{640, 480}})
		plan, err := shapedPlan(pipelineShape{}, &agentpb.StreamVideoRequest{Width: 640, Height: 480}, camera.TransportUSB, pipeWireSource{}, map[string]bool{"x264enc": true})
		check(t, plan, err)
	})
}

func TestPlan_WithoutEncoderNeedsATap(t *testing.T) {
	_, err := shapedPlan(pipelineShape{}, &agentpb.StreamVideoRequest{}, camera.TransportCSI, pipeWireSource{}, map[string]bool{"x264enc": true, "libcamerasrc": true})
	if !errors.Is(err, errNoTapOnlyPipeline) {
		t.Errorf("CSI: err = %v", err)
	}
	_, err = shapedPlan(pipelineShape{}, &agentpb.StreamVideoRequest{}, camera.TransportUSB, pipeWireSource{serial: 42}, map[string]bool{"x264enc": true, "pipewiresrc": true})
	if !errors.Is(err, errNoTapOnlyPipeline) {
		t.Errorf("PipeWire: err = %v", err)
	}
}

func TestPlan_OversizedMJPEGFrameKeepsTodaysPipeline(t *testing.T) {
	withYUYVModes(t, [][2]uint32{{1920, 1080}})
	prev := deviceSupportsMJPEGSize
	deviceSupportsMJPEGSize = func(string, uint32, uint32) bool { return true }
	t.Cleanup(func() { deviceSupportsMJPEGSize = prev })
	plan := mustPlan(t, &agentpb.StreamVideoRequest{Width: 1920, Height: 1080}, map[string]bool{"x264enc": true, "jpegdec": true})
	if plan.pixels != nil {
		t.Errorf("pixels = %v for a frame over the limit", plan.pixels)
	}
	if joined := strings.Join(plan.args, " "); strings.Contains(joined, "tee") || !strings.Contains(joined, "jpegdec") {
		t.Errorf("pipeline changed: %s", joined)
	}
}

func TestPlan_DefaultShapeEqualsUnshapedPlanner(t *testing.T) {
	cases := map[string]struct {
		transport camera.Transport
		avail     map[string]bool
	}{
		"raw yuyv": {camera.TransportUSB, map[string]bool{"x264enc": true}},
		"csi":      {camera.TransportCSI, map[string]bool{"x264enc": true, "libcamerasrc": true}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			withYUYVModes(t, [][2]uint32{{640, 480}})
			req := &agentpb.StreamVideoRequest{Width: 640, Height: 480}
			a, errA := planGStreamerPipeline("gst", "/dev/video4", req, "x264enc", true, c.transport, "", pipeWireSource{}, c.avail)
			b, errB := planGStreamerPipelineShaped(pipelineShape{encode: true}, "gst", "/dev/video4", req, "x264enc", true, c.transport, "", pipeWireSource{}, c.avail)
			if errA != nil || errB != nil {
				t.Fatalf("errs: %v %v", errA, errB)
			}
			a.rawWhyLazy, b.rawWhyLazy = nil, nil
			if !reflect.DeepEqual(a, b) {
				t.Errorf("plans differ:\n%+v\n%+v", a, b)
			}
		})
	}
}

func TestTapFormat_FollowsPixelsNotRaw(t *testing.T) {
	withMJPEGDevice(t)
	mjpeg := mustPlan(t, &agentpb.StreamVideoRequest{Width: 1280, Height: 720}, map[string]bool{"x264enc": true, "jpegdec": true})
	if mjpeg.raw != nil || tapFormat(mjpeg) != mjpeg.pixels || tapFormat(mjpeg) == nil {
		t.Errorf("MJPEG: raw=%v tapFormat=%v", mjpeg.raw, tapFormat(mjpeg))
	}
	withYUYVModes(t, [][2]uint32{{640, 480}})
	raw := mustPlan(t, &agentpb.StreamVideoRequest{Width: 640, Height: 480}, map[string]bool{"x264enc": true})
	if raw.raw == nil || tapFormat(raw) != raw.raw {
		t.Errorf("raw: raw=%v tapFormat=%v", raw.raw, tapFormat(raw))
	}
	csi, err := shapedPlan(pipelineShape{encode: true}, &agentpb.StreamVideoRequest{}, camera.TransportCSI, pipeWireSource{}, map[string]bool{"x264enc": true, "libcamerasrc": true})
	if err != nil || tapFormat(csi) != nil {
		t.Errorf("CSI: err=%v tapFormat=%v", err, tapFormat(csi))
	}
}
