package services

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"go.uber.org/zap"
)

const ms = int64(time.Millisecond)

// preRollFrame builds a broadcast-style frame carrying its own hub receipt and
// sample identity, the two things the pre-roll ring relies on.
func preRollFrame(receipt int64, sampleID uint64, key bool) *videoFrame {
	payload := testInterFrame
	if key {
		payload = testRAUFrame
	}
	f := testH264Frame(payload)
	f.receiptBootNanos = receipt
	f.receiptUncertaintyNanos = 1000
	f.sampleID = sampleID
	return f
}

func fillRing(r *cameraPreRollRing, frames ...*videoFrame) {
	for _, f := range frames {
		r.add(f)
	}
}

// The ring retains only from a keyframe that covers the window start, evicting
// older groups of pictures, so a flushed clip begins on a decodable unit.
func TestPreRollRingRetainsFromKeyframeCoveringWindow(t *testing.T) {
	r := &cameraPreRollRing{buffer: time.Second, limitBytes: preRollCameraLimitBytes}
	fillRing(r,
		preRollFrame(0, 1, true), // GOP 0
		preRollFrame(100*ms, 2, false),
		preRollFrame(500*ms, 3, true), // GOP 1
		preRollFrame(600*ms, 4, false),
		preRollFrame(1000*ms, 5, true), // GOP 2 (== window start for newest 2000ms)
		preRollFrame(1500*ms, 6, true), // GOP 3
		preRollFrame(2000*ms, 7, true), // GOP 4, newest
	)
	// newest = 2000ms, cutoff = 1000ms: the last keyframe at or before 1000ms is
	// the one at 1000ms, so GOPs 0 and 1 are evicted.
	if len(r.frames) == 0 || !r.frames[0].randomAccess {
		t.Fatalf("ring does not begin on a keyframe: %+v", r.frames)
	}
	if got := r.frames[0].frame.receiptBootNanos; got != 1000*ms {
		t.Fatalf("earliest retained frame = %dms, want the keyframe at 1000ms", got/ms)
	}
	if got := r.frames[0].frame.sampleID; got != 5 {
		t.Fatalf("earliest retained sample_id = %d, want 5", got)
	}
}

// When armed less than a buffer before the trigger, the ring cannot reach a full
// buffer back; flush reports the shorter reach honestly instead of fabricating.
func TestPreRollFlushReportsShortReachWhenArmedRecently(t *testing.T) {
	r := &cameraPreRollRing{buffer: time.Second, limitBytes: preRollCameraLimitBytes}
	fillRing(r,
		preRollFrame(0, 1, true),
		preRollFrame(100*ms, 2, false),
		preRollFrame(200*ms, 3, true),
		preRollFrame(300*ms, 4, false),
	)
	trigger := int64(300 * ms)
	frames, achieved, reachedFull := r.flush(trigger)
	if len(frames) == 0 {
		t.Fatal("flush returned no frames")
	}
	if reachedFull {
		t.Fatal("reachedFull true, but the ring holds only 300ms against a 1s buffer")
	}
	if achieved != -300*ms {
		t.Fatalf("achieved offset = %dms, want -300ms (the earliest keyframe)", achieved/ms)
	}
	if !frames[0].resetSegment {
		t.Fatal("first flushed frame must open a fresh segment")
	}
}

// Replaying a flushed ring writes the pre-roll frames as real captured payload:
// negative episode offsets from their broadcast receipts, their real sample ids
// in the index, and the receipt-bracket mapping, all counted as captured.
func TestPreRollReplayWritesNegativeOffsetsAndRealSampleIDs(t *testing.T) {
	trigger := int64(10 * time.Second)
	c := newTestCameraCapture(t, &fakeReceipt{})
	c.session = data.CaptureSession{RequestBootNanos: trigger}
	c.armed = true

	pre := []bufferedCameraFrame{
		{frame: preRollFrame(trigger-800*ms, 11, true), randomAccess: true, resetSegment: true},
		{frame: preRollFrame(trigger-700*ms, 12, false), randomAccess: false},
		{frame: preRollFrame(trigger-100*ms, 13, false), randomAccess: false},
	}
	for _, bf := range pre {
		if err := c.writeBufferedFrame(bf); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.segment.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := c.index.Sync(); err != nil {
		t.Fatal(err)
	}

	if c.result.Count != 3 {
		t.Fatalf("captured count = %d, want the 3 pre-roll frames", c.result.Count)
	}
	if c.result.ActualOffset == nil || *c.result.ActualOffset != -800*ms {
		t.Fatalf("actual offset = %v, want -800ms (the opening keyframe)", c.result.ActualOffset)
	}

	contents, err := os.ReadFile(filepath.Join(c.dir, "index.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(contents), []byte("\n"))
	if len(lines) != 3 {
		t.Fatalf("index lines = %d, want 3", len(lines))
	}
	wantOffsets := []int64{-800 * ms, -700 * ms, -100 * ms}
	wantSamples := []uint64{11, 12, 13}
	for i, line := range lines {
		var rec cameraIndexRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatal(err)
		}
		if rec.CanonicalEpisodeNanos != wantOffsets[i] {
			t.Fatalf("record %d canonical = %dms, want %dms (BEFORE the trigger)", i, rec.CanonicalEpisodeNanos/ms, wantOffsets[i]/ms)
		}
		if rec.SampleID != wantSamples[i] {
			t.Fatalf("record %d sample_id = %d, want %d (the real broadcast identity)", i, rec.SampleID, wantSamples[i])
		}
		if rec.MappingSegment != "receipt-bracket-v1" {
			t.Fatalf("record %d mapping = %q, want receipt-bracket-v1", i, rec.MappingSegment)
		}
	}
	// The opening keyframe's payload landed in the first segment file.
	media, err := os.ReadFile(filepath.Join(c.dir, "segment-000001.h264"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(media, testRAUFrame) {
		t.Fatalf("segment missing the opening keyframe payload: %x", media)
	}
}

// newDefaultHub registers a producer hub running at defaulted parameters, held
// only by a parameter-less subscriber (as the sensor path or a defaulted viewer
// would hold it). This is the hub state arming must join without taking over.
func newDefaultHub(t *testing.T, video *VideoService, path string) (*deviceHub, int) {
	t.Helper()
	hctx, hcancel := context.WithCancel(context.Background())
	hub := &deviceHub{subs: make(map[int]*hubSubscriber), subDrops: make(map[int]uint64), ctx: hctx, cancel: hcancel, done: make(chan struct{}), sampleSeq: video.sampleSeqLocked(path)}
	t.Cleanup(hcancel)
	id, _, err := hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	video.hubs[path] = hub
	return hub, id
}

// Arming subscribes as a non-owning consumer: it joins the running hub, adds no
// explicit-parameter holder, and never restarts the producer, so it plays
// correctly with the parameter-precedence hub-state model.
func TestArmingIsNonOwningAndDoesNotForceTakeover(t *testing.T) {
	video := NewVideoService(context.Background(), zap.NewNop())
	hub, viewerID := newDefaultHub(t, video, "/dev/video0")
	adapter := &cameraDataAdapter{video: video}

	adapter.Arm("cam", time.Second, []data.Source{{ID: "v4l2:/dev/video0", Kind: "camera"}})
	defer adapter.Disarm("cam")

	if hub.wasRestarted() {
		t.Fatal("arming restarted a running producer; it must never take the camera over")
	}
	if _, held := hub.heldExplicitly(); held {
		t.Fatal("arming registered an explicit-parameter holder; it must assert none")
	}
	if video.hubs["/dev/video0"] != hub {
		t.Fatal("arming replaced the existing hub instead of joining it")
	}
	hub.mu.Lock()
	n := len(hub.subs)
	hub.mu.Unlock()
	if n != 2 {
		t.Fatalf("hub subscribers = %d, want the viewer plus the armed ring", n)
	}
	_ = viewerID
}

// End to end on a bare hub: a campaign arms, frames are broadcast before the
// trigger, and the triggered episode opens with those frames at negative
// offsets on the same subscription that then continues live.
func TestArmedCampaignFlushesPreRollOnTrigger(t *testing.T) {
	video := NewVideoService(context.Background(), zap.NewNop())
	hub, _ := newDefaultHub(t, video, "/dev/video0")
	adapter := &cameraDataAdapter{video: video}

	adapter.Arm("cam", 5*time.Second, []data.Source{{ID: "v4l2:/dev/video0", Kind: "camera"}})

	// Broadcast a handful of GOPs before the trigger, paced so the 4-deep
	// subscriber channel never overflows and the fill goroutine keeps up.
	for i := 0; i < 6; i++ {
		hub.produce(testH264Frame(testRAUFrame))
		time.Sleep(2 * time.Millisecond)
		hub.produce(testH264Frame(testInterFrame))
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond)

	_, origin, _, err := data.CaptureReceipt()
	if err != nil {
		t.Fatal(err)
	}
	session := data.CaptureSession{Directory: t.TempDir(), RequestBootNanos: origin, CampaignKey: "cam"}
	capture, err := adapter.Start(context.Background(), session, []data.Source{{ID: "v4l2:/dev/video0", Kind: "camera"}})
	if err != nil {
		t.Fatalf("triggering the armed campaign failed: %v", err)
	}
	if capture == nil {
		t.Fatal("armed campaign produced no capture")
	}
	results, err := capture.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	res := results[0]
	if res.Count == 0 {
		t.Fatal("episode opened with no pre-roll frames")
	}
	if res.ActualOffset == nil || *res.ActualOffset >= 0 {
		t.Fatalf("actual offset = %v, want a negative (before-trigger) offset", res.ActualOffset)
	}
	if got := filepath.Join(session.Directory, "cameras", safeCaptureName("v4l2:/dev/video0"), "index.jsonl"); got == "" {
		t.Fatal("no index path")
	}
	contents, err := os.ReadFile(filepath.Join(session.Directory, "cameras", safeCaptureName("v4l2:/dev/video0"), "index.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var sawNegative bool
	for _, line := range bytes.Split(bytes.TrimSpace(contents), []byte("\n")) {
		var rec cameraIndexRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatal(err)
		}
		if rec.CanonicalEpisodeNanos < 0 && rec.SampleID != 0 {
			sawNegative = true
		}
	}
	if !sawNegative {
		t.Fatal("no index entry carries a before-trigger canonical time with a real sample id")
	}
}
