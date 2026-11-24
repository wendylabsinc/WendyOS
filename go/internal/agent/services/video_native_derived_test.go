package services

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// fakeV4L2 stands in for the kernel's side of a camera that has no H.264.
type fakeV4L2 struct {
	// jpeg captures JPEG; nil delivers a whole frame every 10 ms until ctx ends.
	jpeg func(ctx context.Context, emit func([]byte, frameTimestamp) bool, cfg v4l2CaptureConfig) error

	frame []byte
	mu    sync.Mutex
	asked []v4l2CaptureConfig
}

func (f *fakeV4L2) capture(ctx context.Context, emit func([]byte, frameTimestamp) bool, _ string, cfg v4l2CaptureConfig) error {
	f.mu.Lock()
	f.asked = append(f.asked, cfg)
	f.mu.Unlock()
	if cfg.pixelFormat != v4l2PixFmtMJPEG {
		return cfg.formatUnsupported("VIDIOC_S_FMT H264 rejected: invalid argument")
	}
	if f.jpeg != nil {
		return f.jpeg(ctx, emit, cfg)
	}
	for sequence := uint32(1); ; sequence++ {
		stamp := realtimeFrameTimestamp(time.Now())
		stamp.sequence, stamp.sequenceValid, stamp.auAligned = sequence, true, true
		if !emit(f.frame, stamp) {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func withFrameRates(t *testing.T, byFormat map[uint32][]uint32) {
	t.Helper()
	prev := frameRatesFor
	frameRatesFor = func(_ string, pixfmt, _, _ uint32) []uint32 { return byFormat[pixfmt] }
	t.Cleanup(func() { frameRatesFor = prev })
}

func (f *fakeV4L2) formatsAsked() []uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var formats []uint32
	for _, cfg := range f.asked {
		formats = append(formats, cfg.pixelFormat)
	}
	return formats
}

type fakeLocalCameraCapture struct {
	hub  *deviceHub
	v4l2 *fakeV4L2
	stop func() error
}

// captureFakeLocalCamera runs captureLocalCamera for a hub the way runProducer
// does, once subscribe has attached the test's subscribers.
func captureFakeLocalCamera(t *testing.T, v4l2 *fakeV4L2, req *agentpb.StreamVideoRequest, subscribe func(h *deviceHub)) fakeLocalCameraCapture {
	t.Helper()
	return captureFakeLocalCameraWith(t, &VideoService{logger: zap.NewNop(), encoderIdleGrace: time.Second}, v4l2, req, subscribe)
}

func captureFakeLocalCameraWith(t *testing.T, svc *VideoService, v4l2 *fakeV4L2, req *agentpb.StreamVideoRequest, subscribe func(h *deviceHub)) fakeLocalCameraCapture {
	t.Helper()
	if v4l2.frame == nil {
		v4l2.frame = encodeTestJPEG(t, 64, 48)
	}
	h, cancelHub := newTestHub(t)
	t.Cleanup(cancelHub)
	h.sampleSeq = new(atomic.Uint64)
	subscribe(h)
	svc.captureV4L2 = v4l2.capture
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- svc.captureLocalCamera(ctx, h.produceCaptured, "/dev/video4", req, camera.TransportUSB, "", h)
	}()
	var once sync.Once
	var err error
	stop := func() error {
		once.Do(func() {
			cancel()
			select {
			case err = <-result:
			case <-time.After(10 * time.Second):
				t.Error("captureLocalCamera did not return after its ctx was cancelled")
			}
		})
		return err
	}
	t.Cleanup(func() { stop() }) //nolint:errcheck
	return fakeLocalCameraCapture{hub: h, v4l2: v4l2, stop: stop}
}

var jpegWebcam = jpegSelectionCamera{defaultSize: [2]uint32{640, 480}, jpegSizes: [][2]uint32{{640, 480}}}

func waitForRefusal(t *testing.T, h *deviceHub, id int, ch chan *videoFrame, what string) error {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, open := <-ch:
			if !open {
				return h.subscriberErr(id)
			}
		case <-deadline:
			t.Fatalf("the %s subscriber was left waiting", what)
			return nil
		}
	}
}

func TestCaptureLocalCamera_JPEGWebcamIsCapturedWholeWithTheDerivedLayer(t *testing.T) {
	withJPEGSelectionCamera(t, jpegWebcam)
	withFrameRates(t, map[uint32][]uint32{v4l2PixFmtMJPEG: {30, 60, 15}, v4l2PixFmtYUYV: {5}})
	launchLog := fakeGStreamer(t, 0, derivedLayerElements...)
	var idNative, idRaw, idPixels, idViewer int
	var native, raw, pixels, viewer chan *videoFrame
	capture := captureFakeLocalCamera(t, &fakeV4L2{}, &agentpb.StreamVideoRequest{}, func(h *deviceHub) {
		h.nativeNotOffered(fixedReason("an earlier start of this hub had no native frames"))
		idRaw, raw, _ = h.subscribeKind(rawFrames)
		idPixels, pixels, _ = h.subscribePixels()
	})
	h := capture.hub
	defer h.unsubscribe(idRaw)
	defer h.unsubscribe(idPixels)
	waitUntil(t, "the native capture offers native frames again", func() bool {
		var err error
		idNative, native, err = h.subscribeKind(nativeFrames)
		return err == nil
	})
	defer h.unsubscribe(idNative)

	frame := waitForFrame(t, native, "native JPEG frame")
	if frame.codec != videoCodecMJPEG || !frame.native || !frame.auAligned || frame.sampleID == 0 || !reflect.DeepEqual(frame.data, capture.v4l2.frame) {
		t.Fatalf("native subscriber got codec=%d native=%v auAligned=%v sample=%d, %d bytes; want the camera's whole JPEG frame, numbered",
			int32(frame.codec), frame.native, frame.auAligned, frame.sampleID, len(frame.data))
	}
	refusal := waitForRefusal(t, h, idRaw, raw, "raw")
	if want := "camera is captured as MJPEG at 640x480; raw frames are not offered"; refusal == nil || !strings.Contains(refusal.Error(), want) {
		t.Fatalf("raw refusal = %v, want today's reason %q", refusal, want)
	}
	if err := h.subscriberErr(idPixels); err != nil || len(pixels) != 0 {
		t.Fatalf("pixel subscriber: err=%v, %d frames from a child that wrote none", err, len(pixels))
	}

	if launched := waitForLaunches(t, launchLog, 1); launched[0] != derivedPicturesOnly {
		t.Fatalf("child pipeline\n got %q\nwant %q", launched[0], derivedPicturesOnly)
	}
	if asked := capture.v4l2.formatsAsked(); !reflect.DeepEqual(asked, []uint32{v4l2PixFmtH264, v4l2PixFmtMJPEG}) {
		t.Fatalf("capture formats asked of the camera = %v, want H.264 then JPEG", asked)
	}
	if cfg := capture.v4l2.asked[1]; cfg.width != 640 || cfg.height != 480 || !cfg.exactSize {
		t.Errorf("JPEG capture asked for %dx%d exact=%v, want exactly 640x480", cfg.width, cfg.height, cfg.exactSize)
	} else if cfg.framerate != 60 || !cfg.setFrameInterval {
		t.Errorf("a request without a rate captures at %d fps (set=%v), want the camera's best for JPEG at this size, 60", cfg.framerate, cfg.setFrameInterval)
	} else if cfg.stallLimit != cameraIdleTimeout {
		t.Errorf("JPEG capture stall limit = %v, want %v", cfg.stallLimit, cameraIdleTimeout)
	}
	if cfg := capture.v4l2.asked[0]; cfg.stallLimit != 0 {
		t.Errorf("native H.264 capture got a stall limit of %v; it has none", cfg.stallLimit)
	}

	idViewer, viewer, _ = h.subscribe()
	defer h.unsubscribe(idViewer)
	idLate, late, err := h.subscribeKind(nativeFrames)
	if err != nil {
		t.Fatalf("a native subscriber joining a native capture was refused: %v", err)
	}
	defer h.unsubscribe(idLate)
	waitForFrame(t, late, "native JPEG frame for a late subscriber")
	select {
	case got := <-viewer:
		t.Fatalf("an encoded subscriber received codec %d from a camera whose child produced nothing", int32(got.codec))
	case <-time.After(100 * time.Millisecond):
	}
	for _, pipeline := range launchedPipelines(t, launchLog) {
		if strings.Contains(pipeline, "v4l2src") {
			t.Fatalf("GStreamer opened the camera although it is captured natively: %q", pipeline)
		}
	}
}

func TestCaptureLocalCamera_FallsBackToGStreamer(t *testing.T) {
	jpegNotSupported := func(_ context.Context, _ func([]byte, frameTimestamp) bool, cfg v4l2CaptureConfig) error {
		return cfg.formatUnsupported("VIDIOC_S_FMT MJPG rejected: invalid argument")
	}
	// The planner's own choice for this module stays whatever it is today; the
	// point is that native JPEG capture is never tried on it.
	thermalModule := jpegSelectionCamera{defaultSize: [2]uint32{256, 192}, jpegSizes: [][2]uint32{{256, 192}},
		yuyvSizes: [][2]uint32{{256, 192}, {256, 384}}, thermal: &agentpb.ThermalCamera{CaptureWidth: 256, CaptureHeight: 384}}
	yuyvToo := jpegSelectionCamera{defaultSize: [2]uint32{640, 480}, jpegSizes: [][2]uint32{{640, 480}}, yuyvSizes: [][2]uint32{{640, 480}}}

	for name, c := range map[string]struct {
		cam         jpegSelectionCamera
		elements    []string
		jpeg        func(context.Context, func([]byte, frameTimestamp) bool, v4l2CaptureConfig) error
		req         *agentpb.StreamVideoRequest
		wantAsked   []uint32
		wantWhy     string
		wantCapture string
	}{
		"the derived layer is unavailable": {jpegWebcam, []string{"jpegdec"}, nil, &agentpb.StreamVideoRequest{},
			[]uint32{v4l2PixFmtH264}, "GStreamer lacks gdpdepay, gdppay", "image/jpeg,width=640,height=480"},
		"native JPEG capture is not supported": {jpegWebcam, derivedLayerElements, jpegNotSupported, &agentpb.StreamVideoRequest{},
			[]uint32{v4l2PixFmtH264, v4l2PixFmtMJPEG}, "VIDIOC_S_FMT MJPG rejected", "image/jpeg,width=640,height=480"},
		"a thermal camera": {thermalModule, derivedLayerElements, nil, &agentpb.StreamVideoRequest{},
			[]uint32{v4l2PixFmtH264}, "thermal cameras are captured uncompressed", "image/jpeg,width=256,height=192"},
		"a raw frame request": {yuyvToo, derivedLayerElements, nil,
			&agentpb.StreamVideoRequest{Codec: agentpb.VideoCodec_VIDEO_CODEC_RAW, Width: 640, Height: 480},
			[]uint32{v4l2PixFmtH264}, "raw frame request", "image/jpeg,width=640,height=480"},
	} {
		t.Run(name, func(t *testing.T) {
			withJPEGSelectionCamera(t, c.cam)
			launchLog := fakeGStreamer(t, 0, c.elements...)
			var idNative int
			var native chan *videoFrame
			capture := captureFakeLocalCamera(t, &fakeV4L2{jpeg: c.jpeg}, c.req, func(h *deviceHub) {
				idNative, native, _ = h.subscribeKind(nativeFrames)
			})
			defer capture.hub.unsubscribe(idNative)

			refusal := waitForRefusal(t, capture.hub, idNative, native, "native")
			if status.Code(refusal) != codes.FailedPrecondition || !strings.Contains(refusal.Error(), c.wantWhy) {
				t.Fatalf("native refusal = %v, want FailedPrecondition naming %q", refusal, c.wantWhy)
			}
			launched := waitForLaunches(t, launchLog, 1)
			if !strings.Contains(launched[0], "v4l2src device=/dev/video4 ! "+c.wantCapture) {
				t.Fatalf("pipeline %q, want today's GStreamer capture of the camera as %s", launched[0], c.wantCapture)
			}
			if strings.Contains(launched[0], "gdp") {
				t.Fatalf("the GStreamer capture runs the derived layer's framing: %q", launched[0])
			}
			if asked := capture.v4l2.formatsAsked(); !reflect.DeepEqual(asked, c.wantAsked) {
				t.Errorf("capture formats asked of the camera = %v, want %v", asked, c.wantAsked)
			}
			if _, _, err := capture.hub.subscribeKind(nativeFrames); err == nil || !strings.Contains(err.Error(), c.wantWhy) {
				t.Errorf("a native subscriber joining later got %v, want the same refusal up front", err)
			}
		})
	}
}

func TestCaptureLocalCamera_ALayerThatGivesUpFallsBackToGStreamerAndIsRemembered(t *testing.T) {
	withJPEGSelectionCamera(t, jpegWebcam)
	launchLog := fakeGStreamerScript(t, "case \"$*\" in\n*gdpdepay*) echo 'no element \"gdpdepay\"' >&2; exit 1 ;;\nesac\nexec /bin/sleep 60\n",
		derivedLayerElements...)
	svc := &VideoService{logger: zap.NewNop(), encoderIdleGrace: time.Second}

	var idNative, idViewer int
	var native chan *videoFrame
	first := captureFakeLocalCameraWith(t, svc, &fakeV4L2{}, &agentpb.StreamVideoRequest{}, func(h *deviceHub) {
		idNative, native, _ = h.subscribeKind(nativeFrames)
		idViewer, _, _ = h.subscribe()
	})
	defer first.hub.unsubscribe(idNative)
	defer first.hub.unsubscribe(idViewer)

	launched := waitForLaunches(t, launchLog, derivedFailedStartsBeforeGivingUp+1)
	for i, pipeline := range launched[:derivedFailedStartsBeforeGivingUp] {
		if !strings.Contains(pipeline, "gdpdepay") {
			t.Fatalf("launch %d is not the derived layer's child: %q", i, pipeline)
		}
	}
	if fallback := launched[derivedFailedStartsBeforeGivingUp]; !strings.Contains(fallback, "v4l2src device=/dev/video4 ! image/jpeg,width=640,height=480") || !strings.Contains(fallback, "x264enc") {
		t.Fatalf("after the layer gave up the hub's viewer got %q, want today's GStreamer capture with the encoder", fallback)
	}
	refusal := waitForRefusal(t, first.hub, idNative, native, "native")
	if refusal == nil || !strings.Contains(refusal.Error(), `no element "gdpdepay"`) {
		t.Fatalf("native refusal = %v, want the reason the layer gave up", refusal)
	}
	if err := first.hub.terminalErr(); err != nil {
		t.Fatalf("the hub ended with %v instead of falling back", err)
	}
	if err := first.stop(); !errors.Is(err, context.Canceled) {
		t.Fatalf("the fallback capture ended with %v before it was cancelled", err)
	}

	later := captureFakeLocalCameraWith(t, svc, &fakeV4L2{}, &agentpb.StreamVideoRequest{}, func(h *deviceHub) {
		idViewer, _, _ = h.subscribe()
	})
	defer later.hub.unsubscribe(idViewer)
	launched = waitForLaunches(t, launchLog, derivedFailedStartsBeforeGivingUp+2)
	if straight := launched[len(launched)-1]; !strings.Contains(straight, "v4l2src device=/dev/video4") {
		t.Fatalf("a later hub paid for the layer's failure again: %q", straight)
	}
	if asked := later.v4l2.formatsAsked(); !reflect.DeepEqual(asked, []uint32{v4l2PixFmtH264}) {
		t.Errorf("a later hub asked the camera for %v, want H.264 alone: JPEG capture must not be tried again yet", asked)
	}
}

func TestNativeJPEGSource_ALayerThatGaveUpAfterPublishingEndsTheCaptureInstead(t *testing.T) {
	for _, afterPublishing := range []bool{false, true} {
		svc := &VideoService{logger: zap.NewNop()}
		cam := localCamera{path: "/dev/video4"}
		why, gaveWay := nativeJPEGSource.gaveWay(svc, cam, derivedLayerGaveUp{msg: "encoder stopped", afterPublishing: afterPublishing})
		if gaveWay == afterPublishing || why != "encoder stopped" {
			t.Errorf("afterPublishing=%v: gaveWay = %q, %v; whole access units must not be followed by chunks in one stream", afterPublishing, why, gaveWay)
		}
		if _, failed := svc.derivedFailures.recent("/dev/video4"); !failed {
			t.Errorf("afterPublishing=%v: the camera is not remembered, so the next hub would try the layer again", afterPublishing)
		}
	}
	svc := &VideoService{logger: zap.NewNop()}
	if _, gaveWay := nativeJPEGSource.gaveWay(svc, localCamera{path: "/dev/video4"}, errCameraInUse("/dev/video4")); gaveWay {
		t.Error("a busy camera was handed to GStreamer as if JPEG capture were unsupported")
	}
	if _, failed := svc.derivedFailures.recent("/dev/video4"); failed {
		t.Error("a capture error was remembered as a layer failure")
	}
}

func TestDerivedLayerFailures_AreForgottenAfterABoundedTime(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	failures := derivedLayerFailures{now: func() time.Time { return now }}
	if _, failed := failures.recent("/dev/video4"); failed {
		t.Fatal("a camera with no failure is remembered as failed")
	}
	failures.remember("/dev/video4", "encoder produced nothing")
	if why, failed := failures.recent("/dev/video4"); !failed || why != "encoder produced nothing" {
		t.Fatalf("recent = %q, %v, want the remembered reason", why, failed)
	}
	if _, failed := failures.recent("/dev/video6"); failed {
		t.Fatal("one camera's failure was charged to another")
	}
	now = now.Add(derivedLayerRetryAfter - time.Second)
	if _, failed := failures.recent("/dev/video4"); !failed {
		t.Fatal("the failure was forgotten before its time")
	}
	now = now.Add(time.Second)
	if _, failed := failures.recent("/dev/video4"); failed {
		t.Fatalf("the failure is still remembered after %v", derivedLayerRetryAfter)
	}
}

func TestCaptureNatively_TriesSourcesInOrderAndSaysWhyNoneDelivered(t *testing.T) {
	if len(nativeCaptureSources) != 2 || nativeCaptureSources[0].format != "H.264" || nativeCaptureSources[1].format != "JPEG" {
		t.Fatalf("native capture sources = %d, want H.264 then JPEG", len(nativeCaptureSources))
	}
	prev := nativeCaptureSources
	t.Cleanup(func() { nativeCaptureSources = prev })
	var tried []string
	source := func(format, declined string, result error, gaveWay bool) nativeCaptureSource {
		return nativeCaptureSource{format: format,
			accept: func(*VideoService, localCamera) (func(context.Context) error, string) {
				if declined != "" {
					return nil, declined
				}
				return func(context.Context) error {
					tried = append(tried, format)
					return result
				}, ""
			},
			gaveWay: func(*VideoService, localCamera, error) (string, bool) { return "no frame came", gaveWay },
		}
	}
	delivered := errors.New("the capture's own result")

	nativeCaptureSources = []nativeCaptureSource{
		source("first", "", nil, true), source("second", "not for this camera", nil, false),
		source("third", "", delivered, false), source("fourth", "", nil, false),
	}
	why, err := (&VideoService{}).captureNatively(context.Background(), localCamera{})
	if why != "" || err != delivered || !reflect.DeepEqual(tried, []string{"first", "third"}) {
		t.Fatalf("captureNatively = %q, %v after trying %v; want the third source's result", why, err, tried)
	}

	nativeCaptureSources = nativeCaptureSources[:2]
	why, err = (&VideoService{}).captureNatively(context.Background(), localCamera{})
	if want := "first: no frame came; second: not for this camera"; why != want || err != nil {
		t.Fatalf("captureNatively = %q, %v; want %q", why, err, want)
	}
}

func TestNativeJPEGCapture_DeclinesSizesAboveItsLimit(t *testing.T) {
	big := jpegSelectionCamera{defaultSize: [2]uint32{1920, 1080}, jpegSizes: [][2]uint32{{1920, 1080}, {3840, 2160}, {2560, 1440}, {1080, 1920}}}
	withJPEGSelectionCamera(t, big)
	if w, h, declined := nativeJPEGCapture("/dev/video4", &agentpb.StreamVideoRequest{Width: 1920, Height: 1080}); declined != "" || w != 1920 || h != 1080 {
		t.Fatalf("1920x1080 = %dx%d declined %q, want it captured whole", w, h, declined)
	}
	for _, size := range [][2]uint32{{3840, 2160}, {2560, 1440}, {1080, 1920}} {
		_, _, declined := nativeJPEGCapture("/dev/video4", &agentpb.StreamVideoRequest{Width: size[0], Height: size[1]})
		if !strings.Contains(declined, "above the 1920x1080 limit") {
			t.Errorf("%dx%d declined with %q, want the size limit named", size[0], size[1], declined)
		}
		if _, _, ok := nativeJPEGCaptureSize("/dev/video4", &agentpb.StreamVideoRequest{Width: size[0], Height: size[1]}); ok {
			t.Errorf("%dx%d is chosen for whole-frame JPEG capture", size[0], size[1])
		}
	}
}

func TestNativeJPEGFrameRate_DefaultRequestRunsAtTheRateTheShareModeDeclares(t *testing.T) {
	modes := map[uint32][][2]uint32{v4l2PixFmtYUYV: {{1280, 720}}, v4l2PixFmtMJPEG: {{1280, 720}}}
	for _, rates := range [][]uint32{{15, 60, 30}, {5, 24, 15}, nil} {
		s := shareTestService(t, []uint32{v4l2PixFmtYUYV, v4l2PixFmtMJPEG}, modes, rates)
		shared := s.shareDecisionFor(context.Background(), usbCam())
		if !shared.Shared {
			t.Fatalf("not shared: %s", shared.Reason)
		}
		got := nativeJPEGFrameRate("/dev/video4", &agentpb.StreamVideoRequest{}, 1280, 720)
		if want := highestFrameRateFor("/dev/video4", v4l2PixFmtMJPEG, 1280, 720); got != want {
			t.Errorf("rates %v: capture runs at %d, want the highest advertised, %d", rates, got, want)
		}
		if len(rates) > 0 && got != shared.Mode.FPS {
			t.Errorf("rates %v: capture runs at %d but the share mode declares %d", rates, got, shared.Mode.FPS)
		}
		if len(rates) == 0 && got != 0 {
			t.Errorf("a camera that advertises no rate is captured at %d, want the driver's own", got)
		}
		if asked := nativeJPEGFrameRate("/dev/video4", &agentpb.StreamVideoRequest{Framerate: 10}, 1280, 720); asked != 10 {
			t.Errorf("rates %v: a request for 10 fps captures at %d", rates, asked)
		}
	}
}

func TestCaptureStall_EndsACaptureWhoseCameraWentQuiet(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	never := captureStall{lastFrame: start}
	if err := never.expired(start.Add(24 * time.Hour)); err != nil {
		t.Fatalf("a capture without a stall limit ended: %v", err)
	}

	stall := captureStall{limit: 10 * time.Second, lastFrame: start}
	if err := stall.expired(start.Add(10 * time.Second)); err != nil {
		t.Fatalf("ended at the limit, not after it: %v", err)
	}
	err := stall.expired(start.Add(10*time.Second + time.Millisecond))
	if status.Code(err) != codes.DeadlineExceeded || !strings.Contains(err.Error(), "camera produced no video within 10s") {
		t.Fatalf("a camera that never delivered ended with %v", err)
	}

	stall.frame(start.Add(5 * time.Second))
	if err := stall.expired(start.Add(15 * time.Second)); err != nil {
		t.Fatalf("a frame did not restart the limit: %v", err)
	}
	err = stall.expired(start.Add(15*time.Second + time.Millisecond))
	if status.Code(err) != codes.DeadlineExceeded || !strings.Contains(err.Error(), "camera stopped producing video for 10s") {
		t.Fatalf("a camera that stopped delivering ended with %v, want the GStreamer capture's wording", err)
	}
	if want := gstTimeoutError(true, cameraIdleTimeout, cameraIdleTimeout); nativeJPEGCaptureConfig(640, 480, 30).stallLimit != cameraIdleTimeout || err.Error() != want.Error() {
		t.Errorf("JPEG capture's stall is not the GStreamer capture's: limit %v, error %q, want %q",
			nativeJPEGCaptureConfig(640, 480, 30).stallLimit, err, want)
	}
}

func TestCaptureDeriving_ALayerThatGivesUpEndsTheCaptureWithItsError(t *testing.T) {
	fakeGStreamerScript(t, "echo 'no such encoder' >&2\nexit 1\n", derivedLayerElements...)
	h, cancelHub := newTestHub(t)
	defer cancelHub()
	id, _, _ := h.subscribe()
	defer h.unsubscribe(id)
	layer, _ := fakeDerivedLayer(t, h, time.Second)
	svc := &VideoService{logger: zap.NewNop()}

	captureEnded := make(chan struct{})
	err := svc.captureDeriving(context.Background(), h, layer, func(ctx context.Context) error {
		defer close(captureEnded)
		<-ctx.Done()
		return nil
	})
	select {
	case <-captureEnded:
	default:
		t.Fatal("captureDeriving returned while the capture was still running")
	}
	if !errors.As(err, new(derivedLayerGaveUp)) || !strings.Contains(err.Error(), "no such encoder") {
		t.Fatalf("capture ended with %v, want the layer's derivedLayerGaveUp carrying the child's output", err)
	}
	if h.derived.Load() != nil {
		t.Error("the hub still feeds a layer whose capture ended")
	}
}

func TestCaptureDeriving_ReturnsTheCapturesOwnResultAndStopsTheLayer(t *testing.T) {
	launchLog := fakeGStreamer(t, 0, derivedLayerElements...)
	h, cancelHub := newTestHub(t)
	defer cancelHub()
	id, _, _ := h.subscribePixels()
	defer h.unsubscribe(id)
	layer, _ := fakeDerivedLayer(t, h, time.Second)
	svc := &VideoService{logger: zap.NewNop()}

	notSupported := nativeJPEGNotSupported{msg: "no usable frame"}
	err := svc.captureDeriving(context.Background(), h, layer, func(context.Context) error {
		waitForLaunches(t, launchLog, 1)
		waitUntil(t, "the child takes frames", layer.feeding.Load)
		return notSupported
	})
	if !errors.Is(err, notSupported) {
		t.Fatalf("captureDeriving returned %v, want the capture's own error", err)
	}
	if layer.feeding.Load() {
		t.Error("the layer's child outlived the capture")
	}
}

func TestNativeJPEGFramesAreMarkedNative(t *testing.T) {
	var sent []broadcastFrame
	frames := recordingNativeJPEGFrames(&sent)
	frames.emit(encodeTestJPEG(t, 64, 48), frameTimestamp{auAligned: true})
	if len(sent) != 1 || !sent[0].stamp.native {
		t.Fatalf("a whole JPEG frame from native capture was broadcast %d times, native=%v", len(sent), len(sent) == 1 && sent[0].stamp.native)
	}
}

func TestDeviceHub_NativeOfferedLiftsAnEarlierStartsRefusal(t *testing.T) {
	h, cancel := newTestHub(t)
	defer cancel()
	h.nativeNotOffered(fixedReason("captured through GStreamer"))
	if _, _, err := h.subscribeKind(nativeFrames); err == nil {
		t.Fatal("a native subscriber joined a hub that refused native frames")
	}
	h.nativeOffered()
	id, _, err := h.subscribeKind(nativeFrames)
	if err != nil {
		t.Fatalf("after the hub restarted on a native path: %v", err)
	}
	h.unsubscribe(id)
}

// Each case prepares its fakes on the test's goroutine and returns the producer
// to run against the hub.
type nativeRefusalCase struct {
	why     string
	prepare func(t *testing.T) (produce func(ctx context.Context, h *deviceHub))
}

func TestNativeSubscriberIsTurnedAwayOnEveryPathWithoutNativeFrames(t *testing.T) {
	ignoreFrames := func([]byte, frameTimestamp, agentpb.VideoCodec) bool { return true }
	throughPipeline := func(t *testing.T, plan gstPipelinePlan, err error) func(context.Context, *deviceHub) {
		if err != nil {
			t.Fatal(err)
		}
		plan.args = []string{fakeGstLaunch(t, "")}
		return func(ctx context.Context, h *deviceHub) {
			svc := &VideoService{logger: zap.NewNop()}
			svc.runCameraPipeline(ctx, ignoreFrames, "/dev/video4", gstEncoderResult{element: "x264enc"}, plan, nil, h, nil) //nolint:errcheck
		}
	}
	throughGStreamer := func(t *testing.T, cam jpegSelectionCamera, extra ...string) func(context.Context, *deviceHub) {
		withJPEGSelectionCamera(t, cam)
		fakeGStreamer(t, 0, extra...)
		return func(ctx context.Context, h *deviceHub) {
			svc := &VideoService{logger: zap.NewNop()}
			svc.streamGStreamer(ctx, ignoreFrames, "/dev/video4", &agentpb.StreamVideoRequest{Width: 640, Height: 480}, //nolint:errcheck
				camera.TransportUSB, "", pipeWireSource{}, h)
		}
	}

	cases := map[string]nativeRefusalCase{
		"GStreamer capture of uncompressed frames": {gstEncodedNativeWhy, func(t *testing.T) func(context.Context, *deviceHub) {
			return throughGStreamer(t, jpegSelectionCamera{yuyvSizes: [][2]uint32{{640, 480}}})
		}},
		"GStreamer capture of JPEG": {gstDecodedNativeWhy, func(t *testing.T) func(context.Context, *deviceHub) {
			return throughGStreamer(t, jpegWebcam, "jpegdec")
		}},
		"PipeWire": {pipeWireNativeWhy, func(t *testing.T) func(context.Context, *deviceHub) {
			plan, err := planGStreamerPipeline("gst", "/dev/video4", &agentpb.StreamVideoRequest{}, "x264enc", true,
				camera.TransportUSB, "", pipeWireSource{serial: 42}, map[string]bool{"pipewiresrc": true})
			return throughPipeline(t, plan, err)
		}},
		"held-camera feed": {"another application holds the camera", func(t *testing.T) func(context.Context, *deviceHub) {
			plan, err := planSharedJPEGPipeline("gst", "/dev/video4", &agentpb.StreamVideoRequest{}, gstEncoderResult{element: "x264enc"},
				sharedCameraJPEG{width: 64, height: 48})
			return throughPipeline(t, plan, err)
		}},
		"SensorLink": {sensorLinkNativeWhy, func(t *testing.T) func(context.Context, *deviceHub) {
			fakeGStreamer(t, 0)
			feed := NewSensorFrameFeed()
			svc := &VideoService{logger: zap.NewNop(), sensorFrames: feed}
			writer := feed.WrapWriters(func(string) ros2camera.CameraWriter { return &recordingCameraWriter{} })("/dev/video96")
			frame := ros2camera.Frame{Data: sharedTestJPEG(t), Width: 64, Height: 48, Codec: ros2camera.CodecMJPEG}
			return func(ctx context.Context, h *deviceHub) {
				go func() {
					for ctx.Err() == nil {
						_ = writer.WriteFrame(frame)
						time.Sleep(10 * time.Millisecond)
					}
				}()
				svc.streamSensorLinkCamera(ctx, ignoreFrames, "/dev/video96", &agentpb.StreamVideoRequest{}, h) //nolint:errcheck
			}
		}},
		"CSI": {csiNativeWhy, func(t *testing.T) func(context.Context, *deviceHub) {
			fakeGStreamer(t, 0, "libcamerasrc")
			svc := &VideoService{logger: zap.NewNop(),
				classifyTransport:  func(string) (camera.Transport, string) { return camera.TransportCSI, "" },
				readDeviceName:     func(string) (string, error) { return "imx219", nil },
				enumerateLibcamera: func(context.Context) (map[string]string, error) { return nil, nil },
			}
			return func(ctx context.Context, h *deviceHub) {
				svc.captureNode(ctx, ignoreFrames, "/dev/video4", &agentpb.StreamVideoRequest{}, h) //nolint:errcheck
			}
		}},
		"network camera": {"network cameras are read as an RTSP byte stream", func(*testing.T) func(context.Context, *deviceHub) {
			svc := &VideoService{logger: zap.NewNop(), hubs: map[string]*deviceHub{}}
			return func(ctx context.Context, h *deviceHub) {
				svc.wg.Add(1)
				svc.runProducer(ctx, h, ipHubKeyPrefix+"200", &agentpb.StreamVideoRequest{})
			}
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h, cancelHub := newTestHub(t)
			defer cancelHub()
			idNative, native, err := h.subscribeKind(nativeFrames)
			if err != nil {
				t.Fatal(err)
			}
			defer h.unsubscribe(idNative)
			idViewer, _, _ := h.subscribe()
			defer h.unsubscribe(idViewer)

			produce := c.prepare(t)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				produce(ctx, h)
			}()
			defer func() {
				cancel()
				<-done
			}()

			refusal := waitForRefusal(t, h, idNative, native, "native")
			if status.Code(refusal) != codes.FailedPrecondition || !strings.Contains(refusal.Error(), c.why) {
				t.Fatalf("native refusal = %v, want FailedPrecondition naming %q", refusal, c.why)
			}
		})
	}
}

type codecRecordingStream struct {
	grpc.ServerStreamingServer[agentpb.VideoFrame]
	ctx  context.Context
	mu   sync.Mutex
	sent []agentpb.VideoCodec
}

func (s *codecRecordingStream) Context() context.Context { return s.ctx }

func (s *codecRecordingStream) Send(frame *agentpb.VideoFrame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, frame.GetCodec())
	return nil
}

func TestJPEGFramesNeverLeaveTheAgent(t *testing.T) {
	h, cancel := newTestHub(t)
	defer cancel()
	h.sampleSeq = new(atomic.Uint64)
	idViewer, viewer, _ := h.subscribe()
	defer h.unsubscribe(idViewer)
	sensor := &cameraSensorSubscription{hub: h}
	sensor.subID, sensor.frames, _ = h.subscribe()
	defer sensor.Close()
	idRaw, raw, _ := h.subscribeKind(rawFrames)
	defer h.unsubscribe(idRaw)
	idPixels, pixels, _ := h.subscribePixels()
	defer h.unsubscribe(idPixels)

	jpegStamp := frameTimestamp{wallNs: 1, auAligned: true, native: true}
	h.produceCaptured(encodeTestJPEG(t, 64, 48), jpegStamp, videoCodecMJPEG)
	for name, ch := range map[string]chan *videoFrame{"encoded": viewer, "sensor": sensor.frames, "raw": raw, "pixel": pixels} {
		if len(ch) != 0 {
			t.Errorf("the %s subscriber received a JPEG frame", name)
		}
	}

	// A subscriber kind that does receive JPEG frames still cannot send them.
	idNative, native, _ := h.subscribeKind(nativeFrames)
	h.produceCaptured(encodeTestJPEG(t, 64, 48), jpegStamp, videoCodecMJPEG)
	h.produceCaptured([]byte{0, 0, 0, 1, 0x65}, frameTimestamp{wallNs: 2, auAligned: true, native: true}, agentpb.VideoCodec_VIDEO_CODEC_H264)
	h.produceCaptured(encodeTestJPEG(t, 64, 48), jpegStamp, videoCodecMJPEG)
	if len(native) != 3 {
		t.Fatalf("the native subscriber holds %d frames, want 3", len(native))
	}
	h.mu.Lock()
	h.subs[idNative].closed = true
	close(native)
	h.mu.Unlock()
	stream := &codecRecordingStream{ctx: context.Background()}
	(&VideoService{}).pumpFrames(stream, h, idNative, native) //nolint:errcheck
	h.unsubscribe(idNative)
	if !reflect.DeepEqual(stream.sent, []agentpb.VideoCodec{agentpb.VideoCodec_VIDEO_CODEC_H264}) {
		t.Fatalf("StreamVideo sent codecs %v, want only the H.264 frame", stream.sent)
	}
	for _, codec := range stream.sent {
		if !codecIsOnTheWire(codec) {
			t.Fatalf("codec %d is not a value of the wire enum", int32(codec))
		}
	}
	if codecIsOnTheWire(videoCodecMJPEG) {
		t.Fatal("videoCodecMJPEG counts as a wire codec")
	}
}
