package services

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// newSampleHub builds a hub with a sample counter but no producer, so a test
// can produce frames by hand and inspect the identities they are stamped with.
func newSampleHub(seq *atomic.Uint64) (*deviceHub, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	return &deviceHub{
		subs: map[int]*hubSubscriber{}, subDrops: map[int]uint64{},
		ctx: ctx, cancel: cancel, done: make(chan struct{}), sampleSeq: seq,
	}, cancel
}

// TestProduceAssignsMonotonicSampleIdentities is the foundation of the whole
// correlation: every consumer of a frame must see the same identifier for it,
// and the identifiers must increase by one per produced frame.
func TestProduceAssignsMonotonicSampleIdentities(t *testing.T) {
	hub, cancel := newSampleHub(new(atomic.Uint64))
	defer cancel()
	_, first, err := hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if !hub.produce(&videoFrame{data: []byte{byte(i)}}) {
			t.Fatal("produce reported no subscribers")
		}
	}
	for i := uint64(1); i <= 3; i++ {
		a, b := <-first, <-second
		if a != b {
			t.Fatal("subscribers received different frame allocations; the identity would not be shared")
		}
		if a.sampleID != i {
			t.Fatalf("frame %d has sample id %d", i, a.sampleID)
		}
		if a.receiptBootNanos == 0 {
			t.Fatalf("frame %d carries no boot-clock receipt", i)
		}
	}
}

// TestOversizedFrameConsumesNoSampleIdentity keeps a gap in the identifiers
// meaningful: it must always mean a sample that existed and was lost, never one
// that was never produced.
func TestOversizedFrameConsumesNoSampleIdentity(t *testing.T) {
	hub, cancel := newSampleHub(new(atomic.Uint64))
	defer cancel()
	_, frames, err := hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	hub.produce(&videoFrame{data: make([]byte, maxFrameBytes+1)})
	hub.produce(&videoFrame{data: []byte{1}})
	frame := <-frames
	if frame.sampleID != 1 {
		t.Fatalf("sample id = %d, want 1: the dropped oversized frame consumed an identifier", frame.sampleID)
	}
}

// TestSampleIdentitiesSurviveProducerRestart guards the case that would silently
// corrupt an episode: a producer torn down and restarted mid-episode must not
// reissue identifiers the harness already handed out for that source.
func TestSampleIdentitiesSurviveProducerRestart(t *testing.T) {
	service := &VideoService{sampleSeqs: map[string]*atomic.Uint64{}}
	first := service.sampleSeqLocked("/dev/video0")
	hub, cancel := newSampleHub(first)
	_, frames, err := hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	hub.produce(&videoFrame{data: []byte{1}})
	hub.produce(&videoFrame{data: []byte{2}})
	<-frames
	<-frames
	cancel()

	// A new hub for the same device key, as getOrCreateHub builds after teardown.
	restarted, restartedCancel := newSampleHub(service.sampleSeqLocked("/dev/video0"))
	defer restartedCancel()
	_, restartedFrames, err := restarted.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	restarted.produce(&videoFrame{data: []byte{3}})
	if frame := <-restartedFrames; frame.sampleID != 3 {
		t.Fatalf("sample id after producer restart = %d, want 3", frame.sampleID)
	}
	if other := service.sampleSeqLocked("/dev/video1"); other == first {
		t.Fatal("two devices share one sample counter")
	}
}

// TestCameraSensorSubscriptionReportsDropsPerSample checks the honesty of the
// gap explanation: the subscription must report the drops since the previous
// delivered sample, not a running total.
func TestCameraSensorSubscriptionReportsDropsPerSample(t *testing.T) {
	hub, cancel := newSampleHub(new(atomic.Uint64))
	defer cancel()
	subID, frames, err := hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	subscription := &cameraSensorSubscription{hub: hub, subID: subID, frames: frames}
	// The subscriber channel holds four frames; broadcasting six drops two.
	for i := 0; i < 6; i++ {
		hub.produce(&videoFrame{data: []byte{byte(i)}, codec: agentpb.VideoCodec_VIDEO_CODEC_H264, auAligned: true})
	}
	ctx := context.Background()
	var reported []uint64
	for i := 0; i < 4; i++ {
		sample, err := subscription.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		reported = append(reported, sample.DroppedBefore)
		if sample.Encoding != "h264" || !sample.SelfContained {
			t.Fatalf("sample %d payload description = %q/%v", i, sample.Encoding, sample.SelfContained)
		}
	}
	total := uint64(0)
	for _, drops := range reported {
		total += drops
	}
	if total != 2 {
		t.Fatalf("reported drops %v sum to %d, want the 2 frames the hub dropped", reported, total)
	}
}

// TestCameraSensorSamplesKeepZeroBufferFields pins camera samples to
// single-instant semantics. sample_rate_hz, channels and duration_nanos exist
// for a source that hands over a BUFFER of equally spaced samples; a camera
// frame is one instant, so all three must stay zero and boottime_nanos must
// remain the time of the whole payload. A camera provider that started
// reporting a rate would tell an app to count samples into a frame.
func TestCameraSensorSamplesKeepZeroBufferFields(t *testing.T) {
	hub, cancel := newSampleHub(new(atomic.Uint64))
	defer cancel()
	subID, frames, err := hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	subscription := &cameraSensorSubscription{hub: hub, subID: subID, frames: frames}
	hub.produce(&videoFrame{
		data:      []byte{0, 0, 0, 1, 0x67, 1},
		codec:     agentpb.VideoCodec_VIDEO_CODEC_H264,
		auAligned: true,
	})
	sample, err := subscription.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sample.SampleRateHz != 0 || sample.Channels != 0 || sample.DurationNanos != 0 {
		t.Errorf("a camera sample reports buffer shape %d Hz / %d channels / %d ns, want zeros",
			sample.SampleRateHz, sample.Channels, sample.DurationNanos)
	}
	if sample.BootNanos == 0 {
		t.Error("the camera sample lost its boot-clock receipt")
	}
}

// TestCameraCaptureIndexCarriesSampleIdentity is the other half of the tee: the
// episode's own index must name the frame with the identifier the model saw, so
// the two can be joined without duplicating a single byte of video.
func TestCameraCaptureIndexCarriesSampleIdentity(t *testing.T) {
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
	capture := &cameraCapture{
		source:  data.Source{ID: "v4l2:/dev/video0"},
		session: data.CaptureSession{Directory: filepath.Dir(filepath.Dir(dir)), RequestBootNanos: origin},
		dir:     dir, index: index, mappingFile: mappings,
	}
	frame := &videoFrame{
		data:     []byte{0, 0, 0, 1, 0x67, 1, 0, 0, 0, 1, 0x68, 2, 0, 0, 0, 1, 0x65, 3},
		codec:    agentpb.VideoCodec_VIDEO_CODEC_H264,
		sampleID: 4242,
	}
	if err := capture.writeFrame(frame); err != nil {
		t.Fatal(err)
	}
	if err := index.Sync(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(dir, "index.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var record cameraIndexRecord
	if err := json.Unmarshal([]byte(strings.SplitN(string(contents), "\n", 2)[0]), &record); err != nil {
		t.Fatal(err)
	}
	if record.SampleID != 4242 {
		t.Fatalf("index sample_id = %d, want 4242", record.SampleID)
	}
	if record.Segment == "" || record.ByteSize == 0 {
		t.Fatalf("index entry does not locate the payload: %+v", record)
	}
}

// TestSensorSourceIDAliasesAreRefused is the join-key integrity guard.
// cameraDeviceID is deliberately lossy — it turns an identifier into a device
// number — so several spellings collapse onto one device. Accepting an alias
// would write model-input ledger lines under a source_id that no capture index
// and no manifest source entry carries, and would additionally make the
// manifest report a camera as not captured by an episode that was capturing it.
// It is also an entitlement question: an allowlist naming "ipcamera:0" must not
// silently grant /dev/video0.
func TestSensorSourceIDAliasesAreRefused(t *testing.T) {
	svc := NewVideoService(context.Background(), zap.NewNop())
	defer svc.Shutdown()

	if !svc.SupportsSensorSource("v4l2:/dev/video0") {
		t.Fatal("the canonical camera identifier is not supported")
	}
	for _, alias := range []string{
		"v4l2:/dev/video00",
		"v4l2:/dev/video000",
		"ipcamera:0",
		"ipcamera:00",
		"V4L2:/dev/video0",
		"v4l2:/dev/video0 ",
	} {
		if svc.SupportsSensorSource(alias) {
			t.Errorf("alias %q is accepted as a camera source identifier", alias)
		}
		if _, err := svc.SubscribeSensor(context.Background(), alias); status.Code(err) != codes.NotFound {
			t.Errorf("SubscribeSensor(%q) = %v, want NotFound", alias, err)
		}
	}
}

// TestCanonicalCameraSourceIDMatchesThePublishedIdentifier pins the canonical
// form to the one cameraDataAdapter.Sources publishes, because the two must
// agree for the ledger and the capture index to join at all.
func TestCanonicalCameraSourceIDMatchesThePublishedIdentifier(t *testing.T) {
	v4l2 := videoSource{kind: sourceV4L2, key: "/dev/video3", path: "/dev/video3"}
	if got, want := canonicalCameraSourceID(v4l2, 3), "v4l2:/dev/video3"; got != want {
		t.Errorf("canonical V4L2 id = %q, want %q", got, want)
	}
	ip := videoSource{kind: sourceIP, key: "ip:200"}
	if got, want := canonicalCameraSourceID(ip, 200), "ipcamera:200"; got != want {
		t.Errorf("canonical IP camera id = %q, want %q", got, want)
	}
}

// TestCaptureAndModelSubscriberShareOneProducer is the falsifiable form of the
// claim that deleting campaign-telemetry-only.yaml made: an episode capture and
// a model subscriber can consume the same camera at the same time, and they
// agree on the identity of every frame.
//
// Video4Linux2 admits one holder of a capture device, which is why the reference
// app used to need a telemetry-only campaign variant. The fix is that neither
// consumer opens the device: both are subscribers of the one producer hub. If
// this test can be made to fail — either consumer starved, or the two disagreeing
// on a sample id — the workaround was deleted prematurely.
func TestCaptureAndModelSubscriberShareOneProducer(t *testing.T) {
	hub, cancel := newSampleHub(new(atomic.Uint64))
	defer cancel()

	// The episode capture adapter's subscription.
	captureID, captureFrames, err := hub.subscribe()
	if err != nil {
		t.Fatalf("episode capture could not join the producer: %v", err)
	}
	// The model's subscription, joining the SAME hub afterwards.
	modelID, modelFrames, err := hub.subscribe()
	if err != nil {
		t.Fatalf("a model subscriber could not join a producer an episode already holds: %v", err)
	}
	if captureID == modelID {
		t.Fatal("the two consumers were given the same subscriber id")
	}
	subscription := &cameraSensorSubscription{hub: hub, subID: modelID, frames: modelFrames}

	const frames = 3
	for i := 0; i < frames; i++ {
		if !hub.produce(&videoFrame{
			data:      []byte{0, 0, 0, 1, 0x67, byte(i)},
			codec:     agentpb.VideoCodec_VIDEO_CODEC_H264,
			auAligned: true,
		}) {
			t.Fatalf("the producer was told to stop while two consumers were attached")
		}
	}

	// Both consumers see every frame, and the identity is the same on both
	// sides — that identity is the only thing joining the episode's payload
	// bytes to the model's input ledger.
	for i := 0; i < frames; i++ {
		sample, err := subscription.Next(context.Background())
		if err != nil {
			t.Fatalf("model subscriber frame %d: %v", i, err)
		}
		captured, ok := <-captureFrames
		if !ok {
			t.Fatalf("episode capture was starved at frame %d", i)
		}
		if sample.SampleID != captured.sampleID {
			t.Fatalf("frame %d: the model saw sample %d while the capture recorded %d",
				i, sample.SampleID, captured.sampleID)
		}
		if sample.SampleID != uint64(i+1) {
			t.Fatalf("frame %d has sample id %d, want %d", i, sample.SampleID, i+1)
		}
		if sample.DroppedBefore != 0 {
			t.Errorf("frame %d reported %d drops with both consumers keeping up", i, sample.DroppedBefore)
		}
	}

	// One consumer leaving must not stop the producer for the other.
	subscription.Close()
	if !hub.produce(&videoFrame{data: []byte{1}, codec: agentpb.VideoCodec_VIDEO_CODEC_H264}) {
		t.Fatal("the producer stopped when the model unsubscribed, starving the episode capture")
	}
}

// TestSensorReattachGateSkipsToRandomAccess pins the reattach gate's
// accounting deterministically: after a reattach the subscription delivers
// nothing until a random-access unit arrives, and the skipped frames are
// reported on that first delivered sample so its sample_id gap stays
// explained.
func TestSensorReattachGateSkipsToRandomAccess(t *testing.T) {
	hub, cancel := newSampleHub(new(atomic.Uint64))
	defer cancel()
	subID, frames, err := hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	subscription := &cameraSensorSubscription{hub: hub, subID: subID, frames: frames, awaitRandomAccess: true}

	// A whole access unit that is not random access (no SPS/PPS) is gated.
	hub.produce(&videoFrame{data: []byte{0, 0, 0, 1, 0x41, 1}, codec: agentpb.VideoCodec_VIDEO_CODEC_H264, auAligned: true})
	rau := []byte{0, 0, 0, 1, 0x67, 1, 0, 0, 0, 1, 0x68, 2, 0, 0, 0, 1, 0x65, 3}
	hub.produce(&videoFrame{data: rau, codec: agentpb.VideoCodec_VIDEO_CODEC_H264, auAligned: true})

	sample, err := subscription.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sample.SampleID != 2 || !sample.SelfContained {
		t.Fatalf("delivered sample = id %d selfContained %v, want the random-access unit (id 2)", sample.SampleID, sample.SelfContained)
	}
	if sample.DroppedBefore != 1 {
		t.Fatalf("DroppedBefore = %d, want 1: the gated frame must be reported, not silently absent", sample.DroppedBefore)
	}
}

// TestSensorSubscriptionReattachesAfterCaptureTakeover: the sensor socket's
// guarantee is that subscribing never takes a device away, and its
// counterpart is that a subscription survives episode capture taking the
// producer over. The subscription reattaches to the replacement hub and the
// app's new stream starts on a random-access unit.
func TestSensorSubscriptionReattachesAfterCaptureTakeover(t *testing.T) {
	svc := NewVideoService(context.Background(), zap.NewNop())
	defer svc.Shutdown()
	fp := installFakeProducers(svc)
	ctx := context.Background()

	subscription, err := svc.SubscribeSensor(ctx, "v4l2:/dev/video0")
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	if fp.count() != 1 {
		t.Fatalf("started %d producers, want 1", fp.count())
	}
	rau := &videoFrame{
		data:      []byte{0, 0, 0, 1, 0x67, 1, 0, 0, 0, 1, 0x68, 2, 0, 0, 0, 1, 0x65, 3},
		codec:     agentpb.VideoCodec_VIDEO_CODEC_H264,
		auAligned: true,
	}
	fp.hub(0).produce(rau)
	first, err := subscription.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Episode capture takes the camera over at explicit campaign parameters.
	req := &agentpb.StreamVideoRequest{DeviceId: 0, Width: 640, Height: 480, Framerate: 15}
	newHub, captureID, _, _, _, _, restarted, err := svc.joinHubForCapture(ctx, "/dev/video0", req)
	if err != nil || !restarted {
		t.Fatalf("takeover failed: restarted=%v err=%v", restarted, err)
	}
	defer newHub.unsubscribe(captureID)

	// Next reattaches to the replacement hub and delivers its frames. The
	// reattach is asynchronous relative to this test, so keep broadcasting
	// random-access units until one lands.
	type result struct {
		sample SensorSample
		err    error
	}
	got := make(chan result, 1)
	go func() {
		s, err := subscription.Next(ctx)
		got <- result{s, err}
	}()
	var second SensorSample
	deadline := time.After(5 * time.Second)
	for {
		bcast := &videoFrame{
			data:      append([]byte(nil), rau.data...),
			codec:     agentpb.VideoCodec_VIDEO_CODEC_H264,
			auAligned: true,
		}
		newHub.produce(bcast)
		select {
		case r := <-got:
			if r.err != nil {
				t.Fatalf("Next after takeover: %v", r.err)
			}
			second = r.sample
		case <-time.After(2 * time.Millisecond):
			continue
		case <-deadline:
			t.Fatal("subscription never delivered a frame from the replacement hub")
		}
		break
	}
	if !second.SelfContained {
		t.Fatal("first sample after reattach is not a self-contained random-access unit")
	}
	if second.SampleID <= first.SampleID {
		t.Fatalf("sample id did not advance across the restart: %d then %d", first.SampleID, second.SampleID)
	}
	if cs, ok := subscription.(*cameraSensorSubscription); !ok || cs.hub != newHub {
		t.Fatal("subscription did not reattach to the replacement hub")
	}
}

// TestSensorReattachCarriesTailDrops: drops the old subscription accrued after
// the last delivered sample can no longer ride on a sample of their own once
// the producer is taken over, so reattach must carry them into the first
// sample the new subscription delivers.
func TestSensorReattachCarriesTailDrops(t *testing.T) {
	svc := newTestVideoService(nil, nil)
	_ = installFakeProducers(svc)
	ctx := context.Background()

	hub, subID, frames, err := svc.joinHub(ctx, "/dev/video0", &agentpb.StreamVideoRequest{DeviceId: 0})
	if err != nil {
		t.Fatal(err)
	}
	subscription := &cameraSensorSubscription{video: svc, key: "/dev/video0", hub: hub, subID: subID, frames: frames, lastDrops: 1}
	hub.mu.Lock()
	hub.subDrops[subID] = 4
	hub.mu.Unlock()

	req := &agentpb.StreamVideoRequest{DeviceId: 0, Width: 640, Height: 480, Framerate: 15}
	newHub, captureID, _, _, _, _, restarted, err := svc.joinHubForCapture(ctx, "/dev/video0", req)
	if err != nil || !restarted {
		t.Fatalf("takeover failed: restarted=%v err=%v", restarted, err)
	}
	defer newHub.unsubscribe(captureID)

	if err := subscription.reattach(ctx); err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	if subscription.hub != newHub {
		t.Fatal("reattach did not land on the replacement hub")
	}
	if subscription.gatedSkips != 3 {
		t.Fatalf("gatedSkips = %d, want 3 (4 total drops minus 1 already reported)", subscription.gatedSkips)
	}
	if subscription.lastDrops != 0 || !subscription.awaitRandomAccess {
		t.Fatalf("reattach state: lastDrops=%d awaitRandomAccess=%v, want 0/true", subscription.lastDrops, subscription.awaitRandomAccess)
	}
}
