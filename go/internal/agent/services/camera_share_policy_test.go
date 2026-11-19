package services

import (
	"context"
	"errors"
	"testing"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// shareTestService describes a camera with the package's seams and returns a
// service whose gstElements already holds jpegdec.
func shareTestService(t *testing.T, formats []uint32, sizes map[uint32][][2]uint32, rates []uint32) *VideoService {
	t.Helper()
	withRawModes(t, sizes)
	prevBest, prevMJPEG, prevRates, prevDepth := bestDefaultFrameSizeForDevice, deviceSupportsMJPEGSize, frameRatesFor, isDepthOrInfraredNode
	t.Cleanup(func() {
		bestDefaultFrameSizeForDevice, deviceSupportsMJPEGSize, frameRatesFor, isDepthOrInfraredNode = prevBest, prevMJPEG, prevRates, prevDepth
	})
	bestDefaultFrameSizeForDevice = func(string) (uint32, uint32) {
		var w, h uint32
		for _, list := range sizes {
			for _, m := range list {
				if m[0] <= defaultMaxDefaultWidth && m[1] <= defaultMaxDefaultHeight && m[0]*m[1] > w*h {
					w, h = m[0], m[1]
				}
			}
		}
		return w, h
	}
	deviceSupportsMJPEGSize = func(_ string, w, h uint32) bool {
		for _, m := range sizes[v4l2PixFmtMJPEG] {
			if m[0] == w && m[1] == h {
				return true
			}
		}
		return false
	}
	frameRatesFor = func(string, uint32, uint32, uint32) []uint32 { return rates }
	isDepthOrInfraredNode = func(string) bool { return false }

	s := &VideoService{loopback: newFakeLoopback()}
	s.capturePixelFormats = func(string) ([]uint32, bool) { return formats, true }
	s.gstMemo.gstPath = "gst"
	s.gstMemo.available = map[string]bool{"jpegdec": true}
	return s
}

func usbCam() *agentpb.VideoDevice {
	return &agentpb.VideoDevice{Path: "/dev/video4", Transport: agentpb.VideoTransport_VIDEO_TRANSPORT_USB}
}

func brioSizes() map[uint32][][2]uint32 {
	modes := [][2]uint32{{640, 480}, {1280, 720}, {1920, 1080}}
	return map[uint32][][2]uint32{v4l2PixFmtYUYV: modes, v4l2PixFmtMJPEG: modes}
}

func TestShareDecision_MJPEGCameraSharesYUYVAtItsDefaultSize(t *testing.T) {
	s := shareTestService(t, []uint32{v4l2PixFmtYUYV, v4l2PixFmtMJPEG}, brioSizes(), []uint32{5, 15, 30})
	d := s.shareDecisionFor(context.Background(), usbCam())
	if !d.Shared {
		t.Fatalf("not shared: %s", d.Reason)
	}
	if want := (sharedMode{1280, 720, "YUYV", 2560, 30}); d.Mode != want {
		t.Errorf("Mode = %+v, want %+v", d.Mode, want)
	}
	if got := d.Mode.String(); got != "1280x720 YUYV, 30 fps" {
		t.Errorf("String = %q", got)
	}
	if d.Mode.frameBytes() != 2560*720 || d.Mode.pixelFormat() != v4l2PixFmtYUYV {
		t.Errorf("frameBytes/pixelFormat = %d/%#x", d.Mode.frameBytes(), d.Mode.pixelFormat())
	}
}

func TestShareDecision_RawOnlyCameraSharesItsOwnFormat(t *testing.T) {
	s := shareTestService(t, []uint32{v4l2PixFmtUYVY},
		map[uint32][][2]uint32{v4l2PixFmtUYVY: {{640, 480}}}, nil)
	d := s.shareDecisionFor(context.Background(), usbCam())
	if !d.Shared {
		t.Fatalf("not shared: %s", d.Reason)
	}
	if want := (sharedMode{640, 480, "UYVY", 1280, 30}); d.Mode != want {
		t.Errorf("Mode = %+v, want %+v", d.Mode, want)
	}
}

func TestShareDecision_RefusalsNameTheirReason(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, s *VideoService, dev *agentpb.VideoDevice)
		want   string
	}{
		{"h264", func(_ *testing.T, s *VideoService, _ *agentpb.VideoDevice) {
			s.capturePixelFormats = func(string) ([]uint32, bool) { return []uint32{v4l2PixFmtYUYV, v4l2PixFmtH264}, true }
		}, "the camera produces H.264 itself; sharing it comes in a later release"},
		{"csi", func(_ *testing.T, _ *VideoService, d *agentpb.VideoDevice) {
			d.Transport = agentpb.VideoTransport_VIDEO_TRANSPORT_CSI
		}, "CSI cameras are not shared yet"},
		{"ip", func(_ *testing.T, _ *VideoService, d *agentpb.VideoDevice) {
			d.Transport = agentpb.VideoTransport_VIDEO_TRANSPORT_IP
		}, "only USB cameras are shared in this release"},
		{"depth", func(*testing.T, *VideoService, *agentpb.VideoDevice) {
			isDepthOrInfraredNode = func(string) bool { return true }
		}, "depth and infrared nodes are not shared"},
		{"thermal", func(_ *testing.T, _ *VideoService, d *agentpb.VideoDevice) {
			d.Thermal = &agentpb.ThermalCamera{}
		}, "thermal cameras are not shared; an app reads their measurements at a mode it picks"},
		{"unreadable", func(_ *testing.T, s *VideoService, _ *agentpb.VideoDevice) {
			s.capturePixelFormats = func(string) ([]uint32, bool) { return nil, false }
		}, "the camera's formats could not be read"},
		{"empty", func(_ *testing.T, s *VideoService, _ *agentpb.VideoDevice) {
			s.capturePixelFormats = func(string) ([]uint32, bool) { return []uint32{}, true }
		}, "the camera's formats could not be read"},
		{"no module", func(_ *testing.T, s *VideoService, _ *agentpb.VideoDevice) {
			s.loopback.(*fakeLoopback).availableErr = errors.New("no module")
		}, "no compatible v4l2loopback module on this device"},
		{"nil loopback", func(_ *testing.T, s *VideoService, _ *agentpb.VideoDevice) {
			s.loopback = nil
		}, "no compatible v4l2loopback module on this device"},
		{"no gstreamer", func(t *testing.T, s *VideoService, _ *agentpb.VideoDevice) {
			s.gstMemo.available = nil
			t.Setenv("PATH", t.TempDir())
			prev := gstFallbackDirs
			gstFallbackDirs = nil
			t.Cleanup(func() { gstFallbackDirs = prev })
		}, "GStreamer is not installed on this device"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := shareTestService(t, []uint32{v4l2PixFmtYUYV, v4l2PixFmtMJPEG}, brioSizes(), []uint32{30})
			dev := usbCam()
			tc.mutate(t, s, dev)
			d := s.shareDecisionFor(context.Background(), dev)
			if d.Shared || d.Reason != tc.want {
				t.Errorf("got Shared=%v Reason=%q, want refusal %q", d.Shared, d.Reason, tc.want)
			}
		})
	}
}

func TestShareDecision_RateIsTheHighestAdvertised(t *testing.T) {
	for _, tc := range []struct {
		rates []uint32
		want  uint32
	}{{[]uint32{15, 30}, 30}, {[]uint32{5, 24, 15}, 24}, {nil, 30}} {
		s := shareTestService(t, []uint32{v4l2PixFmtYUYV, v4l2PixFmtMJPEG}, brioSizes(), tc.rates)
		d := s.shareDecisionFor(context.Background(), usbCam())
		if !d.Shared || d.Mode.FPS != tc.want {
			t.Errorf("rates %v: Shared=%v FPS=%d, want %d", tc.rates, d.Shared, d.Mode.FPS, tc.want)
		}
	}
}

func TestSharedMode_Admits(t *testing.T) {
	m := sharedMode{Width: 1280, Height: 720, Fourcc: "YUYV", BytesPerLine: 2560, FPS: 30}
	for _, tc := range []struct {
		req  *agentpb.StreamVideoRequest
		want bool
	}{
		{&agentpb.StreamVideoRequest{}, true},
		{&agentpb.StreamVideoRequest{Width: 1280, Height: 720}, true},
		{&agentpb.StreamVideoRequest{Framerate: 30}, true},
		{&agentpb.StreamVideoRequest{Width: 1280, Height: 720, Framerate: 30}, true},
		{&agentpb.StreamVideoRequest{Width: 1920, Height: 1080}, false},
		{&agentpb.StreamVideoRequest{Width: 1280, Height: 720, Framerate: 15}, false},
	} {
		if got := m.admits(tc.req); got != tc.want {
			t.Errorf("admits(%v) = %v, want %v", tc.req, got, tc.want)
		}
	}
}
