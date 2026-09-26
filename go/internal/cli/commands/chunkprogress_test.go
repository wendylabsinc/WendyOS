package commands

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// TestChunkPushProgressAggregatesConcurrentSends drives newChunkPushProgress
// from 4 real goroutines simultaneously (mirroring pushLayersByChunks'
// maxConcurrentLayerPush=4 fan-out) and checks the aggregated totals are
// exact. Each goroutine plans one layer and then reports every one of that
// layer's missing chunks as sent, in uniform 1000-byte chunks so the expected
// totals are simple sums to verify by hand:
//
//	layer i (i=0..3): totalChunks=(i+1)*100, missing=(i+1)*10, missingBytes=(i+1)*10000
//	=> each of the (i+1)*10 missing chunks is exactly 1000 bytes.
//
// Run with -race (see the task instructions) to confirm the mutex actually
// prevents a data race, not just that the arithmetic comes out right.
func TestChunkPushProgressAggregatesConcurrentSends(t *testing.T) {
	p := newChunkPushProgress()
	p.SetLayerCounts(5, 1) // 5 layers total, 1 already fully on-device; 4 pushed below

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			totalChunks := (i + 1) * 100
			missing := (i + 1) * 10
			missingBytes := int64((i + 1) * 10000)
			p.LayerPlanned(totalChunks, missing, missingBytes)
			const chunkBytes = 1000 // missingBytes / missing is 1000 for every i
			for c := 0; c < missing; c++ {
				p.ChunkSent(chunkBytes)
			}
		}()
	}
	wg.Wait()

	snap := p.Snapshot()
	if snap.LayersTotal != 5 {
		t.Errorf("LayersTotal = %d, want 5", snap.LayersTotal)
	}
	if snap.LayersReused != 1 {
		t.Errorf("LayersReused = %d, want 1", snap.LayersReused)
	}
	if snap.LayersPlanned != 4 {
		t.Errorf("LayersPlanned = %d, want 4", snap.LayersPlanned)
	}
	if snap.TotalChunks != 1000 { // 100+200+300+400
		t.Errorf("TotalChunks = %d, want 1000", snap.TotalChunks)
	}
	if snap.MissingChunks != 100 { // 10+20+30+40
		t.Errorf("MissingChunks = %d, want 100", snap.MissingChunks)
	}
	if snap.PlannedBytes != 100000 { // 10000+20000+30000+40000
		t.Errorf("PlannedBytes = %d, want 100000", snap.PlannedBytes)
	}
	if snap.SentChunks != 100 { // one ChunkSent call per missing chunk
		t.Errorf("SentChunks = %d, want 100", snap.SentChunks)
	}
	if snap.SentBytes != 100000 { // 100 chunks * 1000 bytes
		t.Errorf("SentBytes = %d, want 100000", snap.SentBytes)
	}
}

// TestChunkPushSnapshotLineAndSummary is a string table over Line() and
// Summary() covering: the brief's worked example form (bytes/pct/rate plus
// the "device already has" dedup clause), the zero-chunks-planned case (every
// layer was a full-layer reuse skip, so LayerPlanned was never called), the
// all-reused case (SetLayerCounts alone, no pushed layers), and the
// WDY-2431 resume-legibility case: a layer WAS planned (chunks counted) but
// every one of its chunks turned out already present (e.g. a prior push was
// interrupted after staging them), so sentChunks stays 0 despite
// totalChunks > 0.
func TestChunkPushSnapshotLineAndSummary(t *testing.T) {
	clockAt := func(start time.Time, elapsed time.Duration) func() time.Time {
		return func() time.Time { return start.Add(elapsed) }
	}
	start := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		build       func() *chunkPushProgress
		elapsed     time.Duration
		wantLine    string
		wantSummary string
	}{
		{
			name: "in-progress send with dedup and reused layers",
			build: func() *chunkPushProgress {
				p := newChunkPushProgress()
				p.SetLayerCounts(4, 3)
				p.LayerPlanned(2258, 412, 250_000_000) // 412 chunks missing; only 400 sent so far (in progress)
				for i := 0; i < 400; i++ {
					p.ChunkSent(250_000) // 400 * 250_000 = 100_000_000 exactly
				}
				return p
			},
			elapsed:     10 * time.Second,
			wantLine:    "40%  100.0MB/250.0MB  10.0MB/s (device already has 1846/2258 chunks, 3 layers)",
			wantSummary: "Sent 400 chunk(s) (100.0MB) in 10.0s; device already had 1846 chunk(s) and 3 full layer(s).",
		},
		{
			// Regression for the real production shape of Elapsed: it comes
			// from an actual time.Now() difference, so it carries
			// sub-millisecond noise (unlike every other row here, which uses
			// a clean round-number clock). Duration.String() only trims
			// trailing zeros — it doesn't round — so an unrounded Elapsed
			// would render "in 12.847293831s" instead of "in 12.8s". This
			// pins Summary()'s rounding (%.1fs after Round(time.Millisecond),
			// the same convention as tui's formatDuration and
			// commands.formatElapsedSeconds) against exactly that noise.
			name: "in-progress send with a non-round (real time.Now()-shaped) elapsed",
			build: func() *chunkPushProgress {
				p := newChunkPushProgress()
				p.SetLayerCounts(1, 0)
				p.LayerPlanned(20, 20, 300_000_000)
				for i := 0; i < 20; i++ {
					p.ChunkSent(5_000_000) // 20 * 5_000_000 = 100_000_000 exactly
				}
				return p
			},
			elapsed:     12*time.Second + 847293831*time.Nanosecond, // 12.847293831s
			wantLine:    "33%  100.0MB/300.0MB  7.8MB/s (device already has 0/20 chunks, 0 layers)",
			wantSummary: "Sent 20 chunk(s) (100.0MB) in 12.8s; device already had 0 chunk(s) and 0 full layer(s).",
		},
		{
			name: "fully sent, fractional-second rate matching the brief's numbers",
			build: func() *chunkPushProgress {
				p := newChunkPushProgress()
				p.SetLayerCounts(1, 0)
				p.LayerPlanned(10, 10, 200_000_000)
				for i := 0; i < 10; i++ {
					p.ChunkSent(9_840_000)
				}
				return p
			},
			elapsed:     12300 * time.Millisecond,
			wantLine:    "49%  98.4MB/200.0MB  8.0MB/s (device already has 0/10 chunks, 0 layers)",
			wantSummary: "Sent 10 chunk(s) (98.4MB) in 12.3s; device already had 0 chunk(s) and 0 full layer(s).",
		},
		{
			name: "resume: a planned layer's chunks were all already on the device",
			build: func() *chunkPushProgress {
				p := newChunkPushProgress()
				p.SetLayerCounts(1, 0)
				p.LayerPlanned(2258, 0, 0) // nothing missing: an interrupted prior push already staged everything
				return p
			},
			elapsed:     3 * time.Second,
			wantLine:    "(device already has 2258/2258 chunks, 0 layers)",
			wantSummary: "All 2258 chunk(s) already on device (0 full layer(s) reused).",
		},
		{
			name: "all-reused: every layer skipped whole, no chunk-level work at all",
			build: func() *chunkPushProgress {
				p := newChunkPushProgress()
				p.SetLayerCounts(3, 3)
				return p
			},
			elapsed:     time.Second,
			wantLine:    "(device already has 0/0 chunks, 3 layers)",
			wantSummary: "All 3 layer(s) already on device; nothing to send.",
		},
		{
			name:        "zero-planned: a pristine aggregator with nothing recorded yet",
			build:       func() *chunkPushProgress { return newChunkPushProgress() },
			elapsed:     0,
			wantLine:    "(device already has 0/0 chunks, 0 layers)",
			wantSummary: "All 0 layer(s) already on device; nothing to send.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.build()
			p.now = clockAt(start, tc.elapsed)
			p.start = start
			snap := p.Snapshot()
			if snap.Elapsed != tc.elapsed {
				t.Fatalf("Elapsed = %v, want %v", snap.Elapsed, tc.elapsed)
			}
			if got := snap.Line(); got != tc.wantLine {
				t.Errorf("Line() = %q, want %q", got, tc.wantLine)
			}
			if got := snap.Summary(); got != tc.wantSummary {
				t.Errorf("Summary() = %q, want %q", got, tc.wantSummary)
			}
		})
	}
}

// TestChunkPushProgressNilSafe calls every method, including Snapshot, on a
// nil *chunkPushProgress. A nil aggregator is how callers opt out of progress
// tracking (e.g. a non-interactive/no-renderer path), so every method must
// tolerate it without panicking, and Snapshot must hand back the zero
// snapshot rather than dereferencing anything.
func TestChunkPushProgressNilSafe(t *testing.T) {
	var p *chunkPushProgress

	p.SetLayerCounts(5, 2)
	p.LayerPlanned(100, 10, 12345)
	p.ChunkSent(4096)

	snap := p.Snapshot()
	want := chunkPushSnapshot{}
	if snap != want {
		t.Fatalf("Snapshot() on nil = %+v, want zero value %+v", snap, want)
	}

	// The zero snapshot's rendering methods must also be safe and produce
	// sensible degenerate output, not divide-by-zero panics.
	if got, want := snap.Bytes().String(), ""; got != want {
		t.Errorf("Bytes().String() on zero snapshot = %q, want %q", got, want)
	}
	_ = snap.Line()
	_ = snap.Summary()
}

// steppedClock is a chunkPushProgress clock the test advances by hand.
type steppedClock struct{ t time.Time }

func (c *steppedClock) now() time.Time { return c.t }

func (c *steppedClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newSteppedProgress(c *steppedClock) *chunkPushProgress {
	return &chunkPushProgress{now: c.now, start: c.t}
}

// TestChunkPushSummarySeparatesWireAndDeviceTime is the WDY-3215 example: a
// 1.8s upload followed by 40.5s of device-side preparation must not read as
// a 42.3s transfer.
func TestChunkPushSummarySeparatesWireAndDeviceTime(t *testing.T) {
	clock := &steppedClock{t: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}
	p := newSteppedProgress(clock)
	p.SetLayerCounts(3, 2)
	p.LayerPlanned(5216, 999, 62_900_000)
	clock.advance(500 * time.Millisecond) // host-side manifest resolution, not wire time
	for i := 0; i < 999; i++ {
		p.ChunkSent(62_962) // 999 * 62_962 ≈ 62.9 MB
	}
	clock.advance(1800 * time.Millisecond)
	p.UploadFinished()
	clock.advance(40500 * time.Millisecond)
	p.PrepareFinished()

	got := p.Snapshot().Summary()
	want := "Sent 999 chunk(s) (62.9MB) in 1.8s (34.9MB/s); device already had 4217 chunk(s) and 2 full layer(s). " +
		"The device then took 40.5s to prepare the image (device-side work, not the network)."
	if got != want {
		t.Fatalf("Summary() =\n  %q\nwant\n  %q", got, want)
	}
}

func TestChunkPushSummaryOmitsNegligibleDeviceTime(t *testing.T) {
	clock := &steppedClock{t: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}
	p := newSteppedProgress(clock)
	p.SetLayerCounts(1, 0)
	p.LayerPlanned(10, 10, 10_000_000)
	for i := 0; i < 10; i++ {
		p.ChunkSent(1_000_000)
	}
	clock.advance(2 * time.Second)
	p.UploadFinished()
	clock.advance(50 * time.Millisecond)
	p.PrepareFinished()

	if got := p.Snapshot().Summary(); strings.Contains(got, "device then took") {
		t.Fatalf("Summary() = %q; a 50ms device phase is noise", got)
	}
}

func TestChunkPushLineReportsTheDevicePhase(t *testing.T) {
	clock := &steppedClock{t: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}
	p := newSteppedProgress(clock)
	p.SetLayerCounts(1, 0)
	p.LayerPlanned(10, 10, 10_000_000)
	for i := 0; i < 10; i++ {
		p.ChunkSent(1_000_000)
	}
	clock.advance(time.Second)
	p.UploadFinished()
	clock.advance(12 * time.Second)

	snap := p.Snapshot()
	if !snap.Preparing || snap.Activity() != "device preparing image" {
		t.Fatalf("snapshot = %+v, want the device phase", snap)
	}
	if got, want := snap.Line(), "uploaded 10.0MB in 1.0s; device preparing image for 12.0s (device already has 0/10 chunks, 0 layers)"; got != want {
		t.Fatalf("Line() = %q, want %q", got, want)
	}
}
