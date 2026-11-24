package services

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func encodeTestJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatalf("encode test jpeg: %v", err)
	}
	return buf.Bytes()
}

func TestJPEGFrameInBuffer(t *testing.T) {
	whole := encodeTestJPEG(t, 64, 48)
	join := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	endMarkerMidData := join(whole[:len(whole)-2], []byte{0xFF, 0xD9, 0x12, 0x34}, whole[len(whole)-2:])
	smallest := []byte{0xFF, 0xD8, 0x00, 0xFF, 0xD9}
	cases := []struct {
		name string
		data []byte
		want []byte
	}{
		{"complete image", whole, whole},
		{"one byte between the markers", smallest, smallest},
		{"zero padding after the end marker", join(whole, make([]byte, 4096)), whole},
		{"FF padding after the end marker", join(whole, bytes.Repeat([]byte{0xFF}, 64)), whole},
		{"vendor data after the end marker", join(whole, []byte{0x01, 0x02, 0xFF, 0xD8, 0x7F}), whole},
		{"end marker inside the data with real data after it", endMarkerMidData, endMarkerMidData},
		{"end marker inside the data, padded", join(endMarkerMidData, make([]byte, 16)), endMarkerMidData},
		{"no end marker", whole[:len(whole)-2], nil},
		{"no end marker, padded", join(whole[:len(whole)-2], make([]byte, 16)), nil},
		{"no start marker", whole[2:], nil},
		{"nothing between the markers", []byte{0xFF, 0xD8, 0xFF, 0xD9}, nil},
		{"nothing between the markers, padded", []byte{0xFF, 0xD8, 0xFF, 0xD9, 0x00, 0x00}, nil},
		{"start marker only", []byte{0xFF, 0xD8}, nil},
		{"start and end markers overlap", []byte{0xFF, 0xD8, 0xD9}, nil},
		{"only padding", make([]byte, 16), nil},
		{"empty", nil, nil},
	}
	for _, c := range cases {
		got, ok := jpegFrameInBuffer(c.data)
		if ok != (c.want != nil) || !bytes.Equal(got, c.want) {
			t.Errorf("%s: jpegFrameInBuffer = %d bytes, %v; want %d bytes, %v", c.name, len(got), ok, len(c.want), c.want != nil)
		}
	}
}

type broadcastFrame struct {
	data  []byte
	stamp frameTimestamp
	codec agentpb.VideoCodec
}

func recordingNativeJPEGFrames(sent *[]broadcastFrame) *nativeJPEGFrames {
	return &nativeJPEGFrames{logger: zap.NewNop(), device: "/dev/video4",
		broadcast: func(d []byte, stamp frameTimestamp, c agentpb.VideoCodec) bool {
			*sent = append(*sent, broadcastFrame{d, stamp, c})
			return true
		}}
}

func TestNativeJPEGFramesBroadcastsJPEGFramesAndDropsTheRest(t *testing.T) {
	var sent []broadcastFrame
	frames := recordingNativeJPEGFrames(&sent)
	whole := encodeTestJPEG(t, 64, 48)

	for _, f := range []struct {
		name  string
		data  []byte
		stamp frameTimestamp
	}{
		{"first whole frame", whole, frameTimestamp{auAligned: true, sequence: 1}},
		{"truncated by the camera", whole[:len(whole)-2], frameTimestamp{auAligned: true, sequence: 2}},
		{"truncated by the size cap", whole, frameTimestamp{auAligned: false, sequence: 3}},
		{"flagged as damaged by the driver", whole, frameTimestamp{auAligned: true, sequence: 4, nativeFlags: v4l2BufFlagError}},
		{"whole frame padded with zeros", append(append([]byte{}, whole...), make([]byte, 512)...), frameTimestamp{auAligned: true, sequence: 5}},
	} {
		if !frames.emit(f.data, f.stamp) {
			t.Fatalf("%s stopped the capture", f.name)
		}
	}

	if len(sent) != 2 {
		t.Fatalf("broadcast %d frames, want the 2 usable ones", len(sent))
	}
	for i, wantSequence := range []uint32{1, 5} {
		got := sent[i]
		if got.codec != videoCodecMJPEG || !got.stamp.auAligned || got.stamp.sequence != wantSequence || !bytes.Equal(got.data, whole) {
			t.Errorf("frame %d: codec=%d auAligned=%v sequence=%d len=%d, want JPEG codec, aligned, sequence %d, exactly the image",
				i, int32(got.codec), got.stamp.auAligned, got.stamp.sequence, len(got.data), wantSequence)
		}
	}
	if err := frames.captureResult(nil); err != nil {
		t.Errorf("a capture that delivered frames ended with %v", err)
	}
}

func TestNativeJPEGFramesStopsWhenNoSubscriberRemains(t *testing.T) {
	frames := &nativeJPEGFrames{logger: zap.NewNop(), broadcast: func([]byte, frameTimestamp, agentpb.VideoCodec) bool { return false }}
	if frames.emit(encodeTestJPEG(t, 64, 48), frameTimestamp{auAligned: true}) {
		t.Fatal("capture must stop when broadcast reports no subscribers")
	}
	if err := frames.captureResult(nil); err != nil {
		t.Errorf("a capture that ran out of subscribers ended with %v", err)
	}
}

func TestNativeJPEGFramesGivesUpWhenTheFirstBuffersAreAllUnusable(t *testing.T) {
	var sent []broadcastFrame
	frames := recordingNativeJPEGFrames(&sent)
	notJPEG := []byte("YUYV bytes from a camera that lied about its format")

	for i := 1; i < nativeJPEGUnusableBuffersBeforeNotSupported; i++ {
		if !frames.emit(notJPEG, frameTimestamp{auAligned: true}) {
			t.Fatalf("capture gave up after %d unusable buffers, want %d", i, nativeJPEGUnusableBuffersBeforeNotSupported)
		}
	}
	if frames.emit(notJPEG, frameTimestamp{auAligned: true}) {
		t.Fatalf("capture kept running after %d unusable buffers and no frame", nativeJPEGUnusableBuffersBeforeNotSupported)
	}
	if len(sent) != 0 {
		t.Fatalf("broadcast %d unusable buffers", len(sent))
	}

	err := fmt.Errorf("capture: %w", frames.captureResult(nil))
	var notSupported nativeJPEGNotSupported
	if !errors.As(err, &notSupported) {
		t.Fatalf("a camera with no usable frame must be a nativeJPEGNotSupported, got %T: %v", err, err)
	}
	if !strings.Contains(notSupported.Error(), "not a JPEG image") {
		t.Errorf("the error does not say why the buffers were unusable: %q", notSupported)
	}
}

func TestNativeJPEGFramesKeepsCapturingThroughBadBuffersAfterAGoodFrame(t *testing.T) {
	var sent []broadcastFrame
	frames := recordingNativeJPEGFrames(&sent)
	whole := encodeTestJPEG(t, 64, 48)

	for i := 1; i < nativeJPEGUnusableBuffersBeforeNotSupported; i++ {
		frames.emit(whole[:len(whole)-2], frameTimestamp{auAligned: true})
	}
	if !frames.emit(whole, frameTimestamp{auAligned: true}) {
		t.Fatal("the first good frame stopped the capture")
	}
	for i := 0; i < 3*nativeJPEGUnusableBuffersBeforeNotSupported; i++ {
		if !frames.emit(whole[:len(whole)-2], frameTimestamp{auAligned: true}) {
			t.Fatalf("bad buffer %d after a good frame stopped the capture", i+1)
		}
	}
	if len(sent) != 1 {
		t.Fatalf("broadcast %d frames, want the 1 good one", len(sent))
	}
	if err := frames.captureResult(nil); err != nil {
		t.Errorf("a capture that delivered a frame ended with %v", err)
	}
}

func TestNativeJPEGFramesCaptureResultKeepsTheCaptureError(t *testing.T) {
	frames := recordingNativeJPEGFrames(new([]broadcastFrame))
	for i := 0; i < nativeJPEGUnusableBuffersBeforeNotSupported; i++ {
		frames.emit(nil, frameTimestamp{auAligned: true})
	}
	if err := frames.captureResult(context.Canceled); !errors.Is(err, context.Canceled) {
		t.Errorf("captureResult = %v, want the capture's own error", err)
	}
}

func TestNativeJPEGCaptureConfig(t *testing.T) {
	cfg := nativeJPEGCaptureConfig(1280, 720, 30)
	if cfg.pixelFormat != v4l2PixFmtMJPEG || cfg.width != 1280 || cfg.height != 720 || cfg.framerate != 30 {
		t.Errorf("config = %s %dx%d@%d, want MJPG 1280x720@30", fourccName(cfg.pixelFormat), cfg.width, cfg.height, cfg.framerate)
	}
	if !cfg.exactSize || !cfg.setFrameInterval || cfg.tuneKeyframes {
		t.Errorf("exactSize=%v setFrameInterval=%v tuneKeyframes=%v, want true true false", cfg.exactSize, cfg.setFrameInterval, cfg.tuneKeyframes)
	}

	err := fmt.Errorf("capture: %w", cfg.formatUnsupported("VIDIOC_S_FMT MJPG rejected"))
	var notSupported nativeJPEGNotSupported
	if !errors.As(err, &notSupported) {
		t.Fatalf("a refused format must be a nativeJPEGNotSupported, got %T", err)
	}
	if !strings.Contains(notSupported.Error(), "MJPG rejected") {
		t.Errorf("message lost: %q", notSupported)
	}
}

func TestCaptureNativeJPEGRefusesAMissingDevice(t *testing.T) {
	s := &VideoService{logger: zap.NewNop()}
	err := s.captureNativeJPEG(context.Background(), func([]byte, frameTimestamp, agentpb.VideoCodec) bool {
		t.Fatal("no frame can exist")
		return false
	}, filepath.Join(t.TempDir(), "video99"), 1280, 720, 30)
	if err == nil {
		t.Fatal("capture from a nonexistent node succeeded")
	}
	if errors.As(err, new(nativeJPEGNotSupported)) {
		t.Errorf("a node that cannot be opened is not a camera without JPEG: %v", err)
	}
}

func TestCaptureNativeJPEGRefusesAnUnchosenSize(t *testing.T) {
	s := &VideoService{logger: zap.NewNop()}
	for _, size := range [][2]uint32{{0, 0}, {1280, 0}, {0, 720}} {
		err := s.captureNativeJPEG(context.Background(), func([]byte, frameTimestamp, agentpb.VideoCodec) bool {
			t.Fatal("no frame can exist")
			return false
		}, "/dev/video4", size[0], size[1], 30)
		if err == nil || !strings.Contains(err.Error(), "nativeJPEGCaptureSize") {
			t.Errorf("%dx%d: error %v does not send the caller to nativeJPEGCaptureSize", size[0], size[1], err)
		}
		if errors.As(err, new(nativeJPEGNotSupported)) {
			t.Errorf("%dx%d: a caller's missing size is not a camera without JPEG: %v", size[0], size[1], err)
		}
	}
}

type jpegSelectionCamera struct {
	defaultSize [2]uint32
	jpegSizes   [][2]uint32
	yuyvSizes   [][2]uint32
	thermal     *agentpb.ThermalCamera
}

func withJPEGSelectionCamera(t *testing.T, cam jpegSelectionCamera) (jpegProbes *[][2]uint32) {
	t.Helper()
	withYUYVModes(t, cam.yuyvSizes)
	prevDefault, prevJPEG, prevThermal := bestDefaultFrameSizeForDevice, deviceSupportsMJPEGSize, thermalCaptureProfile
	t.Cleanup(func() {
		bestDefaultFrameSizeForDevice, deviceSupportsMJPEGSize, thermalCaptureProfile = prevDefault, prevJPEG, prevThermal
	})
	bestDefaultFrameSizeForDevice = func(string) (uint32, uint32) { return cam.defaultSize[0], cam.defaultSize[1] }
	thermalCaptureProfile = func(string) *agentpb.ThermalCamera { return cam.thermal }
	jpegProbes = new([][2]uint32)
	deviceSupportsMJPEGSize = func(_ string, w, h uint32) bool {
		*jpegProbes = append(*jpegProbes, [2]uint32{w, h})
		for _, size := range cam.jpegSizes {
			if size == [2]uint32{w, h} {
				return true
			}
		}
		return false
	}
	return jpegProbes
}

func TestNativeJPEGCaptureSize(t *testing.T) {
	webcam := jpegSelectionCamera{defaultSize: [2]uint32{1280, 720}, jpegSizes: [][2]uint32{{1280, 720}, {640, 480}}}
	thermalModule := jpegSelectionCamera{defaultSize: [2]uint32{256, 192}, jpegSizes: [][2]uint32{{256, 192}, {640, 480}},
		thermal: &agentpb.ThermalCamera{CaptureWidth: 256, CaptureHeight: 384}}
	cases := []struct {
		name     string
		cam      jpegSelectionCamera
		req      *agentpb.StreamVideoRequest
		wantSize [2]uint32
		wantOK   bool
	}{
		{"requested size offered as JPEG", webcam, &agentpb.StreamVideoRequest{Width: 640, Height: 480}, [2]uint32{640, 480}, true},
		{"requested size not offered as JPEG", webcam, &agentpb.StreamVideoRequest{Width: 1920, Height: 1080}, [2]uint32{}, false},
		{"no size requested uses the device default", webcam, &agentpb.StreamVideoRequest{}, [2]uint32{1280, 720}, true},
		{"device default not offered as JPEG", jpegSelectionCamera{defaultSize: [2]uint32{1280, 720}, jpegSizes: [][2]uint32{{640, 480}}},
			&agentpb.StreamVideoRequest{}, [2]uint32{}, false},
		{"no size requested and no device default", jpegSelectionCamera{jpegSizes: [][2]uint32{{1280, 720}}},
			&agentpb.StreamVideoRequest{}, [2]uint32{}, false},
		{"default promoted to a stacked mode that is not JPEG",
			jpegSelectionCamera{defaultSize: [2]uint32{512, 384}, jpegSizes: [][2]uint32{{512, 384}}, yuyvSizes: [][2]uint32{{512, 384}, {512, 484}}},
			&agentpb.StreamVideoRequest{}, [2]uint32{}, false},
		{"raw request at a JPEG size", webcam, &agentpb.StreamVideoRequest{Codec: agentpb.VideoCodec_VIDEO_CODEC_RAW, Width: 640, Height: 480}, [2]uint32{}, false},
		{"raw request at the device default", webcam, &agentpb.StreamVideoRequest{Codec: agentpb.VideoCodec_VIDEO_CODEC_RAW}, [2]uint32{}, false},
		{"VP8 request at a JPEG size", webcam, &agentpb.StreamVideoRequest{Codec: agentpb.VideoCodec_VIDEO_CODEC_VP8, Width: 640, Height: 480}, [2]uint32{640, 480}, true},
		{"thermal module at its default", thermalModule, &agentpb.StreamVideoRequest{}, [2]uint32{}, false},
		{"thermal module at a requested JPEG size", thermalModule, &agentpb.StreamVideoRequest{Width: 640, Height: 480}, [2]uint32{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withJPEGSelectionCamera(t, c.cam)
			w, h, ok := nativeJPEGCaptureSize("/dev/video4", c.req)
			if ok != c.wantOK || [2]uint32{w, h} != c.wantSize {
				t.Errorf("nativeJPEGCaptureSize = %dx%d %v, want %dx%d %v", w, h, ok, c.wantSize[0], c.wantSize[1], c.wantOK)
			}
		})
	}
}

func TestNativeJPEGCaptureSizeDoesNotProbeAnUnknownSize(t *testing.T) {
	probes := withJPEGSelectionCamera(t, jpegSelectionCamera{jpegSizes: [][2]uint32{{0, 0}}})
	if _, _, ok := nativeJPEGCaptureSize("/dev/video4", &agentpb.StreamVideoRequest{}); ok {
		t.Fatal("a camera with no known size cannot be captured as JPEG")
	}
	if len(*probes) != 0 {
		t.Errorf("probed the device for JPEG at %v without a size", *probes)
	}
}

func TestPlannerJPEGChoiceIsUnchangedBySharingTheSelectionRule(t *testing.T) {
	webcam := jpegSelectionCamera{defaultSize: [2]uint32{1280, 720}, jpegSizes: [][2]uint32{{1280, 720}}}
	stacked := jpegSelectionCamera{defaultSize: [2]uint32{512, 384}, jpegSizes: [][2]uint32{{512, 384}}, yuyvSizes: [][2]uint32{{512, 384}, {512, 484}}}
	thermalModule := jpegSelectionCamera{defaultSize: [2]uint32{256, 192}, jpegSizes: [][2]uint32{{256, 192}},
		thermal: &agentpb.ThermalCamera{CaptureWidth: 256, CaptureHeight: 384}}
	hd := &agentpb.StreamVideoRequest{Width: 1280, Height: 720}
	const noJPEG = ""
	cases := []struct {
		name         string
		cam          jpegSelectionCamera
		req          *agentpb.StreamVideoRequest
		available    map[string]bool
		wantJPEGCaps string
		wantProbes   [][2]uint32
		// nativeAgrees is false only where the planner's answer rests on a
		// GStreamer element the native capture does not use.
		nativeAgrees bool
	}{
		{"webcam at a JPEG size", webcam, hd,
			map[string]bool{"jpegdec": true}, "image/jpeg,width=1280,height=720", [][2]uint32{{1280, 720}}, true},
		{"webcam at its default size", webcam, &agentpb.StreamVideoRequest{},
			map[string]bool{"jpegdec": true}, "image/jpeg,width=1280,height=720", [][2]uint32{{1280, 720}}, true},
		{"webcam at a size without JPEG", webcam, &agentpb.StreamVideoRequest{Width: 640, Height: 480},
			map[string]bool{"jpegdec": true}, noJPEG, [][2]uint32{{640, 480}}, true},
		{"webcam with no known size", jpegSelectionCamera{jpegSizes: [][2]uint32{{1280, 720}}}, &agentpb.StreamVideoRequest{},
			map[string]bool{"jpegdec": true}, noJPEG, nil, true},
		{"webcam without jpegdec", webcam, hd,
			map[string]bool{"x264enc": true}, noJPEG, nil, false},
		{"stacked default with videocrop", stacked, &agentpb.StreamVideoRequest{},
			map[string]bool{"jpegdec": true, "videocrop": true}, noJPEG, [][2]uint32{{512, 484}}, true},
		{"thermal profile default with videocrop", thermalModule, &agentpb.StreamVideoRequest{},
			map[string]bool{"jpegdec": true, "videocrop": true}, noJPEG, [][2]uint32{{256, 384}}, true},
		{"thermal profile default without videocrop", thermalModule, &agentpb.StreamVideoRequest{},
			map[string]bool{"jpegdec": true}, "image/jpeg,width=256,height=192", [][2]uint32{{256, 192}}, false},
		{"stacked default without videocrop", stacked, &agentpb.StreamVideoRequest{},
			map[string]bool{"jpegdec": true}, "image/jpeg,width=512,height=384", [][2]uint32{{512, 384}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			probes := withJPEGSelectionCamera(t, c.cam)
			plan, err := planGStreamerPipeline("gst", "/dev/video4", c.req, "x264enc", true, camera.TransportUSB, "", pipeWireSource{}, c.available)
			if err != nil {
				t.Fatalf("planGStreamerPipeline: %v", err)
			}
			joined := strings.Join(plan.args, " ")
			if c.wantJPEGCaps == noJPEG {
				if strings.Contains(joined, "image/jpeg") {
					t.Errorf("planner captured JPEG: %s", joined)
				}
			} else if !strings.Contains(joined, c.wantJPEGCaps) {
				t.Errorf("planner did not capture %q: %s", c.wantJPEGCaps, joined)
			}
			if fmt.Sprint(*probes) != fmt.Sprint(c.wantProbes) {
				t.Errorf("planner probed JPEG at %v, want %v", *probes, c.wantProbes)
			}

			_, _, nativeOK := nativeJPEGCaptureSize("/dev/video4", c.req)
			if plannerOK := c.wantJPEGCaps != noJPEG; (nativeOK == plannerOK) != c.nativeAgrees {
				t.Errorf("native JPEG = %v, planner JPEG = %v, want agreement = %v", nativeOK, plannerOK, c.nativeAgrees)
			}
		})
	}
}

func TestPlannerNeverCapturesJPEGFromANonV4L2Source(t *testing.T) {
	hd := &agentpb.StreamVideoRequest{Width: 1280, Height: 720}
	pipewire := pipewireElements()
	pipewire["jpegdec"] = true
	cases := []struct {
		name      string
		transport camera.Transport
		pw        pipeWireSource
		available map[string]bool
	}{
		{"libcamerasrc", camera.TransportCSI, pipeWireSource{}, map[string]bool{"libcamerasrc": true, "jpegdec": true}},
		{"pipewiresrc", camera.TransportUSB, pipeWireSource{serial: testPWSerial}, pipewire},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			probes := withJPEGSelectionCamera(t, jpegSelectionCamera{defaultSize: [2]uint32{1280, 720}, jpegSizes: [][2]uint32{{1280, 720}}})
			plan, err := planGStreamerPipeline("gst", "/dev/video4", hd, "x264enc", true, c.transport, "", c.pw, c.available)
			if err != nil {
				t.Fatalf("planGStreamerPipeline: %v", err)
			}
			if joined := strings.Join(plan.args, " "); strings.Contains(joined, "image/jpeg") {
				t.Errorf("planner captured JPEG: %s", joined)
			}
			if len(*probes) != 0 {
				t.Errorf("planner probed the device for JPEG at %v", *probes)
			}
		})
	}
}
