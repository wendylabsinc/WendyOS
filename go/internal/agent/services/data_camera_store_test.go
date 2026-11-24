package services

import (
	"bytes"
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// The scripted device: its kernel stamps a buffer on CLOCK_MONOTONIC when the
// frame is captured, the agent receives it a little later on CLOCK_BOOTTIME,
// and the two clocks differ by the time the device has spent suspended.
const (
	scriptedBootMinusMonotonic = 250 * time.Millisecond
	scriptedCaptureToReceipt   = 3 * time.Millisecond
	scriptedMappingHalfWidth   = 400
	kernelClock                = "CLOCK_MONOTONIC_V4L2"
	recordingOrigin            = 500 * time.Millisecond
)

func scriptedMonotonicMapping() (data.MonotonicMappingSample, error) {
	offset := int64(scriptedBootMinusMonotonic)
	return data.MonotonicMappingSample{OffsetLowerNanos: offset - scriptedMappingHalfWidth, OffsetUpperNanos: offset + scriptedMappingHalfWidth}, nil
}

func newKernelClockCamera(t *testing.T) *storeTestCamera {
	t.Helper()
	cam := newStoreTestCamera(t)
	cam.adapter.monotonicMapping = scriptedMonotonicMapping
	return cam
}

// kernelStamp stamps a frame the way native capture and the hub do for a
// buffer the agent received at the given time.
func kernelStamp(frame *videoFrame, sampleID uint64, receivedAt time.Duration) *videoFrame {
	frame.sampleID = sampleID
	frame.sequence, frame.sequenceValid = uint32(sampleID), true
	frame.receiptBootNanos = int64(receivedAt)
	frame.receiptUncertaintyNanos = 700
	frame.tsNs = uint64(1_700_000_000*time.Second + receivedAt)
	frame.nativeClock = kernelClock
	frame.nativeNs = int64(receivedAt - scriptedCaptureToReceipt - scriptedBootMinusMonotonic)
	frame.auAligned, frame.native = true, true
	return frame
}

func testJPEG(fill byte, size int) []byte {
	return append(append([]byte{0xFF, 0xD8}, bytes.Repeat([]byte{fill}, size)...), 0xFF, 0xD9)
}

func jpegAt(sampleID uint64, receivedAt time.Duration, payload []byte) *videoFrame {
	return kernelStamp(&videoFrame{data: payload, codec: videoCodecMJPEG}, sampleID, receivedAt)
}

// nativeH264At is a frame of a camera whose own output is H.264: native and
// encoded subscribers both receive it.
func nativeH264At(sampleID uint64, receivedAt time.Duration, payload []byte) *videoFrame {
	return kernelStamp(&videoFrame{data: payload, codec: agentpb.VideoCodec_VIDEO_CODEC_H264}, sampleID, receivedAt)
}

// chunkAt is a read of the old pipeline's byte stream: no access-unit
// alignment, no driver sequence, no kernel timestamp.
func chunkAt(sampleID uint64, receivedAt time.Duration, payload []byte) *videoFrame {
	frame := testChunkedH264Frame(payload)
	frame.sampleID = sampleID
	frame.receiptBootNanos = int64(receivedAt)
	frame.receiptUncertaintyNanos = 700
	return frame
}

// derive broadcasts the H.264 made from a source frame through the hub's own
// path, which gives it the source's identity, times and receipt.
func (cam *storeTestCamera) derive(t *testing.T, source *videoFrame, unit []byte) *videoFrame {
	t.Helper()
	derived := &videoFrame{data: unit, codec: agentpb.VideoCodec_VIDEO_CODEC_H264, auAligned: true}
	if _, err := cam.hub.broadcastDerivedFrom(source, derived); err != nil {
		t.Fatal(err)
	}
	cam.waitUntilTaken(t)
	return derived
}

// jpegCameraEpisode is what a camera captured natively as JPEG delivers: its
// JPEG frames to native subscribers, and the H.264 made from them to encoded
// subscribers a few frames later. The encoder never delivers sample 4.
type jpegCameraEpisode struct {
	jpegs, video []*videoFrame
}

func (cam *storeTestCamera) playJPEGCamera(t *testing.T) *jpegCameraEpisode {
	t.Helper()
	episode := &jpegCameraEpisode{}
	jpeg := func(id uint64) *videoFrame {
		frame := jpegAt(id, time.Second+time.Duration(id-1)*33*time.Millisecond, testJPEG(byte(0x10+id), 40+10*int(id)))
		cam.deliver(t, frame)
		episode.jpegs = append(episode.jpegs, frame)
		return frame
	}
	derive := func(source *videoFrame, unit []byte) {
		episode.video = append(episode.video, cam.derive(t, source, unit))
	}
	j1, j2, j3 := jpeg(1), jpeg(2), jpeg(3)
	derive(j1, testRAUFrame)
	jpeg(4)
	derive(j2, testInterFrame)
	j5 := jpeg(5)
	derive(j3, testInterFrame)
	j6 := jpeg(6)
	derive(j5, testInterFrame)
	derive(j6, testInterFrame)
	return episode
}

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

func (r storeRecording) cut(t *testing.T, record cameraIndexRecord) []byte {
	t.Helper()
	segment := r.files(t)[filepath.Base(record.Segment)]
	end := record.ByteOffset + int64(record.ByteSize)
	if record.ByteOffset < 0 || end > int64(len(segment)) {
		t.Fatalf("index record %+v addresses bytes outside its %d-byte segment", record, len(segment))
	}
	return segment[record.ByteOffset:end]
}

func (r storeRecording) recordsOf(t *testing.T, codec string) []cameraIndexRecord {
	t.Helper()
	var records []cameraIndexRecord
	for _, record := range r.index(t) {
		if record.Codec == codec {
			records = append(records, record)
		}
	}
	return records
}

// assertRecordsAddress checks that the index records of one codec name the
// given frames in order, that each record cuts its frame's exact bytes out of
// its segment, and that it carries the frame's identity and native time.
func assertRecordsAddress(t *testing.T, r storeRecording, codec string, want []*videoFrame) {
	t.Helper()
	records := r.recordsOf(t, codec)
	if len(records) != len(want) {
		t.Fatalf("%d index record(s) with codec %s, want %d", len(records), codec, len(want))
	}
	for i, frame := range want {
		record := records[i]
		if record.SampleID != frame.sampleID || record.SampleID == 0 {
			t.Errorf("%s record %d sample_id = %d, want %d", codec, i, record.SampleID, frame.sampleID)
		}
		if got := r.cut(t, record); !bytes.Equal(got, frame.data) {
			t.Errorf("%s record %d cuts %x out of %s, want the frame %x", codec, i, got, record.Segment, frame.data)
		}
		if record.AgentReceiptBootNanos != frame.receiptBootNanos {
			t.Errorf("%s record %d receipt = %d, want %d", codec, i, record.AgentReceiptBootNanos, frame.receiptBootNanos)
		}
		if record.NativeTimestampNanos != frame.nativeNs || record.NativeClockDomain != frame.nativeClock {
			t.Errorf("%s record %d native time = %d in %q, want %d in %q", codec, i, record.NativeTimestampNanos, record.NativeClockDomain, frame.nativeNs, frame.nativeClock)
		}
		if frame.sequenceValid && (record.Sequence == nil || *record.Sequence != frame.sequence) {
			t.Errorf("%s record %d sequence = %v, want %d", codec, i, record.Sequence, frame.sequence)
		}
	}
}

// assertKernelCaptureTimes checks that records of kernel-stamped frames sit at
// the capture time the kernel recorded, mapped onto the episode timeline.
func assertKernelCaptureTimes(t *testing.T, r storeRecording, codec string) {
	t.Helper()
	for i, record := range r.recordsOf(t, codec) {
		want := record.AgentReceiptBootNanos - int64(scriptedCaptureToReceipt) - int64(recordingOrigin)
		if record.CanonicalEpisodeNanos != want || record.CanonicalUncertaintyNanos != scriptedMappingHalfWidth || record.MappingSegment != "monotonic-boottime-1" {
			t.Errorf("%s record %d is at %dns +/-%d under %s, want the kernel capture time %dns +/-%d under monotonic-boottime-1",
				codec, i, record.CanonicalEpisodeNanos, record.CanonicalUncertaintyNanos, record.MappingSegment, want, scriptedMappingHalfWidth)
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

func assertDetailSays(t *testing.T, r storeRecording, want ...string) {
	t.Helper()
	for _, sentence := range want {
		if !strings.Contains(r.result.SourceDetail, sentence) {
			t.Errorf("detail %q does not say %q", r.result.SourceDetail, sentence)
		}
	}
}

func TestStoreFramesOnAJPEGCameraWritesTheCamerasOwnFrames(t *testing.T) {
	cam := newKernelClockCamera(t)
	var episode *jpegCameraEpisode
	r := cam.record(t, &data.SourceCapture{Store: data.CaptureStoreFrames}, 2, func() { episode = cam.playJPEGCamera(t) }, 6)

	assertFileNames(t, r, "segment-000001.mjpeg")
	assertRecordsAddress(t, r, indexedAsJPEG, episode.jpegs)
	assertKernelCaptureTimes(t, r, indexedAsJPEG)
	for _, record := range r.index(t) {
		if record.Segment != storeTestSegmentDir+"segment-000001.mjpeg" {
			t.Errorf("record names segment %q", record.Segment)
		}
	}
	if r.result.Count != 6 || r.result.Drops == nil || *r.result.Drops != 0 {
		t.Errorf("count = %d, drops = %v; want the 6 JPEG frames and no loss", r.result.Count, r.result.Drops)
	}
	if strings.Contains(r.result.SourceDetail, "store:") {
		t.Errorf("detail %q carries a store note although frames were stored as asked", r.result.SourceDetail)
	}
}

func TestStoreBothOnAJPEGCameraWritesVideoAndFramesSideBySide(t *testing.T) {
	cam := newKernelClockCamera(t)
	var episode *jpegCameraEpisode
	r := cam.record(t, &data.SourceCapture{Store: data.CaptureStoreBoth}, 3, func() { episode = cam.playJPEGCamera(t) }, 11)

	assertFileNames(t, r, "segment-000001.h264", "segment-000001.mjpeg")
	assertRecordsAddress(t, r, indexedAsJPEG, episode.jpegs)
	assertRecordsAddress(t, r, indexedAsH264, episode.video)
	assertKernelCaptureTimes(t, r, indexedAsJPEG)
	assertKernelCaptureTimes(t, r, indexedAsH264)
	for _, record := range r.index(t) {
		wantSegment := storeTestSegmentDir + "segment-000001.h264"
		if record.Codec == indexedAsJPEG {
			wantSegment = storeTestSegmentDir + "segment-000001.mjpeg"
		}
		if record.Segment != wantSegment {
			t.Errorf("%s record names segment %q, want %q", record.Codec, record.Segment, wantSegment)
		}
	}
	if r.result.Count != 11 {
		t.Errorf("count = %d, want 5 video frames plus 6 JPEG frames", r.result.Count)
	}
	if r.result.Drops == nil || *r.result.Drops != 1 {
		t.Errorf("drops = %v, want 1: the video frame of sample 4 never arrived, and a sequence number seen once in each stream is not a gap", r.result.Drops)
	}
	assertDetailSays(t, r, "5 video frame(s) and 6 of the camera's own frame(s) are indexed; the source's frame and drop counts are sums over both")
}

func TestStoreVideoOnAJPEGCameraStoresOnlyTheVideo(t *testing.T) {
	cam := newKernelClockCamera(t)
	var episode *jpegCameraEpisode
	r := cam.record(t, &data.SourceCapture{Store: data.CaptureStoreVideo}, 2, func() { episode = cam.playJPEGCamera(t) }, 5)

	assertFileNames(t, r, "segment-000001.h264")
	assertRecordsAddress(t, r, indexedAsH264, episode.video)
	if strings.Contains(r.result.SourceDetail, "store:") || strings.Contains(r.result.SourceDetail, "own frame") {
		t.Errorf("store: video detail %q speaks of the camera's own frames", r.result.SourceDetail)
	}
}

// A camera whose own frames are H.264 has one stream. Every store value writes
// the files store: video writes, and the detail says so for the two values
// that asked for frames.
func TestStoreOnAnH264CameraWritesTheVideoFilesForEveryValue(t *testing.T) {
	video := newKernelClockCamera(t).recordFrames(t, &data.SourceCapture{Store: data.CaptureStoreVideo}, 2, h264CameraScript(), 3)
	assertFileNames(t, video, "segment-000001.h264")
	assertRecordsAddress(t, video, indexedAsH264, h264CameraScript())
	assertKernelCaptureTimes(t, video, indexedAsH264)
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
			r := newKernelClockCamera(t).recordFrames(t, &data.SourceCapture{Store: store}, tc.subscribers, h264CameraScript(), 3)
			got, want := r.files(t), video.files(t)
			if len(got) != len(want) {
				t.Fatalf("source directory holds %v, want the files store: video writes", r.fileNames(t))
			}
			for _, name := range []string{"index.jsonl", "segment-000001.h264"} {
				if !bytes.Equal(got[name], want[name]) {
					t.Errorf("%s differs from the one store: video writes\n got: %s\nwant: %s", name, got[name], want[name])
				}
			}
			if r.result.Count != 3 {
				t.Errorf("count = %d, want each frame stored once", r.result.Count)
			}
			assertDetailSays(t, r, tc.detail)
		})
	}
}

func rateCappedH264CameraScript() []*videoFrame {
	return []*videoFrame{
		nativeH264At(1, 1000*time.Millisecond, testRAUFrame),
		nativeH264At(2, 1100*time.Millisecond, testInterFrame),
		nativeH264At(3, 1250*time.Millisecond, testRAUFrame),
		nativeH264At(4, 1300*time.Millisecond, testInterFrame),
		nativeH264At(5, 3000*time.Millisecond, testRAUFrame),
		nativeH264At(6, 3100*time.Millisecond, testInterFrame),
	}
}

// The one stream of an H.264 camera is rate capped as video is, by whole
// groups of pictures, whatever store asks for: the second group is over a
// 2 Hz cap and is left out.
func TestStoreOnAnH264CameraUnderARateCapKeepsWholeGroupsOfPicturesForEveryValue(t *testing.T) {
	const capped = "rate cap 2 Hz applied at GOP granularity (best effort)"
	keptOf := func(script []*videoFrame) []*videoFrame {
		return []*videoFrame{script[0], script[1], script[4], script[5]}
	}
	video := newKernelClockCamera(t).recordFrames(t, &data.SourceCapture{Store: data.CaptureStoreVideo, Rate: 2}, 2, rateCappedH264CameraScript(), 4)
	assertFileNames(t, video, "segment-000001.h264")
	assertRecordsAddress(t, video, indexedAsH264, keptOf(rateCappedH264CameraScript()))
	assertDetailSays(t, video, capped)

	for store, tc := range map[string]struct {
		subscribers int
		detail      string
	}{
		data.CaptureStoreFrames: {2, "store: frames: this camera's own frames are its encoded video (VIDEO_CODEC_H264), stored in the same files and index as store: video"},
		data.CaptureStoreBoth:   {3, "store: both: this camera's own frames are its encoded video (VIDEO_CODEC_H264), so there is one stream and it is stored once"},
	} {
		t.Run(store, func(t *testing.T) {
			r := newKernelClockCamera(t).recordFrames(t, &data.SourceCapture{Store: store, Rate: 2}, tc.subscribers, rateCappedH264CameraScript(), 4)
			got, want := r.files(t), video.files(t)
			if len(got) != len(want) {
				t.Fatalf("source directory holds %v, want the files store: video writes under the same cap", r.fileNames(t))
			}
			for _, name := range []string{"index.jsonl", "segment-000001.h264"} {
				if !bytes.Equal(got[name], want[name]) {
					t.Errorf("%s differs from the one store: video writes under the same cap\n got: %s\nwant: %s", name, got[name], want[name])
				}
			}
			if r.result.Count != 4 || r.result.Drops == nil || *r.result.Drops != 0 || r.result.DropAccounting != "driver_sequence_gaps_include_subscriber_drops" {
				t.Errorf("count = %d, drops = %v under %q; want the 4 frames of the two kept groups and no loss by the driver's sequence", r.result.Count, r.result.Drops, r.result.DropAccounting)
			}
			assertDetailSays(t, r, capped, tc.detail)
			for _, sentence := range []string{"applied per frame to the camera's own frames", "the two streams hold different samples"} {
				if strings.Contains(r.result.SourceDetail, sentence) {
					t.Errorf("detail %q says %q although the camera has one stream", r.result.SourceDetail, sentence)
				}
			}
		})
	}
}

func TestJPEGSegmentsRotateEveryCameraSegmentDuration(t *testing.T) {
	cam := newKernelClockCamera(t)
	var frames []*videoFrame
	for i, at := range []time.Duration{1 * time.Second, 5 * time.Second, 11200 * time.Millisecond, 11300 * time.Millisecond, 22 * time.Second} {
		frames = append(frames, jpegAt(uint64(i+1), at, testJPEG(byte(0x20+i), 30+i)))
	}
	r := cam.recordFrames(t, &data.SourceCapture{Store: data.CaptureStoreFrames}, 2, frames, 5)

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

// A new own format is one row in the extension table: nothing else in the
// adapter knows which formats exist.
func TestOneRowInTheExtensionTableIsAllANewOwnFormatNeeds(t *testing.T) {
	const depthFrames = agentpb.VideoCodec(77)
	cameraSegmentExtensions[depthFrames] = ".z16"
	t.Cleanup(func() { delete(cameraSegmentExtensions, depthFrames) })

	cam := newKernelClockCamera(t)
	var frames []*videoFrame
	for i, at := range []time.Duration{1 * time.Second, 2 * time.Second, 12 * time.Second} {
		frames = append(frames, kernelStamp(&videoFrame{data: bytes.Repeat([]byte{byte(i + 1)}, 16+i), codec: depthFrames}, uint64(i+1), at))
	}
	r := cam.recordFrames(t, &data.SourceCapture{Store: data.CaptureStoreFrames, Rate: 1}, 2, frames, 3)

	assertFileNames(t, r, "segment-000001.z16", "segment-000002.z16")
	assertRecordsAddress(t, r, "77", frames)
	assertKernelCaptureTimes(t, r, "77")
	assertDetailSays(t, r, "rate cap 1 Hz applied per frame to the camera's own frames")
	if strings.Contains(r.result.SourceDetail, "not stored") {
		t.Errorf("detail %q refuses a codec the table has a row for", r.result.SourceDetail)
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

func newStoreTestCapture(t *testing.T, clock *fakeReceipt, capture *data.SourceCapture) *cameraCapture {
	t.Helper()
	c := newTestCameraCapture(t, clock)
	c.source.Capture = capture
	c.monotonicMapping = scriptedMonotonicMapping
	if capture != nil {
		c.rateCap = capture.Rate
	}
	return c
}

func TestRateCapKeepsJPEGFramesOnASchedule(t *testing.T) {
	c := newStoreTestCapture(t, &fakeReceipt{}, &data.SourceCapture{Store: data.CaptureStoreFrames, Rate: 2})

	kept := map[uint64]bool{}
	for i, at := range []time.Duration{1000, 1200, 1460, 1900, 1990, 2100, 2499} {
		before := c.result.Count
		frame := jpegAt(uint64(i+1), at*time.Millisecond, testJPEG(byte(i), 20))
		if err := c.handleFrame(frame); err != nil {
			t.Fatal(err)
		}
		kept[frame.sampleID] = c.result.Count > before
	}
	// Slots fall due at 1.0s, 1.5s, 2.0s and 2.5s, and a frame is kept from an
	// eighth of an interval before its slot.
	for id, want := range map[uint64]bool{1: true, 2: false, 3: true, 4: false, 5: true, 6: false, 7: true} {
		if kept[id] != want {
			t.Errorf("frame %d kept = %v, want %v", id, kept[id], want)
		}
	}
	if c.result.Drops != nil && *c.result.Drops != 0 {
		t.Errorf("drops = %d, want 0: a frame skipped for the rate cap is not a loss", *c.result.Drops)
	}
	c.finishResultDetail(0)
	want := "rate cap 2 Hz applied per frame to the camera's own frames; achieved 2 Hz; 3 frame(s) skipped"
	if !strings.Contains(c.result.SourceDetail, want) {
		t.Errorf("detail %q does not say %q", c.result.SourceDetail, want)
	}
	if strings.Contains(c.result.SourceDetail, "GOP") {
		t.Errorf("detail %q describes a group-of-pictures cap although only JPEG frames were stored", c.result.SourceDetail)
	}
}

// A camera running at the cap, or at a multiple of it, puts every frame on the
// cap's threshold, where timestamp jitter must not decide the achieved rate.
func TestPerFrameRateCapHoldsTheCapUnderTimestampJitter(t *testing.T) {
	const seconds = 20
	for _, tc := range []struct {
		name            string
		cameraHz, capHz float64
		wantHz          float64
	}{
		{"15 fps capped at 15", 15, 15, 15},
		{"30 fps capped at 30", 30, 30, 30},
		{"30 fps capped at 15", 30, 15, 15},
		{"30 fps capped at 10", 30, 10, 10},
		{"25 fps capped at 15", 25, 15, 15},
		{"15 fps capped at 30", 15, 30, 15},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newStoreTestCapture(t, &fakeReceipt{}, &data.SourceCapture{Store: data.CaptureStoreFrames, Rate: tc.capHz})
			jitter := rand.New(rand.NewSource(1))
			period := float64(time.Second) / tc.cameraHz
			frames := int(seconds * tc.cameraHz)
			for i := 0; i < frames; i++ {
				kernelJitter := time.Duration(jitter.Int63n(400_000) - 200_000)
				receiptJitter := time.Duration(jitter.Int63n(4_000_000))
				captured := time.Second + time.Duration(float64(i)*period) + kernelJitter
				frame := jpegAt(uint64(i+1), captured+scriptedCaptureToReceipt, testJPEG(1, 8))
				frame.receiptBootNanos += int64(receiptJitter)
				if err := c.handleFrame(frame); err != nil {
					t.Fatal(err)
				}
			}
			achieved := float64(c.result.Count) / seconds
			if achieved < tc.wantHz*0.97 || achieved > tc.wantHz*1.03 {
				t.Fatalf("kept %d of %d frames in %ds: %.2f Hz, want within 3%% of %.0f Hz", c.result.Count, frames, seconds, achieved, tc.wantHz)
			}
		})
	}
}

func TestBothUnderARateCapSaysTheStreamsHoldDifferentSamples(t *testing.T) {
	c := newStoreTestCapture(t, &fakeReceipt{}, &data.SourceCapture{Store: data.CaptureStoreBoth, Rate: 2})
	c.wantsNativeBeside = true
	units := [][]byte{testRAUFrame, testInterFrame, testRAUFrame, testInterFrame}
	for i, at := range []time.Duration{1000, 1100, 1250, 1300} {
		jpeg := jpegAt(uint64(i+1), at*time.Millisecond, testJPEG(byte(i), 20))
		encoded := *jpeg
		encoded.data, encoded.codec, encoded.native = units[i], agentpb.VideoCodec_VIDEO_CODEC_H264, false
		for _, frame := range []*videoFrame{jpeg, &encoded} {
			if err := c.handleFrame(frame); err != nil {
				t.Fatal(err)
			}
		}
	}
	if c.ownFrameStream.indexed != 1 || c.cameraSegmentStream.indexed != 2 {
		t.Fatalf("stored %d JPEG frame(s) and %d video frame(s), want one spaced frame and one whole group of two", c.ownFrameStream.indexed, c.cameraSegmentStream.indexed)
	}
	c.finishResultDetail(int64(2 * time.Second))
	for _, want := range []string{
		"under the rate cap the two streams hold different samples: video keeps whole groups of pictures, and the camera's own frames are kept one by one at the cap's spacing",
		"rate cap 2 Hz applied at GOP granularity",
		"rate cap 2 Hz applied per frame to the camera's own frames",
	} {
		if !strings.Contains(c.result.SourceDetail, want) {
			t.Errorf("detail %q does not say %q", c.result.SourceDetail, want)
		}
	}
}

// The two index records of one sample are written apart, and a clock mapping
// refresh can fall between them. Both must carry the time the first was given.
func TestBothRecordsOfOneSampleShareOneCaptureTimeAcrossAMappingRefresh(t *testing.T) {
	c := newStoreTestCapture(t, &fakeReceipt{}, &data.SourceCapture{Store: data.CaptureStoreBoth})
	c.wantsNativeBeside = true
	const drift = 200 * time.Microsecond
	offset, sampledAt := int64(scriptedBootMinusMonotonic), int64(0)
	c.monotonicMapping = func() (data.MonotonicMappingSample, error) {
		return data.MonotonicMappingSample{BootAfterNanos: sampledAt, OffsetLowerNanos: offset - scriptedMappingHalfWidth, OffsetUpperNanos: offset + scriptedMappingHalfWidth}, nil
	}
	derivedFrom := func(source *videoFrame, unit []byte) *videoFrame {
		derived := *source
		derived.data, derived.codec, derived.native = unit, agentpb.VideoCodec_VIDEO_CODEC_H264, false
		return &derived
	}
	handle := func(at time.Duration, frame *videoFrame) {
		t.Helper()
		sampledAt = int64(at)
		if err := c.handleFrame(frame); err != nil {
			t.Fatal(err)
		}
	}

	first := jpegAt(1, 1000*time.Millisecond, testJPEG(1, 20))
	second := jpegAt(2, 2100*time.Millisecond, testJPEG(2, 20))
	handle(1000*time.Millisecond, first)
	offset += int64(drift)
	handle(2100*time.Millisecond, second)
	handle(2110*time.Millisecond, derivedFrom(first, testRAUFrame))
	handle(2120*time.Millisecond, derivedFrom(second, testInterFrame))

	records := cameraIndexRecords(t, c)
	if len(records) != 4 {
		t.Fatalf("%d index records, want two per sample", len(records))
	}
	bySample := map[uint64][]cameraIndexRecord{}
	for _, record := range records {
		bySample[record.SampleID] = append(bySample[record.SampleID], record)
	}
	for id, pair := range bySample {
		if len(pair) != 2 || pair[0].Codec == pair[1].Codec {
			t.Fatalf("sample %d has records %+v, want one per stored form", id, pair)
		}
		if pair[0].CanonicalEpisodeNanos != pair[1].CanonicalEpisodeNanos || pair[0].CanonicalUncertaintyNanos != pair[1].CanonicalUncertaintyNanos || pair[0].MappingSegment != pair[1].MappingSegment {
			t.Errorf("sample %d: %s record is at %dns +/-%d under %s, %s record at %dns +/-%d under %s; one frame has one capture time",
				id, pair[0].Codec, pair[0].CanonicalEpisodeNanos, pair[0].CanonicalUncertaintyNanos, pair[0].MappingSegment,
				pair[1].Codec, pair[1].CanonicalEpisodeNanos, pair[1].CanonicalUncertaintyNanos, pair[1].MappingSegment)
		}
	}
	refreshed := bySample[2][0].CanonicalEpisodeNanos - bySample[1][0].CanonicalEpisodeNanos - int64(1100*time.Millisecond)
	if refreshed != int64(drift) {
		t.Fatalf("the second sample's time moved by %dns against the first, want the %s the refreshed mapping drifted; without a refresh the test proves nothing", refreshed, drift)
	}
}

const gstreamerHasNoWholeFrames = "the camera is read through GStreamer, which delivers no whole frames"

func TestFramesRefusedAtJoinRecordsVideoAndSaysWhy(t *testing.T) {
	for store, want := range map[string]string{
		data.CaptureStoreFrames: "store: frames was requested, but this camera's own frames cannot be stored: " + gstreamerHasNoWholeFrames + "; video is recorded instead for the whole episode",
		data.CaptureStoreBoth:   "store: both was requested, but this camera's own frames cannot be stored: " + gstreamerHasNoWholeFrames + "; only video is recorded for the whole episode",
	} {
		t.Run(store, func(t *testing.T) {
			cam := newKernelClockCamera(t)
			cam.hub.nativeNotOffered(fixedReason(gstreamerHasNoWholeFrames))
			chunks := []*videoFrame{
				chunkAt(1, 1000*time.Millisecond, testRAUFrame),
				chunkAt(2, 1033*time.Millisecond, []byte{0, 0, 0, 1, 0x41, 1, 0, 0}),
				chunkAt(3, 1066*time.Millisecond, []byte{0, 1, 0x41, 2}),
			}
			r := cam.recordFrames(t, &data.SourceCapture{Store: store}, 2, chunks, 3)

			assertFileNames(t, r, "segment-000001.h264")
			assertRecordsAddress(t, r, indexedAsH264, chunks)
			assertDetailSays(t, r, want)
			if r.result.Discontinuities != 0 {
				t.Errorf("discontinuities = %d, want 0: nothing was stored before the refusal", r.result.Discontinuities)
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

// waitForCaptureSubscriptions waits until the capture holds exactly the given
// open subscriptions and has let go of any the producer closed.
func (cam *storeTestCamera) waitForCaptureSubscriptions(t *testing.T, encoded, native int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cam.openSubscribers(encodedFrames) == encoded && cam.openSubscribers(nativeFrames) == native && cam.subscribers() == 1+encoded+native {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("capture holds %d encoded and %d native open subscription(s) among %d, waited for %d and %d",
		cam.openSubscribers(encodedFrames), cam.openSubscribers(nativeFrames), cam.subscribers(), encoded, native)
}

// When the producer stops offering whole frames it has fallen back to the old
// pipeline, whose video is unaligned byte chunks with no driver sequence. The
// source records that video, and its detail accounts for the switch.
func TestFramesRefusedLaterRecordsTheOldPipelinesVideoAndAccountsForTheSwitch(t *testing.T) {
	type expectation struct {
		subscribers  int
		capture      data.SourceCapture
		videoFiles   []string
		discontinued uint64
		says         []string
		neverSays    []string
	}
	stoppedAfterTwo := "this camera's own frames stopped being available after 2 stored frame(s): " + gstreamerHasNoWholeFrames
	for name, tc := range map[string]expectation{
		"frames": {
			2, data.SourceCapture{Store: data.CaptureStoreFrames}, []string{"segment-000001.h264"}, 1,
			[]string{
				"store: frames was requested, but " + stoppedAfterTwo + "; video is recorded instead for the rest of the episode, even if the camera offers them again",
				"nothing was stored for 200ms between the camera's last own frame and the first video frame; the recording has a discontinuity there, and the number of frames lost across the gap is not knowable",
			}, nil,
		},
		"frames with a rate cap": {
			2, data.SourceCapture{Store: data.CaptureStoreFrames, Rate: 10}, []string{"segment-000001.h264"}, 1,
			[]string{
				"rate cap 10 Hz applied per frame to the camera's own frames",
				"rate cap 10 Hz cannot be enforced at GOP granularity on this transport's byte-chunked stream",
			},
			[]string{"applied at GOP granularity"},
		},
		"both": {
			3, data.SourceCapture{Store: data.CaptureStoreBoth}, []string{"segment-000001.h264", "segment-000002.h264"}, 0,
			[]string{"store: both was requested, but " + stoppedAfterTwo + "; only video is recorded for the rest of the episode, even if the camera offers them again"},
			[]string{"nothing was stored"},
		},
		"both with a rate cap": {
			3, data.SourceCapture{Store: data.CaptureStoreBoth, Rate: 10}, []string{"segment-000001.h264", "segment-000002.h264"}, 0,
			[]string{
				"rate cap 10 Hz applied per frame to the camera's own frames",
				"rate cap 10 Hz cannot be enforced at GOP granularity on this transport's byte-chunked stream",
			},
			[]string{"applied at GOP granularity"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			cam := newKernelClockCamera(t)
			capture := tc.capture
			storesVideo := capture.StoresVideo()
			var jpegs, derived, chunks []*videoFrame
			r := cam.record(t, &capture, tc.subscribers, func() {
				first := jpegAt(1, 1000*time.Millisecond, testJPEG(1, 40))
				third := jpegAt(3, 1100*time.Millisecond, testJPEG(3, 50))
				jpegs = []*videoFrame{first, third}
				cam.deliver(t, first)
				cam.deliver(t, third)
				derived = append(derived, cam.derive(t, first, testRAUFrame))

				cam.hub.nativeNotOffered(fixedReason(gstreamerHasNoWholeFrames))
				cam.waitForCaptureSubscriptions(t, 1, 0)
				chunks = []*videoFrame{
					chunkAt(4, 1300*time.Millisecond, testRAUFrame),
					chunkAt(5, 1333*time.Millisecond, []byte{0, 0, 0, 1, 0x41, 1, 0, 0}),
					chunkAt(7, 1400*time.Millisecond, []byte{0, 1, 0x41, 2}),
				}
				for _, chunk := range chunks {
					cam.deliver(t, chunk)
				}
				cam.hub.mu.Lock()
				cam.hub.subDrops[cam.captureSubscriptionLocked(encodedFrames)]++
				cam.hub.mu.Unlock()
			}, map[bool]int{false: 5, true: 6}[storesVideo])

			assertFileNames(t, r, append(tc.videoFiles, "segment-000001.mjpeg")...)
			assertRecordsAddress(t, r, indexedAsJPEG, jpegs)
			wantVideo := chunks
			if storesVideo {
				wantVideo = append(derived, chunks...)
			}
			assertRecordsAddress(t, r, indexedAsH264, wantVideo)
			assertDetailSays(t, r, tc.says...)
			for _, sentence := range tc.neverSays {
				if strings.Contains(r.result.SourceDetail, sentence) {
					t.Errorf("detail %q says %q", r.result.SourceDetail, sentence)
				}
			}
			if r.result.Discontinuities != tc.discontinued {
				t.Errorf("discontinuities = %d, want %d", r.result.Discontinuities, tc.discontinued)
			}
			// Sample 2 was lost while frames were numbered, and the hub dropped
			// one read of the unnumbered video after the switch.
			if r.result.Drops == nil || *r.result.Drops != 2 || r.result.DropAccounting != "driver_sequence_gaps_then_subscriber_drops" {
				t.Errorf("drops = %v under %q, want 2 under driver_sequence_gaps_then_subscriber_drops", r.result.Drops, r.result.DropAccounting)
			}
		})
	}
}

func (cam *storeTestCamera) captureSubscriptionLocked(kind frameKind) int {
	for id, sub := range cam.hub.subs {
		if id != 0 && sub.kind == kind && !sub.closed {
			return id
		}
	}
	return -1
}

func bareHub(t *testing.T) *deviceHub {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &deviceHub{subs: make(map[int]*hubSubscriber), subDrops: make(map[int]uint64), ctx: ctx, cancel: cancel}
}

func endProducer(hub *deviceHub) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	for _, sub := range hub.subs {
		if !sub.closed {
			sub.closed = true
			close(sub.ch)
		}
	}
}

func subscriberCount(hub *deviceHub) int {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return len(hub.subs)
}

func waitForSubscriberCount(t *testing.T, hub *deviceHub, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for subscriberCount(hub) != want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if subscriberCount(hub) != want {
		t.Fatalf("hub has %d subscriber(s), waited for %d", subscriberCount(hub), want)
	}
}

func waitForOnlyAnEncodedSubscriber(t *testing.T, hub *deviceHub) {
	t.Helper()
	onlyEncoded := func() bool {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		for _, sub := range hub.subs {
			if sub.closed || sub.kind != encodedFrames {
				return false
			}
		}
		return len(hub.subs) == 1
	}
	deadline := time.Now().Add(3 * time.Second)
	for !onlyEncoded() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !onlyEncoded() {
		t.Fatal("the capture did not end up with one open encoded subscription")
	}
}

// rejoiningCapture is a capture on a hub whose producer the test can end, with
// a replacement hub that offers whole frames for it to rejoin.
type rejoiningCapture struct {
	capture       *cameraCapture
	first, second *deviceHub
	rejoinedKinds []frameKind
}

func newRejoiningCapture(t *testing.T, store string) *rejoiningCapture {
	t.Helper()
	r := &rejoiningCapture{first: bareHub(t), second: bareHub(t)}
	capture := &data.SourceCapture{Store: store}
	c := newStoreTestCapture(t, &fakeReceipt{}, capture)
	c.primaryKind, c.wantsNativeBeside = subscriptionKindsFor(capture)
	subID, frames, err := r.first.subscribeKind(c.primaryKind)
	if err != nil {
		t.Fatal(err)
	}
	c.hub, c.subID, c.frames = r.first, subID, frames
	c.joinNativeBeside()
	c.ctx, c.cancel = context.WithCancel(context.Background())
	t.Cleanup(c.cancel)
	c.done, c.ready = make(chan struct{}), make(chan error, 1)
	c.rejoin = func(_ context.Context, kind frameKind) (*deviceHub, int, chan *videoFrame, error) {
		r.rejoinedKinds = append(r.rejoinedKinds, kind)
		id, ch, joinErr := r.second.subscribeKind(kind)
		return r.second, id, ch, joinErr
	}
	r.capture = c
	go c.run()
	return r
}

// A producer that dies under a capture storing both forms is rejoined for
// both: each stream continues in a new segment of its own.
func TestBothCaptureRejoinsBothSubscriptionsAfterTheProducerDies(t *testing.T) {
	r := newRejoiningCapture(t, data.CaptureStoreBoth)
	c := r.capture

	before := jpegAt(1, time.Second, testJPEG(1, 30))
	r.first.broadcast(before)
	if _, err := r.first.broadcastDerivedFrom(before, &videoFrame{data: testRAUFrame, codec: agentpb.VideoCodec_VIDEO_CODEC_H264, auAligned: true}); err != nil {
		t.Fatal(err)
	}
	waitForCaptureCount(t, c, 2)
	endProducer(r.first)
	waitForSubscriberCount(t, r.second, 2)

	after := jpegAt(1, 2*time.Second, testJPEG(2, 30))
	r.second.broadcast(after)
	if _, err := r.second.broadcastDerivedFrom(after, &videoFrame{data: testRAUFrame, codec: agentpb.VideoCodec_VIDEO_CODEC_H264, auAligned: true}); err != nil {
		t.Fatal(err)
	}
	waitForCaptureCount(t, c, 4)

	results, err := c.Stop(context.Background())
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if len(r.rejoinedKinds) != 1 || r.rejoinedKinds[0] != encodedFrames {
		t.Errorf("rejoined for kinds %v, want one encoded rejoin with the native subscription made beside it", r.rejoinedKinds)
	}
	recording := storeRecording{dir: c.dir, result: results[0]}
	assertFileNames(t, recording, "segment-000001.h264", "segment-000002.h264", "segment-000001.mjpeg", "segment-000002.mjpeg")
	if recording.result.Discontinuities != 1 || recording.result.Count != 4 {
		t.Errorf("discontinuities = %d, count = %d; want 1 and 4", recording.result.Discontinuities, recording.result.Count)
	}
	if recording.result.Drops != nil && *recording.result.Drops != 0 {
		t.Errorf("drops = %d, want 0: sequence numbering starts over with the new producer in both streams", *recording.result.Drops)
	}
}

// An episode never alternates between forms: once the camera's own frames have
// been refused, a later producer that offers them again is recorded as video.
func TestRefusedOwnFramesStayRefusedForTheEpisode(t *testing.T) {
	for _, store := range []string{data.CaptureStoreFrames, data.CaptureStoreBoth} {
		t.Run(store, func(t *testing.T) {
			r := newRejoiningCapture(t, store)
			c := r.capture

			stored := jpegAt(1, time.Second, testJPEG(1, 30))
			r.first.broadcast(stored)
			waitForCaptureCount(t, c, 1)
			r.first.nativeNotOffered(fixedReason(gstreamerHasNoWholeFrames))
			waitForOnlyAnEncodedSubscriber(t, r.first)
			endProducer(r.first)
			waitForSubscriberCount(t, r.second, 1)

			offeredAgain := jpegAt(2, 2*time.Second, testJPEG(2, 30))
			r.second.broadcast(offeredAgain)
			if _, err := r.second.broadcastDerivedFrom(offeredAgain, &videoFrame{data: testRAUFrame, codec: agentpb.VideoCodec_VIDEO_CODEC_H264, auAligned: true}); err != nil {
				t.Fatal(err)
			}
			waitForCaptureCount(t, c, 2)
			if n := subscriberCount(r.second); n != 1 {
				t.Errorf("the capture holds %d subscription(s) on the replacement hub, want only the encoded one", n)
			}

			results, err := c.Stop(context.Background())
			if err != nil {
				t.Fatalf("stop: %v", err)
			}
			for _, kind := range r.rejoinedKinds {
				if kind != encodedFrames {
					t.Errorf("rejoined for kinds %v, want encoded only", r.rejoinedKinds)
				}
			}
			recording := storeRecording{dir: c.dir, result: results[0]}
			if jpegRecords := recording.recordsOf(t, indexedAsJPEG); len(jpegRecords) != 1 {
				t.Errorf("%d JPEG record(s), want only the one stored before the refusal", len(jpegRecords))
			}
			if videoRecords := recording.recordsOf(t, indexedAsH264); len(videoRecords) != 1 {
				t.Errorf("%d video record(s), want the one from the replacement producer", len(videoRecords))
			}
			assertDetailSays(t, recording, "for the rest of the episode, even if the camera offers them again")
		})
	}
}

func ringSampleIDs(r *cameraPreRollRing) []uint64 {
	ids := make([]uint64, 0, len(r.frames))
	for _, bf := range r.frames {
		ids = append(ids, bf.frame.sampleID)
	}
	return ids
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
	_, both := armedStoreSource(t, newKernelClockCamera(t), &data.SourceCapture{Store: data.CaptureStoreBoth}, time.Second)
	if both.ownFrameRing == nil {
		t.Fatal("a source that stores both buffers no frames of the camera's own")
	}
	if both.ring.limitBytes <= 0 || both.ownFrameRing.limitBytes <= 0 || both.ring.limitBytes+both.ownFrameRing.limitBytes != preRollCameraLimitBytes {
		t.Fatalf("rings are bounded at %d and %d bytes, want two shares that add up to %d", both.ring.limitBytes, both.ownFrameRing.limitBytes, preRollCameraLimitBytes)
	}

	for _, store := range []string{data.CaptureStoreVideo, data.CaptureStoreFrames} {
		_, one := armedStoreSource(t, newKernelClockCamera(t), &data.SourceCapture{Store: store}, time.Second)
		if one.ownFrameRing != nil || one.ring.limitBytes != preRollCameraLimitBytes {
			t.Fatalf("store: %s buffers one form, so its ring should have the whole %d bytes", store, preRollCameraLimitBytes)
		}
	}
}

// produceOwnFrame sends one of the camera's own frames through the hub's own
// entry point, which gives it its sample ID and receipt, stamped by the kernel
// just before.
func (cam *storeTestCamera) produceOwnFrame(t *testing.T, codec agentpb.VideoCodec, payload []byte) *videoFrame {
	t.Helper()
	_, now, _, err := data.CaptureReceipt()
	if err != nil {
		t.Fatal(err)
	}
	frame := &videoFrame{data: payload, codec: codec, nativeClock: kernelClock, auAligned: true, native: true}
	frame.nativeNs = now - int64(scriptedBootMinusMonotonic)
	cam.hub.produce(frame)
	cam.waitUntilTaken(t)
	if frame.sampleID == 0 || frame.receiptBootNanos == 0 {
		t.Fatal("the hub did not stamp the frame")
	}
	return frame
}

func (cam *storeTestCamera) produceJPEG(t *testing.T, payload []byte) *videoFrame {
	t.Helper()
	return cam.produceOwnFrame(t, videoCodecMJPEG, payload)
}

func (cam *storeTestCamera) trigger(t *testing.T, source data.Source) (runningDataCapture, string) {
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
	return capture, filepath.Join(session.Directory, "cameras", safeCaptureName(source.ID))
}

func stopRecording(t *testing.T, capture runningDataCapture, dir string, wantIndexed int) storeRecording {
	t.Helper()
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

// assertBeforeTheTrigger checks that every record of a codec sits before the
// trigger at its frame's own receipt.
func assertBeforeTheTrigger(t *testing.T, r storeRecording, codec string) {
	t.Helper()
	for i, record := range r.recordsOf(t, codec) {
		if record.CanonicalEpisodeNanos >= 0 || record.MappingSegment != "receipt-bracket-v1" {
			t.Errorf("%s record %d sits at %dns under %s, want a receipt before the trigger", codec, i, record.CanonicalEpisodeNanos, record.MappingSegment)
		}
	}
}

func TestArmedFramesCampaignStoresThePreRollAsJPEGFrames(t *testing.T) {
	cam := newKernelClockCamera(t)
	source, _ := armedStoreSource(t, cam, &data.SourceCapture{Store: data.CaptureStoreFrames}, 5*time.Second)
	cam.waitForCaptureSubscriptions(t, 0, 1)

	var jpegs []*videoFrame
	for i := 0; i < 4; i++ {
		jpeg := cam.produceJPEG(t, testJPEG(byte(0x30+i), 50+i))
		cam.derive(t, jpeg, testRAUFrame)
		jpegs = append(jpegs, jpeg)
		time.Sleep(2 * time.Millisecond)
	}
	capture, dir := cam.trigger(t, source)
	r := stopRecording(t, capture, dir, 4)

	assertFileNames(t, r, "segment-000001.mjpeg")
	assertRecordsAddress(t, r, indexedAsJPEG, jpegs)
	assertBeforeTheTrigger(t, r, indexedAsJPEG)
	if r.result.ActualOffset == nil || *r.result.ActualOffset >= 0 {
		t.Errorf("actual offset = %v, want the first JPEG frame's time before the trigger", r.result.ActualOffset)
	}
	assertDetailSays(t, r, "flushed 4 frame(s)")
}

// The video of the last buffered frame is still in the encoder at the trigger
// and arrives live. It is indexed at the time its JPEG frame was given.
func TestArmedBothCampaignStoresThePreRollInBothForms(t *testing.T) {
	cam := newKernelClockCamera(t)
	source, _ := armedStoreSource(t, cam, &data.SourceCapture{Store: data.CaptureStoreBoth}, 5*time.Second)
	cam.waitForCaptureSubscriptions(t, 1, 1)

	first := cam.produceJPEG(t, testJPEG(0x40, 60))
	second := cam.produceJPEG(t, testJPEG(0x41, 61))
	video := []*videoFrame{cam.derive(t, first, testRAUFrame)}
	third := cam.produceJPEG(t, testJPEG(0x42, 62))
	video = append(video, cam.derive(t, second, testInterFrame))
	time.Sleep(2 * time.Millisecond)

	capture, dir := cam.trigger(t, source)
	video = append(video, cam.derive(t, third, testInterFrame))
	r := stopRecording(t, capture, dir, 6)

	assertFileNames(t, r, "segment-000001.h264", "segment-000001.mjpeg")
	assertRecordsAddress(t, r, indexedAsJPEG, []*videoFrame{first, second, third})
	assertRecordsAddress(t, r, indexedAsH264, video)
	assertBeforeTheTrigger(t, r, indexedAsJPEG)
	assertBeforeTheTrigger(t, r, indexedAsH264)
	jpegRecords, videoRecords := r.recordsOf(t, indexedAsJPEG), r.recordsOf(t, indexedAsH264)
	for i := range jpegRecords {
		if jpegRecords[i].CanonicalEpisodeNanos != videoRecords[i].CanonicalEpisodeNanos || jpegRecords[i].CanonicalUncertaintyNanos != videoRecords[i].CanonicalUncertaintyNanos {
			t.Errorf("sample %d: JPEG record at %dns, video record at %dns; one frame has one capture time", jpegRecords[i].SampleID, jpegRecords[i].CanonicalEpisodeNanos, videoRecords[i].CanonicalEpisodeNanos)
		}
	}
	assertDetailSays(t, r, "flushed 5 frame(s)")
}

func TestArmedFramesCampaignBuffersVideoWhenTheCameraHasNoFramesOfItsOwn(t *testing.T) {
	cam := newKernelClockCamera(t)
	cam.hub.nativeNotOffered(fixedReason(gstreamerHasNoWholeFrames))
	source, armed := armedStoreSource(t, cam, &data.SourceCapture{Store: data.CaptureStoreFrames}, 5*time.Second)
	if armed.primaryKind != encodedFrames {
		t.Fatal("the armed source did not fall back to buffering video")
	}

	chunks := []*videoFrame{testChunkedH264Frame(testRAUFrame), testChunkedH264Frame([]byte{0, 0, 0, 1, 0x41, 1, 0, 0})}
	for _, chunk := range chunks {
		cam.hub.produce(chunk)
		cam.waitUntilTaken(t)
	}
	time.Sleep(2 * time.Millisecond)
	capture, dir := cam.trigger(t, source)
	r := stopRecording(t, capture, dir, 2)

	assertFileNames(t, r, "segment-000001.h264")
	assertRecordsAddress(t, r, indexedAsH264, chunks)
	assertBeforeTheTrigger(t, r, indexedAsH264)
	assertDetailSays(t, r, "store: frames was requested, but this camera's own frames cannot be stored: "+gstreamerHasNoWholeFrames+"; video is recorded instead for the whole episode")
}

// An armed H.264 camera buffers its one stream once, whatever store asks for,
// and the episode opens with it as video.
func TestArmedStoreOnAnH264CameraStoresThePreRollOnceAsVideo(t *testing.T) {
	for store, tc := range map[string]struct {
		armedEncoded, armedNative int
		detail                    string
	}{
		data.CaptureStoreFrames: {0, 1, "store: frames: this camera's own frames are its encoded video (VIDEO_CODEC_H264), stored in the same files and index as store: video"},
		data.CaptureStoreBoth:   {1, 0, "store: both: this camera's own frames are its encoded video (VIDEO_CODEC_H264), so there is one stream and it is stored once"},
	} {
		t.Run(store, func(t *testing.T) {
			cam := newKernelClockCamera(t)
			source, _ := armedStoreSource(t, cam, &data.SourceCapture{Store: store}, 5*time.Second)

			var frames []*videoFrame
			for _, unit := range [][]byte{testRAUFrame, testInterFrame, testRAUFrame, testInterFrame} {
				frames = append(frames, cam.produceOwnFrame(t, agentpb.VideoCodec_VIDEO_CODEC_H264, unit))
				time.Sleep(2 * time.Millisecond)
			}
			cam.waitForCaptureSubscriptions(t, tc.armedEncoded, tc.armedNative)
			capture, dir := cam.trigger(t, source)
			r := stopRecording(t, capture, dir, 4)

			assertFileNames(t, r, "segment-000001.h264")
			assertRecordsAddress(t, r, indexedAsH264, frames)
			assertBeforeTheTrigger(t, r, indexedAsH264)
			if r.result.Count != 4 {
				t.Errorf("count = %d, want each buffered frame stored once", r.result.Count)
			}
			assertDetailSays(t, r, "flushed 4 frame(s)", tc.detail)
			if strings.Contains(r.result.SourceDetail, "own frame(s) are indexed") {
				t.Errorf("detail %q counts two streams although the camera has one", r.result.SourceDetail)
			}
		})
	}
}
