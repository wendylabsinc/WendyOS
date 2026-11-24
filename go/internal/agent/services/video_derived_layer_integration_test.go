package services

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

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

	layer, err := newDerivedLayer(derivedLayerConfig{
		logger: zap.NewNop(), device: "/dev/video44", format: videoCodecMJPEG,
		gstPath: gstPath, available: available, width: derivedTestWidth, height: derivedTestHeight, framerate: 30,
		grace: time.Minute, demand: hub, publish: hub.broadcastDerivedFrom,
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
	checkPairing := func(what string) {
		t.Helper()
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
