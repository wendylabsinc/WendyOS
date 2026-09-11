package services

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
)

func TestCameraDeviceID(t *testing.T) {
	for input, want := range map[string]uint32{"v4l2:/dev/video2": 2, "ipcamera:203": 203} {
		got, ok := cameraDeviceID(input)
		if !ok || got != want {
			t.Fatalf("cameraDeviceID(%q) = %d, %v; want %d, true", input, got, ok, want)
		}
	}
	if _, ok := cameraDeviceID("v4l2:/tmp/video0"); ok {
		t.Fatal("accepted unsafe V4L2 source id")
	}
}

func TestV4L2BufferTimestampMetadata(t *testing.T) {
	var buffer v4l2Buf
	binary.LittleEndian.PutUint32(buffer[12:16], 0x2000)
	binary.LittleEndian.PutUint64(buffer[24:32], 12)
	binary.LittleEndian.PutUint64(buffer[32:40], 3456)
	binary.LittleEndian.PutUint32(buffer[56:60], 91)
	if got := buffer.timestampNanos(); got != 12*int64(time.Second)+3456*int64(time.Microsecond) {
		t.Fatalf("timestamp = %d", got)
	}
	if buffer.flags() != 0x2000 || buffer.sequence() != 91 {
		t.Fatalf("metadata flags=%x sequence=%d", buffer.flags(), buffer.sequence())
	}
}

func TestCameraCaptureWritesSegmentAndAuditableIndex(t *testing.T) {
	dir := t.TempDir()
	index, err := os.OpenFile(filepath.Join(dir, "index.jsonl"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	mappings, err := os.OpenFile(filepath.Join(dir, "clock_samples.jsonl"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, origin, _, err := data.CaptureReceipt()
	if err != nil {
		t.Fatal(err)
	}
	capture := &cameraCapture{source: data.Source{ID: "v4l2:/dev/video2"}, session: data.CaptureSession{Directory: filepath.Dir(filepath.Dir(dir)), RequestBootNanos: origin}, dir: dir, index: index, mappingFile: mappings}
	frame := &videoFrame{data: []byte{0, 0, 0, 1, 0x67, 1, 0, 0, 0, 1, 0x68, 2, 0, 0, 0, 1, 0x65, 3}, tsNs: uint64(time.Now().UnixNano()), nativeNs: time.Now().UnixNano(), nativeClock: "CLOCK_REALTIME_AGENT_CAPTURE", codec: agentpb.VideoCodec_VIDEO_CODEC_H264}
	if err := capture.writeFrame(frame); err != nil {
		t.Fatal(err)
	}
	if err := capture.segment.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := index.Sync(); err != nil {
		t.Fatal(err)
	}
	media, err := os.ReadFile(filepath.Join(dir, "segment-000001.h264"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(media, frame.data) {
		t.Fatalf("media = %x", media)
	}
	contents, err := os.ReadFile(filepath.Join(dir, "index.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var record cameraIndexRecord
	if err := json.Unmarshal(bytes.TrimSpace(contents), &record); err != nil {
		t.Fatal(err)
	}
	if record.CanonicalEpisodeNanos < 0 || record.CanonicalUncertaintyNanos < 0 || record.AgentReceiptBootNanos == 0 {
		t.Fatalf("bad canonical record: %+v", record)
	}
	if record.MappingSegment != "receipt-bracket-v1" || record.ByteSize != len(frame.data) {
		t.Fatalf("bad index metadata: %+v", record)
	}
	_ = capture.segment.Close()
	_ = index.Close()
	_ = mappings.Close()
}

func TestH264SegmentsWaitForSelfContainedRandomAccessUnit(t *testing.T) {
	inter := []byte{0, 0, 0, 1, 0x41, 1}
	key := []byte{9, 9, 9, 0, 0, 1, 0x67, 1, 0, 0, 1, 0x68, 2, 0, 0, 1, 0x65, 3}
	if _, found := h264RandomAccessOffset(inter); found {
		t.Fatal("inter frame reported as random access")
	}
	if offset, found := h264RandomAccessOffset(key); !found || offset != 3 {
		t.Fatal("SPS/PPS/IDR access unit not recognized")
	}
}

// fakeReceipt is a deterministic CLOCK_BOOTTIME source for capture tests.
type fakeReceipt struct{ now int64 }

func (f *fakeReceipt) fn() (int64, int64, int64, error) { return f.now, f.now, f.now, nil }

var (
	// testRAUFrame is an Annex-B SPS/PPS/IDR access unit (decodable standalone).
	testRAUFrame = []byte{0, 0, 0, 1, 0x67, 1, 0, 0, 0, 1, 0x68, 2, 0, 0, 0, 1, 0x65, 3}
	// testInterFrame is a non-IDR slice that references earlier frames.
	testInterFrame = []byte{0, 0, 0, 1, 0x41, 1}
)

// testH264Frame models a native V4L2 frame: one whole access unit per frame.
func testH264Frame(payload []byte) *videoFrame {
	return &videoFrame{data: payload, nativeClock: "CLOCK_REALTIME_AGENT_CAPTURE", codec: agentpb.VideoCodec_VIDEO_CODEC_H264, auAligned: true}
}

// testChunkedH264Frame models a GStreamer or IP camera frame: an arbitrary
// slice of the encoded byte stream with no access-unit alignment.
func testChunkedH264Frame(payload []byte) *videoFrame {
	return &videoFrame{data: payload, nativeClock: "CLOCK_REALTIME_AGENT_CAPTURE", codec: agentpb.VideoCodec_VIDEO_CODEC_H264}
}

func newTestCameraCapture(t *testing.T, clock *fakeReceipt) *cameraCapture {
	t.Helper()
	dir := t.TempDir()
	index, err := os.OpenFile(filepath.Join(dir, "index.jsonl"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	mappings, err := os.OpenFile(filepath.Join(dir, "clock_samples.jsonl"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { index.Close(); mappings.Close() })
	return &cameraCapture{source: data.Source{ID: "v4l2:/dev/video2"}, session: data.CaptureSession{RequestBootNanos: 0}, dir: dir, index: index, mappingFile: mappings, captureReceipt: clock.fn, lastSnapshotIdx: -1}
}

func TestSnapshotModeWritesOneStillPerInterval(t *testing.T) {
	clock := &fakeReceipt{}
	c := newTestCameraCapture(t, clock)
	c.mode, c.interval = captureModeSnapshot, int64(time.Second)

	clock.now = 100 * int64(time.Millisecond)
	if err := c.handleFrame(testH264Frame(testInterFrame)); !errors.Is(err, errAwaitCameraRandomAccess) {
		t.Fatalf("inter frame before the first still: %v", err)
	}
	if err := c.handleFrame(testH264Frame(testRAUFrame)); err != nil {
		t.Fatal(err)
	}
	clock.now = 500 * int64(time.Millisecond)
	if err := c.handleFrame(testH264Frame(testRAUFrame)); err != nil {
		t.Fatal(err) // same interval: discarded, not written
	}
	clock.now = 1300 * int64(time.Millisecond)
	if err := c.handleFrame(testH264Frame(testInterFrame)); err != nil {
		t.Fatal(err) // not decodable standalone: discarded
	}
	if err := c.handleFrame(testH264Frame(testRAUFrame)); err != nil {
		t.Fatal(err)
	}

	if c.result.Count != 2 {
		t.Fatalf("count = %d, want 2", c.result.Count)
	}
	for n := 1; n <= 2; n++ {
		media, err := os.ReadFile(filepath.Join(c.dir, fmt.Sprintf("snapshot-%06d.h264", n)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(media, testRAUFrame) {
			t.Fatalf("snapshot %d = %x", n, media)
		}
	}
	if _, err := os.Stat(filepath.Join(c.dir, "snapshot-000003.h264")); err == nil {
		t.Fatal("extra snapshot written within an interval")
	}
	if err := c.index.Sync(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(c.dir, "index.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(contents), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("index lines = %d, want 2", len(lines))
	}
	for i, line := range lines {
		var record cameraIndexRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if record.ByteOffset != 0 || record.ByteSize != len(testRAUFrame) || record.MappingSegment == "" {
			t.Fatalf("bad snapshot index record %d: %+v", i, record)
		}
		want := fmt.Sprintf("snapshot-%06d.h264", i+1)
		if !strings.HasSuffix(record.Segment, want) {
			t.Fatalf("record %d segment = %q, want suffix %q", i, record.Segment, want)
		}
	}
}

func TestSnapshotModeCountsMissedIntervals(t *testing.T) {
	clock := &fakeReceipt{}
	c := newTestCameraCapture(t, clock)
	c.mode, c.interval = captureModeSnapshot, int64(time.Second)

	clock.now = 2500 * int64(time.Millisecond) // intervals 0 and 1 elapsed before any decodable unit
	if err := c.handleFrame(testH264Frame(testRAUFrame)); err != nil {
		t.Fatal(err)
	}
	clock.now = 5100 * int64(time.Millisecond) // intervals 3 and 4 got no still
	if err := c.handleFrame(testH264Frame(testRAUFrame)); err != nil {
		t.Fatal(err)
	}
	c.finishSnapshotAccounting(8200 * int64(time.Millisecond)) // intervals 6 and 7 elapsed after the last still

	if c.result.Count != 2 {
		t.Fatalf("count = %d, want 2", c.result.Count)
	}
	if c.result.Drops == nil || *c.result.Drops != 6 {
		t.Fatalf("drops = %v, want 6 missed intervals", c.result.Drops)
	}
	if c.result.DropAccounting != "missed_snapshot_intervals" {
		t.Fatalf("drop accounting = %q", c.result.DropAccounting)
	}
}

func TestContinuousRateCapDropsWholeGOPs(t *testing.T) {
	clock := &fakeReceipt{}
	c := newTestCameraCapture(t, clock)
	c.rateCap = 1 // hertz

	written := func(at time.Duration, payload []byte) uint64 {
		clock.now = int64(at)
		if err := c.handleFrame(testH264Frame(payload)); err != nil {
			t.Fatalf("frame at %s: %v", at, err)
		}
		return c.result.Count
	}
	if got := written(0, testRAUFrame); got != 1 {
		t.Fatalf("first GOP not admitted: count %d", got)
	}
	if got := written(100*time.Millisecond, testInterFrame); got != 2 {
		t.Fatalf("inter frame of the admitted GOP was dropped: count %d", got)
	}
	if got := written(250*time.Millisecond, testRAUFrame); got != 2 {
		t.Fatalf("over-cap GOP admitted: count %d", got)
	}
	if got := written(300*time.Millisecond, testInterFrame); got != 2 {
		t.Fatalf("inter frame of a skipped GOP written: count %d", got)
	}
	if got := written(2*time.Second, testRAUFrame); got != 3 {
		t.Fatalf("back-under-cap GOP not admitted: count %d", got)
	}
	if c.result.Drops != nil {
		t.Fatalf("intentionally skipped GOPs must not count as drops: %v", *c.result.Drops)
	}
	c.finishResultDetail(3 * int64(time.Second))
	if !strings.Contains(c.result.SourceDetail, "rate cap 1 Hz") || !strings.Contains(c.result.SourceDetail, "achieved 1 Hz") {
		t.Fatalf("achieved rate not recorded: %q", c.result.SourceDetail)
	}
}

func TestAchievedRateIsComputedOverTheCaptureSpanNotTheWrittenBurst(t *testing.T) {
	clock := &fakeReceipt{}
	c := newTestCameraCapture(t, clock)
	c.rateCap = 1 // hertz

	// One admitted GOP: a keyframe and four inter frames inside 400ms. Every
	// later GOP stays over the cap and is skipped until the capture ends.
	for i, payload := range [][]byte{testRAUFrame, testInterFrame, testInterFrame, testInterFrame, testInterFrame} {
		clock.now = int64(i) * 100 * int64(time.Millisecond)
		if err := c.handleFrame(testH264Frame(payload)); err != nil {
			t.Fatal(err)
		}
	}
	clock.now = 2 * int64(time.Second)
	if err := c.handleFrame(testH264Frame(testRAUFrame)); err != nil {
		t.Fatal(err)
	}
	if c.result.Count != 5 {
		t.Fatalf("count = %d, want the single admitted GOP's 5 frames", c.result.Count)
	}

	c.finishResultDetail(10 * int64(time.Second))
	if !strings.Contains(c.result.SourceDetail, "achieved 0.5 Hz") {
		t.Fatalf("achieved rate must cover the whole capture span (5 frames / 10s), got %q", c.result.SourceDetail)
	}
}

func TestSnapshotModeDegradesToContinuousOnByteChunkedTransport(t *testing.T) {
	clock := &fakeReceipt{now: 100 * int64(time.Millisecond)}
	c := newTestCameraCapture(t, clock)
	c.mode, c.interval = captureModeSnapshot, int64(time.Second)

	// The chunk parses as a random-access prefix, but chunk boundaries are
	// arbitrary: the same bytes could be the head of a truncated keyframe.
	if err := c.handleFrame(testChunkedH264Frame(testRAUFrame)); err != nil {
		t.Fatal(err)
	}
	if c.mode != "continuous" {
		t.Fatalf("mode = %q, want degrade to continuous", c.mode)
	}
	if _, err := os.Stat(filepath.Join(c.dir, "snapshot-000001.h264")); err == nil {
		t.Fatal("a byte-stream chunk was written as a snapshot still")
	}
	if _, err := os.Stat(filepath.Join(c.dir, "segment-000001.h264")); err != nil {
		t.Fatalf("degraded capture did not record continuously: %v", err)
	}
	c.finishResultDetail(0)
	if !strings.Contains(c.result.SourceDetail, "records continuously instead") {
		t.Fatalf("degrade note missing from source detail: %q", c.result.SourceDetail)
	}
}

func TestRateCapIsDisabledOnByteChunkedTransport(t *testing.T) {
	clock := &fakeReceipt{}
	c := newTestCameraCapture(t, clock)
	c.rateCap = 1 // hertz

	// Chunks of a byte stream cannot be skipped at GOP granularity: dropping
	// them corrupts the stream. All three must be written despite the cap.
	for i, payload := range [][]byte{testRAUFrame, testInterFrame, testRAUFrame} {
		clock.now = int64(i) * 100 * int64(time.Millisecond)
		if err := c.handleFrame(testChunkedH264Frame(payload)); err != nil {
			t.Fatal(err)
		}
	}
	if c.result.Count != 3 {
		t.Fatalf("count = %d, want all 3 chunks written", c.result.Count)
	}
	if c.rateCap != 0 {
		t.Fatalf("rate cap still armed on a chunked transport: %v", c.rateCap)
	}
	c.finishResultDetail(0)
	if !strings.Contains(c.result.SourceDetail, "rate cap 1 Hz cannot be enforced") {
		t.Fatalf("disabled-cap note missing: %q", c.result.SourceDetail)
	}
}

func TestSnapshotSkipsTruncatedFrameOnAlignedTransport(t *testing.T) {
	clock := &fakeReceipt{now: 100 * int64(time.Millisecond)}
	c := newTestCameraCapture(t, clock)
	c.mode, c.interval = captureModeSnapshot, int64(time.Second)

	if err := c.handleFrame(testH264Frame(testRAUFrame)); err != nil {
		t.Fatal(err)
	}
	// A single frame truncated by the maxFrameBytes cap: its head still parses
	// as SPS/PPS/IDR, but it must not become a still, and it must not degrade
	// the whole capture either.
	clock.now = 1500 * int64(time.Millisecond)
	truncated := testChunkedH264Frame(testRAUFrame)
	if err := c.handleFrame(truncated); err != nil {
		t.Fatal(err)
	}
	if c.result.Count != 1 {
		t.Fatalf("count = %d after truncated frame, want 1", c.result.Count)
	}
	clock.now = 1700 * int64(time.Millisecond)
	if err := c.handleFrame(testH264Frame(testRAUFrame)); err != nil {
		t.Fatal(err)
	}
	if c.mode != captureModeSnapshot || c.result.Count != 2 {
		t.Fatalf("mode = %q count = %d, want snapshot mode intact with 2 stills", c.mode, c.result.Count)
	}
	media, err := os.ReadFile(filepath.Join(c.dir, "snapshot-000002.h264"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(media, testRAUFrame) {
		t.Fatalf("snapshot 2 = %x", media)
	}
}

// newExplicitlyHeldHub builds a hub as a dashboard viewer's explicit
// 1920x1080@30 StreamVideo request would, registered under /dev/video0.
func newExplicitlyHeldHub(t *testing.T, video *VideoService) (hub *deviceHub, viewerID int, cancel context.CancelFunc) {
	t.Helper()
	hctx, hcancel := context.WithCancel(context.Background())
	hub = &deviceHub{subs: make(map[int]*hubSubscriber), subDrops: make(map[int]uint64), ctx: hctx, cancel: hcancel, done: make(chan struct{}), width: 1920, height: 1080, framerate: 30}
	viewerID, _, err := hub.subscribeAs(hubHolderStreamClient)
	if err != nil {
		t.Fatal(err)
	}
	video.hubs["/dev/video0"] = hub
	return hub, viewerID, hcancel
}

// A capture that asserts no parameters keeps the old fallback: it joins the
// running hub at whatever parameters it has and reports them as achieved.
func TestParameterlessCaptureFallsBackToExistingParameters(t *testing.T) {
	video := NewVideoService(context.Background(), zap.NewNop())
	existing, viewerID, _ := newExplicitlyHeldHub(t, video)
	adapter := &cameraDataAdapter{video: video}

	req := &agentpb.StreamVideoRequest{DeviceId: 0}
	hub, id, ch, w, h, fps, restarted, err := adapter.subscribeHub(context.Background(), "/dev/video0", req)
	if err != nil {
		t.Fatalf("collision failed the episode: %v", err)
	}
	if restarted {
		t.Fatal("a parameter-less capture must never restart a running producer")
	}
	if hub != existing || ch == nil {
		t.Fatal("did not join the existing hub")
	}
	if w != 1920 || h != 1080 || fps != 30 {
		t.Fatalf("achieved parameters = %dx%d@%d, want the existing hub's 1920x1080@30", w, h, fps)
	}
	hub.unsubscribe(id)
	existing.unsubscribe(viewerID)
}

// A capture with explicit campaign parameters conflicting with a hub held at
// another consumer's explicit parameters is refused by name, never downgraded.
func TestExplicitCaptureRefusedAgainstExplicitlyHeldHub(t *testing.T) {
	video := NewVideoService(context.Background(), zap.NewNop())
	existing, viewerID, _ := newExplicitlyHeldHub(t, video)
	defer existing.unsubscribe(viewerID)
	adapter := &cameraDataAdapter{video: video}

	req := &agentpb.StreamVideoRequest{DeviceId: 0, Width: 1280, Height: 720, Framerate: 15}
	_, _, _, _, _, _, _, err := adapter.subscribeHub(context.Background(), "/dev/video0", req)
	if !errors.Is(err, errCameraHeldExplicitly) {
		t.Fatalf("expected errCameraHeldExplicitly, got %v", err)
	}
	for _, want := range []string{hubHolderStreamClient, "1920x1080 at 30 fps", "1280x720 at 15 fps"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err.Error(), want)
		}
	}
	if existing.wasRestarted() {
		t.Fatal("refused takeover must not restart the held hub")
	}
}

// A parameter-less capture joining a hub at someone else's parameters still
// records requested-versus-achieved in the manifest. The rate cap here (10 Hz)
// is deliberately below the smallest source framerate, so the request asserts
// nothing at the source and the cap is enforced adapter-side.
func TestStartOneRecordsRequestedVersusAchievedOnCollision(t *testing.T) {
	video := NewVideoService(context.Background(), zap.NewNop())
	existing, viewerID, _ := newExplicitlyHeldHub(t, video)
	defer existing.unsubscribe(viewerID)
	adapter := &cameraDataAdapter{video: video}

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
				existing.produce(testH264Frame(testRAUFrame))
			}
		}
	}()

	_, origin, _, err := data.CaptureReceipt()
	if err != nil {
		t.Fatal(err)
	}
	session := data.CaptureSession{Directory: t.TempDir(), RequestBootNanos: origin}
	source := data.Source{ID: "v4l2:/dev/video0", Kind: "camera", Capture: &data.SourceCapture{Rate: 10}}
	capture, err := adapter.startOne(context.Background(), session, source, 0)
	if err != nil {
		t.Fatal(err)
	}
	results, err := capture.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d", len(results))
	}
	detail := results[0].SourceDetail
	if !strings.Contains(detail, "capturing at 1920x1080 at 30 fps") || !strings.Contains(detail, "requested") {
		t.Fatalf("requested-vs-achieved resolution not recorded: %q", detail)
	}
	if !strings.Contains(detail, "rate cap 10 Hz") {
		t.Fatalf("rate cap note not recorded: %q", detail)
	}
	if results[0].Count == 0 {
		t.Fatal("no frames captured through the existing hub")
	}
}

// A refused camera source must not fail the episode: Start continues, and the
// refusal is delivered at Stop as that source's manifest entry, naming who
// holds the camera and at what parameters.
func TestStartRecordsRefusalAndContinuesEpisode(t *testing.T) {
	video := NewVideoService(context.Background(), zap.NewNop())
	existing, viewerID, _ := newExplicitlyHeldHub(t, video)
	defer existing.unsubscribe(viewerID)
	adapter := &cameraDataAdapter{video: video}

	_, origin, _, err := data.CaptureReceipt()
	if err != nil {
		t.Fatal(err)
	}
	session := data.CaptureSession{Directory: t.TempDir(), RequestBootNanos: origin}
	source := data.Source{ID: "v4l2:/dev/video0", Kind: "camera", Detail: "USB Cam", Capture: &data.SourceCapture{Rate: 15, MaxResolution: "1280x720"}}
	capture, err := adapter.Start(context.Background(), session, []data.Source{source})
	if err != nil {
		t.Fatalf("a refused camera source failed the whole episode: %v", err)
	}
	if capture == nil {
		t.Fatal("refusal produced no capture group, so the manifest would never see the refusal note")
	}
	results, err := capture.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].SourceID != "v4l2:/dev/video0" {
		t.Fatalf("results = %+v", results)
	}
	detail := results[0].SourceDetail
	for _, want := range []string{"not captured", hubHolderStreamClient, "1920x1080 at 30 fps", "1280x720 at 15 fps"} {
		if !strings.Contains(detail, want) {
			t.Errorf("refusal detail %q does not name %q", detail, want)
		}
	}
	if results[0].Count != 0 {
		t.Fatalf("refused source claims %d captured frames", results[0].Count)
	}
}

func TestDeviceHubCountsSubscriberDrops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	hub := &deviceHub{subs: make(map[int]*hubSubscriber), subDrops: make(map[int]uint64), ctx: ctx, cancel: cancel}
	id, _, err := hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		hub.produce(&videoFrame{data: []byte{byte(i)}})
	}
	if got := hub.unsubscribe(id); got != 2 {
		t.Fatalf("drops = %d, want 2", got)
	}
}
