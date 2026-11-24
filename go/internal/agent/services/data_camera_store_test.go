package services

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
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

// deliver broadcasts one frame and waits until every subscriber but the idle
// viewer has taken it, so a four-deep channel never drops a scripted frame.
func (cam *storeTestCamera) deliver(t *testing.T, frame *videoFrame) {
	t.Helper()
	cam.hub.broadcast(frame)
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

type storeRecording struct {
	dir    string
	result data.CaptureResult
}

// record runs one episode through startOne: the capture joins the hub, the
// frames are delivered in order, and the capture is stopped once it has
// indexed wantIndexed of them.
func (cam *storeTestCamera) record(t *testing.T, capture *data.SourceCapture, subscribers int, frames []*videoFrame, wantIndexed int) storeRecording {
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
	for _, frame := range frames {
		cam.deliver(t, frame)
	}
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

// cut reads the bytes an index record addresses, the way any reader of an
// episode reaches a frame.
func (r storeRecording) cut(t *testing.T, record cameraIndexRecord) []byte {
	t.Helper()
	segment := r.files(t)[filepath.Base(record.Segment)]
	end := record.ByteOffset + int64(record.ByteSize)
	if record.ByteOffset < 0 || end > int64(len(segment)) {
		t.Fatalf("index record %+v addresses bytes outside its %d-byte segment", record, len(segment))
	}
	return segment[record.ByteOffset:end]
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

// nativeH264At is a frame from a camera whose own output is H.264: native
// subscribers and encoded subscribers both receive it.
func nativeH264At(sampleID uint64, at time.Duration, payload []byte) *videoFrame {
	frame := h264At(sampleID, at, payload)
	frame.native = true
	return frame
}

func testJPEG(fill byte, size int) []byte {
	return append(append([]byte{0xFF, 0xD8}, bytes.Repeat([]byte{fill}, size)...), 0xFF, 0xD9)
}

// jpegAt is one whole JPEG frame from a camera captured natively as JPEG.
func jpegAt(sampleID uint64, at time.Duration, payload []byte) *videoFrame {
	frame := &videoFrame{data: payload, nativeClock: "CLOCK_REALTIME_AGENT_CAPTURE", codec: videoCodecMJPEG, auAligned: true, native: true}
	return storeTestStamp(frame, sampleID, at)
}

// storeLayoutGolden is everything a default episode leaves for one camera
// source: each file's bytes and the manifest fields the capture reports.
type storeLayoutGolden struct {
	Files          map[string]string `json:"files_hex"`
	Count          uint64            `json:"count"`
	Drops          *uint64           `json:"drops"`
	DropAccounting string            `json:"drop_accounting"`
	ClockDomain    string            `json:"clock_domain"`
	SourceDetail   string            `json:"source_detail"`
	ActualOffset   *int64            `json:"actual_offset"`
}

func layoutOf(t *testing.T, r storeRecording) storeLayoutGolden {
	t.Helper()
	layout := storeLayoutGolden{
		Files: map[string]string{}, Count: r.result.Count, Drops: r.result.Drops,
		DropAccounting: r.result.DropAccounting, ClockDomain: r.result.ClockDomain,
		SourceDetail: r.result.SourceDetail, ActualOffset: r.result.ActualOffset,
	}
	for name, content := range r.files(t) {
		layout.Files[name] = hex.EncodeToString(content)
	}
	return layout
}

// defaultLayoutScript exercises what shapes the default layout: a segment that
// waits for its first random-access unit and trims the bytes before it, a
// sequence gap, and a rotation.
func defaultLayoutScript() []*videoFrame {
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

// testdata/camera_store/default_layout.json was written by the adapter as it
// stood before it acted on capture.store, from defaultLayoutScript with no
// capture block. It is the reference for the layout a default episode had, so
// it is never regenerated to make this test pass.
func TestStoreUnsetAndVideoKeepTheLayoutFromBeforeStore(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "camera_store", "default_layout.json"))
	if err != nil {
		t.Fatal(err)
	}
	var want storeLayoutGolden
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if len(want.Files) != 4 || want.Count != 6 {
		t.Fatalf("golden layout holds %d file(s) and %d frame(s); it no longer describes the scripted episode", len(want.Files), want.Count)
	}

	for name, capture := range map[string]*data.SourceCapture{
		"no capture block": nil,
		"no store":         {},
		"store video":      {Store: data.CaptureStoreVideo},
	} {
		t.Run(name, func(t *testing.T) {
			cam := newStoreTestCamera(t)
			got := layoutOf(t, cam.record(t, capture, 2, defaultLayoutScript(), 6))
			gotJSON, _ := json.MarshalIndent(got, "", "  ")
			wantJSON, _ := json.MarshalIndent(want, "", "  ")
			if !bytes.Equal(gotJSON, wantJSON) {
				t.Fatalf("layout differs from the one recorded before store existed\n got: %s\nwant: %s", gotJSON, wantJSON)
			}
		})
	}
}

// jpegCameraScript is three frames of a camera captured natively as JPEG: each
// JPEG frame goes to native subscribers, and the H.264 made from it goes to
// encoded subscribers under the same sample ID.
func jpegCameraScript() (jpegs, video, interleaved []*videoFrame) {
	units := [][]byte{testRAUFrame, testInterFrame, testInterFrame}
	for i, at := range []time.Duration{1000 * time.Millisecond, 1033 * time.Millisecond, 1066 * time.Millisecond} {
		id := uint64(i + 1)
		jpeg := jpegAt(id, at, testJPEG(byte(0x10+i), 40+10*i))
		encoded := h264At(id, at, units[i])
		jpegs, video = append(jpegs, jpeg), append(video, encoded)
		interleaved = append(interleaved, jpeg, encoded)
	}
	return jpegs, video, interleaved
}

// h264CameraScript is three frames of a camera whose own output is H.264.
func h264CameraScript() []*videoFrame {
	return []*videoFrame{
		nativeH264At(1, 1000*time.Millisecond, testRAUFrame),
		nativeH264At(2, 1033*time.Millisecond, testInterFrame),
		nativeH264At(3, 1066*time.Millisecond, testInterFrame),
	}
}

const (
	storeTestSegmentDir = "cameras/v4l2__dev_video0/"
	indexedAsJPEG       = "VIDEO_CODEC_MJPEG"
	indexedAsH264       = "VIDEO_CODEC_H264"
)

// assertRecordsAddress checks that the index records of one codec name the
// given frames in order and that each record cuts its frame's exact bytes out
// of its segment.
func assertRecordsAddress(t *testing.T, r storeRecording, codec string, want []*videoFrame) {
	t.Helper()
	var records []cameraIndexRecord
	for _, record := range r.index(t) {
		if record.Codec == codec {
			records = append(records, record)
		}
	}
	if len(records) != len(want) {
		t.Fatalf("%d index record(s) with codec %s, want %d", len(records), codec, len(want))
	}
	for i, frame := range want {
		record := records[i]
		if record.SampleID != frame.sampleID {
			t.Errorf("%s record %d sample_id = %d, want %d", codec, i, record.SampleID, frame.sampleID)
		}
		if got := r.cut(t, record); !bytes.Equal(got, frame.data) {
			t.Errorf("%s record %d cuts %x out of %s, want the frame %x", codec, i, got, record.Segment, frame.data)
		}
		if record.CanonicalEpisodeNanos != frame.receiptBootNanos-int64(500*time.Millisecond) {
			t.Errorf("%s record %d canonical time = %d, want the frame's receipt on the episode timeline", codec, i, record.CanonicalEpisodeNanos)
		}
		if record.NativeTimestampNanos != frame.nativeNs || record.NativeClockDomain != frame.nativeClock {
			t.Errorf("%s record %d native time = %d in %q, want %d in %q", codec, i, record.NativeTimestampNanos, record.NativeClockDomain, frame.nativeNs, frame.nativeClock)
		}
		if record.Sequence == nil || *record.Sequence != frame.sequence {
			t.Errorf("%s record %d sequence = %v, want %d", codec, i, record.Sequence, frame.sequence)
		}
	}
}

func assertFileNames(t *testing.T, r storeRecording, want ...string) {
	t.Helper()
	want = append(want, "clock_samples.jsonl", "index.jsonl")
	sort.Strings(want)
	if got := r.fileNames(t); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("source directory holds %v, want %v", got, want)
	}
}

func TestStoreFramesOnAJPEGCameraWritesTheCamerasOwnFrames(t *testing.T) {
	cam := newStoreTestCamera(t)
	jpegs, _, interleaved := jpegCameraScript()
	r := cam.record(t, &data.SourceCapture{Store: data.CaptureStoreFrames}, 2, interleaved, 3)

	assertFileNames(t, r, "segment-000001.mjpeg")
	assertRecordsAddress(t, r, indexedAsJPEG, jpegs)
	for _, record := range r.index(t) {
		if record.Segment != storeTestSegmentDir+"segment-000001.mjpeg" {
			t.Errorf("record names segment %q", record.Segment)
		}
	}
	if r.result.Count != 3 {
		t.Errorf("count = %d, want the 3 JPEG frames", r.result.Count)
	}
	if strings.Contains(r.result.SourceDetail, "store:") {
		t.Errorf("detail %q carries a store note although frames were stored as asked", r.result.SourceDetail)
	}
}

func TestStoreBothOnAJPEGCameraWritesVideoAndFramesSideBySide(t *testing.T) {
	cam := newStoreTestCamera(t)
	jpegs, video, interleaved := jpegCameraScript()
	r := cam.record(t, &data.SourceCapture{Store: data.CaptureStoreBoth}, 3, interleaved, 6)

	assertFileNames(t, r, "segment-000001.h264", "segment-000001.mjpeg")
	assertRecordsAddress(t, r, indexedAsJPEG, jpegs)
	assertRecordsAddress(t, r, indexedAsH264, video)
	for _, record := range r.index(t) {
		wantSegment := storeTestSegmentDir + "segment-000001.h264"
		if record.Codec == indexedAsJPEG {
			wantSegment = storeTestSegmentDir + "segment-000001.mjpeg"
		}
		if record.Segment != wantSegment {
			t.Errorf("%s record names segment %q, want %q", record.Codec, record.Segment, wantSegment)
		}
	}
	if r.result.Count != 6 {
		t.Errorf("count = %d, want 3 video frames plus 3 JPEG frames", r.result.Count)
	}
	if r.result.Drops == nil || *r.result.Drops != 0 {
		t.Errorf("drops = %v, want 0: the two streams repeat each sequence number, which is not a gap", r.result.Drops)
	}
	want := "3 video frame(s) and 3 of the camera's own frame(s) are indexed; the source's frame and drop counts are sums over both"
	if !strings.Contains(r.result.SourceDetail, want) {
		t.Errorf("detail %q does not say %q", r.result.SourceDetail, want)
	}
}

func TestStoreVideoOnAJPEGCameraStoresOnlyTheVideo(t *testing.T) {
	cam := newStoreTestCamera(t)
	_, video, interleaved := jpegCameraScript()
	r := cam.record(t, &data.SourceCapture{Store: data.CaptureStoreVideo}, 2, interleaved, 3)

	assertFileNames(t, r, "segment-000001.h264")
	assertRecordsAddress(t, r, indexedAsH264, video)
}

// A camera whose own frames are H.264 has one stream. Every store value writes
// the files store: video writes, and the detail says so for the two values
// that asked for frames.
func TestStoreOnAnH264CameraWritesTheVideoFilesForEveryValue(t *testing.T) {
	video := newStoreTestCamera(t).record(t, &data.SourceCapture{Store: data.CaptureStoreVideo}, 2, h264CameraScript(), 3)
	assertFileNames(t, video, "segment-000001.h264")
	assertRecordsAddress(t, video, indexedAsH264, h264CameraScript())
	if strings.Contains(video.result.SourceDetail, "store:") {
		t.Errorf("store: video detail %q carries a store note", video.result.SourceDetail)
	}

	for store, tc := range map[string]struct {
		subscribers int
		detail      string
	}{
		data.CaptureStoreFrames: {2, "store: frames: this camera's own frames are its encoded video (VIDEO_CODEC_H264), stored in the same files and index as store: video"},
		data.CaptureStoreBoth:   {3, "store: both: this camera's own frames are its encoded video (VIDEO_CODEC_H264), so there is one stream and it is stored once"},
	} {
		t.Run(store, func(t *testing.T) {
			r := newStoreTestCamera(t).record(t, &data.SourceCapture{Store: store}, tc.subscribers, h264CameraScript(), 3)
			got, want := r.files(t), video.files(t)
			if len(got) != len(want) {
				t.Fatalf("source directory holds %v, want the files store: video writes", r.fileNames(t))
			}
			for name, content := range want {
				if !bytes.Equal(got[name], content) {
					t.Errorf("%s differs from the one store: video writes\n got: %s\nwant: %s", name, got[name], content)
				}
			}
			if r.result.Count != 3 {
				t.Errorf("count = %d, want each frame stored once", r.result.Count)
			}
			if !strings.Contains(r.result.SourceDetail, tc.detail) {
				t.Errorf("detail %q does not say %q", r.result.SourceDetail, tc.detail)
			}
		})
	}
}

func TestJPEGSegmentsRotateEveryCameraSegmentDuration(t *testing.T) {
	cam := newStoreTestCamera(t)
	var frames []*videoFrame
	for i, at := range []time.Duration{1 * time.Second, 5 * time.Second, 11200 * time.Millisecond, 11300 * time.Millisecond, 22 * time.Second} {
		frames = append(frames, jpegAt(uint64(i+1), at, testJPEG(byte(0x20+i), 30+i)))
	}
	r := cam.record(t, &data.SourceCapture{Store: data.CaptureStoreFrames}, 2, frames, 5)

	assertFileNames(t, r, "segment-000001.mjpeg", "segment-000002.mjpeg", "segment-000003.mjpeg")
	assertRecordsAddress(t, r, indexedAsJPEG, frames)
	wantSegments := []string{"segment-000001.mjpeg", "segment-000001.mjpeg", "segment-000002.mjpeg", "segment-000002.mjpeg", "segment-000003.mjpeg"}
	wantOffsets := []int64{0, int64(len(frames[0].data)), 0, int64(len(frames[2].data)), 0}
	for i, record := range r.index(t) {
		if record.Segment != storeTestSegmentDir+wantSegments[i] || record.ByteOffset != wantOffsets[i] {
			t.Errorf("frame %d is at %s offset %d, want %s offset %d", i+1, record.Segment, record.ByteOffset, wantSegments[i], wantOffsets[i])
		}
	}
}

func newStoreTestCapture(t *testing.T, clock *fakeReceipt, capture *data.SourceCapture) *cameraCapture {
	t.Helper()
	c := newTestCameraCapture(t, clock)
	c.source.Capture = capture
	if capture != nil {
		c.rateCap = capture.Rate
	}
	return c
}

func TestRateCapKeepsJPEGFramesPerFrame(t *testing.T) {
	clock := &fakeReceipt{}
	c := newStoreTestCapture(t, clock, &data.SourceCapture{Store: data.CaptureStoreFrames, Rate: 2})

	kept := map[uint64]bool{}
	for i, at := range []time.Duration{1000, 1200, 1400, 1500, 1900, 2000, 2100} {
		clock.now = int64(at * time.Millisecond)
		before := c.result.Count
		frame := jpegAt(uint64(i+1), 0, testJPEG(byte(i), 20))
		frame.receiptBootNanos = 0
		if err := c.handleFrame(frame); err != nil {
			t.Fatal(err)
		}
		kept[frame.sampleID] = c.result.Count > before
	}
	for id, want := range map[uint64]bool{1: true, 2: false, 3: false, 4: true, 5: false, 6: true, 7: false} {
		if kept[id] != want {
			t.Errorf("frame %d kept = %v, want %v: a frame is kept once 500ms have passed since the last kept one", id, kept[id], want)
		}
	}
	if c.result.Drops != nil && *c.result.Drops != 0 {
		t.Errorf("drops = %d, want 0: a frame skipped for the rate cap is not a loss", *c.result.Drops)
	}
	c.finishResultDetail(int64(2500 * time.Millisecond))
	want := "rate cap 2 Hz applied per frame to the camera's own frames; achieved 2 Hz; 4 frame(s) skipped"
	if !strings.Contains(c.result.SourceDetail, want) {
		t.Errorf("detail %q does not say %q", c.result.SourceDetail, want)
	}
	if strings.Contains(c.result.SourceDetail, "GOP") {
		t.Errorf("detail %q describes a group-of-pictures cap although only JPEG frames were stored", c.result.SourceDetail)
	}
}

const gstreamerHasNoWholeFrames = "the camera is read through GStreamer, which delivers no whole frames"

func TestFramesRefusedAtJoinRecordsVideoAndSaysWhy(t *testing.T) {
	for store, want := range map[string]string{
		data.CaptureStoreFrames: "store: frames was requested, but this camera's own frames cannot be stored: " + gstreamerHasNoWholeFrames + "; video is recorded instead",
		data.CaptureStoreBoth:   "store: both was requested, but this camera's own frames cannot be stored: " + gstreamerHasNoWholeFrames + "; only video is recorded",
	} {
		t.Run(store, func(t *testing.T) {
			cam := newStoreTestCamera(t)
			cam.hub.nativeNotOffered(fixedReason(gstreamerHasNoWholeFrames))
			_, video, _ := jpegCameraScript()
			r := cam.record(t, &data.SourceCapture{Store: store}, 2, video, 3)

			assertFileNames(t, r, "segment-000001.h264")
			assertRecordsAddress(t, r, indexedAsH264, video)
			if !strings.Contains(r.result.SourceDetail, want) {
				t.Errorf("detail %q does not say %q", r.result.SourceDetail, want)
			}
		})
	}
}

func (cam *storeTestCamera) openSubscribers(kind frameKind) int {
	cam.hub.mu.Lock()
	defer cam.hub.mu.Unlock()
	n := 0
	for id, sub := range cam.hub.subs {
		if id != 0 && sub.kind == kind && !sub.closed {
			n++
		}
	}
	return n
}

func (cam *storeTestCamera) waitForOpenSubscribers(t *testing.T, kind frameKind, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cam.openSubscribers(kind) == want && cam.subscribers() == 1+cam.openSubscribers(encodedFrames)+cam.openSubscribers(nativeFrames) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("capture holds %d open subscription(s) of kind %d, waited for %d", cam.openSubscribers(kind), kind, want)
}

func TestFramesRefusedLaterRecordsVideoFromThereOnAndSaysWhy(t *testing.T) {
	for store, tc := range map[string]struct {
		subscribers int
		before      func(jpegs, video []*videoFrame) []*videoFrame
		indexed     int
		detail      string
	}{
		data.CaptureStoreFrames: {
			2, func(jpegs, _ []*videoFrame) []*videoFrame { return jpegs[:1] }, 3,
			"store: frames was requested, but this camera's own frames stopped being available after 1 stored frame(s): " + gstreamerHasNoWholeFrames + "; video is recorded instead from there on",
		},
		data.CaptureStoreBoth: {
			3, func(jpegs, video []*videoFrame) []*videoFrame { return []*videoFrame{jpegs[0], video[0]} }, 4,
			"store: both was requested, but this camera's own frames stopped being available after 1 stored frame(s): " + gstreamerHasNoWholeFrames + "; only video is recorded from there on",
		},
	} {
		t.Run(store, func(t *testing.T) {
			cam := newStoreTestCamera(t)
			jpegs, video, _ := jpegCameraScript()
			session := data.CaptureSession{Directory: t.TempDir(), RequestBootNanos: int64(500 * time.Millisecond)}
			source := data.Source{ID: storeTestSource, Kind: "camera", Detail: "Test Cam", Capture: &data.SourceCapture{Store: store}}
			dir := filepath.Join(session.Directory, "cameras", safeCaptureName(source.ID))

			started := make(chan *cameraCapture, 1)
			go func() {
				c, err := cam.adapter.startOne(context.Background(), session, source, 0)
				if err != nil {
					t.Errorf("startOne: %v", err)
				}
				started <- c
			}()
			cam.waitForSubscribers(t, tc.subscribers)
			for _, frame := range tc.before(jpegs, video) {
				cam.deliver(t, frame)
			}
			capture := <-started
			if capture == nil {
				t.FailNow()
			}

			cam.hub.nativeNotOffered(fixedReason(gstreamerHasNoWholeFrames))
			cam.waitForOpenSubscribers(t, nativeFrames, 0)
			cam.waitForOpenSubscribers(t, encodedFrames, 1)
			restart := h264At(4, 1200*time.Millisecond, testRAUFrame)
			tail := h264At(5, 1233*time.Millisecond, testInterFrame)
			cam.deliver(t, restart)
			cam.deliver(t, tail)
			waitForIndexLines(t, dir, tc.indexed)

			results, err := capture.Stop(context.Background())
			if err != nil {
				t.Fatalf("stop: %v", err)
			}
			r := storeRecording{dir: dir, result: results[0]}
			assertFileNames(t, r, "segment-000001.h264", "segment-000001.mjpeg")
			assertRecordsAddress(t, r, indexedAsJPEG, jpegs[:1])
			wantVideo := []*videoFrame{restart, tail}
			if store == data.CaptureStoreBoth {
				wantVideo = []*videoFrame{video[0], restart, tail}
			}
			assertRecordsAddress(t, r, indexedAsH264, wantVideo)
			if !strings.Contains(r.result.SourceDetail, tc.detail) {
				t.Errorf("detail %q does not say %q", r.result.SourceDetail, tc.detail)
			}
		})
	}
}

func TestSegmentExtensionTableNamesEveryStoredCodec(t *testing.T) {
	want := map[agentpb.VideoCodec]string{
		agentpb.VideoCodec_VIDEO_CODEC_H264: ".h264",
		agentpb.VideoCodec_VIDEO_CODEC_VP8:  ".webm",
		videoCodecMJPEG:                     ".mjpeg",
	}
	if len(cameraSegmentExtensions) != len(want) {
		t.Fatalf("extension table = %v, want %v", cameraSegmentExtensions, want)
	}
	for codec, extension := range want {
		if cameraSegmentExtensions[codec] != extension {
			t.Errorf("extension for %s = %q, want %q", codecIndexName(codec), cameraSegmentExtensions[codec], extension)
		}
	}
}

func TestACodecOutsideTheExtensionTableIsNotWrittenAndIsNamed(t *testing.T) {
	const unknownCodec = agentpb.VideoCodec(99)
	c := newStoreTestCapture(t, &fakeReceipt{}, nil)
	raw := &videoFrame{data: []byte{1, 2, 3, 4}, codec: agentpb.VideoCodec_VIDEO_CODEC_RAW, nativeClock: "CLOCK_REALTIME_AGENT_CAPTURE"}
	ownUnknown := &videoFrame{data: []byte{5, 6, 7}, codec: unknownCodec, native: true, auAligned: true, nativeClock: "CLOCK_REALTIME_AGENT_CAPTURE"}
	for _, frame := range []*videoFrame{raw, raw, ownUnknown} {
		if err := c.handleFrame(frame); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.handleFrame(testH264Frame(testRAUFrame)); err != nil {
		t.Fatal(err)
	}

	names, err := filepath.Glob(filepath.Join(c.dir, "segment-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || filepath.Base(names[0]) != "segment-000001.h264" {
		t.Fatalf("segments = %v, want only the H.264 one", names)
	}
	if err := c.segment.Sync(); err != nil {
		t.Fatal(err)
	}
	if media, _ := os.ReadFile(names[0]); !bytes.Equal(media, testRAUFrame) {
		t.Fatalf("the H.264 segment holds %x, want only the H.264 frame", media)
	}
	if records := cameraIndexRecords(t, c); len(records) != 1 || c.result.Count != 1 {
		t.Fatalf("%d index record(s) and count %d, want 1: refused frames are not indexed", len(records), c.result.Count)
	}
	buffered := bufferedCameraFrame{frame: ownUnknown, randomAccess: true, resetSegment: true}
	if err := c.writeBufferedFrame(buffered); err != nil {
		t.Fatalf("a buffered frame in an unknown codec failed the pre-roll replay: %v", err)
	}
	if c.result.Count != 1 {
		t.Fatalf("count = %d after replaying a refused pre-roll frame, want 1", c.result.Count)
	}
	c.finishResultDetail(0)
	for _, want := range []string{
		"2 frame(s) in codec VIDEO_CODEC_RAW were not stored: no segment file format is defined for that codec",
		"2 frame(s) in codec 99 were not stored: no segment file format is defined for that codec",
	} {
		if !strings.Contains(c.result.SourceDetail, want) {
			t.Errorf("detail %q does not say %q", c.result.SourceDetail, want)
		}
	}
}

func ringSampleIDs(r *cameraPreRollRing) []uint64 {
	ids := make([]uint64, 0, len(r.frames))
	for _, bf := range r.frames {
		ids = append(ids, bf.frame.sampleID)
	}
	return ids
}

// Each JPEG frame is its own group of pictures, so the ring trims one frame at
// a time, for the window and for the byte bound alike.
func TestPreRollRingEvictsJPEGFramesOneAtATime(t *testing.T) {
	window := &cameraPreRollRing{buffer: 100 * time.Millisecond, limitBytes: 1 << 20}
	for i := 0; i < 50; i++ {
		window.add(jpegAt(uint64(i+1), time.Duration(i)*10*time.Millisecond, testJPEG(1, 100)))
	}
	if got, want := ringSampleIDs(window), []uint64{40, 41, 42, 43, 44, 45, 46, 47, 48, 49, 50}; !sameIDs(got, want) {
		t.Errorf("window-bounded ring holds %v, want %v: the newest frame at or before the window start and every frame after it", got, want)
	}
	if window.droppedForBytes {
		t.Error("the window bound was reported as the byte bound")
	}

	const limit = 1000
	frameBytes := len(testJPEG(1, 196))
	bytesBound := &cameraPreRollRing{buffer: time.Hour, limitBytes: limit}
	for i := 0; i < 50; i++ {
		bytesBound.add(jpegAt(uint64(i+1), time.Duration(i)*10*time.Millisecond, testJPEG(1, 196)))
		if bytesBound.bytes > limit {
			t.Fatalf("ring holds %d bytes after frame %d, above its %d bound", bytesBound.bytes, i+1, limit)
		}
	}
	if got, want := ringSampleIDs(bytesBound), []uint64{46, 47, 48, 49, 50}; !sameIDs(got, want) || bytesBound.bytes != 5*frameBytes {
		t.Errorf("byte-bounded ring holds %v (%d bytes), want the newest five %v", got, bytesBound.bytes, want)
	}
	if !bytesBound.droppedForBytes {
		t.Error("the ring did not report that the byte bound shortened it")
	}
}

func sameIDs(got, want []uint64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func armedStoreSource(t *testing.T, cam *storeTestCamera, capture *data.SourceCapture, buffer time.Duration) (data.Source, *armedCameraSource) {
	t.Helper()
	source := data.Source{ID: storeTestSource, Kind: "camera", Detail: "Test Cam", Capture: capture}
	cam.adapter.Arm("cam", buffer, []data.Source{source})
	t.Cleanup(func() { cam.adapter.Disarm("cam") })
	cam.adapter.armedMu.Lock()
	armed := cam.adapter.armed["cam"][source.ID]
	cam.adapter.armedMu.Unlock()
	if armed == nil {
		t.Fatal("the campaign armed no source")
	}
	return source, armed
}

// The byte bound is one figure for the source: a source that buffers video and
// frames divides it between the two rings, and a source with one ring has all
// of it.
func TestPreRollByteBoundIsATotalAcrossTheRingsOfOneSource(t *testing.T) {
	_, both := armedStoreSource(t, newStoreTestCamera(t), &data.SourceCapture{Store: data.CaptureStoreBoth}, time.Second)
	if both.ownFrameRing == nil {
		t.Fatal("a source that stores both buffers no frames of the camera's own")
	}
	if both.ring.limitBytes <= 0 || both.ownFrameRing.limitBytes <= 0 || both.ring.limitBytes+both.ownFrameRing.limitBytes != preRollCameraLimitBytes {
		t.Fatalf("rings are bounded at %d and %d bytes, want two shares that add up to %d", both.ring.limitBytes, both.ownFrameRing.limitBytes, preRollCameraLimitBytes)
	}

	for _, store := range []string{data.CaptureStoreVideo, data.CaptureStoreFrames} {
		_, one := armedStoreSource(t, newStoreTestCamera(t), &data.SourceCapture{Store: store}, time.Second)
		if one.ownFrameRing != nil || one.ring.limitBytes != preRollCameraLimitBytes {
			t.Fatalf("store: %s buffers one form, so its ring should have the whole %d bytes", store, preRollCameraLimitBytes)
		}
	}
}

func (cam *storeTestCamera) produceAndWait(t *testing.T, frame *videoFrame) *videoFrame {
	t.Helper()
	if before, receipt, after, err := data.CaptureReceipt(); err == nil {
		frame.receiptBootNanos, frame.receiptUncertaintyNanos = receipt, (after-before+1)/2
	}
	cam.deliver(t, frame)
	return frame
}

func (cam *storeTestCamera) triggerAndStop(t *testing.T, source data.Source, wantIndexed int) storeRecording {
	t.Helper()
	_, origin, _, err := data.CaptureReceipt()
	if err != nil {
		t.Fatal(err)
	}
	session := data.CaptureSession{Directory: t.TempDir(), RequestBootNanos: origin, CampaignKey: "cam"}
	capture, err := cam.adapter.Start(context.Background(), session, []data.Source{source})
	if err != nil || capture == nil {
		t.Fatalf("triggering the armed campaign: capture %v, err %v", capture, err)
	}
	dir := filepath.Join(session.Directory, "cameras", safeCaptureName(source.ID))
	waitForIndexLines(t, dir, wantIndexed)
	results, err := capture.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	return storeRecording{dir: dir, result: results[0]}
}

// assertPreRollRecords is assertRecordsAddress for frames stamped with the real
// clock: every record must sit before the trigger and cut its frame's bytes.
func assertPreRollRecords(t *testing.T, r storeRecording, codec string, want []*videoFrame) {
	t.Helper()
	var records []cameraIndexRecord
	for _, record := range r.index(t) {
		if record.Codec == codec {
			records = append(records, record)
		}
	}
	if len(records) != len(want) {
		t.Fatalf("%d index record(s) with codec %s, want %d", len(records), codec, len(want))
	}
	for i, frame := range want {
		record := records[i]
		if record.SampleID != frame.sampleID || record.AgentReceiptBootNanos != frame.receiptBootNanos {
			t.Errorf("%s record %d is sample %d received at %d, want sample %d received at %d", codec, i, record.SampleID, record.AgentReceiptBootNanos, frame.sampleID, frame.receiptBootNanos)
		}
		if record.CanonicalEpisodeNanos >= 0 {
			t.Errorf("%s record %d sits at %dns, want a time before the trigger", codec, i, record.CanonicalEpisodeNanos)
		}
		if got := r.cut(t, record); !bytes.Equal(got, frame.data) {
			t.Errorf("%s record %d cuts %x out of %s, want the frame %x", codec, i, got, record.Segment, frame.data)
		}
	}
}

func TestArmedFramesCampaignStoresThePreRollAsJPEGFrames(t *testing.T) {
	cam := newStoreTestCamera(t)
	source, _ := armedStoreSource(t, cam, &data.SourceCapture{Store: data.CaptureStoreFrames}, 5*time.Second)
	cam.waitForOpenSubscribers(t, nativeFrames, 1)

	var jpegs []*videoFrame
	for i := 0; i < 4; i++ {
		jpeg := cam.produceAndWait(t, jpegAt(uint64(i+1), 0, testJPEG(byte(0x30+i), 50+i)))
		cam.produceAndWait(t, h264At(uint64(i+1), 0, testRAUFrame))
		jpegs = append(jpegs, jpeg)
		time.Sleep(2 * time.Millisecond)
	}
	r := cam.triggerAndStop(t, source, 4)

	assertFileNames(t, r, "segment-000001.mjpeg")
	assertPreRollRecords(t, r, indexedAsJPEG, jpegs)
	if r.result.ActualOffset == nil || *r.result.ActualOffset >= 0 {
		t.Errorf("actual offset = %v, want the first JPEG frame's time before the trigger", r.result.ActualOffset)
	}
	if !strings.Contains(r.result.SourceDetail, "flushed 4 frame(s)") {
		t.Errorf("detail %q does not report the flushed pre-roll", r.result.SourceDetail)
	}
}

func TestArmedBothCampaignStoresThePreRollInBothForms(t *testing.T) {
	cam := newStoreTestCamera(t)
	source, _ := armedStoreSource(t, cam, &data.SourceCapture{Store: data.CaptureStoreBoth}, 5*time.Second)
	cam.waitForOpenSubscribers(t, nativeFrames, 1)

	var jpegs, video []*videoFrame
	units := [][]byte{testRAUFrame, testInterFrame, testInterFrame}
	for i, unit := range units {
		jpegs = append(jpegs, cam.produceAndWait(t, jpegAt(uint64(i+1), 0, testJPEG(byte(0x40+i), 60+i))))
		video = append(video, cam.produceAndWait(t, h264At(uint64(i+1), 0, unit)))
		time.Sleep(2 * time.Millisecond)
	}
	r := cam.triggerAndStop(t, source, 6)

	assertFileNames(t, r, "segment-000001.h264", "segment-000001.mjpeg")
	assertPreRollRecords(t, r, indexedAsJPEG, jpegs)
	assertPreRollRecords(t, r, indexedAsH264, video)
	if !strings.Contains(r.result.SourceDetail, "flushed 6 frame(s)") {
		t.Errorf("detail %q does not report the flushed pre-roll", r.result.SourceDetail)
	}
}

func TestArmedFramesCampaignBuffersVideoWhenTheCameraHasNoFramesOfItsOwn(t *testing.T) {
	cam := newStoreTestCamera(t)
	cam.hub.nativeNotOffered(fixedReason(gstreamerHasNoWholeFrames))
	source, armed := armedStoreSource(t, cam, &data.SourceCapture{Store: data.CaptureStoreFrames}, 5*time.Second)
	if armed.primaryKind != encodedFrames {
		t.Fatal("the armed source did not fall back to buffering video")
	}

	video := []*videoFrame{
		cam.produceAndWait(t, h264At(1, 0, testRAUFrame)),
		cam.produceAndWait(t, h264At(2, 0, testInterFrame)),
	}
	time.Sleep(2 * time.Millisecond)
	r := cam.triggerAndStop(t, source, 2)

	assertFileNames(t, r, "segment-000001.h264")
	assertPreRollRecords(t, r, indexedAsH264, video)
	want := "store: frames was requested, but this camera's own frames cannot be stored: " + gstreamerHasNoWholeFrames + "; video is recorded instead"
	if !strings.Contains(r.result.SourceDetail, want) {
		t.Errorf("detail %q does not say %q", r.result.SourceDetail, want)
	}
}

// A producer that dies under a capture storing both forms is rejoined for
// both: each stream continues in a new segment of its own.
func TestBothCaptureRejoinsBothSubscriptionsAfterTheProducerDies(t *testing.T) {
	newHub := func() *deviceHub {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		return &deviceHub{subs: make(map[int]*hubSubscriber), subDrops: make(map[int]uint64), ctx: ctx, cancel: cancel}
	}
	deadHub, liveHub := newHub(), newHub()
	subID, frames, err := deadHub.subscribe()
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := newStoreTestCapture(t, &fakeReceipt{}, &data.SourceCapture{Store: data.CaptureStoreBoth})
	c.hub, c.subID, c.frames = deadHub, subID, frames
	c.wantsNativeBeside = true
	c.joinNativeBeside()
	c.ctx, c.cancel = ctx, cancel
	c.done, c.ready = make(chan struct{}), make(chan error, 1)
	var rejoinedKinds []frameKind
	c.rejoin = func(_ context.Context, kind frameKind) (*deviceHub, int, chan *videoFrame, error) {
		rejoinedKinds = append(rejoinedKinds, kind)
		id, ch, joinErr := liveHub.subscribeKind(kind)
		return liveHub, id, ch, joinErr
	}
	go c.run()

	deadHub.broadcast(jpegAt(1, time.Second, testJPEG(1, 30)))
	deadHub.broadcast(h264At(1, time.Second, testRAUFrame))
	waitForCaptureCount(t, c, 2)
	deadHub.mu.Lock()
	for _, sub := range deadHub.subs {
		if !sub.closed {
			sub.closed = true
			close(sub.ch)
		}
	}
	deadHub.mu.Unlock()

	deadline := time.Now().Add(3 * time.Second)
	subscribedAgain := func() int {
		liveHub.mu.Lock()
		defer liveHub.mu.Unlock()
		return len(liveHub.subs)
	}
	for subscribedAgain() != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if subscribedAgain() != 2 {
		t.Fatalf("the capture holds %d subscription(s) on the replacement hub, want the encoded one and the native one", subscribedAgain())
	}
	liveHub.broadcast(jpegAt(1, 2*time.Second, testJPEG(2, 30)))
	liveHub.broadcast(h264At(1, 2*time.Second, testRAUFrame))
	waitForCaptureCount(t, c, 4)

	results, err := c.Stop(context.Background())
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if len(rejoinedKinds) != 1 || rejoinedKinds[0] != encodedFrames {
		t.Errorf("rejoined for kinds %v, want one encoded rejoin with the native subscription made beside it", rejoinedKinds)
	}
	r := storeRecording{dir: c.dir, result: results[0]}
	assertFileNames(t, r, "segment-000001.h264", "segment-000002.h264", "segment-000001.mjpeg", "segment-000002.mjpeg")
	if r.result.Discontinuities != 1 || r.result.Count != 4 {
		t.Errorf("discontinuities = %d, count = %d; want 1 and 4", r.result.Discontinuities, r.result.Count)
	}
	if r.result.Drops != nil && *r.result.Drops != 0 {
		t.Errorf("drops = %d, want 0: sequence numbering starts over with the new producer in both streams", *r.result.Drops)
	}
}
