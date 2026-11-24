package services

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func numberedJPEG(sampleID uint64) *videoFrame {
	return &videoFrame{data: []byte{0xFF, 0xD8, byte(sampleID), 0xFF, 0xD9}, codec: videoCodecMJPEG, native: true,
		sampleID: sampleID, tsNs: 1_000 + sampleID, nativeNs: int64(2_000 + sampleID), sequence: uint32(sampleID), sequenceValid: true}
}

func TestDerivedFrameTags_ExactMatchReturnsTheSourceIdentityWithoutItsBytes(t *testing.T) {
	var tags derivedFrameTags
	first := tags.assign(numberedJPEG(7), 0)
	second := tags.assign(numberedJPEG(8), 33*time.Millisecond)
	if first != derivedTagBase || second != derivedTagBase+uint64(33*time.Millisecond) {
		t.Fatalf("tags = %d, %d, want the 1000-hour base plus the time since the child started", first, second)
	}
	for tag, want := range map[uint64]uint64{first: 7, second: 8} {
		source, known := tags.source(tag, 0)
		if !known || source.sampleID != want || source.tsNs != 1_000+want {
			t.Fatalf("source(%d) = %+v, %v, want sample %d", tag, source, known, want)
		}
		if source.data != nil {
			t.Error("the table keeps a source frame's bytes alive")
		}
	}
	if n := tags.unknownOutputs(); n != 0 {
		t.Errorf("unknown outputs = %d, want 0", n)
	}
}

func TestDerivedFrameTags_UndoesAConstantShift(t *testing.T) {
	var tags derivedFrameTags
	tag := tags.assign(numberedJPEG(3), time.Second)
	const shift = uint64(1000 * time.Hour)
	if source, known := tags.source(tag+shift, shift); !known || source.sampleID != 3 {
		t.Fatalf("source = %+v, %v, want sample 3", source, known)
	}
	if _, known := tags.source(tag+shift, 0); known {
		t.Fatal("a shifted timestamp matched without undoing the shift")
	}
	if _, known := tags.source(tag, shift); known {
		t.Fatal("a timestamp below the shift matched a frame")
	}
}

func TestDerivedFrameTags_UnknownTagIsCountedAndNeverGivenToANeighbour(t *testing.T) {
	var tags derivedFrameTags
	tag := tags.assign(numberedJPEG(1), 0)
	tags.assign(numberedJPEG(2), 2*time.Nanosecond)
	if _, known := tags.source(tag+1, 0); known {
		t.Fatal("a timestamp between two tags was matched to one of them")
	}
	if n := tags.unknownOutputs(); n != 1 {
		t.Fatalf("unknown outputs = %d, want 1", n)
	}
}

func TestDerivedFrameTags_ForgetsTheOldestBeyondItsBound(t *testing.T) {
	var tags derivedFrameTags
	var assigned []uint64
	for i := 0; i < derivedTagTableFrames+10; i++ {
		assigned = append(assigned, tags.assign(numberedJPEG(uint64(i+1)), time.Duration(i)*time.Millisecond))
	}
	if len(tags.sources) != derivedTagTableFrames || len(tags.oldest) != derivedTagTableFrames {
		t.Fatalf("table holds %d frames (%d in order), want %d", len(tags.sources), len(tags.oldest), derivedTagTableFrames)
	}
	if _, known := tags.source(assigned[9], 0); known {
		t.Error("a frame older than the bound is still in the table")
	}
	if source, known := tags.source(assigned[10], 0); !known || source.sampleID != 11 {
		t.Errorf("the oldest frame within the bound = %+v, %v, want sample 11", source, known)
	}
}

func TestDerivedFrameTags_ClearedTableKnowsNoEarlierTag(t *testing.T) {
	var tags derivedFrameTags
	before := tags.assign(numberedJPEG(1), time.Second)
	tags.clear()
	if _, known := tags.source(before, 0); known {
		t.Fatal("a tag from before the restart still names a frame")
	}
	if after := tags.assign(numberedJPEG(2), 0); after != derivedTagBase {
		t.Errorf("first tag after the restart = %d, want the base %d", after, derivedTagBase)
	}
}

func TestDerivedFrameTags_EveryTagIsAboveTheOneBefore(t *testing.T) {
	var tags derivedFrameTags
	var last uint64
	for i, sinceStart := range []time.Duration{time.Second, time.Second, time.Millisecond, -time.Hour, 2 * time.Second} {
		tag := tags.assign(numberedJPEG(uint64(i+1)), sinceStart)
		if tag <= last {
			t.Fatalf("tag %d = %d is not above the previous %d", i, tag, last)
		}
		last = tag
	}
	if last != derivedTagBase+uint64(2*time.Second) {
		t.Errorf("a tag whose time is ahead again = %d, want base plus 2s", last)
	}
}

type derivedPublications struct {
	mu     sync.Mutex
	frames []*videoFrame
}

func (p *derivedPublications) publish(source, derived *videoFrame) (bool, error) {
	if source.sampleID == 0 {
		return true, errSourceHasNoSampleID
	}
	derived.sampleID, derived.tsNs = source.sampleID, source.tsNs
	p.mu.Lock()
	defer p.mu.Unlock()
	p.frames = append(p.frames, derived)
	return true, nil
}

func (p *derivedPublications) published() []*videoFrame {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*videoFrame(nil), p.frames...)
}

var derivedLayerElements = []string{"gdpdepay", "gdppay", "jpegdec"}

// fakeDerivedLayer builds a layer on whatever gst-launch-1.0 is first on PATH.
func fakeDerivedLayer(t *testing.T, demand encodeDemand, grace time.Duration) (*derivedLayer, *derivedPublications) {
	t.Helper()
	gstPath, available, err := gstDiscover()
	if err != nil {
		t.Fatal(err)
	}
	out := &derivedPublications{}
	layer, err := newDerivedLayer(derivedLayerConfig{
		logger: zap.NewNop(), device: "/dev/video4", format: videoCodecMJPEG,
		gstPath: gstPath, available: available, width: 640, height: 480, framerate: 30,
		grace: grace, demand: demand, publish: out.publish,
	})
	if err != nil {
		t.Fatal(err)
	}
	layer.restartPause = time.Millisecond
	return layer, out
}

func runDerivedLayer(t *testing.T, layer *derivedLayer) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- layer.run(ctx) }()
	var once sync.Once
	var err error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case err = <-result:
			case <-time.After(5 * time.Second):
				t.Error("the derived layer did not stop when its ctx was cancelled")
			}
		})
		return err
	}
	t.Cleanup(func() { stop() }) //nolint:errcheck
	return stop
}

func waitForLaunches(t *testing.T, launchLog string, n int) []string {
	t.Helper()
	var launched []string
	// A launch creates the log before it writes its line, and an empty log
	// reads as one empty entry.
	waitUntil(t, "the child pipeline has started", func() bool {
		launched = launchedPipelines(t, launchLog)
		return len(launched) >= n && launched[len(launched)-1] != ""
	})
	if len(launched) != n {
		t.Fatalf("%d child pipelines started, want %d: %q", len(launched), n, launched)
	}
	return launched
}

const (
	derivedPicturesOutput = "videoconvert ! video/x-raw,format=YUY2 ! gdppay ! fdsink fd=3"
	derivedEncodedOutput  = "x264enc tune=zerolatency key-int-max=15 bframes=0 ! video/x-h264,profile=high" +
		" ! h264parse config-interval=-1 ! video/x-h264,stream-format=byte-stream,alignment=au ! gdppay ! fdsink fd=1"
)

func TestDerivedLayer_RunsNoChildForNativeSubscribersAlone(t *testing.T) {
	launchLog := fakeGStreamer(t, 0, derivedLayerElements...)
	demand := newFakeEncodeDemand(false, false)
	demand.native = true
	layer, _ := fakeDerivedLayer(t, demand, time.Second)
	stop := runDerivedLayer(t, layer)

	waitUntil(t, "the layer has read the demand", func() bool {
		_, encodes := demand.reported()
		return len(encodes) == 1
	})
	time.Sleep(100 * time.Millisecond)
	if launched := launchedPipelines(t, launchLog); len(launched) != 0 {
		t.Fatalf("a child ran with only native subscribers: %q", launched)
	}
	if _, encodes := demand.reported(); !reflect.DeepEqual(encodes, []bool{false}) {
		t.Errorf("setPipelineEncodes = %v, want [false]", encodes)
	}
	for i := uint64(1); i <= 10; i++ {
		layer.take(numberedJPEG(i))
	}
	if counts := layer.counts(); counts.notFed != 0 || len(layer.feed) != 0 {
		t.Errorf("frames were queued or counted as lost with no child to feed: %+v, %d queued", counts, len(layer.feed))
	}
	if err := stop(); err != context.Canceled {
		t.Errorf("run returned %v, want context.Canceled", err)
	}
}

func TestDerivedLayer_ShapesTheChildToItsSubscribers(t *testing.T) {
	for name, c := range map[string]struct {
		encoded, pixels bool
		want            string
	}{
		"pictures only": {false, true, "-q fdsrc fd=0 ! gdpdepay ! jpegdec ! " + derivedPicturesOutput},
		"H.264 only":    {true, false, "-q fdsrc fd=0 ! gdpdepay ! jpegdec ! videoconvert ! video/x-raw,format=I420 ! " + derivedEncodedOutput},
		"both": {true, true, "-q fdsrc fd=0 ! gdpdepay ! jpegdec ! tee name=derived" +
			" derived. ! queue ! " + derivedPicturesOutput +
			" derived. ! queue ! videoconvert ! video/x-raw,format=I420 ! " + derivedEncodedOutput},
	} {
		t.Run(name, func(t *testing.T) {
			launchLog := fakeGStreamer(t, 0, derivedLayerElements...)
			demand := newFakeEncodeDemand(c.encoded, c.pixels)
			layer, _ := fakeDerivedLayer(t, demand, time.Second)
			runDerivedLayer(t, layer)
			if launched := waitForLaunches(t, launchLog, 1); launched[0] != c.want {
				t.Errorf("child pipeline\n got %q\nwant %q", launched[0], c.want)
			}
			if _, encodes := demand.reported(); !reflect.DeepEqual(encodes, []bool{c.encoded}) {
				t.Errorf("setPipelineEncodes = %v, want [%v]", encodes, c.encoded)
			}
		})
	}
}

func TestDerivedLayer_AddsABranchAtOnceAndDropsOneAfterTheGrace(t *testing.T) {
	launchLog := fakeGStreamer(t, 0, derivedLayerElements...)
	demand := newFakeEncodeDemand(false, true)
	const grace = 400 * time.Millisecond
	layer, _ := fakeDerivedLayer(t, demand, grace)
	runDerivedLayer(t, layer)
	waitForLaunches(t, launchLog, 1)

	demand.set(true, true)
	launched := waitForLaunches(t, launchLog, 2)
	if !strings.Contains(launched[1], "tee name=derived") || !strings.Contains(launched[1], "x264enc") {
		t.Fatalf("a viewer joining did not restart the child with both branches: %q", launched[1])
	}

	viewerLeft := time.Now()
	demand.set(false, true)
	time.Sleep(grace / 2)
	if launched := launchedPipelines(t, launchLog); len(launched) != 2 {
		t.Fatalf("the encoder was dropped %v after the viewer left, inside the %v grace", time.Since(viewerLeft), grace)
	}
	launched = waitForLaunches(t, launchLog, 3)
	if strings.Contains(launched[2], "x264enc") || !strings.Contains(launched[2], "fdsink fd=3") {
		t.Fatalf("after the grace the child still encodes, or lost its pictures: %q", launched[2])
	}
	restarts, encodes := demand.reported()
	if restarts != 2 || !reflect.DeepEqual(encodes, []bool{false, true, false}) {
		t.Errorf("restarts=%d setPipelineEncodes=%v, want 2 and [false true false]", restarts, encodes)
	}
}

func TestDerivedLayer_StopsItsChildWhenOnlyNativeSubscribersRemain(t *testing.T) {
	launchLog := fakeGStreamer(t, 0, derivedLayerElements...)
	demand := newFakeEncodeDemand(true, false)
	demand.native = true
	layer, _ := fakeDerivedLayer(t, demand, 50*time.Millisecond)
	runDerivedLayer(t, layer)
	waitForLaunches(t, launchLog, 1)
	waitUntil(t, "the child takes frames", layer.feeding.Load)

	demand.set(false, false)
	waitUntil(t, "the child is stopped", func() bool { return !layer.feeding.Load() })
	waitUntil(t, "the layer reports it no longer encodes", func() bool {
		_, encodes := demand.reported()
		return reflect.DeepEqual(encodes, []bool{true, false})
	})
	time.Sleep(100 * time.Millisecond)
	if launched := launchedPipelines(t, launchLog); len(launched) != 1 {
		t.Fatalf("a child ran again with only native subscribers: %q", launched)
	}
}

func TestDerivedLayer_FeedingNeverBlocksCaptureAndCountsWhatItCouldNotFeed(t *testing.T) {
	fakeGStreamer(t, 0, derivedLayerElements...)
	layer, _ := fakeDerivedLayer(t, newFakeEncodeDemand(false, true), time.Second)
	runDerivedLayer(t, layer)
	waitUntil(t, "the child takes frames", layer.feeding.Load)

	// The stand-in child never reads its input, so the pipe fills and stays full.
	const offered = 2000
	frame := numberedJPEG(1)
	frame.data = make([]byte, 64*1024)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < offered; i++ {
			layer.take(frame)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("take blocked on a child that does not read")
	}
	counts := layer.counts()
	if counts.notFed == 0 || counts.notFed > offered {
		t.Fatalf("not fed = %d of %d offered to a child that reads nothing", counts.notFed, offered)
	}
	if queued := len(layer.feed); queued > derivedFeedQueueFrames {
		t.Errorf("%d frames queued, bound is %d", queued, derivedFeedQueueFrames)
	}
}

func TestDerivedLayer_TakesOnlyNumberedFramesOfItsOwnFormat(t *testing.T) {
	layer := &derivedLayer{format: videoCodecMJPEG, feed: make(chan *videoFrame, derivedFeedQueueFrames)}
	layer.feeding.Store(true)
	unnumbered := numberedJPEG(0)
	h264 := numberedJPEG(5)
	h264.codec = agentpb.VideoCodec_VIDEO_CODEC_H264
	layer.take(unnumbered)
	layer.take(h264)
	if len(layer.feed) != 0 {
		t.Fatalf("%d frames queued, want none", len(layer.feed))
	}
	layer.take(numberedJPEG(6))
	if len(layer.feed) != 1 {
		t.Fatal("a numbered frame of the layer's format was not queued")
	}
}

func TestDerivedLayer_RestartsAChildThatExitsAndGivesUpWhenNoneProduces(t *testing.T) {
	launchLog := fakeGStreamerScript(t, "echo 'jpegdec: cannot decode' >&2\nexit 1\n", derivedLayerElements...)
	layer, _ := fakeDerivedLayer(t, newFakeEncodeDemand(true, true), time.Second)
	layer.tags.assign(numberedJPEG(1), 0)
	result := make(chan error, 1)
	go func() { result <- layer.run(context.Background()) }()
	var err error
	select {
	case err = <-result:
	case <-time.After(10 * time.Second):
		t.Fatal("the layer kept restarting a child that never produced a frame")
	}
	if status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "jpegdec: cannot decode") {
		t.Fatalf("run returned %v, want Internal carrying the child's own error output", err)
	}
	if launched := launchedPipelines(t, launchLog); len(launched) != derivedFailedStartsBeforeGivingUp {
		t.Errorf("%d children started, want %d", len(launched), derivedFailedStartsBeforeGivingUp)
	}
	if _, known := layer.tags.source(derivedTagBase, 0); known {
		t.Error("a tag from before the restarts still names a frame")
	}
}

func derivedOutputPipe(t *testing.T) (r, w *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		r.Close() //nolint:errcheck
		w.Close() //nolint:errcheck
	})
	return r, w
}

func TestDerivedLayer_PublishesEachOutputUnderTheFrameItsTagNames(t *testing.T) {
	out := &derivedPublications{}
	layer := &derivedLayer{publish: out.publish, firstOutputTimeout: 5 * time.Second, idleOutputTimeout: 5 * time.Second}
	first := layer.tags.assign(numberedJPEG(41), 0)
	second := layer.tags.assign(numberedJPEG(42), time.Millisecond)

	r, w := derivedOutputPipe(t)
	gdp := &gdpWriter{w: w}
	if err := gdp.streamHeader("video/x-h264"); err != nil {
		t.Fatal(err)
	}
	for _, unit := range []struct {
		timestamp uint64
		payload   string
	}{
		{second, "unit-42"},
		{second + 1, "nobody's"},
		{first, "unit-41"},
		{first, ""},
	} {
		if err := gdp.buffer(unit.timestamp, []byte(unit.payload)); err != nil {
			t.Fatal(err)
		}
	}
	w.Close() //nolint:errcheck

	err := layer.publishOutput(derivedOutput{name: "H.264", pipe: r, maxPayload: maxFrameBytes, frame: accessUnitFrame})
	if err == nil || !strings.Contains(err.Error(), "EOF") {
		t.Fatalf("publishOutput returned %v, want the end of the pipe", err)
	}
	var got []string
	for _, frame := range out.published() {
		if frame.codec != agentpb.VideoCodec_VIDEO_CODEC_H264 || !frame.auAligned {
			t.Errorf("published frame codec=%v auAligned=%v, want a whole H.264 access unit", frame.codec, frame.auAligned)
		}
		got = append(got, string(frame.data)+"@"+string(rune('0'+frame.sampleID-40)))
	}
	if want := []string{"unit-42@2", "unit-41@1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("published %v, want %v", got, want)
	}
	if counts := layer.counts(); counts.unknownTags != 1 || counts.unusable != 1 || counts.published != 2 {
		t.Errorf("counts = %+v, want 1 unknown tag, 1 unusable output, 2 published", counts)
	}
}

func TestDerivedLayer_UndoesTheShiftTheChildReportsAndDropsMisSizedPictures(t *testing.T) {
	out := &derivedPublications{}
	layer := &derivedLayer{publish: out.publish, pictures: tinyTapFormat,
		firstOutputTimeout: 5 * time.Second, idleOutputTimeout: 5 * time.Second}
	tag := layer.tags.assign(numberedJPEG(9), 0)

	r, w := derivedOutputPipe(t)
	if _, err := w.Write(gdpFixture(t, gdpShiftedSegmentPacket)); err != nil {
		t.Fatal(err)
	}
	gdp := &gdpWriter{w: w}
	const shift = uint64(1000 * time.Hour)
	for _, picture := range []string{"abcd", "abcdefgh"} {
		if err := gdp.buffer(tag+shift, []byte(picture)); err != nil {
			t.Fatal(err)
		}
	}
	w.Close() //nolint:errcheck

	layer.publishOutput(derivedOutput{name: "pictures", pipe: r, maxPayload: maxRawFrameBytes, frame: layer.pictureFrame}) //nolint:errcheck
	published := out.published()
	if len(published) != 1 || string(published[0].data) != "abcd" || published[0].sampleID != 9 {
		t.Fatalf("published %d pictures, want the one four-byte picture of sample 9", len(published))
	}
	if published[0].codec != agentpb.VideoCodec_VIDEO_CODEC_RAW || published[0].rawFmt != tinyTapFormat {
		t.Errorf("picture codec=%v format=%v, want RAW in the layer's picture format", published[0].codec, published[0].rawFmt)
	}
	if counts := layer.counts(); counts.unknownTags != 0 || counts.unusable != 1 {
		t.Errorf("counts = %+v, want no unknown tag and 1 unusable output", counts)
	}
}

func TestDerivedLayer_OutputThatGoesQuietEndsTheChild(t *testing.T) {
	layer := &derivedLayer{publish: (&derivedPublications{}).publish, firstOutputTimeout: 50 * time.Millisecond, idleOutputTimeout: 50 * time.Millisecond}
	r, _ := derivedOutputPipe(t)
	done := make(chan error, 1)
	go func() {
		done <- layer.publishOutput(derivedOutput{name: "H.264", pipe: r, maxPayload: maxFrameBytes, frame: accessUnitFrame})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("a quiet output ended with %v, want a read deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a child that produces nothing was never noticed")
	}
}

func TestNewDerivedLayer_IsUnavailableWithoutEveryElementItNeeds(t *testing.T) {
	complete := map[string]bool{"gdpdepay": true, "gdppay": true, "jpegdec": true, "h264parse": true, "x264enc": true}
	build := func(format agentpb.VideoCodec, available map[string]bool) (*derivedLayer, error) {
		return newDerivedLayer(derivedLayerConfig{logger: zap.NewNop(), format: format, gstPath: "gst-launch-1.0",
			available: available, width: 640, height: 480})
	}
	layer, err := build(videoCodecMJPEG, complete)
	if err != nil {
		t.Fatalf("a device with every element: %v", err)
	}
	if layer.encoder.element != "x264enc" || layer.framerate != 30 {
		t.Errorf("encoder=%q framerate=%d, want x264enc at the default 30", layer.encoder.element, layer.framerate)
	}
	if want := mjpegPixelsFormat(640, 480); !reflect.DeepEqual(layer.pictures, want) {
		t.Errorf("pictures = %v, want the format today's tap produces, %v", layer.pictures, want)
	}

	for _, element := range []string{"gdpdepay", "gdppay", "jpegdec", "h264parse", "x264enc"} {
		without := map[string]bool{}
		for name := range complete {
			without[name] = name != element
		}
		if _, err := build(videoCodecMJPEG, without); err == nil {
			t.Errorf("a layer was built without %s", element)
		} else if element != "x264enc" && !strings.Contains(err.Error(), element) {
			t.Errorf("without %s the refusal is %q, which does not name it", element, err)
		}
	}
	vp8Only := map[string]bool{"gdpdepay": true, "gdppay": true, "jpegdec": true, "vp8enc": true, "webmmux": true}
	if _, err := build(videoCodecMJPEG, vp8Only); err == nil {
		t.Error("a layer was built on a device that can only encode VP8")
	}
	if _, err := build(videoCodecMJPEG, nil); err == nil {
		t.Error("a layer was built without an element listing")
	}
	if _, err := build(agentpb.VideoCodec_VIDEO_CODEC_H264, complete); err == nil {
		t.Error("a layer was built for a format with no transform")
	}
}

func TestDerivedLayer_OffersPixelsAndRefusesRawAsAnMJPEGCaptureDoes(t *testing.T) {
	h, cancel := newTestHub(t)
	defer cancel()
	idRaw, chRaw, _ := h.subscribeKind(rawFrames)
	idPix, chPix, _ := h.subscribePixels()
	defer h.unsubscribe(idRaw)
	defer h.unsubscribe(idPix)

	layer, err := newDerivedLayer(derivedLayerConfig{logger: zap.NewNop(), format: videoCodecMJPEG, gstPath: "gst-launch-1.0",
		available: map[string]bool{"gdpdepay": true, "gdppay": true, "jpegdec": true, "h264parse": true, "x264enc": true},
		width:     640, height: 480, demand: h, publish: h.broadcastDerivedFrom})
	if err != nil {
		t.Fatal(err)
	}
	layer.offerTo(h)

	if _, open := <-chRaw; open {
		t.Fatal("a raw subscriber was left on a camera captured as JPEG")
	}
	refusal := h.subscriberErr(idRaw)
	if want := "camera is captured as MJPEG at 640x480; raw frames are not offered"; refusal == nil || !strings.Contains(refusal.Error(), want) {
		t.Fatalf("raw refusal = %v, want today's reason %q", refusal, want)
	}
	if err := h.subscriberErr(idPix); err != nil || len(chPix) != 0 {
		t.Fatalf("the pixel subscriber was turned away: %v", err)
	}
	if !layer.needs(h).pixels {
		t.Error("a pixel subscriber does not make the layer want pictures")
	}

	tooLarge, err := newDerivedLayer(derivedLayerConfig{logger: zap.NewNop(), format: videoCodecMJPEG, gstPath: "gst-launch-1.0",
		available: map[string]bool{"gdpdepay": true, "gdppay": true, "jpegdec": true, "h264parse": true, "x264enc": true},
		width:     1920, height: 1080, demand: h, publish: h.broadcastDerivedFrom})
	if err != nil {
		t.Fatal(err)
	}
	if tooLarge.pictures != nil || tooLarge.needs(h).pixels {
		t.Error("a layer whose pictures exceed the raw frame limit still plans a picture branch")
	}
}

func TestDeviceHub_HandsEachNumberedFrameToItsDerivedLayer(t *testing.T) {
	h, cancel := newTestHub(t)
	defer cancel()
	h.sampleSeq = new(atomic.Uint64)
	id, _, _ := h.subscribeKind(nativeFrames)
	defer h.unsubscribe(id)

	layer := &derivedLayer{format: videoCodecMJPEG, feed: make(chan *videoFrame, derivedFeedQueueFrames)}
	layer.feeding.Store(true)
	detach := h.feedDerivedLayer(layer)
	h.produceCaptured([]byte{0xFF, 0xD8, 0xFF, 0xD9}, frameTimestamp{wallNs: 5, native: true}, videoCodecMJPEG)
	if len(layer.feed) != 1 {
		t.Fatal("the hub did not hand its layer the frame it produced")
	}
	if frame := <-layer.feed; frame.sampleID != 1 || frame.tsNs != 5 {
		t.Fatalf("the layer got sample %d at %d, want the numbered frame", frame.sampleID, frame.tsNs)
	}
	h.broadcast(&videoFrame{data: []byte{1}, codec: videoCodecMJPEG, sampleID: 1})
	if len(layer.feed) != 0 {
		t.Fatal("a frame that was only broadcast reached the layer")
	}
	detach()
	h.produceCaptured([]byte{0xFF, 0xD8, 0xFF, 0xD9}, frameTimestamp{wallNs: 6, native: true}, videoCodecMJPEG)
	if len(layer.feed) != 0 {
		t.Fatal("the hub fed a layer after it was detached")
	}
}

func TestKeyframeArg_VideoToolboxDoesNotReorderFrames(t *testing.T) {
	for _, encoder := range []string{"vtenc_h264", "vtenc_h264_hw"} {
		if got, want := keyframeArg(encoder, 15), " max-keyframe-interval=15 realtime=true allow-frame-reordering=false"; got != want {
			t.Errorf("keyframeArg(%q) = %q, want %q", encoder, got, want)
		}
	}
}

func TestRunPipelinesShapedBy_AddsAndDropsThePictureBranch(t *testing.T) {
	demand := newFakeEncodeDemand(false, false)
	demand.native = true
	launches := newRecordedLaunches()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	const grace = 100 * time.Millisecond
	go func() { result <- runPipelinesShapedBy(ctx, demand, grace, branchesOnDemand, launches.launch) }()

	if shape := launches.next(t, time.Second); shape != (pipelineShape{}) {
		t.Fatalf("first launch %+v, want no branch for native subscribers alone", shape)
	}
	demand.set(false, true)
	if shape := launches.next(t, time.Second); shape != (pipelineShape{pixels: true}) {
		t.Fatalf("launch after a pixel subscriber joined = %+v, want pictures only", shape)
	}
	demand.set(true, true)
	if shape := launches.next(t, time.Second); shape != (pipelineShape{encode: true, pixels: true}) {
		t.Fatalf("launch after a viewer joined = %+v, want both branches", shape)
	}
	demand.set(true, false)
	launches.none(t, grace/2)
	if shape := launches.next(t, time.Second); shape != (pipelineShape{encode: true}) {
		t.Fatalf("launch after the grace without a pixel subscriber = %+v, want H.264 only", shape)
	}
	demand.set(false, false)
	if shape := launches.next(t, time.Second); shape != (pipelineShape{}) {
		t.Fatalf("launch after the grace with native subscribers alone = %+v, want no branch", shape)
	}
	cancel()
	if err := <-result; err != context.Canceled {
		t.Fatalf("returned %v, want context.Canceled", err)
	}
}

func TestRunPipelinesShapedBy_RestartsALaunchThatEndedRestartable(t *testing.T) {
	demand := newFakeEncodeDemand(true, false)
	launches := newRecordedLaunches()
	var failures atomic.Int32
	launches.failWith = func(pipelineShape) error {
		if failures.Add(1) <= 2 {
			return errPipelineEndedRestartable
		}
		return nil
	}
	stop := runShaped(t, demand, time.Second, launches)
	for i := 0; i < 3; i++ {
		if shape := launches.next(t, time.Second); !shape.encode {
			t.Fatalf("launch %d has no encoder", i)
		}
	}
	launches.none(t, 50*time.Millisecond)
	if restarts, _ := demand.reported(); restarts != 2 {
		t.Errorf("encodedStreamRestarted called %d times, want once per restart, 2", restarts)
	}
	if err := stop(); err != context.Canceled {
		t.Errorf("returned %v, want context.Canceled", err)
	}
}
