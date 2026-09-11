package services

import (
	"math"
	"sync"
	"testing"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// TestBindingTableResolvesSequenceToSample is the base case: what the pump
// recorded is what a consumer resolves, field for field. The identity fields
// must survive the round trip exactly, because a consumer joins episode capture
// and the model-input ledger on them.
func TestBindingTableResolvesSequenceToSample(t *testing.T) {
	table := newLoopbackBindingTable(8)
	want := loopbackBinding{
		LoopbackSequence: 41,
		SampleID:         1007,
		BootNanos:        1234567890,
		UncertaintyNanos: 1500,
		HubDropsBefore:   0,
	}
	table.Record(want)

	got, ok := table.Lookup(41)
	if !ok {
		t.Fatalf("Lookup(41) not found after Record")
	}
	if got != want {
		t.Errorf("Lookup(41) = %+v, want %+v", got, want)
	}
}

// TestBindingTableUnknownSequenceFailsClosed pins the deliberate refusal to
// guess. A consumer asking about a frame we never wrote, or one that has aged
// out, must be told so rather than handed a neighbouring sample: a wrong join
// is worse than an absent one, because it silently misattributes which frame a
// model scored.
func TestBindingTableUnknownSequenceFailsClosed(t *testing.T) {
	table := newLoopbackBindingTable(4)
	table.Record(loopbackBinding{LoopbackSequence: 10, SampleID: 100})
	table.Record(loopbackBinding{LoopbackSequence: 11, SampleID: 101})

	for _, seq := range []uint32{0, 9, 12, 4096} {
		if got, ok := table.Lookup(seq); ok {
			t.Errorf("Lookup(%d) = %+v, want not found", seq, got)
		}
	}
}

// TestBindingTableEvictsOldestAndDoesNotResurrect covers the bound on retention.
// The ring must drop the oldest binding AND drop its index entry, or a lookup
// would resolve a stale slot and name the wrong sample.
func TestBindingTableEvictsOldestAndDoesNotResurrect(t *testing.T) {
	const retain = 4
	table := newLoopbackBindingTable(retain)
	for i := 0; i < retain*2; i++ {
		table.Record(loopbackBinding{LoopbackSequence: uint32(i), SampleID: uint64(1000 + i)})
	}

	if got := table.Len(); got != retain {
		t.Errorf("Len() = %d, want %d", got, retain)
	}
	// The first `retain` sequences must be gone, not merely shadowed.
	for i := 0; i < retain; i++ {
		if got, ok := table.Lookup(uint32(i)); ok {
			t.Errorf("Lookup(%d) = %+v, want evicted", i, got)
		}
	}
	// The most recent `retain` must all still resolve to their own sample.
	for i := retain; i < retain*2; i++ {
		got, ok := table.Lookup(uint32(i))
		if !ok {
			t.Fatalf("Lookup(%d) not found, want retained", i)
		}
		if got.SampleID != uint64(1000+i) {
			t.Errorf("Lookup(%d).SampleID = %d, want %d", i, got.SampleID, 1000+i)
		}
	}
}

// TestBindingSurvivesHubDroppedFrames is drop case 1 from the file comment, and
// the reason HubDropsBefore exists.
//
// When the hub drops frames for the pump, those samples never reach the node.
// The kernel counts only what we wrote, so the loopback sequence stays DENSE
// while sample_id jumps. A consumer watching only the sequence would conclude
// nothing was lost. This test pins both halves: the sequence really is dense,
// and the binding still names the correct (non-contiguous) sample ids and
// reports the loss.
func TestBindingSurvivesHubDroppedFrames(t *testing.T) {
	table := newLoopbackBindingTable(16)

	// The hub delivered samples 500, 501, then dropped 502-504, then 505.
	// The pump wrote three frames, so the kernel assigned three dense sequences.
	table.Record(loopbackBinding{LoopbackSequence: 70, SampleID: 500, HubDropsBefore: 0})
	table.Record(loopbackBinding{LoopbackSequence: 71, SampleID: 501, HubDropsBefore: 0})
	table.Record(loopbackBinding{LoopbackSequence: 72, SampleID: 505, HubDropsBefore: 3})

	// Sequence is dense across the drop: this is exactly why the data plane
	// alone cannot report a hub-side loss.
	for i, wantSample := range map[uint32]uint64{70: 500, 71: 501, 72: 505} {
		got, ok := table.Lookup(i)
		if !ok {
			t.Fatalf("Lookup(%d) not found", i)
		}
		if got.SampleID != wantSample {
			t.Errorf("Lookup(%d).SampleID = %d, want %d", i, got.SampleID, wantSample)
		}
	}

	// The loss is recoverable only from the control plane's record.
	got, _ := table.Lookup(72)
	if got.HubDropsBefore != 3 {
		t.Errorf("Lookup(72).HubDropsBefore = %d, want 3", got.HubDropsBefore)
	}
	// And the sample_id jump is the corroborating signal.
	prev, _ := table.Lookup(71)
	if delta := got.SampleID - prev.SampleID - 1; delta != got.HubDropsBefore {
		t.Errorf("sample_id gap %d disagrees with HubDropsBefore %d", delta, got.HubDropsBefore)
	}
}

// TestSequenceGapDetectsReaderDroppedFrames is drop case 2: the application
// reading the node fell behind and v4l2loopback fast-forwarded it. Here the gap
// IS on the data plane, and it is the reader's only honest loss signal.
func TestSequenceGapDetectsReaderDroppedFrames(t *testing.T) {
	tests := []struct {
		name       string
		prev, curr uint32
		want       uint64
	}{
		{name: "consecutive delivery loses nothing", prev: 10, curr: 11, want: 0},
		{name: "one frame skipped", prev: 10, curr: 12, want: 1},
		{name: "reader fast-forwarded past many", prev: 10, curr: 400, want: 389},
		// v4l2loopback re-serves the newest frame to a reader that has caught up,
		// so the same sequence can be dequeued twice. That is a repeat, not a loss.
		{name: "repeated buffer is not a loss", prev: 10, curr: 10, want: 0},
		{name: "out-of-order observation is not a loss", prev: 10, curr: 9, want: 0},
		// The counter is __u32 and is not reset per open, so a long-lived node
		// wraps. The gap must stay correct across the wrap rather than becoming
		// a ~4 billion frame phantom loss.
		{name: "wrap is not a phantom loss", prev: math.MaxUint32, curr: 0, want: 0},
		{name: "gap spanning the wrap", prev: math.MaxUint32 - 2, curr: 1, want: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sequenceGap(tt.prev, tt.curr); got != tt.want {
				t.Errorf("sequenceGap(%d, %d) = %d, want %d", tt.prev, tt.curr, got, tt.want)
			}
		})
	}
}

// TestBindingTableResolvesAcrossSequenceWrap pins that a table spanning the
// 32-bit wrap still resolves both sides, and that Newest picks the genuinely
// newest rather than the numerically largest.
func TestBindingTableResolvesAcrossSequenceWrap(t *testing.T) {
	table := newLoopbackBindingTable(8)
	seqs := []uint32{math.MaxUint32 - 1, math.MaxUint32, 0, 1}
	for i, seq := range seqs {
		table.Record(loopbackBinding{LoopbackSequence: seq, SampleID: uint64(900 + i)})
	}

	for i, seq := range seqs {
		got, ok := table.Lookup(seq)
		if !ok {
			t.Fatalf("Lookup(%d) not found across wrap", seq)
		}
		if got.SampleID != uint64(900+i) {
			t.Errorf("Lookup(%d).SampleID = %d, want %d", seq, got.SampleID, 900+i)
		}
	}

	newest, ok := table.Newest()
	if !ok {
		t.Fatal("Newest() not found")
	}
	if newest.LoopbackSequence != 1 || newest.SampleID != 903 {
		t.Errorf("Newest() = seq %d sample %d, want seq 1 sample 903 (wrap-aware, not numeric max)",
			newest.LoopbackSequence, newest.SampleID)
	}
}

// TestSequenceAfterHandlesWrap pins the ordering primitive the rest depends on.
func TestSequenceAfterHandlesWrap(t *testing.T) {
	tests := []struct {
		name string
		a, b uint32
		want bool
	}{
		{name: "plainly after", a: 5, b: 4, want: true},
		{name: "plainly before", a: 4, b: 5, want: false},
		{name: "equal is not after", a: 5, b: 5, want: false},
		{name: "just past the wrap is after", a: 0, b: math.MaxUint32, want: true},
		{name: "just before the wrap is not after", a: math.MaxUint32, b: 0, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sequenceAfter(tt.a, tt.b); got != tt.want {
				t.Errorf("sequenceAfter(%d, %d) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// TestNewestOnEmptyTable pins that an untouched table reports nothing rather
// than a zero-valued binding that would resolve sequence 0 to sample 0.
func TestNewestOnEmptyTable(t *testing.T) {
	table := newLoopbackBindingTable(4)
	if got, ok := table.Newest(); ok {
		t.Errorf("Newest() on empty table = %+v, want not found", got)
	}
	if got, ok := table.Lookup(0); ok {
		t.Errorf("Lookup(0) on empty table = %+v, want not found", got)
	}
}

// TestBindingTableSequenceZeroIsRecordable guards the eviction bookkeeping: a
// genuine sequence of 0 must be distinguishable from an untouched ring slot,
// whose zero value also has LoopbackSequence 0.
func TestBindingTableSequenceZeroIsRecordable(t *testing.T) {
	table := newLoopbackBindingTable(4)
	table.Record(loopbackBinding{LoopbackSequence: 0, SampleID: 77})
	table.Record(loopbackBinding{LoopbackSequence: 1, SampleID: 78})

	got, ok := table.Lookup(0)
	if !ok {
		t.Fatal("Lookup(0) not found; a real sequence 0 was mistaken for an empty slot")
	}
	if got.SampleID != 77 {
		t.Errorf("Lookup(0).SampleID = %d, want 77", got.SampleID)
	}
}

// TestBindingTableConcurrentRecordAndLookup exercises the intended access
// pattern (pump records, control plane looks up) under -race.
func TestBindingTableConcurrentRecordAndLookup(t *testing.T) {
	table := newLoopbackBindingTable(64)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			table.Record(loopbackBinding{LoopbackSequence: uint32(i), SampleID: uint64(i)})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			if b, ok := table.Lookup(uint32(i)); ok && b.SampleID != uint64(i) {
				t.Errorf("Lookup(%d) resolved to sample %d", i, b.SampleID)
			}
			table.Newest()
		}
	}()
	wg.Wait()
}

// TestFrameBindableToLoopback pins the honest scope of the feature: which hub
// frames can be written frame-for-frame, and which are refused because no sound
// binding exists for them.
func TestFrameBindableToLoopback(t *testing.T) {
	tests := []struct {
		name  string
		frame *videoFrame
		want  bool
	}{
		{
			name:  "whole h264 access unit is bindable",
			frame: &videoFrame{data: []byte{0, 0, 1}, auAligned: true, codec: agentpb.VideoCodec_VIDEO_CODEC_H264},
			want:  true,
		},
		{
			// The GStreamer and IP camera producers deliver arbitrary chunks of a
			// byte stream. A sequence counted over those names chunks, not frames.
			name:  "unaligned byte stream chunk is refused",
			frame: &videoFrame{data: []byte{0, 0, 1}, auAligned: false, codec: agentpb.VideoCodec_VIDEO_CODEC_H264},
			want:  false,
		},
		{
			// VP8 arrives inside a WebM container, so it is not a per-frame payload.
			name:  "vp8 in webm is refused",
			frame: &videoFrame{data: []byte{1}, auAligned: true, codec: agentpb.VideoCodec_VIDEO_CODEC_VP8},
			want:  false,
		},
		{name: "empty frame is refused", frame: &videoFrame{data: nil, auAligned: true}, want: false},
		{name: "nil frame is refused", frame: nil, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := frameBindableToLoopback(tt.frame)
			if got != tt.want {
				t.Errorf("frameBindableToLoopback() = %v (%q), want %v", got, reason, tt.want)
			}
			if !got && reason == "" {
				t.Error("refusal must carry a reason a caller can log")
			}
		})
	}
}
