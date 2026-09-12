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

// The pre-roll flush is disk work far longer than a frame interval, and the
// live hub subscription it inherits is four frames deep. Before the drain, the
// frames arriving during the flush, which are the most interesting ones in the
// episode, were dropped by the hub and the tail resumed mid group of pictures.
func TestPreRollFlushLosesNoFramesWhileWriting(t *testing.T) {
	const preRollFrames = 4
	const liveFrames = 12

	video := NewVideoService(context.Background(), zap.NewNop())
	hub, _ := newDefaultHub(t, video, "/dev/video0")
	subID, frames, err := hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}

	trigger := int64(10 * time.Second)
	c := newTestCameraCapture(t, &fakeReceipt{now: trigger})
	c.session = data.CaptureSession{RequestBootNanos: trigger}
	c.hub, c.subID, c.frames = hub, subID, frames
	c.armed, c.mode = true, "continuous"
	c.ctx, c.cancel = context.WithCancel(context.Background())
	c.done, c.ready = make(chan struct{}), make(chan error, 1)
	for i := 0; i < preRollFrames; i++ {
		c.preRoll = append(c.preRoll, bufferedCameraFrame{
			frame:        preRollFrame(trigger-int64(preRollFrames-i)*100*ms, uint64(i+1), i == 0),
			randomAccess: i == 0,
			resetSegment: i == 0,
		})
	}

	// The producer keeps delivering at a steady rate throughout the flush, and
	// each written pre-roll frame takes long enough for several of them to
	// arrive: without a reader on the channel the four-deep buffer overflows.
	produced := make(chan struct{})
	c.preRollFlushHook = func() { time.Sleep(20 * time.Millisecond) }
	go func() {
		defer close(produced)
		for i := 0; i < liveFrames; i++ {
			payload := testInterFrame
			if i%4 == 0 {
				payload = testRAUFrame
			}
			hub.produce(testH264Frame(payload))
			time.Sleep(5 * time.Millisecond)
		}
	}()

	go c.run()
	if err := <-c.ready; err != nil {
		t.Fatalf("capture never became ready: %v", err)
	}
	<-produced
	// Let the live loop drain whatever is still queued, then stop.
	time.Sleep(100 * time.Millisecond)
	c.cancel()
	<-c.done

	// The capture's teardown has already unsubscribed, so the hub's own
	// counter is gone; the figure that survives is the one the manifest would
	// carry.
	if c.result.Drops == nil || *c.result.Drops != 0 {
		t.Fatalf("capture reports %d drops; the drain must lose no frame during the flush", *c.result.Drops)
	}
	if c.result.Count < preRollFrames+liveFrames {
		t.Fatalf("wrote %d frames, want at least the %d pre-roll plus %d live ones",
			c.result.Count, preRollFrames, liveFrames)
	}
}

// Any gap the flush could not avoid must land on a segment boundary rather
// than inside a file a decoder reads as one continuous timeline.
func TestLiveTailAfterPreRollOpensANewSegmentAtTheNextKeyframe(t *testing.T) {
	trigger := int64(10 * time.Second)
	clock := &fakeReceipt{now: trigger}
	c := newTestCameraCapture(t, clock)
	c.session = data.CaptureSession{RequestBootNanos: trigger}
	c.mode = "continuous"

	c.preRoll = []bufferedCameraFrame{
		{frame: preRollFrame(trigger-200*ms, 1, true), randomAccess: true, resetSegment: true},
		{frame: preRollFrame(trigger-100*ms, 2, false)},
	}
	if err := c.flushPreRoll(); err != nil {
		t.Fatal(err)
	}
	preRollSegment := c.segmentRel
	c.awaitLiveSegmentReset = true

	// An inter frame stays in the pre-roll's segment: rotating here would
	// leave a file no decoder can open.
	clock.now = trigger + 50*ms
	if err := c.handleFrame(testH264Frame(testInterFrame)); err != nil {
		t.Fatal(err)
	}
	if c.segmentRel != preRollSegment {
		t.Fatal("the live tail rotated the segment on an inter frame")
	}
	// The first live keyframe opens the new one.
	clock.now = trigger + 100*ms
	if err := c.handleFrame(testH264Frame(testRAUFrame)); err != nil {
		t.Fatal(err)
	}
	if c.segmentRel == preRollSegment {
		t.Fatal("the first live keyframe after the flush did not open a new segment")
	}
	if c.awaitLiveSegmentReset {
		t.Fatal("the pending reset was not cleared, so every later keyframe would rotate too")
	}
}

// A producer emitting one very long group of pictures gives the ring no
// keyframe boundary to trim at, so the byte cap must be enforced by starting
// over rather than by letting the ring grow without bound.
func TestPreRollRingRestartsWhenOneGOPExceedsTheByteCap(t *testing.T) {
	const cap = 4096
	r := &cameraPreRollRing{buffer: time.Second, limitBytes: cap}
	big := func(receipt int64, id uint64, key bool) *videoFrame {
		f := preRollFrame(receipt, id, key)
		f.data = append(append([]byte(nil), f.data...), make([]byte, 1024)...)
		return f
	}
	r.add(big(0, 1, true))
	for i := 1; i <= 40; i++ {
		r.add(big(int64(i)*10*ms, uint64(i+1), false))
		if r.bytes > cap+1024 {
			t.Fatalf("ring holds %d bytes after %d keyframe-free frames, above the %d cap", r.bytes, i, cap)
		}
	}
	if !r.droppedForBytes {
		t.Fatal("the ring did not report that the byte cap bounded it")
	}
	// After the restart the ring waits for a keyframe and opens a new stream.
	r.add(big(500*ms, 100, false))
	if len(r.frames) != 0 {
		t.Fatal("the restarted ring retained an inter frame before its next keyframe")
	}
	r.add(big(600*ms, 101, true))
	if len(r.frames) != 1 || !r.frames[0].resetSegment {
		t.Fatalf("the restarted ring did not begin a new stream on its next keyframe: %+v", r.frames)
	}
}

// A mid-arm producer restart must not lose the drops it had already counted,
// and the armed period's drops must be reported apart from the episode's own.
func TestArmedDropsAreCarriedAndReportedSeparately(t *testing.T) {
	video := NewVideoService(context.Background(), zap.NewNop())
	hub, _ := newDefaultHub(t, video, "/dev/video0")
	adapter := &cameraDataAdapter{video: video}
	source := data.Source{ID: "v4l2:/dev/video0", Kind: "camera"}

	adapter.Arm("cam", 5*time.Second, []data.Source{source})
	adapter.armedMu.Lock()
	armed := adapter.armed["cam"][source.ID]
	adapter.armedMu.Unlock()
	if armed == nil {
		t.Fatal("the campaign armed no source")
	}
	// Stand in for a long armed period during which the hub fell behind.
	const armedPeriodDrops = 7
	hub.mu.Lock()
	hub.subDrops[armed.subID] += armedPeriodDrops
	hub.mu.Unlock()

	_, origin, _, err := data.CaptureReceipt()
	if err != nil {
		t.Fatal(err)
	}
	session := data.CaptureSession{Directory: t.TempDir(), RequestBootNanos: origin, CampaignKey: "cam"}
	// One keyframe so the ring holds something to flush.
	hub.produce(testH264Frame(testRAUFrame))
	time.Sleep(20 * time.Millisecond)

	capture, err := adapter.Start(context.Background(), session, []data.Source{source})
	if err != nil {
		t.Fatal(err)
	}
	results, err := capture.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	res := results[0]
	if res.ArmedDrops == nil || *res.ArmedDrops != armedPeriodDrops {
		t.Fatalf("armed-period drops = %v, want %d reported separately", res.ArmedDrops, armedPeriodDrops)
	}
	if res.Drops == nil || *res.Drops != 0 {
		t.Fatalf("episode drops = %v, want 0: the armed period's losses are not the episode's", res.Drops)
	}
}

// A reattach during arming must keep the count it had already accumulated
// rather than zeroing it the moment after adding to it.
func TestArmedSourceKeepsDropsAcrossAReattach(t *testing.T) {
	video := NewVideoService(context.Background(), zap.NewNop())
	hub, _ := newDefaultHub(t, video, "/dev/video0")
	subID, frames, err := hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	hub.mu.Lock()
	hub.subDrops[subID] += 3
	hub.mu.Unlock()

	a := &armedCameraSource{
		video: video, source: data.Source{ID: "v4l2:/dev/video0"}, key: "/dev/video0",
		devID: 0, buffer: time.Second, hub: hub, subID: subID, frames: frames, alive: true,
		ring: &cameraPreRollRing{buffer: time.Second, limitBytes: preRollCameraLimitBytes},
	}
	if err := a.reattach(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.carriedDrops != 3 {
		t.Fatalf("carried drops = %d, want the 3 the left subscription had counted", a.carriedDrops)
	}
	a.hub.unsubscribe(a.subID)
}
