package services

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
)

const storeTestCameraPath = "/dev/video0"

var storeTestSource = "v4l2:" + storeTestCameraPath

// storeTestCamera is a hub a test broadcasts into by hand, held open by one
// encoded viewer the way a dashboard holds a camera at its defaults.
type storeTestCamera struct {
	video   *VideoService
	hub     *deviceHub
	adapter *cameraDataAdapter
}

func newStoreTestCamera(t *testing.T) *storeTestCamera {
	t.Helper()
	video := NewVideoService(context.Background(), zap.NewNop(), nil)
	hub, _ := newDefaultHub(t, video, storeTestCameraPath)
	return &storeTestCamera{video: video, hub: hub, adapter: &cameraDataAdapter{video: video}}
}

func (cam *storeTestCamera) subscribers() int {
	cam.hub.mu.Lock()
	defer cam.hub.mu.Unlock()
	return len(cam.hub.subs)
}

func (cam *storeTestCamera) waitForSubscribers(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cam.subscribers() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("hub has %d subscriber(s), waited for %d", cam.subscribers(), want)
}

// waitUntilTaken waits until every subscriber but the idle viewer has taken
// what was broadcast, so a four-deep channel never drops a scripted frame.
func (cam *storeTestCamera) waitUntilTaken(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		queued := 0
		cam.hub.mu.Lock()
		for id, sub := range cam.hub.subs {
			if id != 0 && !sub.closed {
				queued += len(sub.ch)
			}
		}
		cam.hub.mu.Unlock()
		if queued == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("a subscriber never took the frame")
}

func (cam *storeTestCamera) deliver(t *testing.T, frame *videoFrame) {
	t.Helper()
	cam.hub.broadcast(frame)
	cam.waitUntilTaken(t)
}

type storeRecording struct {
	dir    string
	result data.CaptureResult
}

// record runs one episode through startOne: the capture joins the hub, the
// script delivers its frames, and the capture is stopped once it has indexed
// wantIndexed of them.
func (cam *storeTestCamera) record(t *testing.T, capture *data.SourceCapture, subscribers int, script func(), wantIndexed int) storeRecording {
	t.Helper()
	session := data.CaptureSession{Directory: t.TempDir(), RequestBootNanos: int64(500 * time.Millisecond)}
	source := data.Source{ID: storeTestSource, Kind: "camera", Detail: "Test Cam", Capture: capture}
	dir := filepath.Join(session.Directory, "cameras", safeCaptureName(source.ID))

	type started struct {
		capture *cameraCapture
		err     error
	}
	done := make(chan started, 1)
	go func() {
		c, err := cam.adapter.startOne(context.Background(), session, source, 0)
		done <- started{c, err}
	}()
	cam.waitForSubscribers(t, subscribers)
	script()
	waitForIndexLines(t, dir, wantIndexed)
	var running started
	select {
	case running = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the capture never became ready")
	}
	if running.err != nil {
		t.Fatalf("startOne: %v", running.err)
	}
	results, err := running.capture.Stop(context.Background())
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	return storeRecording{dir: dir, result: results[0]}
}

func (cam *storeTestCamera) recordFrames(t *testing.T, capture *data.SourceCapture, subscribers int, frames []*videoFrame, wantIndexed int) storeRecording {
	t.Helper()
	return cam.record(t, capture, subscribers, func() {
		for _, frame := range frames {
			cam.deliver(t, frame)
		}
	}, wantIndexed)
}

func waitForIndexLines(t *testing.T, dir string, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	got := 0
	for time.Now().Before(deadline) {
		raw, _ := os.ReadFile(filepath.Join(dir, "index.jsonl"))
		got = bytes.Count(raw, []byte("\n"))
		if got >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("capture indexed %d frame(s), waited for %d", got, want)
}

func (r storeRecording) files(t *testing.T) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		b, err := os.ReadFile(filepath.Join(r.dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[entry.Name()] = b
	}
	return out
}

func (r storeRecording) fileNames(t *testing.T) []string {
	t.Helper()
	var names []string
	for name := range r.files(t) {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r storeRecording) index(t *testing.T) []cameraIndexRecord {
	t.Helper()
	var records []cameraIndexRecord
	for _, line := range bytes.Split(bytes.TrimSpace(r.files(t)["index.jsonl"]), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var record cameraIndexRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

func storeTestStamp(frame *videoFrame, sampleID uint64, at time.Duration) *videoFrame {
	frame.sampleID = sampleID
	frame.sequence, frame.sequenceValid = uint32(sampleID), true
	frame.receiptBootNanos = int64(at)
	frame.receiptUncertaintyNanos = 700
	frame.tsNs = uint64(1_700_000_000*time.Second + at)
	frame.nativeNs = int64(at) + 4242
	return frame
}

// h264At is an encoded access unit as encoded subscribers receive it.
func h264At(sampleID uint64, at time.Duration, payload []byte) *videoFrame {
	return storeTestStamp(testH264Frame(payload), sampleID, at)
}

// scriptedCapture is a capture on a bare hub with a scripted clock, run by its
// own loop, so its teardown reads the scripted end of the episode.
type scriptedCapture struct {
	capture *cameraCapture
	clock   *fakeReceipt
	frames  chan *videoFrame
}

func newScriptedCapture(t *testing.T) *scriptedCapture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	hub := &deviceHub{subs: make(map[int]*hubSubscriber), subDrops: make(map[int]uint64), ctx: ctx, cancel: cancel}
	subID, frames, err := hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeReceipt{}
	c := newTestCameraCapture(t, clock)
	c.source.Detail = "Test Cam"
	c.session = data.CaptureSession{RequestBootNanos: int64(500 * time.Millisecond)}
	c.hub, c.subID, c.frames = hub, subID, frames
	c.ctx, c.cancel = ctx, cancel
	c.done, c.ready = make(chan struct{}), make(chan error, 1)
	c.mode = "continuous"
	return &scriptedCapture{capture: c, clock: clock, frames: frames}
}

// record hands the capture each frame as its subscription would and stops it
// at the scripted end time.
func (s *scriptedCapture) record(t *testing.T, frames []*videoFrame, end time.Duration) storeRecording {
	t.Helper()
	go s.capture.run()
	for _, frame := range frames {
		s.frames <- frame
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(s.frames) > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	s.clock.now = int64(end)
	results, err := s.capture.Stop(context.Background())
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	return storeRecording{dir: s.capture.dir, result: results[0]}
}

// cameraLayoutGolden is everything a default episode leaves for one camera
// source: each file's bytes and the result the capture reports.
type cameraLayoutGolden struct {
	Files  map[string]string  `json:"files_hex"`
	Result data.CaptureResult `json:"result"`
}

func layoutOf(t *testing.T, r storeRecording) cameraLayoutGolden {
	t.Helper()
	layout := cameraLayoutGolden{Files: map[string]string{}, Result: r.result}
	for name, content := range r.files(t) {
		layout.Files[name] = hex.EncodeToString(content)
	}
	return layout
}

func continuousLayoutScript() []*videoFrame {
	leadingBytesThenRAU := append([]byte{9, 9, 9}, testRAUFrame...)
	return []*videoFrame{
		h264At(1, 900*time.Millisecond, testInterFrame),
		h264At(2, 1000*time.Millisecond, leadingBytesThenRAU),
		h264At(3, 1033*time.Millisecond, testInterFrame),
		h264At(5, 1100*time.Millisecond, testInterFrame),
		h264At(6, 5*time.Second, testRAUFrame),
		h264At(7, 11500*time.Millisecond, testRAUFrame),
		h264At(8, 11600*time.Millisecond, testInterFrame),
	}
}

func continuousLayout(t *testing.T, capture *data.SourceCapture) storeRecording {
	return newStoreTestCamera(t).recordFrames(t, capture, 2, continuousLayoutScript(), 6)
}

func snapshotLayout(t *testing.T) storeRecording {
	s := newScriptedCapture(t)
	s.capture.mode, s.capture.interval = captureModeSnapshot, int64(2*time.Second)
	return s.record(t, []*videoFrame{
		h264At(1, 600*time.Millisecond, testInterFrame),
		h264At(2, 700*time.Millisecond, testRAUFrame),
		h264At(3, 1000*time.Millisecond, testRAUFrame),
		h264At(4, 3*time.Second, testInterFrame),
		h264At(5, 7*time.Second, append([]byte{9, 9}, testRAUFrame...)),
	}, 11*time.Second)
}

func rateCapLayout(t *testing.T) storeRecording {
	s := newScriptedCapture(t)
	s.capture.rateCap = 2
	return s.record(t, []*videoFrame{
		h264At(1, 1000*time.Millisecond, testRAUFrame),
		h264At(2, 1100*time.Millisecond, testInterFrame),
		h264At(3, 1250*time.Millisecond, testRAUFrame),
		h264At(4, 1300*time.Millisecond, testInterFrame),
		h264At(5, 3*time.Second, testRAUFrame),
		h264At(6, 3100*time.Millisecond, testInterFrame),
	}, 4*time.Second)
}

// vp8Layout records a WebM stream cut into pipe reads, one of which the
// subscription loses inside the third Cluster.
func vp8Layout(t *testing.T) storeRecording {
	stream, _, clusters := webmTestStream()
	const readSize = 7
	reads := vp8TestFrames(t, stream, readSize)
	lost := clusters[2].tag/readSize + 1
	var frames []*videoFrame
	for k, frame := range reads {
		frame.receiptBootNanos = int64(time.Second) + int64(k)*100*ms
		frame.receiptUncertaintyNanos = 700
		if k != lost {
			frames = append(frames, frame)
		}
	}
	return newScriptedCapture(t).record(t, frames, 60*time.Second)
}

// preRollLayout arms a campaign, buffers three groups of pictures before the
// trigger and records two more after it.
func preRollLayout(t *testing.T) storeRecording {
	cam := newStoreTestCamera(t)
	source := data.Source{ID: storeTestSource, Kind: "camera", Detail: "Test Cam"}
	cam.adapter.Arm("cam", 5*time.Second, []data.Source{source})
	t.Cleanup(func() { cam.adapter.Disarm("cam") })
	cam.waitForSubscribers(t, 2)

	const trigger = 10 * time.Second
	id := uint64(0)
	group := func(at time.Duration) {
		id++
		cam.deliver(t, h264At(id, at, testRAUFrame))
		id++
		cam.deliver(t, h264At(id, at+33*time.Millisecond, testInterFrame))
	}
	group(trigger - 900*time.Millisecond)
	group(trigger - 600*time.Millisecond)
	group(trigger - 300*time.Millisecond)

	session := data.CaptureSession{Directory: t.TempDir(), RequestBootNanos: int64(trigger), CampaignKey: "cam"}
	capture, err := cam.adapter.Start(context.Background(), session, []data.Source{source})
	if err != nil || capture == nil {
		t.Fatalf("triggering the armed campaign: capture %v, err %v", capture, err)
	}
	id++
	cam.deliver(t, h264At(id, trigger+50*time.Millisecond, testInterFrame))
	group(trigger + 100*time.Millisecond)
	group(trigger + 400*time.Millisecond)
	dir := filepath.Join(session.Directory, "cameras", safeCaptureName(source.ID))
	waitForIndexLines(t, dir, 11)
	results, err := capture.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return storeRecording{dir: dir, result: results[0]}
}

func TestDefaultCameraLayoutsMatchTheAdapterBeforeStore(t *testing.T) {
	layouts := []struct {
		golden string
		name   string
		record func(t *testing.T) storeRecording
	}{
		{"continuous", "no capture block", func(t *testing.T) storeRecording { return continuousLayout(t, nil) }},
		{"continuous", "no store", func(t *testing.T) storeRecording { return continuousLayout(t, &data.SourceCapture{}) }},
		{"continuous", "store video", func(t *testing.T) storeRecording {
			return continuousLayout(t, &data.SourceCapture{Store: "video"})
		}},
		{"snapshot", "snapshot", snapshotLayout},
		{"rate_cap", "rate cap on aligned H.264", rateCapLayout},
		{"vp8", "VP8", vp8Layout},
		{"pre_roll", "pre-roll flush", preRollLayout},
	}
	for _, layout := range layouts {
		t.Run(layout.name, func(t *testing.T) {
			got, err := json.MarshalIndent(layoutOf(t, layout.record(t)), "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join("testdata", "camera_store", layout.golden+"_layout.json"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(bytes.TrimSpace(got), bytes.TrimSpace(want)) {
				t.Fatalf("layout differs from the adapter before store\n got: %s\nwant: %s", got, want)
			}
		})
	}
}
