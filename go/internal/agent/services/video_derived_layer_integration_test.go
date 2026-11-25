package services

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

const (
	derivedTestWidth, derivedTestHeight = 320, 240
	derivedTestShades                   = 32
	derivedTestCaptureEpoch             = 1_700_000_000_000_000_000
	derivedTestFramePeriod              = 33_333_333
)

// derivedTestShade is the gray level of the picture a frame with this sample
// ID shows. Neighbouring sample IDs are 42 levels apart or more, so a picture
// paired with a neighbour's identity cannot pass for its own.
func derivedTestShade(sampleID uint64) int {
	return 24 + 6*int(sampleID*7%derivedTestShades)
}

func derivedTestJPEGs(t *testing.T) [][]byte {
	t.Helper()
	jpegs := make([][]byte, derivedTestShades)
	for sampleID := uint64(0); sampleID < derivedTestShades; sampleID++ {
		shade := uint8(derivedTestShade(sampleID))
		img := image.NewRGBA(image.Rect(0, 0, derivedTestWidth, derivedTestHeight))
		draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{R: shade, G: shade, B: shade, A: 255}), image.Point{}, draw.Src)
		var out bytes.Buffer
		if err := jpeg.Encode(&out, img, nil); err != nil {
			t.Fatal(err)
		}
		jpegs[sampleID] = out.Bytes()
	}
	return jpegs
}

// yuyvMeanLuma averages the luma samples of a YUYV picture, the even bytes.
func yuyvMeanLuma(picture []byte) int {
	sum, n := 0, 0
	for at := 0; at < len(picture); at += 2 * 97 {
		sum += int(picture[at])
		n++
	}
	return sum / n
}

type derivedFramesSeen struct {
	mu     sync.Mutex
	frames []*videoFrame
}

func (s *derivedFramesSeen) collect(ch <-chan *videoFrame) {
	for frame := range ch {
		s.mu.Lock()
		s.frames = append(s.frames, frame)
		s.mu.Unlock()
	}
}

func (s *derivedFramesSeen) add(frame *videoFrame) {
	s.mu.Lock()
	s.frames = append(s.frames, frame)
	s.mu.Unlock()
}

func (s *derivedFramesSeen) all() []*videoFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*videoFrame(nil), s.frames...)
}

func (s *derivedFramesSeen) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.frames)
}

// Real gst-launch-1.0, real elements and the real layer on a real hub; only the
// camera is replaced, by JPEG frames produced into the hub as capture does.
func TestDerivedLayerGivesEveryPictureAndAccessUnitItsSourceIdentity(t *testing.T) {
	skip := func(why string) {
		if os.Getenv("WENDY_GSTREAMER_REQUIRED") == "1" {
			t.Fatal(why)
		}
		t.Skip(why)
	}
	if _, err := exec.LookPath("gst-launch-1.0"); err != nil {
		skip("GStreamer is not installed")
	}
	gstPath, available, err := gstDiscover()
	if err != nil {
		skip("GStreamer elements could not be listed: " + err.Error())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	hub, cancelHub := newTestHub(t)
	defer cancelHub()
	hub.sampleSeq = new(atomic.Uint64)

	// Every access unit the layer publishes, in order. The subscriber below
	// can lose one in the burst, and what follows a lost unit does not decode.
	var stream derivedFramesSeen
	layer, err := newDerivedLayer(derivedLayerConfig{
		logger: zap.NewNop(), device: "/dev/video44", format: videoCodecMJPEG,
		gstPath: gstPath, available: available, width: derivedTestWidth, height: derivedTestHeight, framerate: 30,
		grace: time.Minute, demand: hub,
		publish: func(source, derived *videoFrame) (bool, error) {
			keepProducing, err := hub.broadcastDerivedFrom(source, derived)
			if err == nil && derived.codec == agentpb.VideoCodec_VIDEO_CODEC_H264 {
				stream.add(derived)
			}
			return keepProducing, err
		},
	})
	if err != nil {
		skip("this GStreamer cannot run the derived layer: " + err.Error())
	}
	t.Logf("H.264 encoder: %s", layer.encoder.element)

	idPictures, chPictures, err := hub.subscribePixels()
	if err != nil {
		t.Fatal(err)
	}
	idUnits, chUnits, err := hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	var pictures, units derivedFramesSeen
	var collecting sync.WaitGroup
	collecting.Go(func() { pictures.collect(chPictures) })
	collecting.Go(func() { units.collect(chUnits) })
	defer func() {
		hub.mu.Lock()
		for _, id := range []int{idPictures, idUnits} {
			if sub := hub.subs[id]; sub != nil && !sub.closed {
				sub.closed = true
				close(sub.ch)
			}
		}
		hub.mu.Unlock()
		collecting.Wait()
	}()

	detach := hub.feedDerivedLayer(layer)
	defer detach()
	layer.offerTo(hub)
	layerCtx, stopLayer := context.WithCancel(ctx)
	layerDone := make(chan error, 1)
	go func() { layerDone <- layer.run(layerCtx) }()
	defer func() {
		stopLayer()
		<-layerDone
	}()

	jpegs := derivedTestJPEGs(t)
	captureTime := func(sampleID uint64) uint64 { return derivedTestCaptureEpoch + sampleID*derivedTestFramePeriod }
	// produce hands the hub one frame as capture does and returns its sample
	// ID. A nil payload is the JPEG whose shade that sample ID calls for.
	produce := func(payload []byte) uint64 {
		sampleID := hub.sampleSeq.Load() + 1
		if payload == nil {
			payload = jpegs[sampleID%derivedTestShades]
		}
		hub.produceCaptured(payload, frameTimestamp{wallNs: captureTime(sampleID), nativeNs: int64(sampleID),
			nativeClock: "CLOCK_MONOTONIC_V4L2", sequence: uint32(sampleID), sequenceValid: true, auAligned: true, native: true}, videoCodecMJPEG)
		return sampleID
	}
	// producePaced waits for the frame's picture and access unit, so the
	// queues that drop under load have nothing to drop.
	producePaced := func(what string) uint64 {
		t.Helper()
		waitUntil(t, "the child takes frames", layer.feeding.Load)
		wantPictures, wantUnits := pictures.count()+1, units.count()+1
		sampleID := produce(nil)
		deadline := time.Now().Add(20 * time.Second)
		for pictures.count() < wantPictures || units.count() < wantUnits {
			if time.Now().After(deadline) {
				t.Fatalf("%s: sample %d came back as %d of %d pictures and %d of %d access units (layer counts %+v)",
					what, sampleID, pictures.count(), wantPictures, units.count(), wantUnits, layer.counts())
			}
			time.Sleep(time.Millisecond)
		}
		return sampleID
	}

	// A JPEG decodes to its own gray level, or to that level scaled into video
	// range; which one is this GStreamer's choice, read off the first picture.
	var lumaOf func(shade int) int
	var decoded decodedGray
	// Where the burst began in what the layer published, and its counts then.
	burstFrom, beforeBurst := -1, derivedLayerCounts{}
	checkPairing := func(what string) {
		t.Helper()
		// Each access unit must decode to the picture of the frame whose
		// sample ID it carries.
		published := stream.all()
		payloads := make([][]byte, len(published))
		for i, unit := range published {
			if unit.codec != agentpb.VideoCodec_VIDEO_CODEC_H264 || !unit.auAligned || !bytes.HasPrefix(unit.data, []byte{0, 0, 0, 1}) {
				t.Fatalf("%s: published access unit %d: codec=%v auAligned=%v, %d bytes, want one whole Annex B access unit", what, i, unit.codec, unit.auAligned, len(unit.data))
			}
			payloads[i] = unit.data
		}
		// In the burst a tag may leave the table before its output comes back,
		// and that output is not published. The H.264 then has a gap, and what
		// follows a gap does not decode to its own frame.
		if burstFrom >= 0 {
			if left := layer.counts().since(beforeBurst); left.unknownTags+left.unusable > 0 {
				t.Logf("%s: in the burst the layer left out %d outputs whose tag was gone and %d unusable ones, so the H.264 published since access unit %d may have a gap; only the %d before it are decoded",
					what, left.unknownTags, left.unusable, burstFrom, burstFrom)
				payloads = payloads[:burstFrom]
			} else {
				t.Logf("%s: the layer left no output out in the burst, so all %d access units it published are decoded", what, len(payloads))
			}
		}
		for i, picture := range decodeTestH264(t, gstPath, available, skip, payloads) {
			if luma, shade := yuyvMeanLuma(picture), derivedTestShade(published[i].sampleID); !decoded.shows(luma, shade) {
				t.Errorf("%s: access unit %d carries sample %d but decodes to luma %d; that sample's JPEG is gray level %d, which decodes to %s",
					what, i, published[i].sampleID, luma, shade, decoded.lumasOf(shade))
				break
			}
		}
		for i, picture := range pictures.all() {
			if picture.codec != agentpb.VideoCodec_VIDEO_CODEC_RAW || len(picture.data) != derivedTestWidth*2*derivedTestHeight ||
				picture.rawFmt.GetFourcc() != fourccYUYV || picture.rawFmt.GetBytesPerLine() != derivedTestWidth*2 ||
				picture.rawFmt.GetWidth() != derivedTestWidth || picture.rawFmt.GetHeight() != derivedTestHeight {
				t.Fatalf("%s: picture %d: codec=%v bytes=%d format=%v, want a %dx%d YUYV picture", what, i,
					picture.codec, len(picture.data), picture.rawFmt, derivedTestWidth, derivedTestHeight)
			}
			luma, shade := yuyvMeanLuma(picture.data), derivedTestShade(picture.sampleID)
			if lumaOf == nil {
				videoRange := func(shade int) int { return 16 + shade*219/255 }
				switch {
				case luma >= shade-3 && luma <= shade+3:
					lumaOf = func(shade int) int { return shade }
				case luma >= videoRange(shade)-3 && luma <= videoRange(shade)+3:
					lumaOf = videoRange
				default:
					t.Fatalf("%s: the first picture, of sample %d, has luma %d; its JPEG is gray level %d", what, picture.sampleID, luma, shade)
				}
			}
			if want := lumaOf(shade); luma < want-3 || luma > want+3 {
				t.Fatalf("%s: picture %d carries sample %d but shows luma %d; that sample's JPEG shows %d", what, i, picture.sampleID, luma, want)
			}
		}
		for name, frames := range map[string][]*videoFrame{"picture": pictures.all(), "access unit": units.all()} {
			var previous uint64
			for i, frame := range frames {
				if frame.sampleID <= previous {
					t.Fatalf("%s: %s %d carries sample %d after sample %d", what, name, i, frame.sampleID, previous)
				}
				previous = frame.sampleID
				if frame.tsNs != captureTime(frame.sampleID) || frame.nativeNs != int64(frame.sampleID) ||
					frame.sequence != uint32(frame.sampleID) || !frame.sequenceValid || frame.native {
					t.Fatalf("%s: %s %d carries sample %d with capture time %d, native time %d, sequence %d, native=%v; want that sample's own",
						what, name, i, frame.sampleID, frame.tsNs, frame.nativeNs, frame.sequence, frame.native)
				}
			}
		}
		for i, unit := range units.all() {
			if unit.codec != agentpb.VideoCodec_VIDEO_CODEC_H264 || !unit.auAligned || !bytes.HasPrefix(unit.data, []byte{0, 0, 0, 1}) {
				t.Fatalf("%s: access unit %d: codec=%v auAligned=%v, %d bytes, want one whole Annex B access unit", what, i, unit.codec, unit.auAligned, len(unit.data))
			}
		}
	}
	samplesOf := func(frames []*videoFrame) []uint64 {
		ids := make([]uint64, len(frames))
		for i, frame := range frames {
			ids[i] = frame.sampleID
		}
		return ids
	}
	expectExactly := func(what string, want []uint64) {
		t.Helper()
		time.Sleep(200 * time.Millisecond)
		for name, got := range map[string][]uint64{"pictures": samplesOf(pictures.all()), "access units": samplesOf(units.all())} {
			if len(got) != len(want) {
				t.Fatalf("%s: %s carry samples %v, want %v", what, name, got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("%s: %s carry samples %v, want %v", what, name, got, want)
				}
			}
		}
		checkPairing(what)
	}

	var want []uint64
	for i := 0; i < 30; i++ {
		if i == 15 {
			produce(bytes.Repeat([]byte{0x5A, 0xC3, 0x00, 0x7F}, 4096))
		}
		want = append(want, producePaced("30 frames with a garbage one in the middle"))
	}
	expectExactly("30 frames with a garbage one in the middle", want)
	if starts := annexBKeyframeStarts(units.all()[0].data); len(starts) == 0 {
		t.Error("the first access unit is not a keyframe a viewer can start on")
	}
	if counts := layer.counts(); counts.notFed != 0 || counts.unknownTags != 0 || counts.unusable != 0 || counts.childRuns != 1 {
		t.Fatalf("after the garbage frame the layer counts %+v, want one child, nothing unfed, no unknown tag", counts)
	}

	produce(jpegs[0][:len(jpegs[0])/20])
	waitUntil(t, "the child that hit the truncated frame is replaced", func() bool { return layer.counts().childRuns == 2 })
	for i := 0; i < 5; i++ {
		want = append(want, producePaced("5 frames after the child restarted"))
	}
	expectExactly("5 frames after the child restarted", want)
	if starts := annexBKeyframeStarts(units.all()[30].data); len(starts) == 0 {
		t.Error("the first access unit after the restart is not a keyframe")
	}
	if counts := layer.counts(); counts.unknownTags != 0 {
		t.Errorf("the restarted child's outputs named %d unknown tags", counts.unknownTags)
	}

	// A burst faster than the child: frames are refused at the feed queue,
	// tags leave the table as newer ones are assigned, and the child's own
	// queues drop. Whatever comes back must still be its own frame.
	before, picturesBefore := layer.counts(), pictures.count()
	burstFrom, beforeBurst = len(stream.all()), before
	burstEnds := time.Now().Add(30 * time.Second)
	for layer.counts().fed-before.fed < 2*derivedTagTableFrames {
		if time.Now().After(burstEnds) {
			t.Fatalf("the burst fed only %d frames in 30 s", layer.counts().fed-before.fed)
		}
		produce(nil)
	}
	for settled := -1; settled != pictures.count()+units.count(); {
		settled = pictures.count() + units.count()
		time.Sleep(300 * time.Millisecond)
	}
	burst := layer.counts().since(before)
	t.Logf("burst: %+v, %d pictures and %d access units arrived in all", burst, pictures.count(), units.count())
	if burst.notFed == 0 {
		t.Error("the burst never filled the feed queue")
	}
	if pictures.count() == picturesBefore {
		t.Fatal("no picture came back from the burst")
	}
	checkPairing("the burst")
}

// The layer's tags start at 1000 hours, where GStreamer's video encoders leave
// timestamps alone, and the layer subtracts the segment start its child sends
// in case an encoder moves them anyway. This runs the layer's H.264 pipeline
// on a real encoder and checks the tag table's arithmetic against the segment
// start that encoder sends: derivedFrameTags.source must name, for each access
// unit, the frame the unit decodes to. It logs whether the encoder moved the
// timestamps and asserts neither answer.
//
// It does not run the layer's publishOutput: the layer's own tags cannot be
// made to start lower, so a real moved segment never reaches that call. Its
// subtraction there is covered by the scripted segment of
// TestDerivedLayer_UndoesTheShiftTheChildReportsAndDropsMisSizedPictures.
func TestDerivedTagTableUndoesTheSegmentStartARealH264EncoderSends(t *testing.T) {
	gstPath, available, skip := derivedTestGStreamer(t)
	layer, err := newDerivedLayer(derivedLayerConfig{
		logger: zap.NewNop(), device: "/dev/video44", format: videoCodecMJPEG,
		gstPath: gstPath, available: available, width: derivedTestWidth, height: derivedTestHeight, framerate: 30, grace: time.Minute,
	})
	if err != nil {
		skip("this GStreamer cannot run the derived layer: " + err.Error())
	}
	checkTagTableOnARealEncoder(t, layer, available, skip, "timestamps from where the layer's tags start", derivedTagBase)
	checkTagTableOnARealEncoder(t, layer, available, skip, "timestamps from one second", uint64(time.Second))
}

// checkTagTableOnARealEncoder runs the layer's H.264 pipeline in a child of the
// test's on frames timed from first, logs the segment start that child sent,
// and fails unless a tag table of those frames, given that segment start,
// pairs each access unit with the frame the unit decodes to.
func checkTagTableOnARealEncoder(t *testing.T, layer *derivedLayer, available map[string]bool, skip func(why string), what string, first uint64) {
	t.Helper()
	const frames = 8
	jpegs := derivedTestJPEGs(t)
	tags := derivedFrameTags{sources: map[uint64]*videoFrame{}}
	timestamps := make([]uint64, frames)
	for i := range timestamps {
		timestamps[i] = nearestDerivedTag(first + uint64(i)*derivedTestFramePeriod)
		tags.sources[timestamps[i]] = &videoFrame{sampleID: uint64(i + 1)}
	}

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer outR.Close() //nolint:errcheck
	args := layer.pipelineArgs(pipelineShape{encode: true})
	cmd := exec.Command(args[0], args[1:]...)
	cmd.ExtraFiles = []*os.File{outW}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stderr, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	outW.Close() //nolint:errcheck
	go func() {
		defer stdin.Close() //nolint:errcheck
		gdp := &gdpWriter{w: stdin}
		if gdp.streamHeader(layer.transform.inputCaps(layer.width, layer.height, layer.framerate)) != nil {
			return
		}
		for i, timestamp := range timestamps {
			if gdp.buffer(timestamp, jpegs[i+1]) != nil {
				return
			}
			// Slower than the encoder, whose queue drops what it has no room for.
			time.Sleep(10 * time.Millisecond)
		}
	}()
	var units [][]byte
	var came []uint64
	gdp := &gdpReader{r: outR, maxPayload: maxFrameBytes}
	for {
		timestamp, payload, err := gdp.nextBuffer()
		if err != nil {
			break
		}
		units, came = append(units, payload), append(came, timestamp)
	}
	if err := cmd.Wait(); err != nil || len(units) == 0 {
		t.Fatalf("%s: the layer's H.264 pipeline gave %d access units for %d frames: %v\n%s", what, len(units), frames, err, stderr.String())
	}
	moved := "it left the timestamps as fed"
	if gdp.segmentStart != 0 {
		moved = "it moved the timestamps by that much"
	}
	t.Logf("%s: %s sent segment start %d ns and %d access units: %s; checking the tag table's arithmetic on them, not the layer's publishOutput",
		what, layer.encoder.element, gdp.segmentStart, len(units), moved)
	var decoded decodedGray
	for i, picture := range decodeTestH264(t, layer.gstPath, available, skip, units) {
		source, known := tags.source(came[i], gdp.segmentStart)
		if !known {
			t.Fatalf("%s: access unit %d, timed %d ns in a segment starting at %d ns, is paired with no frame; the frames were timed %v",
				what, i, came[i], gdp.segmentStart, timestamps)
		}
		if luma, shade := yuyvMeanLuma(picture), derivedTestShade(source.sampleID); !decoded.shows(luma, shade) {
			t.Fatalf("%s: access unit %d, timed %d ns in a segment starting at %d ns, is paired with frame %d but decodes to luma %d; that frame's JPEG is gray level %d, which decodes to %s",
				what, i, came[i], gdp.segmentStart, source.sampleID, luma, shade, decoded.lumasOf(shade))
		}
	}
}

// Real gst-launch-1.0 and the real layer between a JPEG camera's hub and an
// episode that stores both: every sample must be indexed twice, as the JPEG the
// camera sent and as the H.264 the layer made from it, at one capture time.
func TestStoreBothThroughTheDerivedLayerIndexesEverySampleAsItsJPEGAndItsH264(t *testing.T) {
	gstPath, available, skip := derivedTestGStreamer(t)
	cam := newStoreTestCamera(t)
	// Every reading of the clock mapping differs from the one before, and each
	// time computed reads it anew, so two records of one sample agree only if
	// the second takes the time of the first.
	readings := int64(0)
	cam.adapter.monotonicMapping = func() (data.MonotonicMappingSample, error) {
		readings++
		offset := int64(scriptedBootMinusMonotonic) + readings*int64(time.Microsecond)
		return data.MonotonicMappingSample{BootAfterNanos: -int64(time.Hour),
			OffsetLowerNanos: offset - scriptedMappingHalfWidth, OffsetUpperNanos: offset + scriptedMappingHalfWidth}, nil
	}
	layer, err := newDerivedLayer(derivedLayerConfig{
		logger: zap.NewNop(), device: storeTestCameraPath, format: videoCodecMJPEG,
		gstPath: gstPath, available: available, width: derivedTestWidth, height: derivedTestHeight, framerate: 30,
		grace: time.Minute, demand: cam.hub, publish: cam.hub.broadcastDerivedFrom,
	})
	if err != nil {
		skip("this GStreamer cannot run the derived layer: " + err.Error())
	}
	detach := cam.hub.feedDerivedLayer(layer)
	defer detach()
	layer.offerTo(cam.hub)
	layerCtx, stopLayer := context.WithCancel(context.Background())
	layerDone := make(chan error, 1)
	go func() { layerDone <- layer.run(layerCtx) }()
	defer func() {
		stopLayer()
		<-layerDone
	}()

	const frames = 20
	jpegs := derivedTestJPEGs(t)
	type captured struct {
		jpeg     []byte
		nativeNs int64
	}
	fed := map[uint64]captured{}
	r := cam.record(t, &data.SourceCapture{Store: data.CaptureStoreBoth}, 3, func() {
		waitUntil(t, "the child takes frames", layer.feeding.Load)
		for i := 0; i < frames; i++ {
			sampleID := cam.hub.sampleSeq.Load() + 1
			_, receipt, _, err := data.CaptureReceipt()
			if err != nil {
				t.Fatal(err)
			}
			frame := captured{jpegs[sampleID%derivedTestShades], receipt - int64(scriptedCaptureToReceipt) - int64(scriptedBootMinusMonotonic)}
			fed[sampleID] = frame
			cam.hub.produceCaptured(frame.jpeg, frameTimestamp{wallNs: uint64(time.Now().UnixNano()), nativeNs: frame.nativeNs,
				nativeClock: kernelClock, sequence: uint32(sampleID), sequenceValid: true, auAligned: true, native: true}, videoCodecMJPEG)
			// The frame's access unit is out before the next frame is fed, so
			// the queues that drop under load have nothing to drop.
			deadline := time.Now().Add(20 * time.Second)
			for layer.counts().published < uint64(i+1) {
				if time.Now().After(deadline) {
					t.Fatalf("sample %d came back as no access unit (layer counts %+v)", sampleID, layer.counts())
				}
				time.Sleep(time.Millisecond)
			}
			cam.waitUntilTaken(t)
		}
	}, 2*frames)

	own, video := r.recordsOf(t, indexedAsJPEG), r.recordsOf(t, indexedAsH264)
	if len(own) != frames || len(video) != frames || len(r.index(t)) != 2*frames {
		t.Fatalf("%d JPEG and %d H.264 records among %d, want %d of each for the %d frames fed\ndetail: %s",
			len(own), len(video), len(r.index(t)), frames, frames, r.result.SourceDetail)
	}
	madeFrom := map[uint64]cameraIndexRecord{}
	units := make([][]byte, len(video))
	for i, record := range video {
		if _, twice := madeFrom[record.SampleID]; twice {
			t.Fatalf("sample %d has two H.264 records", record.SampleID)
		}
		if _, known := fed[record.SampleID]; !known {
			t.Fatalf("H.264 record %d names sample %d, which was not fed", i, record.SampleID)
		}
		madeFrom[record.SampleID] = record
		units[i] = r.cut(t, record)
	}
	// With as many records of each kind as frames fed, a sample that is fed
	// and recorded once in each is every sample, exactly once.
	recorded := map[uint64]bool{}
	for i, record := range own {
		frame, known := fed[record.SampleID]
		if !known {
			t.Fatalf("JPEG record %d names sample %d, which was not fed", i, record.SampleID)
		}
		if recorded[record.SampleID] {
			t.Fatalf("sample %d has two JPEG records", record.SampleID)
		}
		recorded[record.SampleID] = true
		if got := r.cut(t, record); !bytes.Equal(got, frame.jpeg) {
			t.Errorf("JPEG record of sample %d cuts %d bytes out of %s at %d that are not the %d-byte JPEG fed as that sample",
				record.SampleID, len(got), record.Segment, record.ByteOffset, len(frame.jpeg))
		}
		made, both := madeFrom[record.SampleID]
		if !both {
			t.Fatalf("sample %d has a JPEG record and no H.264 record", record.SampleID)
		}
		if made.CanonicalEpisodeNanos != record.CanonicalEpisodeNanos || made.CanonicalUncertaintyNanos != record.CanonicalUncertaintyNanos ||
			made.MappingSegment != record.MappingSegment {
			t.Errorf("sample %d: its H.264 record is at %dns +/-%d under %s and its JPEG record at %dns +/-%d under %s; want one time for both",
				record.SampleID, made.CanonicalEpisodeNanos, made.CanonicalUncertaintyNanos, made.MappingSegment,
				record.CanonicalEpisodeNanos, record.CanonicalUncertaintyNanos, record.MappingSegment)
		}
		if made.NativeTimestampNanos != frame.nativeNs || record.NativeTimestampNanos != frame.nativeNs ||
			made.NativeClockDomain != kernelClock || record.NativeClockDomain != kernelClock {
			t.Errorf("sample %d: its records carry native times %d in %q (H.264) and %d in %q (JPEG), want the kernel's %d in %q",
				record.SampleID, made.NativeTimestampNanos, made.NativeClockDomain, record.NativeTimestampNanos, record.NativeClockDomain, frame.nativeNs, kernelClock)
		}
		// The kernel's capture time on the episode timeline, under a mapping
		// read at most twice per frame so far.
		mapped := frame.nativeNs + int64(scriptedBootMinusMonotonic) - int64(recordingOrigin)
		if late := record.CanonicalEpisodeNanos - mapped; late <= 0 || late > 2*frames*int64(time.Microsecond) || record.MappingSegment != "monotonic-boottime-1" {
			t.Errorf("sample %d is at %dns under %s, want its kernel capture time %dns mapped under monotonic-boottime-1",
				record.SampleID, record.CanonicalEpisodeNanos, record.MappingSegment, mapped)
		}
	}
	var decoded decodedGray
	for i, picture := range decodeTestH264(t, gstPath, available, skip, units) {
		if luma, shade := yuyvMeanLuma(picture), derivedTestShade(video[i].SampleID); !decoded.shows(luma, shade) {
			t.Fatalf("H.264 record %d names sample %d but its bytes decode to luma %d; that sample's JPEG is gray level %d, which decodes to %s",
				i, video[i].SampleID, luma, shade, decoded.lumasOf(shade))
		}
	}
	if r.result.Count != 2*frames {
		t.Errorf("count = %d, want %d: %d video frames and %d JPEG frames", r.result.Count, 2*frames, frames, frames)
	}
}

// decodedGray says whether a picture decoded from H.264 shows a gray level. A
// decoder gives the level back as it is, or scaled into video range; which one
// is this GStreamer's choice, read off the first picture that shows either.
type decodedGray struct {
	lumaOf func(shade int) int
}

func videoRangeLuma(shade int) int { return 16 + shade*219/255 }

func (g *decodedGray) shows(luma, shade int) bool {
	near := func(want int) bool { return luma >= want-4 && luma <= want+4 }
	if g.lumaOf == nil {
		switch {
		case near(shade):
			g.lumaOf = func(shade int) int { return shade }
		case near(videoRangeLuma(shade)):
			g.lumaOf = videoRangeLuma
		default:
			return false
		}
	}
	return near(g.lumaOf(shade))
}

func (g *decodedGray) lumasOf(shade int) string {
	if g.lumaOf == nil {
		return fmt.Sprintf("%d or %d", shade, videoRangeLuma(shade))
	}
	return fmt.Sprint(g.lumaOf(shade))
}

// decodeTestH264 decodes access units in a gst-launch-1.0 child of its own,
// which is given no tag of the layer's, and returns the YUYV picture of each
// unit, in the order of the units.
func decodeTestH264(t *testing.T, gstPath string, available map[string]bool, skip func(why string), units [][]byte) [][]byte {
	t.Helper()
	transform := derivedTransforms[agentpb.VideoCodec_VIDEO_CODEC_H264]
	decoder, err := derivedLayerDecoder(transform, available)
	if err != nil {
		skip("this GStreamer cannot decode H.264 to check an access unit's picture: " + err.Error())
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer outR.Close() //nolint:errcheck
	cmd := exec.Command(gstPath, append([]string{"-q"},
		strings.Fields("fdsrc fd=0 ! gdpdepay ! "+decoder.stages+" ! "+yuyvPictureStages+" ! gdppay ! fdsink fd=3")...)...)
	cmd.ExtraFiles = []*os.File{outW}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stderr, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	outW.Close() //nolint:errcheck
	go func() {
		defer stdin.Close() //nolint:errcheck
		gdp := &gdpWriter{w: stdin}
		if gdp.streamHeader(transform.inputCaps(derivedTestWidth, derivedTestHeight, 30)) != nil {
			return
		}
		for i, unit := range units {
			if gdp.buffer(derivedTagBase+uint64(i)*derivedTestFramePeriod, unit) != nil {
				return
			}
		}
	}()
	var pictures [][]byte
	var problem string
	gdp := &gdpReader{r: outR, maxPayload: maxRawFrameBytes}
	for {
		timestamp, payload, err := gdp.nextBuffer()
		if err != nil {
			break
		}
		of := int((timestamp - gdp.segmentStart - derivedTagBase + derivedTestFramePeriod/2) / derivedTestFramePeriod)
		if problem == "" && (of != len(pictures) || len(payload) != derivedTestWidth*2*derivedTestHeight) {
			problem = fmt.Sprintf("picture %d is %d bytes and is timed as the picture of unit %d", len(pictures), len(payload), of)
		}
		pictures = append(pictures, payload)
	}
	if err := cmd.Wait(); err != nil || problem != "" || len(pictures) != len(units) {
		t.Fatalf("decoding %d access units gave %d pictures: %v %s\n%s", len(units), len(pictures), err, problem, stderr.String())
	}
	return pictures
}

// derivedTestGStreamer is the real GStreamer of this machine, or a skip.
func derivedTestGStreamer(t *testing.T) (gstPath string, available map[string]bool, skip func(why string)) {
	t.Helper()
	skip = func(why string) {
		t.Helper()
		if os.Getenv("WENDY_GSTREAMER_REQUIRED") == "1" {
			t.Fatal(why)
		}
		t.Skip(why)
	}
	if _, err := exec.LookPath("gst-launch-1.0"); err != nil {
		skip("GStreamer is not installed")
	}
	gstPath, available, err := gstDiscover()
	if err != nil {
		skip("GStreamer elements could not be listed: " + err.Error())
	}
	return gstPath, available, skip
}

// h264TestUnit is one access unit of a test stream and the frame it encodes,
// counted in the order the frames were shown to the encoder.
type h264TestUnit struct {
	data  []byte
	frame int
}

const h264TestByteStream = " ! video/x-h264,profile=high ! h264parse config-interval=-1 ! video/x-h264,stream-format=byte-stream,alignment=au"

// encodeTestH264 encodes one flat gray frame per sample shade and returns the
// access units in the order an encoder sends them.
func encodeTestH264(t *testing.T, gstPath string, frames int, encoderStages string) []h264TestUnit {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer outR.Close() //nolint:errcheck
	cmd := exec.Command(gstPath, append([]string{"-q"}, strings.Fields("fdsrc fd=0 ! gdpdepay ! jpegdec ! "+encoderStages+" ! gdppay ! fdsink fd=3")...)...)
	cmd.ExtraFiles = []*os.File{outW}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stderr, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	outW.Close() //nolint:errcheck
	jpegs := derivedTestJPEGs(t)
	go func() {
		defer stdin.Close() //nolint:errcheck
		gdp := &gdpWriter{w: stdin}
		if gdp.streamHeader(derivedTransforms[videoCodecMJPEG].inputCaps(derivedTestWidth, derivedTestHeight, 30)) != nil {
			return
		}
		for frame := 0; frame < frames; frame++ {
			if gdp.buffer(derivedTagBase+uint64(frame)*derivedTestFramePeriod, jpegs[frame%derivedTestShades]) != nil {
				return
			}
		}
	}()
	var units []h264TestUnit
	gdp := &gdpReader{r: outR, maxPayload: maxFrameBytes}
	for {
		timestamp, payload, err := gdp.nextBuffer()
		if err != nil {
			break
		}
		shown := timestamp - gdp.segmentStart - derivedTagBase
		units = append(units, h264TestUnit{data: payload, frame: int((shown + derivedTestFramePeriod/2) / derivedTestFramePeriod)})
	}
	if err := cmd.Wait(); err != nil || len(units) != frames {
		t.Fatalf("encoding %d test frames gave %d access units: %v\n%s", frames, len(units), err, stderr.String())
	}
	return units
}

// h264PicturesTest runs the real H.264 picture layer on a real hub, as the
// capture of a camera that produces H.264 runs it, with the camera replaced by
// access units produced into the hub as native capture does. The service's
// share policy sees that camera through the seams of shareTestService.
type h264PicturesTest struct {
	t        *testing.T
	svc      *VideoService
	hub      *deviceHub
	layer    *derivedLayer
	pictures derivedFramesSeen
	shadeOf  map[uint64]int
	lumaOf   func(shade int) int
}

func startH264PicturesTest(t *testing.T, gstPath string, available map[string]bool, skip func(string)) *h264PicturesTest {
	t.Helper()
	svc := shareTestService(t, []uint32{v4l2PixFmtYUYV, v4l2PixFmtH264},
		map[uint32][][2]uint32{v4l2PixFmtH264: {{derivedTestWidth, derivedTestHeight}}}, []uint32{30})
	svc.logger, svc.encoderIdleGrace = zap.NewNop(), 200*time.Millisecond
	svc.gstMemo.gstPath, svc.gstMemo.available = gstPath, available
	hub, cancelHub := newTestHub(t)
	t.Cleanup(cancelHub)
	hub.sampleSeq = new(atomic.Uint64)
	own, declined, _ := svc.ownFramesAtDefaults(usbCam().GetPath())
	if declined != "" || own.codec != agentpb.VideoCodec_VIDEO_CODEC_H264 || own.width != derivedTestWidth || own.height != derivedTestHeight {
		t.Fatalf("the test camera's own frames = %+v (declined %q), want %dx%d H.264", own, declined, derivedTestWidth, derivedTestHeight)
	}
	layer, whyNot := svc.pictureLayerFor(hub, usbCam().GetPath(), own)
	if layer == nil {
		skip("this GStreamer cannot decode pictures from H.264: " + whyNot)
	}
	t.Logf("H.264 decoder: %s", layer.decoder.stages)
	p := &h264PicturesTest{t: t, svc: svc, hub: hub, layer: layer, shadeOf: map[uint64]int{}}
	// The viewer keeps the hub's audience while the pixel reader comes and goes.
	idViewer, viewer, err := hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range viewer {
		}
	}()
	layer.offerTo(hub)
	ctx, stop := context.WithCancel(context.Background())
	captureEnded := make(chan error, 1)
	go func() {
		captureEnded <- svc.captureDeriving(ctx, hub, layer, func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	t.Cleanup(func() {
		stop()
		if err := <-captureEnded; !errors.Is(err, context.Canceled) {
			t.Errorf("the capture ended with %v before the test stopped it: its viewers lost the camera", err)
		}
		hub.unsubscribe(idViewer)
	})
	return p
}

// whyPicturesGivenUp is the reason the capture remembers once the layer has
// given the camera's pictures up.
func (p *h264PicturesTest) whyPicturesGivenUp() (why string, givenUp bool) {
	return p.svc.derivedFailures.recent(p.svc.cameraIdentityKey(usbCam().GetPath()))
}

func (p *h264PicturesTest) readPictures() (leave func()) {
	p.t.Helper()
	id, ch, err := p.hub.subscribePixels()
	if err != nil {
		p.t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.pictures.collect(ch)
	}()
	return func() {
		p.hub.mu.Lock()
		if sub := p.hub.subs[id]; sub != nil && !sub.closed {
			sub.closed = true
			close(sub.ch)
		}
		p.hub.mu.Unlock()
		p.hub.unsubscribe(id)
		<-done
	}
}

// capture hands the hub one access unit as native capture does.
func (p *h264PicturesTest) capture(unit h264TestUnit) (sampleID uint64) {
	sampleID = p.hub.sampleSeq.Load() + 1
	p.shadeOf[sampleID] = derivedTestShade(uint64(unit.frame % derivedTestShades))
	p.hub.produceCaptured(unit.data, frameTimestamp{wallNs: derivedTestCaptureEpoch + sampleID*derivedTestFramePeriod, nativeNs: int64(sampleID),
		nativeClock: "CLOCK_MONOTONIC_V4L2", sequence: uint32(sampleID), sequenceValid: true, auAligned: true, native: true},
		agentpb.VideoCodec_VIDEO_CODEC_H264)
	return sampleID
}

// produceTogether hands the hub as many access units as the feed queue holds
// with no wait between them, so the last is fed before the picture of the
// first comes back, and then waits until the layer has fed them all.
func (p *h264PicturesTest) produceTogether(units []h264TestUnit) {
	p.t.Helper()
	before := p.layer.counts()
	var last uint64
	for _, unit := range units {
		last = p.capture(unit)
	}
	deadline := time.Now().Add(10 * time.Second)
	for p.layer.counts().fed < before.fed+uint64(len(units)) {
		if time.Now().After(deadline) {
			p.t.Fatalf("the layer did not feed the %d frames up to sample %d (counts %+v, before them %+v)", len(units), last, p.layer.counts(), before)
		}
		time.Sleep(time.Millisecond)
	}
	p.awaitPictureOf(last)
}

// produce hands the hub one access unit as native capture does and waits until
// the layer has fed it or passed it over, so the feed queue drops nothing.
func (p *h264PicturesTest) produce(unit h264TestUnit) (sampleID uint64, fed bool) {
	p.t.Helper()
	before := p.layer.counts()
	sampleID = p.capture(unit)
	deadline := time.Now().Add(10 * time.Second)
	for {
		counts := p.layer.counts()
		if counts.fed > before.fed {
			p.awaitPictureOf(sampleID)
			return sampleID, true
		}
		if _, givenUp := p.whyPicturesGivenUp(); givenUp || counts.awaitedStart > before.awaitedStart {
			return sampleID, false
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("the layer neither fed sample %d nor passed it over (counts %+v)", sampleID, counts)
		}
		time.Sleep(time.Millisecond)
	}
}

// awaitPictureOf gives a fed frame's picture time to arrive, so the child's
// queues, which drop under load, have nothing to drop. A decoder may hold its
// newest pictures back, so the wait is bounded.
func (p *h264PicturesTest) awaitPictureOf(sampleID uint64) {
	deadline := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(deadline) {
		pictures := p.pictures.all()
		if len(pictures) > 0 && pictures[len(pictures)-1].sampleID >= sampleID {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func (p *h264PicturesTest) settledPictures() []*videoFrame {
	for settled := -1; settled != p.pictures.count(); {
		settled = p.pictures.count()
		time.Sleep(400 * time.Millisecond)
	}
	return p.pictures.all()
}

// checkPictures fails unless every picture shows the frame whose identity it
// carries, in the order the frames were produced.
func (p *h264PicturesTest) checkPictures(what string, pictures []*videoFrame) {
	p.t.Helper()
	var previous uint64
	for i, picture := range pictures {
		if picture.codec != agentpb.VideoCodec_VIDEO_CODEC_RAW || len(picture.data) != derivedTestWidth*2*derivedTestHeight ||
			picture.rawFmt.GetFourcc() != fourccYUYV || picture.rawFmt.GetWidth() != derivedTestWidth || picture.rawFmt.GetHeight() != derivedTestHeight {
			p.t.Fatalf("%s: picture %d: codec=%v bytes=%d format=%v, want a %dx%d YUYV picture", what, i,
				picture.codec, len(picture.data), picture.rawFmt, derivedTestWidth, derivedTestHeight)
		}
		if picture.sampleID <= previous {
			p.t.Fatalf("%s: picture %d carries sample %d after sample %d", what, i, picture.sampleID, previous)
		}
		previous = picture.sampleID
		if picture.tsNs != derivedTestCaptureEpoch+picture.sampleID*derivedTestFramePeriod || picture.nativeNs != int64(picture.sampleID) ||
			picture.sequence != uint32(picture.sampleID) || !picture.sequenceValid || picture.native {
			p.t.Fatalf("%s: picture %d carries sample %d with capture time %d, native time %d, sequence %d, native=%v; want that sample's own",
				what, i, picture.sampleID, picture.tsNs, picture.nativeNs, picture.sequence, picture.native)
		}
		luma, shade := yuyvMeanLuma(picture.data), p.shadeOf[picture.sampleID]
		if p.lumaOf == nil {
			videoRange := func(shade int) int { return 16 + shade*219/255 }
			switch {
			case luma >= shade-4 && luma <= shade+4:
				p.lumaOf = func(shade int) int { return shade }
			case luma >= videoRange(shade)-4 && luma <= videoRange(shade)+4:
				p.lumaOf = videoRange
			default:
				p.t.Fatalf("%s: picture %d carries sample %d but shows luma %d; that sample's frame shows %d, or %d in video range",
					what, i, picture.sampleID, luma, shade, videoRange(shade))
			}
		}
		if want := p.lumaOf(shade); luma < want-4 || luma > want+4 {
			p.t.Fatalf("%s: picture %d carries sample %d but shows luma %d; that sample's frame shows %d", what, i, picture.sampleID, luma, want)
		}
	}
}

func h264TestStarts(units []h264TestUnit) (starts []int) {
	for i, unit := range units {
		if _, startsHere := h264RandomAccessOffset(unit.data); startsHere {
			starts = append(starts, i)
		}
	}
	return starts
}

// Real gst-launch-1.0, a real encoder and a real decoder: each decoded picture
// must show the shade of the frame whose sample ID it carries, and no child may
// be fed a frame from before the one it can start on.
func TestDerivedLayerDecodesEachH264AccessUnitToThePictureOfItsOwnFrame(t *testing.T) {
	gstPath, available, skip := derivedTestGStreamer(t)
	encoder, err := derivedLayerEncoder(available)
	if err != nil {
		skip("this GStreamer has no H.264 encoder to make a test stream with: " + err.Error())
	}
	encoderStages := encoderSegment(encoder.element, encoder.hasH264Parse, 15, noPreset)
	if available["x264enc"] {
		// Without scene cuts every 15th frame alone is one decoding can start on.
		encoderStages = "videoconvert ! video/x-raw,format=I420 ! x264enc tune=zerolatency key-int-max=15 bframes=0 option-string=scenecut=0" + h264TestByteStream
	}
	p := startH264PicturesTest(t, gstPath, available, skip)
	// The last 30 are fed as many at a time as the feed queue holds.
	const paced = 90
	units := encodeTestH264(t, gstPath, paced+30, encoderStages)
	starts := h264TestStarts(units)
	t.Logf("test stream: %d access units, decoding can start at %v", len(units), starts)
	for i, unit := range units {
		if unit.frame != i {
			t.Fatalf("access unit %d encodes frame %d: this encoder reorders, and a camera does not", i, unit.frame)
		}
	}
	if len(starts) < 4 || starts[0] != 0 {
		t.Fatalf("the test stream offers %v as starts, want its first unit and three more", starts)
	}

	leave := p.readPictures()
	waitUntil(t, "the first child takes frames", p.layer.feeding.Load)
	var want []uint64
	produceFrom := func(what string, from, to int) {
		t.Helper()
		for _, unit := range units[from:to] {
			_, startsHere := h264RandomAccessOffset(unit.data)
			mayFeed := len(want) > 0 || startsHere
			sampleID, fed := p.produce(unit)
			if fed != mayFeed {
				t.Fatalf("%s: frame %d (sample %d, a start: %v) fed=%v, want %v", what, unit.frame, sampleID, startsHere, fed, mayFeed)
			}
			if fed {
				want = append(want, sampleID)
			}
		}
	}
	// The first child sees the stream from a frame behind its first start.
	produceFrom("the first child", 1, starts[2])
	first := append([]uint64(nil), want...)
	if len(first) != starts[2]-starts[1] {
		t.Fatalf("the first child was fed %d frames, want the %d from the stream's second start on", len(first), starts[2]-starts[1])
	}
	pictures := p.settledPictures()
	p.checkPictures("the first child", pictures)
	// A decoder may still hold its newest pictures.
	const heldBack = 3
	if len(pictures) < len(first)-heldBack || pictures[0].sampleID != first[0] {
		t.Fatalf("the first child was fed %d frames and gave %d pictures; want nearly all, from sample %d on", len(first), len(pictures), first[0])
	}
	t.Logf("the first child was fed %d frames and gave %d pictures", len(first), len(pictures))
	if counts := p.layer.counts(); counts.unknownTags != 0 || counts.unusable != 0 || counts.notFed != 0 || counts.childRuns != 1 {
		t.Fatalf("after the first child the layer counts %+v, want one child, no unknown tag and nothing unusable", counts)
	}

	// The pixel reader leaves, its child is stopped, and it comes back to a
	// stream that is between two starts.
	leave()
	waitUntil(t, "the idle child is stopped", func() bool { return !p.layer.feeding.Load() })
	leave = p.readPictures()
	defer leave()
	waitUntil(t, "the second child takes frames", func() bool { return p.layer.feeding.Load() && p.layer.counts().childRuns == 2 })
	want = nil
	produceFrom("the second child", starts[2]+1, paced)
	second := want
	if len(second) != paced-starts[3] {
		t.Fatalf("the second child was fed %d frames, want the %d from the stream's fourth start on", len(second), paced-starts[3])
	}
	pictures = p.settledPictures()
	p.checkPictures("the second child", pictures)
	var afterRestart []*videoFrame
	for _, picture := range pictures {
		if picture.sampleID > first[len(first)-1] {
			afterRestart = append(afterRestart, picture)
		}
	}
	if len(afterRestart) < len(second)-heldBack || afterRestart[0].sampleID != second[0] {
		t.Fatalf("the second child was fed %d frames and gave %d pictures; want nearly all, from sample %d on", len(second), len(afterRestart), second[0])
	}
	t.Logf("the second child was fed %d frames and gave %d pictures", len(second), len(afterRestart))
	if counts := p.layer.counts(); counts.unknownTags != 0 || counts.unusable != 0 || counts.notFed != 0 {
		t.Fatalf("the layer counts %+v, want no unknown tag and nothing unusable", counts)
	}

	// Until here every picture came back before the next frame was fed, so the
	// newest frame fed was always the picture's own.
	before := len(pictures)
	for at := paced; at < len(units); at += derivedFeedQueueFrames {
		p.produceTogether(units[at:min(at+derivedFeedQueueFrames, len(units))])
	}
	pictures = p.settledPictures()
	p.checkPictures("frames fed together", pictures)
	if got, fed := len(pictures)-before, len(units)-paced; got < fed-heldBack {
		t.Fatalf("%d frames fed together gave %d pictures; want nearly all", fed, got)
	}
	t.Logf("%d frames fed %d at a time gave %d pictures", len(units)-paced, derivedFeedQueueFrames, len(pictures)-before)
	if counts := p.layer.counts(); counts.unknownTags != 0 || counts.unusable != 0 || counts.notFed != 0 {
		t.Fatalf("after frames fed together the layer counts %+v, want no unknown tag, nothing unusable and nothing unfed", counts)
	}
}

// A decoder hands B pictures back out of turn and GStreamer then moves
// timestamps between pictures, so a tag would name another frame. The layer
// must give such a stream up before it pairs a picture with the wrong frame,
// and the camera's share must then be its own H.264.
func TestDerivedLayerPairsNoPictureOfAnH264StreamItsDecoderReorders(t *testing.T) {
	gstPath, available, skip := derivedTestGStreamer(t)
	if !available["x264enc"] {
		skip("x264enc, the one encoder asked for B frames here, is not installed")
	}
	p := startH264PicturesTest(t, gstPath, available, skip)
	picturesMode := sharedMode{derivedTestWidth, derivedTestHeight, "YUYV", derivedTestWidth * 2, 30, appconfig.CameraFormatPixels, 0}
	if d := p.svc.shareDecisionFor(context.Background(), usbCam(), appconfig.CameraFormatPixels); d != (shareDecision{Shared: true, Mode: picturesMode}) {
		t.Fatalf("before the stream is seen the camera is shared as %+v, want pictures", d)
	}
	units := encodeTestH264(t, gstPath, 30, "videoconvert ! video/x-raw,format=I420 ! x264enc bframes=2 b-adapt=0 key-int-max=10 option-string=scenecut=0"+h264TestByteStream)
	firstReordered := -1
	for i, unit := range units {
		if h264PictureIsBidirectional(unit.data) && firstReordered < 0 {
			firstReordered = i
		}
	}
	if firstReordered < 2 || units[firstReordered].frame >= units[firstReordered-1].frame {
		t.Fatalf("the test stream's first B picture is unit %d; want one that follows a later frame", firstReordered)
	}

	idPictures, chPictures, err := p.hub.subscribePixels()
	if err != nil {
		t.Fatal(err)
	}
	defer p.hub.unsubscribe(idPictures)
	turnedAway := make(chan struct{})
	go func() {
		defer close(turnedAway)
		p.pictures.collect(chPictures)
	}()
	waitUntil(t, "the child takes frames", p.layer.feeding.Load)
	for i, unit := range units {
		sampleID, fed := p.produce(unit)
		if fed != (i < firstReordered) {
			t.Fatalf("unit %d (frame %d, sample %d) fed=%v; want every unit ahead of the first B picture and none after", i, unit.frame, sampleID, fed)
		}
	}

	// The guard fired, once, for good.
	why, givenUp := p.whyPicturesGivenUp()
	if !givenUp || why != errDerivedFramesReordered.Error() {
		t.Fatalf("pictures given up: %v, because %q; want the reordered stream named", givenUp, why)
	}
	if counts := p.layer.counts(); counts.fed != uint64(firstReordered) || counts.childRuns != 1 {
		t.Fatalf("the layer counts %+v, want %d frames fed to one child and the B picture never fed", counts, firstReordered)
	}
	p.svc.derivedFailures.now = func() time.Time { return time.Now().Add(100 * captureFailureRetryAfter) }
	if _, still := p.whyPicturesGivenUp(); !still {
		t.Error("a stream that reorders is forgotten after the bounded time")
	}

	// No picture was published, and the pixel reader was told why.
	select {
	case <-turnedAway:
	case <-time.After(5 * time.Second):
		t.Fatal("the pixel reader was left waiting on pictures that will not come")
	}
	if refusal := p.hub.subscriberErr(idPictures); !errors.Is(refusal, errPixelsUnavailable) || !strings.Contains(refusal.Error(), why) {
		t.Fatalf("pixel refusal = %v, want errPixelsUnavailable naming the reordered stream", refusal)
	}
	if published := p.pictures.all(); len(published) != 0 || p.layer.counts().published != 0 {
		t.Fatalf("%d pictures of a reordered stream were published (the layer counts %d)", len(published), p.layer.counts().published)
	}

	// The camera's share falls back to its own H.264, and a picture node made
	// earlier holds the reason.
	whyNoPictures := "decoding this camera's frames failed: " + why
	fallback := p.svc.shareDecisionFor(context.Background(), usbCam(), appconfig.CameraFormatPixels)
	wantFallback := shareDecision{Shared: true, AskedFormat: appconfig.CameraFormatPixels, AskedWhyNot: whyNoPictures,
		Mode: compressedMode(agentpb.VideoCodec_VIDEO_CODEC_H264, appconfig.CameraFormatNative, derivedTestWidth, derivedTestHeight, 30)}
	if fallback != wantFallback {
		t.Fatalf("after the stream is seen the camera is shared as %+v\nwant %+v", fallback, wantFallback)
	}
	if got, want := p.svc.nodeUnservedAt(usbCam().GetPath(), picturesMode, appconfig.CameraStampTime), fmt.Sprintf(picturesNotMade, whyNoPictures); got != want {
		t.Fatalf("a picture node made before = %q\nwant %q", got, want)
	}
}
