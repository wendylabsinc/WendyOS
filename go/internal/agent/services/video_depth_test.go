package services

import (
	"bytes"
	"context"
	"encoding/binary"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/shared/streamreason"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

const (
	testDepthNode = "/dev/video7"
	testDepthID   = "v4l2:/dev/video7"
	testMJPEGNode = "/dev/video8"
)

// withPixelFormats describes which formats each node lists through
// VIDIOC_ENUM_FMT. A node not in the map lists nothing.
func withPixelFormats(t *testing.T, byNode map[string][]uint32) {
	t.Helper()
	prev := devicePixelFormats
	devicePixelFormats = func(path string) []uint32 { return byNode[path] }
	t.Cleanup(func() { devicePixelFormats = prev })
}

// fakeDepthCapture stands in for the native V4L2 depth capture. It negotiates
// nothing: it reports the packed stride, then delivers one frame per value sent
// on frames, and holds the "device" until the producer's context ends.
type fakeDepthCapture struct {
	frames   chan []byte
	started  chan [2]uint32
	returned chan struct{}
}

func installFakeDepthCapture(t *testing.T) *fakeDepthCapture {
	t.Helper()
	fake := &fakeDepthCapture{frames: make(chan []byte), started: make(chan [2]uint32, 1), returned: make(chan struct{})}
	prev := captureDepthFrames
	captureDepthFrames = func(ctx context.Context, _ *VideoService, _ string, width, height uint32, offered func(uint32), deliver func([]byte) bool) error {
		defer close(fake.returned)
		offered(twoBytesPerPixel(width))
		fake.started <- [2]uint32{width, height}
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case frame := <-fake.frames:
				if !deliver(frame) {
					return nil
				}
			}
		}
	}
	t.Cleanup(func() { captureDepthFrames = prev })
	return fake
}

// depthFrame builds a packed Z16 frame whose every pixel holds millimetres.
func depthFrame(width, height uint32, millimetres uint16) []byte {
	frame := make([]byte, depthFrameBytes(width, height))
	for i := 0; i < len(frame); i += 2 {
		binary.LittleEndian.PutUint16(frame[i:], millimetres)
	}
	return frame
}

func TestIsDepthNode_ClassifiesByAdvertisedZ16(t *testing.T) {
	withPixelFormats(t, map[string][]uint32{
		testDepthNode:  {v4l2PixFmtZ16},
		"/dev/video9":  {v4l2PixFmtGrey, v4l2PixFmtZ16}, // a depth node that also lists infrared
		testMJPEGNode:  {v4l2PixFmtMJPEG},
		"/dev/video10": {v4l2PixFmtYUYV, v4l2PixFmtMJPEG},
		"/dev/video11": {v4l2PixFmtY16}, // thermal, not depth
	})
	for path, want := range map[string]bool{
		testDepthNode: true, "/dev/video9": true,
		testMJPEGNode: false, "/dev/video10": false, "/dev/video11": false, "/dev/video12": false,
	} {
		if got := isDepthNode(path); got != want {
			t.Errorf("isDepthNode(%s) = %v, want %v", path, got, want)
		}
	}
}

func TestDepthFrameSize_LargestZ16ModeWithinTheRawFrameLimit(t *testing.T) {
	cases := []struct {
		name  string
		modes [][2]uint32
		w, h  uint32
	}{
		// 1920x1080 Z16 is 4147200 bytes, over maxRawFrameBytes; 1280x720 fits.
		{"skips a mode over the limit", [][2]uint32{{480, 270}, {1920, 1080}, {848, 480}, {1280, 720}}, 1280, 720},
		{"one mode", [][2]uint32{{640, 480}}, 640, 480},
		{"nothing fits", [][2]uint32{{1920, 1080}}, 0, 0},
		{"no Z16 modes", nil, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withRawModes(t, map[uint32][][2]uint32{v4l2PixFmtZ16: tc.modes, v4l2PixFmtYUYV: {{3840, 2160}}})
			if w, h := depthFrameSize(testDepthNode); w != tc.w || h != tc.h {
				t.Fatalf("depthFrameSize = %dx%d, want %dx%d", w, h, tc.w, tc.h)
			}
		})
	}
}

// TestCapturePixelFormatsIncludeZ16 pins the fix that keeps a Z16-only node
// from being refused as advertising no size: bestDefaultFrameSizeForDevice and
// deviceAdvertisesFrameSize both walk this list.
func TestCapturePixelFormatsIncludeZ16(t *testing.T) {
	want := map[uint32]bool{v4l2PixFmtYUYV: false, v4l2PixFmtMJPEG: false, v4l2PixFmtZ16: false}
	for _, f := range capturePixelFormats {
		if _, ok := want[f]; ok {
			want[f] = true
		}
	}
	for f, seen := range want {
		if !seen {
			t.Errorf("capturePixelFormats is missing %s", fourccLabel(f))
		}
	}
}

func fourccLabel(f uint32) string {
	return string([]byte{byte(f), byte(f >> 8), byte(f >> 16), byte(f >> 24)})
}

// TestSubscribeSensorRaw_DepthNodeDeliversZ16OnTheCanonicalTimeline drives the
// real hub and the real runProducer against a Z16-only node whose capture is a
// fake frame source. The node must be classified as depth and get a raw-only
// hub, and two frames must reach a SubscribeSensorRaw subscriber described as
// z16 at the enumerated size, with raw sample ids 1 and 2 and boot-clock
// receipts inside the bracket of their delivery.
func TestSubscribeSensorRaw_DepthNodeDeliversZ16OnTheCanonicalTimeline(t *testing.T) {
	withPixelFormats(t, map[string][]uint32{testDepthNode: {v4l2PixFmtZ16}})
	withRawModes(t, map[uint32][][2]uint32{v4l2PixFmtZ16: {{640, 480}, {848, 480}, {1920, 1080}}})
	fake := installFakeDepthCapture(t)
	svc := newTestVideoService(nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sub, err := svc.SubscribeSensorRaw(ctx, testDepthID)
	if err != nil {
		t.Fatalf("SubscribeSensorRaw: %v", err)
	}
	var size [2]uint32
	select {
	case size = <-fake.started:
	case <-ctx.Done():
		t.Fatal("the depth producer never started: the node was not given the native depth capture")
	}
	if size != [2]uint32{848, 480} {
		t.Fatalf("depth producer captured at %dx%d, want the largest fitting Z16 mode 848x480", size[0], size[1])
	}

	var lastBoot int64
	for i := uint64(1); i <= 2; i++ {
		before, _, _, err := data.CaptureReceipt()
		if err != nil {
			t.Fatal(err)
		}
		want := depthFrame(848, 480, uint16(1000*i))
		select {
		case fake.frames <- want:
		case <-ctx.Done():
			t.Fatal("the depth producer stopped taking frames")
		}
		sample, err := sub.Next(ctx)
		if err != nil {
			t.Fatalf("Next %d: %v", i, err)
		}
		_, _, after, _ := data.CaptureReceipt()

		if sample.Encoding != "z16" || !sample.SelfContained {
			t.Fatalf("sample %d described as %q self-contained=%v, want z16 and true", i, sample.Encoding, sample.SelfContained)
		}
		if sample.Width != 848 || sample.Height != 480 {
			t.Fatalf("sample %d is %dx%d, want 848x480", i, sample.Width, sample.Height)
		}
		if !bytes.Equal(sample.Payload, want) {
			t.Fatalf("sample %d payload differs from the frame the device produced", i)
		}
		if sample.SampleID != i {
			t.Fatalf("sample %d has id %d", i, sample.SampleID)
		}
		if sample.BootNanos < before || sample.BootNanos > after {
			t.Fatalf("sample %d receipt %d lies outside its delivery bracket [%d, %d]", i, sample.BootNanos, before, after)
		}
		if sample.BootNanos <= lastBoot {
			t.Fatalf("sample %d receipt %d does not follow the previous %d", i, sample.BootNanos, lastBoot)
		}
		if sample.UncertaintyNanos < 0 || sample.DecoderInit != nil {
			t.Fatalf("sample %d: uncertainty %d, decoder init %v", i, sample.UncertaintyNanos, sample.DecoderInit)
		}
		lastBoot = sample.BootNanos
	}

	// The hub is raw-only: an H.264 viewer is refused with the reason, and so
	// is an encoded model subscriber.
	svc.mu.Lock()
	hub := svc.hubs[testDepthNode]
	svc.mu.Unlock()
	if hub == nil {
		t.Fatal("no hub for the depth node")
	}
	hub.mu.Lock()
	reason := hub.encodedReason
	hub.mu.Unlock()
	if reason != depthEncodedReason {
		t.Fatalf("depth hub encoded refusal = %q, want %q", reason, depthEncodedReason)
	}
	_, _, _, err = svc.getOrCreateHub(ctx, testDepthNode, &agentpb.StreamVideoRequest{Codec: agentpb.VideoCodec_VIDEO_CODEC_H264}, hubHolderStreamClient)
	assertEncodedRefusal(t, "H.264 viewer", err)
	// SubscribeSensor on a depth node is the raw subscription, not a refusal:
	// the world view job names depth sources through it. It sees the next frame.
	plain, err := svc.SubscribeSensor(ctx, testDepthID)
	if err != nil {
		t.Fatalf("SubscribeSensor on the depth node: %v", err)
	}
	select {
	case fake.frames <- depthFrame(848, 480, 3000):
	case <-ctx.Done():
		t.Fatal("the depth producer stopped taking frames")
	}
	if sample, err := plain.Next(ctx); err != nil || sample.Encoding != "z16" || sample.Width != 848 || sample.Height != 480 || sample.SampleID != 3 {
		t.Fatalf("SubscribeSensor on the depth node got %q %dx%d id %d, err %v; want z16 848x480 id 3",
			sample.Encoding, sample.Width, sample.Height, sample.SampleID, err)
	}
	if sample, err := sub.Next(ctx); err != nil || sample.SampleID != 3 {
		t.Fatalf("the first raw subscriber did not share frame 3: id %d, err %v", sample.SampleID, err)
	}
	plain.Close()
	if hub.sampleSeq.Load() != 0 {
		t.Fatalf("the depth hub minted %d encoded sample ids", hub.sampleSeq.Load())
	}

	sub.Close()
	select {
	case <-fake.returned:
	case <-time.After(2 * time.Second):
		t.Fatal("depth capture kept running after its last subscriber left")
	}
}

// TestDepthHub_EncodedViewerThatStartedItIsTurnedAway covers the order the
// up-front refusal cannot: a viewer that created the hub before the producer
// found out the node is a depth sensor. It must be told why, not left waiting.
func TestDepthHub_EncodedViewerThatStartedItIsTurnedAway(t *testing.T) {
	withPixelFormats(t, map[string][]uint32{testDepthNode: {v4l2PixFmtZ16}})
	withRawModes(t, map[uint32][][2]uint32{v4l2PixFmtZ16: {{640, 480}}})
	installFakeDepthCapture(t)
	svc := newTestVideoService(nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h, id, ch, err := svc.getOrCreateHub(ctx, testDepthNode, &agentpb.StreamVideoRequest{}, hubHolderStreamClient)
	if err != nil {
		t.Fatalf("getOrCreateHub: %v", err)
	}
	defer h.unsubscribe(id)
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("the encoded viewer of a depth node received a frame")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the encoded viewer of a depth node was left waiting")
	}
	assertEncodedRefusal(t, "viewer that started the hub", h.subscriberErr(id))
}

func assertEncodedRefusal(t *testing.T, who string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s joined a raw-only depth hub", who)
	}
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition {
		t.Fatalf("%s: code %v, want FailedPrecondition: %v", who, st.Code(), err)
	}
	if !strings.Contains(err.Error(), depthEncodedReason) {
		t.Fatalf("%s: refusal does not name the reason: %v", who, err)
	}
}

// TestDepthProducer_RefusesRawWhenNoZ16ModeFits keeps the no-size case a
// refusal with a reason rather than a producer that opens the device anyway.
func TestDepthProducer_RefusesRawWhenNoZ16ModeFits(t *testing.T) {
	withPixelFormats(t, map[string][]uint32{testDepthNode: {v4l2PixFmtZ16}})
	withRawModes(t, map[uint32][][2]uint32{v4l2PixFmtZ16: {{1920, 1080}}})
	fake := installFakeDepthCapture(t)
	svc := newTestVideoService(nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sub, err := svc.SubscribeSensorRaw(ctx, testDepthID)
	if err != nil {
		t.Fatalf("SubscribeSensorRaw: %v", err)
	}
	defer sub.Close()
	_, err = sub.Next(ctx)
	if !streamreason.Has(err, streamreason.RawUnavailable) || !strings.Contains(err.Error(), "no discrete Z16 frame size") {
		t.Fatalf("Next = %v, want the raw refusal naming the missing Z16 size", err)
	}
	select {
	case <-fake.started:
		t.Fatal("the depth device was opened although no mode fits")
	default:
	}
}

// TestSubscribeSensorRaw_MJPEGOnlyNodeRefusesRaw walks an MJPEG-only camera
// through the pieces that decide it: it is not a depth node, its capture plan
// declines raw with the MJPEG reason, and a raw sensor subscriber is handed
// that reason verbatim, whether the producer decided before it joined or after.
func TestSubscribeSensorRaw_MJPEGOnlyNodeRefusesRaw(t *testing.T) {
	withPixelFormats(t, map[string][]uint32{testMJPEGNode: {v4l2PixFmtMJPEG}})
	withRawModes(t, map[uint32][][2]uint32{})
	prevMJPEG := deviceSupportsMJPEGSize
	deviceSupportsMJPEGSize = func(string, uint32, uint32) bool { return true }
	prevBest := bestDefaultFrameSizeForDevice
	bestDefaultFrameSizeForDevice = func(string) (uint32, uint32) { return 1280, 720 }
	t.Cleanup(func() { deviceSupportsMJPEGSize, bestDefaultFrameSizeForDevice = prevMJPEG, prevBest })

	if isDepthNode(testMJPEGNode) {
		t.Fatal("an MJPEG-only node was classified as depth")
	}
	plan, err := planGStreamerPipeline("gst", testMJPEGNode, &agentpb.StreamVideoRequest{}, "x264enc", true,
		0, "", pipeWireSource{}, map[string]bool{"x264enc": true, "jpegdec": true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.raw != nil || !strings.Contains(plan.rawWhy, "MJPEG") {
		t.Fatalf("MJPEG-only plan offered raw %v (reason %q)", plan.raw, plan.rawWhy)
	}

	assertTapRefusal := func(who string, err error) {
		t.Helper()
		if !streamreason.Has(err, streamreason.RawUnavailable) || status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("%s: %v, want FailedPrecondition with reason %s", who, err, streamreason.RawUnavailable)
		}
		if !strings.Contains(err.Error(), plan.rawWhy) {
			t.Fatalf("%s: refusal %q does not carry the tap's reason %q", who, err, plan.rawWhy)
		}
	}

	// Decided after the raw subscriber joined: Next returns the refusal.
	svc := newTestVideoService(nil, nil)
	fp := installFakeProducers(svc)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	early, err := svc.SubscribeSensorRaw(ctx, "v4l2:"+testMJPEGNode)
	if err != nil {
		t.Fatalf("SubscribeSensorRaw before the decision: %v", err)
	}
	fp.hub(0).rawNotOffered(plan.rawWhy)
	_, err = early.Next(ctx)
	assertTapRefusal("subscriber that joined first", err)

	// Decided before: SubscribeSensorRaw itself refuses. A viewer keeps the hub up.
	h, id, _, err := svc.getOrCreateHub(ctx, testMJPEGNode, &agentpb.StreamVideoRequest{}, hubHolderStreamClient)
	if err != nil {
		t.Fatal(err)
	}
	defer h.unsubscribe(id)
	h.rawNotOffered(plan.rawWhy)
	early.Close()
	_, err = svc.SubscribeSensorRaw(ctx, "v4l2:"+testMJPEGNode)
	assertTapRefusal("subscriber that joined after", err)
}

// TestSubscribeSensorRaw_TeeFramesNameTheirFourcc covers the non-depth raw
// source: a thermal camera's Y16 frames through the tap reach a raw sensor
// subscriber named by the lowercase trimmed fourcc, with their size.
func TestSubscribeSensorRaw_TeeFramesNameTheirFourcc(t *testing.T) {
	hub, cancel := newSampleHub(new(atomic.Uint64))
	defer cancel()
	hub.rawSampleSeq = new(atomic.Uint64)
	subID, frames, err := hub.subscribeKind(true)
	if err != nil {
		t.Fatal(err)
	}
	sub := &cameraSensorSubscription{hub: hub, subID: subID, frames: frames, raw: true}
	for _, tc := range []struct {
		fourcc, want string
		w, h         uint32
	}{{"Y16 ", "y16", 4, 2}, {fourccYUYV, "yuyv", 2, 2}} {
		format := &agentpb.RawFormat{Width: tc.w, Height: tc.h, Fourcc: tc.fourcc, BytesPerLine: tc.w * 2}
		hub.publishRaw(make([]byte, tc.w*2*tc.h), 1, format)
		sample, err := sub.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if sample.Encoding != tc.want || sample.Width != int(tc.w) || sample.Height != int(tc.h) || !sample.SelfContained {
			t.Fatalf("%q frame reached the subscriber as %q %dx%d self-contained=%v", tc.fourcc, sample.Encoding, sample.Width, sample.Height, sample.SelfContained)
		}
	}
}

// TestSensorSampleWidthHeightStayZeroForEncodedVideo keeps the new fields raw
// only, as SensorSample documents.
func TestSensorSampleWidthHeightStayZeroForEncodedVideo(t *testing.T) {
	hub, cancel := newSampleHub(new(atomic.Uint64))
	defer cancel()
	subID, frames, err := hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	sub := &cameraSensorSubscription{hub: hub, subID: subID, frames: frames}
	hub.produce(&videoFrame{data: []byte{0, 0, 0, 1, 0x65}, codec: agentpb.VideoCodec_VIDEO_CODEC_H264, auAligned: true})
	sample, err := sub.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sample.Width != 0 || sample.Height != 0 || sample.Encoding != "h264" {
		t.Fatalf("encoded sample reports %q %dx%d", sample.Encoding, sample.Width, sample.Height)
	}
}
