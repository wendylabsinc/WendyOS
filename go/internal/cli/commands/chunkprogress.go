package commands

import (
	"fmt"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

// chunkPushProgress aggregates transfer counters across the ≤4 concurrent
// layer-push goroutines started by pushLayersByChunks (maxConcurrentLayerPush).
// Every method is nil-safe — a nil *chunkPushProgress is a valid "no progress
// tracking" value, so callers never need a separate opt-out check — and every
// method that touches the counters is guarded by a single mutex. Snapshot
// takes that lock just long enough to copy the counters into an immutable
// chunkPushSnapshot and is the only method safe to call from a renderer
// goroutine (A4's ticker) while push goroutines are still calling
// LayerPlanned/ChunkSent concurrently.
type chunkPushProgress struct {
	mu sync.Mutex

	now   func() time.Time // clock seam; time.Now in production, injected in tests
	start time.Time

	layersTotal, layersReused, layersPlanned int
	totalChunks, missingChunks, sentChunks   int
	sentBytes, plannedBytes                  int64

	// firstSent, uploadDone and prepareDone split the push into upload time
	// (first chunk sent to last acknowledgement) and device time, so a slow
	// device is not reported as a slow network (WDY-3215). Zero until the
	// corresponding event happens. prepareFailed ends the device phase without
	// a device time: a failed preparation prepared nothing.
	firstSent, uploadDone, prepareDone time.Time
	prepareFailed                      bool
}

// newChunkPushProgress returns a ready-to-use aggregator with its clock
// started now.
func newChunkPushProgress() *chunkPushProgress {
	return &chunkPushProgress{now: time.Now, start: time.Now()}
}

// SetLayerCounts records the layer-level pre-check result — how many layers
// the image has in total and how many of those the device already holds in
// full (queryPresentLayers), so they are skipped entirely and never chunked.
// Called once, before any per-layer chunk-diff push begins.
func (p *chunkPushProgress) SetLayerCounts(total, reused int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.layersTotal = total
	p.layersReused = reused
}

// LayerPlanned records one layer's chunk-diff plan, once QueryChunks has told
// us which content hashes the device is missing: the unique-hash count in the
// layer's manifest, how many of those are missing, and the byte total of the
// missing ones (what this layer will actually push over WriteChunks). Called
// once per layer that was not skipped by the layer-level pre-check, from
// whichever of the concurrent push goroutines is handling that layer.
func (p *chunkPushProgress) LayerPlanned(totalChunks, missing int, missingBytes int64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.layersPlanned++
	p.totalChunks += totalChunks
	p.missingChunks += missing
	p.plannedBytes += missingBytes
}

// ChunkSent records one missing chunk's bytes having been streamed to the
// device (one WriteChunks Send call). Called once per chunk sent, from
// whichever concurrent goroutine is pushing that layer.
func (p *chunkPushProgress) ChunkSent(n int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.firstSent.IsZero() {
		p.firstSent = p.now()
	}
	p.sentChunks++
	p.sentBytes += int64(n)
}

// UploadFinished records that the device acknowledged every missing chunk.
// From here until PrepareFinished, the push is waiting on the device.
func (p *chunkPushProgress) UploadFinished() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.uploadDone.IsZero() {
		p.uploadDone = p.now()
	}
}

// PrepareFinished records that the device finished preparing the image (or
// that there was no device-side preparation to wait for). The first of
// PrepareFinished and PrepareFailed ends the device phase.
func (p *chunkPushProgress) PrepareFinished() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.prepareDone.IsZero() && !p.prepareFailed {
		p.prepareDone = p.now()
	}
}

// PrepareFailed records that device-side preparation failed or was
// cancelled. It prepared nothing (RunContainer does that work during start),
// so the push then reports no device time. The first of PrepareFinished and
// PrepareFailed ends the device phase.
func (p *chunkPushProgress) PrepareFailed() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.prepareDone.IsZero() {
		p.prepareFailed = true
	}
}

// Snapshot copies the current counters under lock and stamps Elapsed, giving
// the caller an immutable point-in-time view it can format without racing the
// push goroutines still mutating p. Safe to call on a nil *chunkPushProgress,
// which returns the zero snapshot.
func (p *chunkPushProgress) Snapshot() chunkPushSnapshot {
	if p == nil {
		return chunkPushSnapshot{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	s := chunkPushSnapshot{
		LayersTotal:   p.layersTotal,
		LayersReused:  p.layersReused,
		LayersPlanned: p.layersPlanned,
		TotalChunks:   p.totalChunks,
		MissingChunks: p.missingChunks,
		SentChunks:    p.sentChunks,
		SentBytes:     p.sentBytes,
		PlannedBytes:  p.plannedBytes,
		Elapsed:       now.Sub(p.start),
	}
	if !p.uploadDone.IsZero() {
		s.Uploaded = true
		if !p.firstSent.IsZero() && p.uploadDone.After(p.firstSent) {
			s.UploadTime = p.uploadDone.Sub(p.firstSent)
		}
		if !p.prepareFailed {
			end := p.prepareDone
			if end.IsZero() {
				end = now
				s.Preparing = true
			} else {
				s.Prepared = true
			}
			if end.After(p.uploadDone) {
				s.DeviceTime = end.Sub(p.uploadDone)
			}
		}
	}
	return s
}

// chunkPushSnapshot is an immutable mirror of chunkPushProgress's counters
// plus how long the push has been running. Its rendering methods (Bytes,
// Line, Summary) are pure functions of these fields — no locking, since the
// snapshot cannot change after Snapshot() returns it.
type chunkPushSnapshot struct {
	LayersTotal, LayersReused, LayersPlanned int
	TotalChunks, MissingChunks, SentChunks   int
	SentBytes, PlannedBytes                  int64
	Elapsed                                  time.Duration
	// UploadTime is the upload time (first chunk sent to last
	// acknowledgement); zero until the upload finishes or when nothing was
	// sent. It also covers host-side work that overlaps the upload
	// (QueryChunks round trips, decompressing and chunking cache-hit layers to
	// read their missing chunks), so a rate derived from it is a lower bound
	// on the wire rate.
	UploadTime time.Duration
	// Uploaded is true once the device acknowledged every missing chunk
	// (UploadFinished), even when the upload was too short for the clock to
	// measure.
	Uploaded bool
	// DeviceTime is how long the device took, after the upload, to finish
	// preparing the image (so far, while Preparing). Zero after PrepareFailed:
	// a failed preparation is not device preparation.
	DeviceTime time.Duration
	// Preparing is true while the upload is done but the device is not.
	Preparing bool
	// Prepared is true once the device finished preparing the image after
	// the upload (PrepareFinished); false while Preparing and after
	// PrepareFailed.
	Prepared bool
}

// alreadyHasChunks is the chunk count the device reported already having,
// out of TotalChunks, when each layer was planned (TotalChunks-MissingChunks).
// It reflects the dedup/resume state observed at plan time and does not
// change as chunks are subsequently sent.
func (s chunkPushSnapshot) alreadyHasChunks() int {
	return s.TotalChunks - s.MissingChunks
}

// Bytes renders the snapshot as a tui.ByteProgress: bytes sent against bytes
// planned, at the cumulative average rate (SentBytes over Elapsed).
func (s chunkPushSnapshot) Bytes() tui.ByteProgress {
	var rate float64
	if secs := s.Elapsed.Seconds(); secs > 0 {
		rate = float64(s.SentBytes) / secs
	}
	return tui.ByteProgress{Current: s.SentBytes, Total: s.PlannedBytes, Rate: rate}
}

// Line renders the one-line live progress detail shown under a running step:
// the byte progress (percent/current/total/rate, via tui.ByteProgress) plus a
// parenthetical noting how much dedup or layer-reuse the device is giving
// this push, e.g.
//
//	"38%  98.4MB/259.1MB  8.0MB/s (device already has 1846/2258 chunks, 3 layers)"
//
// When there is no byte progress yet (nothing planned or sent), the leading
// byte-progress clause is omitted and only the parenthetical is shown. While
// the device prepares the image, the line reports the finished upload (or that
// there was nothing to upload) and the device's time so far.
func (s chunkPushSnapshot) Line() string {
	detail := fmt.Sprintf("device already has %d/%d chunks, %d layers", s.alreadyHasChunks(), s.TotalChunks, s.LayersReused)
	if s.Preparing {
		if s.SentChunks == 0 {
			// Resume: every chunk was already staged on the device.
			return fmt.Sprintf("nothing to upload; device preparing image for %s (%s)", formatChunkPushElapsed(s.DeviceTime), detail)
		}
		return fmt.Sprintf("uploaded %s in %s; device preparing image for %s (%s)",
			tui.ByteProgress{Current: s.SentBytes}.String(), formatChunkPushElapsed(s.UploadTime), formatChunkPushElapsed(s.DeviceTime), detail)
	}
	if bp := s.Bytes().String(); bp != "" {
		return bp + " (" + detail + ")"
	}
	return "(" + detail + ")"
}

// Summary renders the end-of-push resume-legibility line — the WDY-2431
// deliverable this aggregator exists for: what was actually sent, and how
// much of the push was NOT needed because the device (often left over from an
// interrupted prior attempt) already had the chunks, or whole layers outright.
//
//	"Sent 412 chunk(s) (98.4MB) in 12.3s; device already had 1846 chunk(s) and 3 full layer(s)."
//
// When nothing was sent, the whole push resolved from what the device already
// had: if any layers were even planned (chunked and checked), that reads as
// "all N chunks already on device"; if every layer was skipped whole by the
// layer-level pre-check (so no chunk-level plan exists at all), it reads
// purely in terms of reused layers instead.
func (s chunkPushSnapshot) Summary() string {
	already := s.alreadyHasChunks()
	var line string
	switch {
	case s.SentChunks > 0:
		// Once the upload has finished, report its time and rate, not the
		// whole push: device-side preparation gets its own sentence below.
		// The rate is a lower bound on the wire rate, since UploadTime also
		// covers host-side work that overlaps the upload.
		took, rate := s.Elapsed, ""
		if s.Uploaded {
			took = s.UploadTime
			if secs := s.UploadTime.Seconds(); secs > 0 {
				rate = fmt.Sprintf(" (%s/s)", tui.ByteProgress{Current: int64(float64(s.SentBytes) / secs)}.String())
			}
		}
		line = fmt.Sprintf("Sent %d chunk(s) (%s) in %s%s; device already had %d chunk(s) and %d full layer(s).",
			s.SentChunks, tui.ByteProgress{Current: s.SentBytes}.String(), formatChunkPushElapsed(took), rate, already, s.LayersReused)
	case s.TotalChunks == 0:
		line = fmt.Sprintf("All %d layer(s) already on device; nothing to send.", s.LayersReused)
	default:
		line = fmt.Sprintf("All %d chunk(s) already on device (%d full layer(s) reused).", already, s.LayersReused)
	}
	return line + s.deviceClause()
}

// deviceClause reports the device's share of the push, so a deploy slowed by
// device-side assembly and unpacking is not blamed on the network (WDY-3215).
func (s chunkPushSnapshot) deviceClause() string {
	if s.DeviceTime < 100*time.Millisecond {
		return ""
	}
	clause := fmt.Sprintf(" The device then took %s to prepare the image", formatChunkPushElapsed(s.DeviceTime))
	if s.DeviceTime >= 5*time.Second && s.DeviceTime >= 2*s.UploadTime {
		return clause + " (device-side work, not the network)."
	}
	return clause + "."
}

// Activity names what the push is waiting on, for the plain heartbeat line.
func (s chunkPushSnapshot) Activity() string {
	if s.Preparing {
		return "device preparing image"
	}
	return "sending chunks"
}

// formatChunkPushElapsed renders an elapsed duration to one decimal place
// ("12.3s"), rounding away the sub-millisecond noise a real time.Now()-derived
// Elapsed otherwise carries (e.g. "12.847293831s") — Duration.String() only
// trims trailing zeros, it doesn't round, so an unrounded Elapsed would blow
// past Summary()'s one-decimal example. Same %.1fs-after-rounding convention
// as tui's formatDuration (buildplain.go) and formatElapsedSeconds
// (helpers.go), for consistency with the rest of the CLI's elapsed-time text.
func formatChunkPushElapsed(d time.Duration) string {
	return fmt.Sprintf("%.1fs", d.Round(time.Millisecond).Seconds())
}
