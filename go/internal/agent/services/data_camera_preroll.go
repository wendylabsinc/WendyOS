package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// preRollCameraLimitBytes bounds one armed camera ring's retained encoded
// payload. The ring keeps the smaller of `buffer` seconds and this many bytes,
// so a high-bitrate stream cannot let a long buffer exhaust device memory; when
// the byte bound bites first, the achieved pre-roll is honestly shorter than
// the requested buffer and the manifest says so. It is the camera analogue of
// the application ring's fixed byte ceiling in the data manager (preRollLimit),
// counted per armed source against the device's memory.
const preRollCameraLimitBytes = 64 << 20

// bufferedCameraFrame is one encoded frame held in an armed campaign's standby
// ring. The frame pointer is retained, not copied: hub frames are immutable
// from the moment they are broadcast (identity, receipt, and payload never
// change once a subscriber can see them), so the ring shares them exactly the
// way the sensor path shares frame.data with a model subscriber.
type bufferedCameraFrame struct {
	frame        *videoFrame
	randomAccess bool
	// resetSegment marks the first frame of a new producer stream, either the
	// clip's opening frame or the first frame after a mid-arm producer restart
	// (an unrelated explicit-parameter capture took the camera over). Replaying
	// it forces a fresh segment so two sequence parameter sets never land in one
	// file, mirroring reattachAfterRestart on the live path.
	resetSegment bool
}

// cameraPreRollRing is a keyframe-aligned, byte- and window-bounded ring of
// encoded frames. It is the camera analogue of the data manager's application
// pre-roll ring (manager.preRoll / evictPreRoll): the same eviction discipline
// of dropping the oldest until the ring is within its time window and its byte
// cap, specialised in two ways the application ring does not need. It only ever
// begins on a random-access unit, and it only evicts whole groups of pictures,
// because a flushed clip that does not start on a keyframe will not decode.
//
// The ring is not internally synchronised: it is owned by the fill goroutine
// while arming and read only after that goroutine has stopped (activate waits
// on it), so there is never concurrent access.
type cameraPreRollRing struct {
	buffer     time.Duration
	limitBytes int
	frames     []bufferedCameraFrame
	bytes      int
	// droppedForBytes records that the byte cap evicted frames the time window
	// would otherwise have kept, so even at steady state the ring reaches back
	// less than the requested buffer. Reported as an honesty note at flush.
	droppedForBytes bool
	// nextResetSegment marks the ring so the next keyframe it retains opens a new
	// stream (set after a mid-arm producer restart).
	nextResetSegment bool
}

// add appends one broadcast frame, then evicts. A ring must begin on a
// decodable unit, so leading inter frames are dropped until the first keyframe;
// likewise, after a restart the reset boundary attaches to the next keyframe.
func (r *cameraPreRollRing) add(frame *videoFrame) {
	_, ra := frameRandomAccess(frame)
	if len(r.frames) == 0 && !ra {
		return
	}
	if r.nextResetSegment && !ra {
		return
	}
	reset := false
	if r.nextResetSegment && ra {
		reset, r.nextResetSegment = true, false
	}
	r.frames = append(r.frames, bufferedCameraFrame{frame: frame, randomAccess: ra, resetSegment: reset})
	r.bytes += len(frame.data)
	r.evict()
}

// markStreamReset makes the next retained keyframe open a new stream.
func (r *cameraPreRollRing) markStreamReset() { r.nextResetSegment = true }

func (r *cameraPreRollRing) evict() {
	if len(r.frames) == 0 {
		return
	}
	newest := r.frames[len(r.frames)-1].frame.receiptBootNanos
	cutoff := newest - r.buffer.Nanoseconds()
	keep := r.lastKeyframeAtOrBefore(cutoff)
	for r.bytesFrom(keep) > r.limitBytes {
		next := r.nextKeyframeAfter(keep)
		if next < 0 {
			// One group of pictures left: cannot shrink further without leaving
			// the ring undecodable. Keep it and let the byte total ride; the flush
			// reports the shortened reach.
			break
		}
		keep = next
		r.droppedForBytes = true
	}
	if keep <= 0 {
		return
	}
	for i := 0; i < keep; i++ {
		r.bytes -= len(r.frames[i].frame.data)
	}
	n := copy(r.frames, r.frames[keep:])
	for i := n; i < len(r.frames); i++ {
		r.frames[i] = bufferedCameraFrame{}
	}
	r.frames = r.frames[:n]
}

// lastKeyframeAtOrBefore returns the index of the newest retained keyframe whose
// receipt is at or before cutoff, or 0 when none is (frames[0] is always a
// keyframe, so 0 is a safe, decodable floor).
func (r *cameraPreRollRing) lastKeyframeAtOrBefore(cutoff int64) int {
	keep := 0
	for i, bf := range r.frames {
		if bf.randomAccess && bf.frame.receiptBootNanos <= cutoff {
			keep = i
		}
	}
	return keep
}

func (r *cameraPreRollRing) nextKeyframeAfter(idx int) int {
	for j := idx + 1; j < len(r.frames); j++ {
		if r.frames[j].randomAccess {
			return j
		}
	}
	return -1
}

func (r *cameraPreRollRing) bytesFrom(idx int) int {
	total := 0
	for i := idx; i < len(r.frames); i++ {
		total += len(r.frames[i].frame.data)
	}
	return total
}

// flush selects the frames that open an episode triggered at triggerBoot: from
// the last keyframe at or before (triggerBoot - buffer) through the newest
// retained frame. It returns a copy (the ring is left intact), the achieved
// pre-roll offset (the opening frame's episode-relative time, negative at steady
// state), and whether the ring reached a full buffer back. The opening frame is
// marked to start a fresh segment.
func (r *cameraPreRollRing) flush(triggerBoot int64) (frames []bufferedCameraFrame, achievedOffset int64, reachedFull bool) {
	if len(r.frames) == 0 {
		return nil, 0, false
	}
	windowStart := triggerBoot - r.buffer.Nanoseconds()
	start := r.lastKeyframeAtOrBefore(windowStart)
	frames = make([]bufferedCameraFrame, len(r.frames)-start)
	copy(frames, r.frames[start:])
	frames[0].resetSegment = true
	earliest := frames[0].frame.receiptBootNanos
	return frames, earliest - triggerBoot, earliest <= windowStart
}

// armedCameraSource is one camera source's standby state for a campaign that
// requested a buffer. It subscribes to the device hub as a NON-owning consumer
// (asserting no stream parameters, exactly like the sensor path), so it never
// takes a running stream away from a viewer and never forces the
// parameter-precedence takeover: an empty StreamVideoRequest registers no
// explicit holder in the hub, so subExplicit stays empty and takeOverDefaultedHub
// is never reached. A background goroutine copies broadcast frames into the ring
// until the campaign triggers (activate) or the source is disarmed.
type armedCameraSource struct {
	video  *VideoService
	source data.Source
	key    string // device hub key
	devID  uint32
	buffer time.Duration

	// hub, subID and frames are the current subscription. They are owned by the
	// fill goroutine (which reassigns them on reattach) and read elsewhere only
	// after that goroutine has stopped.
	hub    *deviceHub
	subID  int
	frames chan *videoFrame
	ring   *cameraPreRollRing
	// lastDrops tracks the hub's drop counter for this subscription so a reattach
	// carries the unaccounted drops forward as a ring stream reset rather than
	// losing them silently.
	lastDrops uint64
	// alive reports that the subscription is still delivering. The fill goroutine
	// clears it if the producer stops or a reattach fails, so activate knows to
	// re-subscribe for the live tail instead of handing capture a dead channel.
	alive bool

	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	consumed bool
	disarmed bool
}

// fill copies broadcast frames into the ring until the source is activated or
// disarmed (context cancelled). It mirrors the sensor path's reattach: if an
// explicit-parameter capture restarts the producer, this non-owning subscriber
// rejoins the replacement stream and marks a ring stream reset so the flush
// rotates a segment at the boundary.
func (a *armedCameraSource) fill(ctx context.Context) {
	defer close(a.done)
	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-a.frames:
			if !ok {
				if err := a.hub.terminalErr(); err != nil {
					a.alive = false
					return
				}
				if a.hub.wasRestarted() {
					if err := a.reattach(ctx); err != nil {
						a.alive = false
						return
					}
					continue
				}
				a.alive = false
				return
			}
			a.ring.add(frame)
		}
	}
}

func (a *armedCameraSource) reattach(ctx context.Context) error {
	a.lastDrops += a.hub.unsubscribe(a.subID)
	hub, subID, frames, err := a.video.joinHub(ctx, a.key, &agentpb.StreamVideoRequest{DeviceId: a.devID})
	if err != nil {
		return err
	}
	a.hub, a.subID, a.frames = hub, subID, frames
	a.lastDrops = 0
	a.ring.markStreamReset()
	return nil
}

// activate stops the fill goroutine (without unsubscribing) and turns this
// armed source into a running cameraCapture bound to the episode. The standby
// ring is flushed as the capture's pre-roll and the SAME hub subscription
// continues delivering live frames, so there is no gap and no duplicate: frames
// already copied into the ring are not re-read from the channel, and frames
// still queued on the channel are consumed by the live loop. Returns the
// running capture, whether the achieved pre-roll fell short of the request, and
// an error only when neither the retained ring nor a fresh live subscription
// could open.
func (a *armedCameraSource) activate(session data.CaptureSession) (*cameraCapture, bool, error) {
	a.cancel()
	<-a.done

	a.mu.Lock()
	if a.disarmed {
		a.mu.Unlock()
		return nil, false, errors.New("camera pre-roll source was disarmed before the trigger")
	}
	a.consumed = true
	a.mu.Unlock()

	preRoll, achievedOffset, reachedFull := a.ring.flush(session.RequestBootNanos)
	shortfall := !reachedFull && len(preRoll) > 0

	dir := filepath.Join(session.Directory, "cameras", safeCaptureName(a.source.ID))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, false, err
	}
	index, err := os.OpenFile(filepath.Join(dir, "index.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, false, err
	}
	mappings, err := os.OpenFile(filepath.Join(dir, "clock_samples.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		index.Close()
		return nil, false, err
	}

	// If arming lost its producer (the camera faulted or stopped mid-arm), the
	// retained ring still opens the episode, but the live tail needs a fresh
	// subscription rather than the dead channel.
	if !a.alive || a.hub == nil {
		hub, subID, frames, joinErr := a.video.joinHub(context.Background(), a.key, &agentpb.StreamVideoRequest{DeviceId: a.devID})
		if joinErr != nil {
			if len(preRoll) == 0 {
				index.Close()
				mappings.Close()
				return nil, false, joinErr
			}
		} else {
			a.hub, a.subID, a.frames = hub, subID, frames
		}
	}

	var rateCap float64
	if a.source.Capture != nil && a.source.Capture.Rate > 0 {
		rateCap = a.source.Capture.Rate
	}
	notes := a.preRollNotes(preRoll, achievedOffset, shortfall)

	captureCtx, cancel := context.WithCancel(context.Background())
	devID := a.devID
	rejoin := func(ctx context.Context) (*deviceHub, int, chan *videoFrame, error) {
		return a.video.joinHub(ctx, a.key, &agentpb.StreamVideoRequest{DeviceId: devID})
	}
	c := &cameraCapture{
		source: a.source, session: session, dir: dir, hub: a.hub, subID: a.subID, frames: a.frames,
		rejoin: rejoin, index: index, mappingFile: mappings, ctx: captureCtx, cancel: cancel,
		done: make(chan struct{}), ready: make(chan error, 1), mode: "continuous", rateCap: rateCap,
		notes: notes, lastSnapshotIdx: -1, preRoll: preRoll, armed: true,
	}
	go c.run()
	select {
	case err := <-c.ready:
		if err != nil {
			cancel()
			<-c.done
			return nil, false, err
		}
		return c, shortfall, nil
	case <-time.After(20 * time.Second):
		cancel()
		<-c.done
		return nil, false, errors.New("timed out flushing camera pre-roll")
	}
}

// preRollNotes builds the honesty notes folded into the capture's manifest
// detail: what pre-roll was achieved against what was requested, and the fact
// that a request for an explicit resolution cannot be honoured by a pre-roll
// subscription (which asserts no parameters so it never takes the camera over).
func (a *armedCameraSource) preRollNotes(preRoll []bufferedCameraFrame, achievedOffset int64, shortfall bool) []string {
	var notes []string
	if len(preRoll) == 0 {
		notes = append(notes, fmt.Sprintf("camera pre-roll: requested %s buffer, but no decodable frame was buffered before the trigger; the episode opens at the trigger", a.buffer))
	} else {
		note := fmt.Sprintf("camera pre-roll: requested %s buffer, flushed %d frame(s) reaching %s before the trigger", a.buffer, len(preRoll), time.Duration(-achievedOffset))
		if shortfall {
			note += "; armed less than the buffer before the trigger, so achieved pre-roll is shorter than requested"
		}
		if a.ring.droppedForBytes {
			note += fmt.Sprintf("; the %d MiB pre-roll ring cap bounded the retained window below the requested buffer", preRollCameraLimitBytes>>20)
		}
		notes = append(notes, note)
	}
	if a.source.Capture != nil {
		if _, _, ok := a.source.Capture.MaxResolutionPixels(); ok {
			notes = append(notes, "pre-roll subscribes without asserting stream parameters so it never takes the camera over; recorded at the producer's running parameters rather than the requested max_resolution")
		}
	}
	return notes
}

// disarm stops arming and releases the subscription. It is a no-op once the
// source has been consumed by activate, which then owns the subscription.
func (a *armedCameraSource) disarm() {
	a.mu.Lock()
	if a.consumed || a.disarmed {
		a.mu.Unlock()
		return
	}
	a.disarmed = true
	a.mu.Unlock()
	a.cancel()
	<-a.done
	if a.hub != nil {
		a.hub.unsubscribe(a.subID)
	}
}
