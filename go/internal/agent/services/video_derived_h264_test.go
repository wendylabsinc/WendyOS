package services

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

var h264PictureLayerElements = []string{"gdpdepay", "gdppay", "avdec_h264"}

const h264PicturesOnly = "-q fdsrc fd=0 ! gdpdepay ! h264parse ! avdec_h264 thread-type=slice ! " + derivedPicturesOutput

var sfH264BSlice = []byte{0, 0, 0, 1, 0x01, 0x9C, 0x02}

// numberedH264 is one of an H.264 camera's own access units.
func numberedH264(sampleID uint64, randomAccess bool) *videoFrame {
	return hcOwnFrame(sampleID, randomAccess, int64(sampleID))
}

func numberedH264BFrame(sampleID uint64) *videoFrame {
	frame := numberedH264(sampleID, false)
	frame.data = append([]byte(nil), sfH264BSlice...)
	return frame
}

func fakeH264PictureLayer(t *testing.T, demand encodeDemand, publish func(source, derived *videoFrame) (bool, error)) *derivedLayer {
	t.Helper()
	gstPath, available, err := gstDiscover()
	if err != nil {
		t.Fatal(err)
	}
	layer, err := newDerivedLayer(derivedLayerConfig{
		logger: zap.NewNop(), device: "/dev/video4", format: agentpb.VideoCodec_VIDEO_CODEC_H264,
		gstPath: gstPath, available: available, width: 640, height: 480, framerate: 30,
		grace: time.Second, demand: demand, publish: publish,
	})
	if err != nil {
		t.Fatal(err)
	}
	layer.restartPause = time.Millisecond
	return layer
}

func TestH264PictureIsBidirectional(t *testing.T) {
	aud, sei := []byte{0, 0, 0, 1, 0x09, 0xF0}, []byte{0, 0, 1, 0x06, 0x05, 0x01, 0x80}
	for name, tc := range map[string]struct {
		unit []byte
		want bool
	}{
		"an IDR slice":                             {sfH264IDRSlice, false},
		"a P slice":                                {sfH264InterSlice, false},
		"a B slice":                                {sfH264BSlice, true},
		"a B slice of the first kind":              {[]byte{0, 0, 1, 0x01, 0xA0}, true},
		"a B slice behind a delimiter and SEI":     {append(append(append([]byte(nil), aud...), sei...), sfH264BSlice...), true},
		"a P slice behind parameter sets":          {append(append([]byte(nil), sfH264ParameterSets...), sfH264InterSlice...), false},
		"a slice header cut short":                 {[]byte{0, 0, 0, 1, 0x01}, false},
		"parameter sets alone":                     {sfH264ParameterSets, false},
		"a later slice of a B picture, first_mb 5": {[]byte{0, 0, 1, 0x01, 0x32}, true},
	} {
		if got := h264PictureIsBidirectional(tc.unit); got != tc.want {
			t.Errorf("%s: %x reported as %v, want %v", name, tc.unit, got, tc.want)
		}
	}
}

// fedBuffer is one buffer a child was fed: the frame its tag names and its bytes.
type fedBuffer struct {
	sampleID uint64
	data     []byte
}

func fedSamples(buffers []fedBuffer) []uint64 {
	ids := make([]uint64, len(buffers))
	for i, buffer := range buffers {
		ids[i] = buffer.sampleID
	}
	return ids
}

// feedInto runs one child's feeding over a pipe and returns what reaches the
// child once the frames have been taken.
func feedInto(t *testing.T, layer *derivedLayer, frames ...*videoFrame) (fed []fedBuffer, ended error) {
	t.Helper()
	r, w := derivedOutputPipe(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- layer.feedChild(ctx, w) }()
	read := make(chan []fedBuffer, 1)
	go func() {
		var buffers []fedBuffer
		gdp := &gdpReader{r: r, maxPayload: maxFrameBytes}
		for {
			tag, payload, err := gdp.nextBuffer()
			if err != nil {
				read <- buffers
				return
			}
			source, known := layer.tags.source(tag, 0)
			if !known {
				t.Errorf("the child was fed a buffer whose tag %d names no frame", tag)
				continue
			}
			buffers = append(buffers, fedBuffer{source.sampleID, payload})
		}
	}()
	for _, frame := range frames {
		select {
		case layer.feed <- frame:
		case ended = <-done:
			cancel()
			w.Close() //nolint:errcheck
			return <-read, ended
		case <-time.After(5 * time.Second):
			t.Fatal("the feed did not take a frame")
		}
	}
	waitUntil(t, "the frames are taken", func() bool { return len(layer.feed) == 0 })
	time.Sleep(20 * time.Millisecond)
	cancel()
	ended = <-done
	w.Close() //nolint:errcheck
	return <-read, ended
}

func h264FeedTestLayer() *derivedLayer {
	return &derivedLayer{format: agentpb.VideoCodec_VIDEO_CODEC_H264, transform: derivedTransforms[agentpb.VideoCodec_VIDEO_CODEC_H264],
		feed: make(chan *videoFrame), created: time.Now(), framerate: 30}
}

func TestDerivedLayer_FeedsEachChildFromAFrameDecodingCanStartOn(t *testing.T) {
	layer := h264FeedTestLayer()
	fed, _ := feedInto(t, layer,
		numberedH264(1, false), numberedH264(2, false), numberedH264(3, true), numberedH264(4, false),
		numberedH264(6, false), numberedH264(7, false), // 5 never came
		numberedH264(8, true), numberedH264(9, false))
	if want := []uint64{3, 4, 8, 9}; !reflect.DeepEqual(fedSamples(fed), want) {
		t.Fatalf("the first child was fed frames %v, want %v", fedSamples(fed), want)
	}
	if counts := layer.counts(); counts.fed != 4 || counts.awaitedStart != 4 {
		t.Errorf("counts = %+v, want 4 fed and 4 passed over", counts)
	}

	// A child started later knows nothing of the stream so far.
	fed, _ = feedInto(t, layer, numberedH264(10, false), numberedH264(11, false), numberedH264(12, true), numberedH264(13, false))
	if want := []uint64{12, 13}; !reflect.DeepEqual(fedSamples(fed), want) {
		t.Fatalf("the second child was fed frames %v, want %v", fedSamples(fed), want)
	}
}

func TestDerivedLayer_EveryJPEGFrameIsOneDecodingCanStartOn(t *testing.T) {
	layer := &derivedLayer{format: videoCodecMJPEG, transform: derivedTransforms[videoCodecMJPEG],
		feed: make(chan *videoFrame), created: time.Now(), framerate: 30}
	fed, _ := feedInto(t, layer, numberedJPEG(1), numberedJPEG(2), numberedJPEG(5), numberedJPEG(6))
	if want := []uint64{1, 2, 5, 6}; !reflect.DeepEqual(fedSamples(fed), want) {
		t.Fatalf("fed frames %v, want every one, %v", fedSamples(fed), want)
	}
}

// A decoder that pairs each picture with its oldest pending buffer would move
// every later picture one frame off if it were fed a buffer without a picture.
func TestDerivedLayer_ParameterSetsAloneAreFedWithTheNextPicture(t *testing.T) {
	layer := h264FeedTestLayer()
	sets := numberedH264(1, false)
	sets.data = append([]byte(nil), sfH264ParameterSets...)
	bareIDR := numberedH264(2, false)
	bareIDR.data = append([]byte(nil), sfH264IDRSlice...)
	laterSets := numberedH264(4, false)
	laterSets.data = append([]byte(nil), sfH264ParameterSets...)

	fed, _ := feedInto(t, layer, sets, bareIDR, numberedH264(3, false), laterSets, numberedH264(5, false))
	if want := []uint64{2, 3, 5}; !reflect.DeepEqual(fedSamples(fed), want) {
		t.Fatalf("fed buffers carry samples %v, want only the frames with a picture, %v", fedSamples(fed), want)
	}
	if want := append(append([]byte(nil), sfH264ParameterSets...), sfH264IDRSlice...); !bytes.Equal(fed[0].data, want) {
		t.Errorf("the first buffer = %x, want the parameter sets ahead of the picture they came before, %x", fed[0].data, want)
	}
	if want := append(append([]byte(nil), sfH264ParameterSets...), sfH264InterSlice...); !bytes.Equal(fed[2].data, want) {
		t.Errorf("the buffer of sample 5 = %x, want the later parameter sets ahead of its picture, %x", fed[2].data, want)
	}
	if counts := layer.counts(); counts.fed != 3 || counts.awaitedStart != 0 || counts.damaged != 0 {
		t.Errorf("counts = %+v, want 3 fed and nothing passed over", counts)
	}
}

func TestDerivedLayer_NeverFeedsAFrameTheDriverFlaggedOrCutShort(t *testing.T) {
	for name, damage := range map[string]func(*videoFrame){
		"flagged as damaged": func(f *videoFrame) { f.nativeFlags |= v4l2BufFlagError },
		"cut short":          func(f *videoFrame) { f.auAligned = false },
	} {
		t.Run(name, func(t *testing.T) {
			layer := h264FeedTestLayer()
			damaged, damagedStart := numberedH264(2, false), numberedH264(4, true)
			damage(damaged)
			damage(damagedStart)
			fed, _ := feedInto(t, layer, numberedH264(1, true), damaged, numberedH264(3, false),
				damagedStart, numberedH264(5, false), numberedH264(6, true), numberedH264(7, false))
			if want := []uint64{1, 6, 7}; !reflect.DeepEqual(fedSamples(fed), want) {
				t.Fatalf("fed frames %v, want %v: nothing damaged, and nothing that needs a frame that was not fed", fedSamples(fed), want)
			}
			if counts := layer.counts(); counts.damaged != 2 || counts.awaitedStart != 2 {
				t.Errorf("counts = %+v, want 2 damaged and 2 passed over", counts)
			}
		})
	}
}

func TestDerivedLayer_StopsFeedingAStreamItsDecoderReorders(t *testing.T) {
	layer := h264FeedTestLayer()
	fed, ended := feedInto(t, layer, numberedH264(1, true), numberedH264(2, false), numberedH264BFrame(3), numberedH264(4, false))
	if !errors.Is(ended, errDerivedFramesReordered) {
		t.Fatalf("feeding ended with %v, want errDerivedFramesReordered", ended)
	}
	if want := []uint64{1, 2}; !reflect.DeepEqual(fedSamples(fed), want) {
		t.Fatalf("fed frames %v, want only those ahead of the reordered one, %v", fedSamples(fed), want)
	}
}

// One frame that is not fed leaves the child without input until the camera's
// next keyframe. When none comes within the stall limit, the picture output
// has failed, and the layer says so instead of looking alive.
func TestDerivedLayer_GivesPicturesUpWhenNoFrameToStartOnComesWithinTheStallLimit(t *testing.T) {
	fakeGStreamerScript(t, "exec /bin/cat > /dev/null\n", h264PictureLayerElements...)
	layer := fakeH264PictureLayer(t, newFakeEncodeDemand(true, true), (&derivedPublications{}).publish)
	layer.firstOutputTimeout, layer.idleOutputTimeout = 300*time.Millisecond, 300*time.Millisecond
	result := make(chan error, 1)
	go func() { result <- layer.run(context.Background()) }()
	waitUntil(t, "the child takes frames", layer.feeding.Load)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for sampleID := uint64(1); ; sampleID++ {
			layer.take(numberedH264(sampleID, false))
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()

	var err error
	select {
	case err = <-result:
	case <-time.After(10 * time.Second):
		t.Fatal("the layer ran on while every frame was passed over")
	}
	var gaveUp derivedLayerGaveUp
	if !errors.As(err, &gaveUp) || gaveUp.lasting || !strings.Contains(gaveUp.msg, "the camera sent no frame that decoding can start on for 300ms") ||
		!strings.Contains(gaveUp.msg, "frames passed over, 0 flagged or cut short") {
		t.Fatalf("run returned %v (lasting %v), want it to give pictures up for the missing start frame, for now", err, gaveUp.lasting)
	}
	if counts := layer.counts(); counts.fed != 0 || counts.awaitedStart == 0 || counts.childRuns != 1 {
		t.Errorf("counts = %+v, want nothing fed, frames passed over and one child", counts)
	}
}

func TestDerivedLayer_GivesUpOnAStreamItsDecoderReorders(t *testing.T) {
	fakeGStreamerScript(t, "exec /bin/cat > /dev/null\n", h264PictureLayerElements...)
	out := &derivedPublications{}
	layer := fakeH264PictureLayer(t, newFakeEncodeDemand(true, true), out.publish)
	result := make(chan error, 1)
	go func() { result <- layer.run(context.Background()) }()
	waitUntil(t, "the child takes frames", layer.feeding.Load)
	layer.take(numberedH264(1, true))
	waitUntil(t, "the first frame is fed", func() bool { return layer.counts().fed == 1 })
	layer.take(numberedH264BFrame(2))

	var err error
	select {
	case err = <-result:
	case <-time.After(10 * time.Second):
		t.Fatal("the layer ran on after a frame its decoder reorders")
	}
	var gaveUp derivedLayerGaveUp
	if !errors.As(err, &gaveUp) || gaveUp.msg != errDerivedFramesReordered.Error() || !gaveUp.lasting {
		t.Fatalf("run returned %v (lasting %v), want derivedLayerGaveUp saying the frames are reordered, for good", err, gaveUp.lasting)
	}
	if runs := layer.counts().childRuns; runs != 1 {
		t.Errorf("%d children started, want one: a reordered stream is not retried", runs)
	}
}

func TestDerivedLayer_H264HasAPictureBranchAndNoEncoder(t *testing.T) {
	launchLog := fakeGStreamer(t, 0, h264PictureLayerElements...)
	for name, tc := range map[string]struct {
		viewer, pixelReader bool
		want                pipelineShape
	}{
		"a viewer alone":              {true, false, pipelineShape{}},
		"a pixel reader alone":        {false, true, pipelineShape{pixels: true}},
		"a viewer and a pixel reader": {true, true, pipelineShape{pixels: true}},
	} {
		layer := fakeH264PictureLayer(t, nil, nil)
		if shape := layer.needs(newFakeEncodeDemand(tc.viewer, tc.pixelReader)); shape != tc.want {
			t.Errorf("%s: shape = %+v, want %+v", name, shape, tc.want)
		}
	}

	demand := newFakeEncodeDemand(true, true)
	layer := fakeH264PictureLayer(t, demand, (&derivedPublications{}).publish)
	if layer.encoder.element != "" || layer.decoder.element != "avdec_h264" || layer.elementInUse() != "avdec_h264" {
		t.Errorf("encoder=%q decoder=%q, want avdec_h264 and no encoder", layer.encoder.element, layer.decoder.element)
	}
	if want := yuyvPictureFormat(640, 480); !reflect.DeepEqual(layer.pictures, want) {
		t.Errorf("pictures = %v, want YUYV at the capture size, %v", layer.pictures, want)
	}
	runDerivedLayer(t, layer)
	if launched := waitForLaunches(t, launchLog, 1); launched[0] != h264PicturesOnly {
		t.Errorf("child pipeline\n got %q\nwant %q", launched[0], h264PicturesOnly)
	}
	if _, encodes := demand.reported(); !reflect.DeepEqual(encodes, []bool{false}) {
		t.Errorf("setPipelineEncodes = %v, want [false]", encodes)
	}
}

func TestNewDerivedLayer_H264NeedsFramingAndOneDecoderAndNoEncoder(t *testing.T) {
	build := func(available map[string]bool) (*derivedLayer, error) {
		return newDerivedLayer(derivedLayerConfig{logger: zap.NewNop(), format: agentpb.VideoCodec_VIDEO_CODEC_H264, gstPath: "gst-launch-1.0",
			available: available, width: 1280, height: 720})
	}
	layer, err := build(map[string]bool{"gdpdepay": true, "gdppay": true, "h264parse": true, "nvv4l2decoder": true, "nvvidconv": true})
	if err != nil {
		t.Fatalf("a device with a decoder and no encoder: %v", err)
	}
	want := "gst-launch-1.0 -q fdsrc fd=0 ! gdpdepay ! h264parse ! nvv4l2decoder ! nvvidconv ! video/x-raw ! " + derivedPicturesOutput
	if got := strings.Join(layer.pipelineArgs(pipelineShape{pixels: true}), " "); got != want {
		t.Errorf("child pipeline\n got %q\nwant %q", got, want)
	}
	if caps := layer.transform.inputCaps(1280, 720, 30); caps != "video/x-h264, stream-format=(string)byte-stream, alignment=(string)au, framerate=(fraction)30/1" {
		t.Errorf("input caps = %q", caps)
	}
	for _, element := range []string{"gdpdepay", "gdppay", "h264parse"} {
		available := map[string]bool{"gdpdepay": true, "gdppay": true, "h264parse": true, "avdec_h264": true}
		delete(available, element)
		if _, err := build(available); err == nil || !strings.Contains(err.Error(), element) {
			t.Errorf("without %s: %v, want a refusal that names it", element, err)
		}
	}
	if _, err := build(map[string]bool{"gdpdepay": true, "gdppay": true, "h264parse": true, "x264enc": true}); err == nil ||
		!strings.Contains(err.Error(), "a decoder (nvv4l2decoder with nvvidconv, v4l2h264dec, avdec_h264, openh264dec)") {
		t.Errorf("without any decoder: %v, want a refusal that names the choices", err)
	}
}

// h264V4L2 stands in for the kernel's side of a camera that produces H.264.
type h264V4L2 struct {
	asked atomic.Pointer[v4l2CaptureConfig]
}

func (f *h264V4L2) capture(ctx context.Context, emit func([]byte, frameTimestamp) bool, _ string, cfg v4l2CaptureConfig) error {
	f.asked.Store(&cfg)
	if cfg.pixelFormat != v4l2PixFmtH264 {
		return cfg.formatUnsupported("not this camera's format")
	}
	for sequence := uint32(1); ; sequence++ {
		unit := numberedH264(uint64(sequence), sequence%5 == 1)
		stamp := realtimeFrameTimestamp(time.Now())
		stamp.sequence, stamp.sequenceValid, stamp.auAligned = sequence, true, true
		if !emit(unit.data, stamp) {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func captureFakeH264Camera(t *testing.T, subscribe func(h *deviceHub)) (*VideoService, *deviceHub, *h264V4L2) {
	t.Helper()
	prevSize := captureSizeFor
	captureSizeFor = func(string, uint32, uint32, uint32) (uint32, uint32) { return 640, 480 }
	t.Cleanup(func() { captureSizeFor = prevSize })
	withFrameRates(t, map[uint32][]uint32{v4l2PixFmtH264: {30}})

	svc := &VideoService{logger: zap.NewNop(), encoderIdleGrace: time.Second}
	svc.capturePixelFormats = func(string) ([]uint32, bool) { return []uint32{v4l2PixFmtYUYV, v4l2PixFmtH264}, true }
	v4l2 := &h264V4L2{}
	svc.captureV4L2 = v4l2.capture
	h, cancelHub := newTestHub(t)
	t.Cleanup(cancelHub)
	h.sampleSeq = new(atomic.Uint64)
	subscribe(h)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- svc.captureLocalCamera(ctx, h.produceCaptured, "/dev/video4", &agentpb.StreamVideoRequest{}, camera.TransportUSB, "", h)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-result:
		case <-time.After(10 * time.Second):
			t.Error("captureLocalCamera did not return after its ctx was cancelled")
		}
	})
	return svc, h, v4l2
}

func TestCaptureLocalCamera_H264CameraHandsOnItsOwnFramesAndDecodesPicturesFromThem(t *testing.T) {
	launchLog := fakeGStreamer(t, 0, h264PictureLayerElements...)
	var idViewer, idNative, idRaw, idPixels int
	var viewer, native, raw, pixels chan *videoFrame
	_, h, v4l2 := captureFakeH264Camera(t, func(h *deviceHub) {
		idViewer, viewer, _ = h.subscribe()
		idNative, native, _ = h.subscribeKind(nativeFrames)
		idRaw, raw, _ = h.subscribeKind(rawFrames)
		idPixels, pixels, _ = h.subscribePixels()
	})
	defer h.unsubscribe(idViewer)
	defer h.unsubscribe(idNative)
	defer h.unsubscribe(idRaw)
	defer h.unsubscribe(idPixels)

	for what, ch := range map[string]chan *videoFrame{"viewer": viewer, "native subscriber": native} {
		frame := waitForFrame(t, ch, "the camera's own H.264 for the "+what)
		if frame.codec != agentpb.VideoCodec_VIDEO_CODEC_H264 || !frame.native || !frame.auAligned || frame.sampleID == 0 {
			t.Fatalf("%s got codec=%v native=%v auAligned=%v sample=%d, want the camera's own access unit, numbered",
				what, frame.codec, frame.native, frame.auAligned, frame.sampleID)
		}
	}
	if refusal := waitForRefusal(t, h, idRaw, raw, "raw"); refusal == nil || !strings.Contains(refusal.Error(), h264RawWhy) {
		t.Fatalf("raw refusal = %v, want %q", refusal, h264RawWhy)
	}
	if err := h.subscriberErr(idPixels); err != nil || len(pixels) != 0 {
		t.Fatalf("pixel subscriber: err=%v, %d frames from a child that wrote none", err, len(pixels))
	}
	if launched := waitForLaunches(t, launchLog, 1); launched[0] != h264PicturesOnly {
		t.Fatalf("child pipeline\n got %q\nwant %q", launched[0], h264PicturesOnly)
	}
	layer := h.derived.Load()
	if layer == nil || layer.format != agentpb.VideoCodec_VIDEO_CODEC_H264 || layer.width != 640 || layer.height != 480 {
		t.Fatalf("the hub's layer = %+v, want one decoding 640x480 H.264", layer)
	}
	waitUntil(t, "the child is fed from a frame it can start on", func() bool { return layer.counts().fed > 0 })
	if cfg := v4l2.asked.Load(); cfg.pixelFormat != v4l2PixFmtH264 || cfg.width != 0 || cfg.height != 0 {
		t.Errorf("the camera was asked for %+v, want H.264 at the size the capture chooses", cfg)
	}
}

func TestCaptureLocalCamera_H264CameraWithoutADecoderRefusesPixelsAndKeepsItsViewers(t *testing.T) {
	launchLog := fakeGStreamer(t, 0, "gdpdepay", "gdppay")
	var idViewer, idPixels int
	var viewer, pixels chan *videoFrame
	_, h, _ := captureFakeH264Camera(t, func(h *deviceHub) {
		idViewer, viewer, _ = h.subscribe()
		idPixels, pixels, _ = h.subscribePixels()
	})
	defer h.unsubscribe(idViewer)
	defer h.unsubscribe(idPixels)

	waitForFrame(t, viewer, "the camera's own H.264")
	if refusal := waitForRefusal(t, h, idPixels, pixels, "pixel"); !errors.Is(refusal, errPixelsUnavailable) || !strings.Contains(refusal.Error(), h264RawWhy) {
		t.Fatalf("pixel refusal = %v, want errPixelsUnavailable with %q", refusal, h264RawWhy)
	}
	if h.derived.Load() != nil || len(launchedPipelines(t, launchLog)) != 0 {
		t.Errorf("a layer or a child runs on a device that cannot decode H.264: %v", launchedPipelines(t, launchLog))
	}
}

func TestCaptureDeriving_APictureLayerThatGivesUpLeavesTheCaptureToItsViewers(t *testing.T) {
	fakeGStreamerScript(t, "echo 'no such decoder' >&2\nexit 1\n", h264PictureLayerElements...)
	h, cancelHub := newTestHub(t)
	defer cancelHub()
	idViewer, viewer, _ := h.subscribe()
	defer h.unsubscribe(idViewer)
	idPixels, pixels, _ := h.subscribePixels()
	defer h.unsubscribe(idPixels)
	layer := fakeH264PictureLayer(t, h, h.broadcastDerivedFrom)
	layer.offerTo(h)
	svc := &VideoService{logger: zap.NewNop()}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	captureCancelled := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- svc.captureDeriving(ctx, h, layer, func(ctx context.Context) error {
			<-ctx.Done()
			close(captureCancelled)
			return ctx.Err()
		})
	}()

	refusal := waitForRefusal(t, h, idPixels, pixels, "pixel")
	if !errors.Is(refusal, errPixelsUnavailable) || !strings.Contains(refusal.Error(), "pictures cannot be decoded from this camera's frames") ||
		!strings.Contains(refusal.Error(), "no such decoder") {
		t.Fatalf("pixel refusal = %v, want errPixelsUnavailable carrying the child's output", refusal)
	}
	select {
	case <-captureCancelled:
		t.Fatal("the capture was ended although its viewers need nothing from the layer")
	case <-time.After(100 * time.Millisecond):
	}
	if err := h.subscriberErr(idViewer); err != nil || len(viewer) != 0 {
		t.Errorf("the viewer was turned away: %v", err)
	}
	if why, failed := svc.derivedFailures.recent(svc.cameraFailureKey("/dev/video4")); !failed || !strings.Contains(why, "no such decoder") {
		t.Errorf("the failure is not remembered for the share policy: %q, %v", why, failed)
	}
	if _, _, err := h.subscribePixels(); !errors.Is(err, errPixelsUnavailable) {
		t.Errorf("a pixel subscriber joining afterwards: %v, want errPixelsUnavailable", err)
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Errorf("captureDeriving returned %v, want the capture's own result", err)
	}
}
