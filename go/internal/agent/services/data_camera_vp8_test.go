package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// vp8TestFrames cuts a WebM stream into pipe reads of the given size and
// passes them through a hub the way the producer does, so each frame carries
// the sample identity, initialization and join point a subscriber is given.
func vp8TestFrames(t *testing.T, stream []byte, size int) []*videoFrame {
	t.Helper()
	h, cancel := newTestHub(t)
	defer cancel()
	h.sampleSeq = new(atomic.Uint64)
	subscribeDeep(t, h)
	var frames []*videoFrame
	for off := 0; off < len(stream); off += size {
		frame := &videoFrame{codec: agentpb.VideoCodec_VIDEO_CODEC_VP8, data: stream[off:min(off+size, len(stream))], nativeClock: "CLOCK_REALTIME_AGENT_CAPTURE"}
		h.produce(frame)
		frames = append(frames, frame)
	}
	return frames
}

// recordVP8 feeds frames to a capture the way its run loop does, skipping the
// ones it is still waiting on.
func recordVP8(t *testing.T, c *cameraCapture, frames []*videoFrame) {
	t.Helper()
	for _, frame := range frames {
		if err := c.handleFrame(frame); err != nil && !errors.Is(err, errAwaitCameraRandomAccess) {
			t.Fatal(err)
		}
	}
}

// vp8Segments returns the WebM segment files a capture wrote, in order.
func vp8Segments(t *testing.T, c *cameraCapture) [][]byte {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(c.dir, "segment-*.webm"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	var segments [][]byte
	for _, name := range names {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		segments = append(segments, b)
	}
	return segments
}

func cameraIndexRecords(t *testing.T, c *cameraCapture) []cameraIndexRecord {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(c.dir, "index.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var records []cameraIndexRecord
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
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

// A VP8 hub frame is a pipe read of one WebM container, not a VP8 frame, so
// its first byte says nothing about key frames. Only the hub's join point does.
func TestVP8RandomAccessIsAJoinPointNotAFrameTag(t *testing.T) {
	stream, _, clusters := webmTestStream()
	frames := vp8TestFrames(t, stream, 7)
	for k, frame := range frames {
		offset, randomAccess := frameRandomAccess(frame)
		if randomAccess != (frame.webmJoin != nil) {
			t.Fatalf("read %d (first byte %#x): random access %v, join point %v", k, frame.data[0], randomAccess, frame.webmJoin != nil)
		}
		if randomAccess && offset != frame.webmJoin.offset {
			t.Fatalf("read %d: offset %d, want the join point's %d", k, offset, frame.webmJoin.offset)
		}
	}
	// The unit is where the hub can prove it: the read holding the first
	// block's frame tag, from the Cluster's start if that is in the same read.
	k := clusters[2].tag / 7
	want := max(clusters[2].start-k*7, 0)
	if offset, randomAccess := frameRandomAccess(frames[k]); !randomAccess || offset != want {
		t.Fatalf("read %d: random access %v at %d, want the key frame Cluster at %d", k, randomAccess, offset, want)
	}
}

// A capture that joins a stream already running is handed reads from its
// middle. The segment it writes must still be a WebM file from its first byte:
// the initialization, then a Cluster opening on a key frame.
func TestVP8SegmentJoiningARunningStreamOpensWithTheInitialization(t *testing.T) {
	stream, init, clusters := webmTestStream()
	pristine := append([]byte(nil), stream...)
	for _, size := range []int{7, 64, len(stream)} {
		frames := vp8TestFrames(t, stream, size)
		for from := range frames {
			c := newTestCameraCapture(t, &fakeReceipt{})
			recordVP8(t, c, frames[from:])
			segments := vp8Segments(t, c)
			want := lateWebMViewer(stream, init, clusters, from*size)
			if want == nil {
				if len(segments) != 0 {
					t.Fatalf("read size %d, joined at read %d: wrote %x with no key frame Cluster to start at", size, from, segments)
				}
				continue
			}
			if len(segments) != 1 || !bytes.Equal(segments[0], want) {
				t.Fatalf("read size %d, joined at read %d:\n got %x\nwant %x", size, from, segments, want)
			}
			// Every index entry addresses bytes of its own sample: the join
			// prefix at the head of the file belongs to no sample, and the
			// entries after it tile the rest of the file.
			records := cameraIndexRecords(t, c)
			next := int64(len(want))
			for i := len(records) - 1; i >= 0; i-- {
				r := records[i]
				if r.ByteOffset+int64(r.ByteSize) != next || !strings.HasSuffix(r.Segment, "/segment-000001.webm") {
					t.Fatalf("read size %d, joined at read %d: entry %d covers [%d,+%d) of %s, want it to end at %d", size, from, i, r.ByteOffset, r.ByteSize, r.Segment, next)
				}
				sample := frames[r.SampleID-1].data
				if !bytes.HasSuffix(sample, want[r.ByteOffset:next]) {
					t.Fatalf("read size %d, joined at read %d: entry %d holds %x, not bytes of sample %d (%x)", size, from, i, want[r.ByteOffset:next], r.SampleID, sample)
				}
				next = r.ByteOffset
			}
			if (from == 0 && next != 0) || (from > 0 && next < int64(len(init))) {
				t.Fatalf("read size %d, joined at read %d: index starts at byte %d; the %d-byte initialization belongs to no sample", size, from, next, len(init))
			}
		}
	}
	if !bytes.Equal(stream, pristine) {
		t.Fatal("recording wrote into the shared frame payloads")
	}
}

// When the hub drops reads for the capture, the bytes that follow cannot be
// appended: the file would carry a hole in the middle of a Cluster. The
// recording continues in a new segment at the next join point instead, and
// the reads it had to discard on the way there count as lost.
func TestVP8RecordingStartsANewSegmentAfterLostSamples(t *testing.T) {
	stream, init, clusters := webmTestStream()
	for _, size := range []int{7, 64} {
		frames := vp8TestFrames(t, stream, size)
		for hole := 1; hole < len(frames); hole++ {
			for _, width := range []int{1, 3} {
				if hole+width > len(frames) || (width > 1 && hole%4 != 0) {
					continue
				}
				c := newTestCameraCapture(t, &fakeReceipt{})
				recordVP8(t, c, frames[:hole])
				recordVP8(t, c, frames[hole+width:])
				want := [][]byte{stream[:hole*size]}
				resumed := lateWebMViewer(stream, init, clusters, (hole+width)*size)
				discarded := len(frames) - hole - width
				if resumed != nil {
					want = append(want, resumed)
					discarded = 0
					for k := hole + width; frames[k].webmJoin == nil; k++ {
						discarded++
					}
				}
				segments := vp8Segments(t, c)
				if len(segments) != len(want) {
					t.Fatalf("read size %d, reads %d+%d lost: %d segments, want %d", size, hole, width, len(segments), len(want))
				}
				for i := range want {
					if !bytes.Equal(segments[i], want[i]) {
						t.Fatalf("read size %d, reads %d+%d lost, segment %d:\n got %x\nwant %x", size, hole, width, i+1, segments[i], want[i])
					}
				}
				if c.carriedDrops != uint64(discarded) {
					t.Fatalf("read size %d, reads %d+%d lost: %d discarded reads counted as lost, want %d", size, hole, width, c.carriedDrops, discarded)
				}
			}
		}
	}
}

// The drop reaches the manifest: the hub's own count, the reads discarded
// while waiting for the next key frame, and a note saying why there are two
// files.
func TestVP8RecordingReportsReadsDiscardedAfterAHubDrop(t *testing.T) {
	stream, init, clusters := webmTestStream()
	const size = 7
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := &deviceHub{subs: make(map[int]*hubSubscriber), subDrops: make(map[int]uint64), ctx: ctx, cancel: cancel, sampleSeq: new(atomic.Uint64)}
	subID, frames, err := hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	c := newTestCameraCapture(t, &fakeReceipt{})
	c.hub, c.subID, c.frames = hub, subID, frames
	c.ctx, c.cancel = ctx, cancel
	c.done, c.ready = make(chan struct{}), make(chan error, 1)
	go c.run()

	// The read holding the first Cluster's delta frame is dropped for the
	// capture while it is busy: its channel is full, so the hub counts the
	// loss and moves on.
	lost := clusters[0].tag/size + 1
	written, discarded := 0, 0
	for k, off := 0, 0; off < len(stream); k, off = k+1, off+size {
		frame := &videoFrame{codec: agentpb.VideoCodec_VIDEO_CODEC_VP8, data: stream[off:min(off+size, len(stream))], nativeClock: "CLOCK_REALTIME_AGENT_CAPTURE"}
		if k == lost {
			hub.mu.Lock()
			full := hub.subs[subID].ch
			hub.subs[subID].ch = make(chan *videoFrame)
			hub.mu.Unlock()
			hub.produce(frame)
			hub.mu.Lock()
			hub.subs[subID].ch = full
			hub.mu.Unlock()
			continue
		}
		hub.produce(frame)
		if k < lost || off+size > clusters[2].tag {
			written++
			waitForCaptureCount(t, c, written)
			continue
		}
		// A discarded read leaves no index entry to wait for; an empty channel
		// is enough to keep the next one from overflowing it.
		discarded++
		for deadline := time.Now().Add(3 * time.Second); len(frames) > 0 && time.Now().Before(deadline); {
			time.Sleep(100 * time.Microsecond)
		}
	}
	results, err := c.Stop(context.Background())
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	segments := vp8Segments(t, c)
	want := [][]byte{stream[:lost*size], append(append([]byte(nil), init...), stream[clusters[2].start:]...)}
	if len(segments) != 2 || !bytes.Equal(segments[0], want[0]) || !bytes.Equal(segments[1], want[1]) {
		t.Fatalf("segments:\n got %x\nwant %x", segments, want)
	}
	if results[0].Drops == nil || *results[0].Drops != uint64(1+discarded) {
		t.Fatalf("drops = %v, want the 1 read the hub dropped plus the %d discarded before the next key frame", results[0].Drops, discarded)
	}
	if !strings.Contains(results[0].SourceDetail, "new segment") {
		t.Fatalf("source detail %q does not explain the second segment", results[0].SourceDetail)
	}
}

// An armed ring outlives the read that carried the stream's header, and its
// own subscription can lose reads. The flushed clip must open with the
// initialization all the same, and never run across a loss.
func TestVP8PreRollFlushOpensWithTheInitialization(t *testing.T) {
	stream, init, clusters := webmTestStream()
	const size = 7
	frames := vp8TestFrames(t, stream, size)
	for k, frame := range frames {
		frame.receiptBootNanos = int64(k) * 100 * ms
	}
	// A hole inside the last Cluster the ring keeps whole: the clip resumes at
	// the Cluster after it.
	lost := clusters[2].tag/size + 1
	if lost*size >= clusters[3].start {
		t.Fatalf("read %d is not inside the third Cluster", lost)
	}
	r := &cameraPreRollRing{buffer: time.Duration(len(frames)-clusters[1].start/size) * 100 * time.Millisecond, limitBytes: preRollCameraLimitBytes}
	for k, frame := range frames {
		if k != lost {
			r.add(frame)
		}
	}
	if r.frames[0].frame == frames[0] {
		t.Fatal("the ring still holds the read that carried the stream header; the test proves nothing")
	}
	trigger := frames[len(frames)-1].receiptBootNanos
	c := newTestCameraCapture(t, &fakeReceipt{now: trigger})
	c.session = data.CaptureSession{RequestBootNanos: trigger}
	c.preRoll, _, _ = r.flush(trigger)
	if err := c.flushPreRoll(); err != nil {
		t.Fatal(err)
	}
	if c.segment != nil {
		if err := c.segment.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	first := r.frames[0].frame
	if first.webmJoin == nil {
		t.Fatalf("the ring opens on read %d, which no viewer or file can start at", first.sampleID-1)
	}
	opening := first.webmJoin.join(first.data)
	for k := int(first.sampleID); k < lost; k++ {
		opening = append(opening, frames[k].data...)
	}
	want := [][]byte{opening, append(append([]byte(nil), init...), stream[clusters[3].start:]...)}
	segments := vp8Segments(t, c)
	if len(segments) != 2 || !bytes.Equal(segments[0], want[0]) || !bytes.Equal(segments[1], want[1]) {
		t.Fatalf("flushed segments:\n got %x\nwant %x", segments, want)
	}
	if !bytes.HasPrefix(segments[0], init) {
		t.Fatalf("flushed clip does not open with the initialization: %x", segments[0])
	}
}
