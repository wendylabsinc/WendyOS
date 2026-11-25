package services

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
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

// numberFrames has hub number n frames and returns the sample IDs they got.
func numberFrames(hub *deviceHub, n int) []uint64 {
	var ids []uint64
	for range n {
		frame := &videoFrame{data: []byte{1}, codec: agentpb.VideoCodec_VIDEO_CODEC_H264}
		hub.produce(frame)
		ids = append(ids, frame.sampleID)
	}
	return ids
}

// viewerHub joins the camera at path as a viewer that names no parameters
// does, and returns its hub and how to leave it and wait for it to end.
func viewerHub(t *testing.T, s *VideoService, path string, req *agentpb.StreamVideoRequest) (*deviceHub, func()) {
	t.Helper()
	hub, id, _, err := s.getOrCreateHub(context.Background(), path, req, hubHolderStreamClient)
	if err != nil {
		t.Fatalf("joining %s: %v", path, err)
	}
	return hub, func() {
		hub.unsubscribe(id)
		select {
		case <-hub.done:
		case <-time.After(2 * time.Second):
			t.Fatalf("the hub of %s did not end", path)
		}
	}
}

// putCameraAt has c answer at /dev/<base>, as udev gives a camera that comes
// back whatever video node is free.
func (h *csHarness) putCameraAt(base string, c csCamera) {
	c.base = base
	h.mu.Lock()
	h.cams[base] = c
	h.mu.Unlock()
}

// A camera that comes back at another device path is the same camera: the
// sample IDs go on from where they were, however the hub at its new path is
// made. An app that reads them off its node, and an episode's index, never
// see one twice.
func TestSampleIdentitiesGoOnWhenACameraComesBackAtAnotherPath(t *testing.T) {
	for name, hubAtNewPath := range map[string]func(t *testing.T, s *VideoService) (hub *deviceHub, wantNext uint64){
		"a viewer's join": func(t *testing.T, s *VideoService) (*deviceHub, uint64) {
			hub, leave := viewerHub(t, s, "/dev/video2", &agentpb.StreamVideoRequest{})
			t.Cleanup(leave)
			return hub, 3
		},
		"an episode capture that takes a viewer's hub over": func(t *testing.T, s *VideoService) (*deviceHub, uint64) {
			defaulted, leave := viewerHub(t, s, "/dev/video2", &agentpb.StreamVideoRequest{})
			t.Cleanup(leave)
			if ids := numberFrames(defaulted, 1); !slices.Equal(ids, []uint64{3}) {
				t.Fatalf("the viewer's hub numbered its frame %v, want [3]", ids)
			}
			req := &agentpb.StreamVideoRequest{Width: 640, Height: 480, Framerate: 30}
			hub, id, _, _, _, _, _, err := s.joinHubForCapture(context.Background(), "/dev/video2", req, encodedFrames, noPreset)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { hub.unsubscribe(id) })
			if hub == defaulted {
				t.Fatal("the capture joined the viewer's hub instead of taking it over")
			}
			return hub, 4
		},
		"a camera node that takes a viewer's hub over": func(t *testing.T, s *VideoService) (*deviceHub, uint64) {
			held, leave := viewerHub(t, s, "/dev/video2", &agentpb.StreamVideoRequest{Width: 640, Height: 480})
			t.Cleanup(leave)
			if ids := numberFrames(held, 1); !slices.Equal(ids, []uint64{3}) {
				t.Fatalf("the viewer's hub numbered its frame %v, want [3]", ids)
			}
			share, err := s.joinHubForShare(context.Background(), "/dev/video2", 2, testSharedMode, noPreset)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(share.Leave)
			hub := currentHub(s, "/dev/video2")
			if hub == nil || hub == held {
				t.Fatal("the node joined the viewer's hub instead of taking it over")
			}
			return hub, 4
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCSHarness(t, csBrio())
			installFakeProducers(h.s)
			hub, leave := viewerHub(t, h.s, "/dev/video0", &agentpb.StreamVideoRequest{})
			if ids := numberFrames(hub, 2); !slices.Equal(ids, []uint64{1, 2}) {
				t.Fatalf("sample IDs before the camera went = %v, want [1 2]", ids)
			}
			leave()

			h.takeOut("video0")
			h.putCameraAt("video2", csBrio())
			back, wantNext := hubAtNewPath(t, h.s)
			if ids := numberFrames(back, 2); !slices.Equal(ids, []uint64{wantNext, wantNext + 1}) {
				t.Fatalf("sample IDs at the camera's new path = %v, want them to go on at %d", ids, wantNext)
			}
		})
	}
}

// Another camera that takes a path has its own sample IDs, and the camera that
// had the path before goes on with its own when it is back.
func TestSampleIdentitiesOfACameraThatTakesAnothersPathAreItsOwn(t *testing.T) {
	h := newCSHarness(t, csBrio())
	installFakeProducers(h.s)
	for _, turn := range []struct {
		camera csCamera
		want   []uint64
	}{
		{csBrio(), []uint64{1, 2}},
		{csC920(), []uint64{1, 2}},
		{csBrio(), []uint64{3, 4}},
		{csC920(), []uint64{3, 4}},
	} {
		h.putCameraAt("video0", turn.camera)
		hub, leave := viewerHub(t, h.s, "/dev/video0", &agentpb.StreamVideoRequest{})
		if ids := numberFrames(hub, 2); !slices.Equal(ids, turn.want) {
			t.Fatalf("%s at /dev/video0: sample IDs = %v, want %v", turn.camera.name, ids, turn.want)
		}
		leave()
	}
}

// Two cameras of one model that udev gives no by-id name are told apart by
// the port each is on: plugged in together they number their frames apart,
// and each goes on with its own numbers when the two come back at each other's
// paths.
func TestSampleIdentitiesOfSameModelCamerasPluggedInTogetherStayApart(t *testing.T) {
	left := csBrio()
	right := csCamera{"video2", "Brio 101", left.formats, camera.Identity{Device: csPort("1-2.2"), Index: "0", Name: "Brio 101"}}
	h := newCSHarness(t, left, right)
	installFakeProducers(h.s)
	if names := h.s.readStableNames(); len(names) != 0 {
		t.Fatalf("the test needs cameras without by-id names, got %v", names)
	}

	leftHub, leaveLeft := viewerHub(t, h.s, "/dev/video0", &agentpb.StreamVideoRequest{})
	rightHub, leaveRight := viewerHub(t, h.s, "/dev/video2", &agentpb.StreamVideoRequest{})
	var leftIDs, rightIDs []uint64
	for range 2 {
		leftIDs = append(leftIDs, numberFrames(leftHub, 1)...)
		rightIDs = append(rightIDs, numberFrames(rightHub, 1)...)
	}
	leftIDs = append(leftIDs, numberFrames(leftHub, 1)...)
	if !slices.Equal(leftIDs, []uint64{1, 2, 3}) || !slices.Equal(rightIDs, []uint64{1, 2}) {
		t.Fatalf("sample IDs = %v and %v, want [1 2 3] and [1 2]: each camera numbers its own frames", leftIDs, rightIDs)
	}
	leaveLeft()
	leaveRight()

	h.putCameraAt("video0", right)
	h.putCameraAt("video2", left)
	leftHub, leaveLeft = viewerHub(t, h.s, "/dev/video2", &agentpb.StreamVideoRequest{})
	defer leaveLeft()
	rightHub, leaveRight = viewerHub(t, h.s, "/dev/video0", &agentpb.StreamVideoRequest{})
	defer leaveRight()
	if l, r := numberFrames(leftHub, 1), numberFrames(rightHub, 1); !slices.Equal(l, []uint64{4}) || !slices.Equal(r, []uint64{3}) {
		t.Fatalf("after the two swapped paths, sample IDs = %v and %v, want [4] and [3]", l, r)
	}
}

// A network camera and a ROS 2 topic have no device node: their hub key is
// already their own, and stays their sample counter's key.
func TestSampleSeqKey_OnlyALocalVideoNodeIsKeyedByItsCamera(t *testing.T) {
	s := &VideoService{}
	s.cameraIdentity = func(base string) (camera.Identity, error) {
		return camera.Identity{Device: csPort("1-3"), Index: "0", Name: "the camera at " + base}, nil
	}
	for hubKey, want := range map[string]string{
		"/dev/video4":  "sysfs\x00" + csPort("1-3") + "\x000\x00the camera at video4",
		"ip:200":       "ip:200",
		"ros2:3":       "ros2:3",
		"/dev/media0":  "/dev/media0",
		"/dev/video":   "/dev/video",
		"video4":       "video4",
		"/dev/video4x": "/dev/video4x",
	} {
		if got := s.sampleSeqKey(hubKey); got != want {
			t.Errorf("sample counter key of %q = %q, want %q", hubKey, got, want)
		}
	}
}

// TestCameraSensorSubscriptionReportsDropsPerSample checks the honesty of the
// gap explanation: a loss must be charged to the sample it actually preceded,
// not to whichever sample the consumer happened to reach next.
//
// The subscriber channel holds four frames, so samples 1 to 4 are queued, 5 and
// 6 are dropped while they sit there, and 7 is queued once the consumer has
// drained the buffer. Nothing was lost before samples 1 to 4: they were all in
// the channel before the first drop happened. The two losses belong to sample 7,
// the first one delivered after them. Reading the hub's running total when a
// sample is DEQUEUED reported them against sample 1, up to a channel depth
// earlier than the gap they describe, which is the join a model input ledger
// exists to make.
func TestCameraSensorSubscriptionReportsDropsPerSample(t *testing.T) {
	hub, cancel := newSampleHub(new(atomic.Uint64))
	defer cancel()
	subID, frames, err := hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	subscription := &cameraSensorSubscription{hub: hub, subID: subID, frames: frames}
	produce := func() {
		hub.produce(&videoFrame{data: []byte{1}, codec: agentpb.VideoCodec_VIDEO_CODEC_H264, auAligned: true})
	}
	for i := 0; i < 6; i++ {
		produce()
	}
	ctx := context.Background()
	for i := 1; i <= 4; i++ {
		sample, err := subscription.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if sample.SampleID != uint64(i) {
			t.Fatalf("sample %d has id %d", i, sample.SampleID)
		}
		if sample.DroppedBefore != 0 {
			t.Fatalf("sample %d reports %d drops before it, but every drop happened after it was already queued", i, sample.DroppedBefore)
		}
		if sample.Encoding != "h264" || !sample.SelfContained {
			t.Fatalf("sample %d payload description = %q/%v", i, sample.Encoding, sample.SelfContained)
		}
	}
	produce()
	sample, err := subscription.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sample.SampleID != 7 {
		t.Fatalf("sample id after the gap = %d, want 7", sample.SampleID)
	}
	if sample.DroppedBefore != 2 {
		t.Fatalf("sample 7 reports %d drops before it, want the 2 the hub lost between samples 4 and 7", sample.DroppedBefore)
	}
	if total := hub.drops(subID); total != 2 {
		t.Fatalf("hub running drop total = %d, want 2", total)
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
	svc := NewVideoService(context.Background(), zap.NewNop(), nil)
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
	svc := NewVideoService(context.Background(), zap.NewNop(), nil)
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
	newHub, captureID, _, _, _, _, restarted, err := svc.joinHubForCapture(ctx, "/dev/video0", req, encodedFrames, noPreset)
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
	subscription := &cameraSensorSubscription{video: svc, key: "/dev/video0", hub: hub, subID: subID, frames: frames}
	hub.mu.Lock()
	hub.subDrops[subID] = 4
	// Three of the four have not been handed to a sample yet: one is still
	// riding on a frame sitting in the channel, two arrived after it.
	hub.subs[subID].queuedDrops = []uint64{1}
	hub.subs[subID].pendingDrops = 2
	hub.mu.Unlock()

	req := &agentpb.StreamVideoRequest{DeviceId: 0, Width: 640, Height: 480, Framerate: 15}
	newHub, captureID, _, _, _, _, restarted, err := svc.joinHubForCapture(ctx, "/dev/video0", req, encodedFrames, noPreset)
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
	if subscription.pending != 3 {
		t.Fatalf("pending = %d, want 3 (4 total drops minus 1 already reported)", subscription.pending)
	}
	if !subscription.awaitRandomAccess {
		t.Fatal("reattach must gate the new stream to a random-access unit")
	}
}
