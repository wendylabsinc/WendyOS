package services

import (
	"bytes"
	"context"
	"image"
	"image/color"
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

func derivedTestJPEG(t *testing.T, width, height, shade int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: uint8(shade * 8), G: uint8(x), B: uint8(y), A: 255})
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, nil); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
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

	const width, height = 320, 240
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	hub, cancelHub := newTestHub(t)
	defer cancelHub()
	hub.sampleSeq = new(atomic.Uint64)

	layer, err := newDerivedLayer(derivedLayerConfig{
		logger: zap.NewNop(), device: "/dev/video44", format: videoCodecMJPEG,
		gstPath: gstPath, available: available, width: width, height: height, framerate: 30,
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
	closeSubscribers := func() {
		hub.mu.Lock()
		for _, id := range []int{idPictures, idUnits} {
			if sub := hub.subs[id]; sub != nil && !sub.closed {
				sub.closed = true
				close(sub.ch)
			}
		}
		hub.mu.Unlock()
		collecting.Wait()
	}
	defer closeSubscribers()

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

	type sourceFrame struct {
		sampleID uint64
		tsNs     uint64
		nativeNs int64
	}
	var produced uint64
	produce := func(payload []byte) sourceFrame {
		t.Helper()
		// Capture never waits for the layer. The test does, so that a frame the
		// layer had no room for cannot be mistaken for one the child lost.
		waitUntil(t, "the child takes frames", layer.feeding.Load)
		waitUntil(t, "the feed queue has room", func() bool { return len(layer.feed) == 0 })
		produced++
		source := sourceFrame{sampleID: produced, tsNs: 1_700_000_000_000_000_000 + produced*33_333_333, nativeNs: int64(produced) * 33_333_333}
		if !hub.produceCaptured(payload, frameTimestamp{wallNs: source.tsNs, nativeNs: source.nativeNs,
			nativeClock: "CLOCK_MONOTONIC_V4L2", sequence: uint32(produced), sequenceValid: true, auAligned: true, native: true}, videoCodecMJPEG) {
			t.Fatal("the hub has no subscribers")
		}
		return source
	}
	expectDerived := func(what string, want []sourceFrame) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for len(pictures.all()) < len(want) || len(units.all()) < len(want) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: %d pictures and %d access units arrived, want %d of each (layer counts %+v)",
					what, len(pictures.all()), len(units.all()), len(want), layer.counts())
			}
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(200 * time.Millisecond)
		for name, got := range map[string][]*videoFrame{"picture": pictures.all(), "access unit": units.all()} {
			if len(got) != len(want) {
				t.Fatalf("%s: %d %ss arrived, want %d", what, len(got), name, len(want))
			}
			for i, frame := range got {
				if frame.sampleID != want[i].sampleID || frame.tsNs != want[i].tsNs || frame.nativeNs != want[i].nativeNs {
					t.Fatalf("%s: %s %d carries sample %d captured at %d (native %d), want sample %d at %d (native %d)", what, name, i,
						frame.sampleID, frame.tsNs, frame.nativeNs, want[i].sampleID, want[i].tsNs, want[i].nativeNs)
				}
				if frame.native || frame.sequence != uint32(want[i].sampleID) || !frame.sequenceValid {
					t.Fatalf("%s: %s %d native=%v sequence=%d", what, name, i, frame.native, frame.sequence)
				}
			}
		}
	}

	var want []sourceFrame
	for i := 0; i < 30; i++ {
		if i == 15 {
			produce(bytes.Repeat([]byte{0x5A, 0xC3, 0x00, 0x7F}, 4096))
		}
		want = append(want, produce(derivedTestJPEG(t, width, height, i)))
	}
	expectDerived("30 frames with a garbage one in the middle", want)

	wantPicture := &agentpb.RawFormat{Width: width, Height: height, Fourcc: fourccYUYV, BytesPerLine: width * 2}
	for i, picture := range pictures.all() {
		if picture.codec != agentpb.VideoCodec_VIDEO_CODEC_RAW || len(picture.data) != width*2*height ||
			picture.rawFmt.GetFourcc() != wantPicture.Fourcc || picture.rawFmt.GetBytesPerLine() != wantPicture.BytesPerLine ||
			picture.rawFmt.GetWidth() != width || picture.rawFmt.GetHeight() != height {
			t.Fatalf("picture %d: codec=%v bytes=%d format=%v, want %d bytes of %v", i, picture.codec, len(picture.data), picture.rawFmt, width*2*height, wantPicture)
		}
	}
	for i, unit := range units.all() {
		if unit.codec != agentpb.VideoCodec_VIDEO_CODEC_H264 || !unit.auAligned || !bytes.HasPrefix(unit.data, []byte{0, 0, 0, 1}) {
			t.Fatalf("access unit %d: codec=%v auAligned=%v, %d bytes, want one whole Annex B access unit", i, unit.codec, unit.auAligned, len(unit.data))
		}
	}
	if starts := annexBKeyframeStarts(units.all()[0].data); len(starts) == 0 {
		t.Error("the first access unit is not a keyframe a viewer can start on")
	}
	if counts := layer.counts(); counts.notFed != 0 || counts.unknownTags != 0 || counts.childRuns != 1 {
		t.Fatalf("after the garbage frame the layer counts %+v, want one child, nothing unfed, no unknown tag", counts)
	}

	whole := derivedTestJPEG(t, width, height, 31)
	produce(whole[:len(whole)/20])
	waitUntil(t, "the child that hit the truncated frame is replaced", func() bool { return layer.counts().childRuns == 2 })
	for i := 0; i < 5; i++ {
		want = append(want, produce(derivedTestJPEG(t, width, height, 32+i)))
	}
	expectDerived("5 frames after the child restarted", want)
	if starts := annexBKeyframeStarts(units.all()[30].data); len(starts) == 0 {
		t.Error("the first access unit after the restart is not a keyframe")
	}
	if counts := layer.counts(); counts.unknownTags != 0 {
		t.Errorf("the restarted child's outputs named %d unknown tags", counts.unknownTags)
	}
}
