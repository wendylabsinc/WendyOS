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

// preRollCameraLimitBytes bounds the payload one armed camera source retains,
// in total across its rings (see shareRingBytes). A ring keeps the smaller of
// `buffer` seconds and its share of this many bytes, so a high-bitrate stream
// cannot let a long buffer exhaust device memory; when the byte bound bites
// first, the achieved pre-roll is honestly shorter than the requested buffer
// and the manifest says so. It is the camera analogue of the application
// ring's fixed byte ceiling in the data manager (preRollLimit), counted per
// armed source against the device's memory.
const preRollCameraLimitBytes = 64 << 20

// preRollGOPWindowFactor bounds how far past the requested buffer one
// undivided group of pictures may reach before the ring gives up on it. A
// producer emitting keyframes at all keeps GOPs well inside a factor of eight;
// a stream that does not is a stream the ring cannot trim at all, so the only
// bound left is to start over.
const preRollGOPWindowFactor = 8

// bufferedCameraFrame is one frame held in an armed campaign's standby ring.
// The frame pointer is retained, not copied: hub frames are immutable from the
// moment they are broadcast (identity, receipt, and payload never change once
// a subscriber can see them), so the ring shares them exactly the way the
// sensor path shares frame.data with a model subscriber.
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
// the frames of one subscription. It is the camera analogue of the data
// manager's application pre-roll ring (manager.preRoll / evictPreRoll): the
// same eviction discipline of dropping the oldest until the ring is within its
// time window and its byte cap, specialised in two ways the application ring
// does not need. It only ever begins on a random-access unit, and it only
// evicts whole groups of pictures, because a flushed clip that does not start
// on a keyframe will not decode. A frame that decodes alone, as each of a
// camera's own JPEG frames does, is its own group of pictures.
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
			// One group of pictures left and it alone exceeds the cap. Keeping
			// it would let a producer that emits a single very long GOP grow
			// the ring without bound, because every other eviction rule works
			// at keyframe boundaries and there is no second boundary to move
			// to. The ring is dropped whole and refilled from the next
			// keyframe, which is the only bound available that still leaves a
			// decodable ring.
			r.restart()
			return
		}
		keep = next
		r.droppedForBytes = true
	}
	// The same trap measured in time rather than bytes: one undivided group of
	// pictures reaching far past the requested buffer is a ring nothing can
	// trim, so it is dropped rather than allowed to keep growing.
	if r.nextKeyframeAfter(keep) < 0 &&
		newest-r.frames[keep].frame.receiptBootNanos > preRollGOPWindowFactor*r.buffer.Nanoseconds() {
		r.restart()
		return
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

// restart drops the whole ring and waits for the next keyframe to begin a new
// one. It is the eviction of last resort, used when a single group of pictures
// has grown past a bound and there is no keyframe boundary left to trim at.
// The retained pre-roll is honestly shorter afterwards, which droppedForBytes
// reports, and the frames that follow open a new stream because the discarded
// tail leaves a gap no decoder should be asked to cross.
func (r *cameraPreRollRing) restart() {
	r.frames = r.frames[:0]
	r.bytes = 0
	r.droppedForBytes = true
	r.nextResetSegment = true
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

// flushRings reports the latest of the rings' openings as the achieved offset:
// the window every stored form covers.
func (a *armedCameraSource) flushRings(triggerBoot int64) (frames []bufferedCameraFrame, achievedOffset int64, shortfall bool) {
	frames, achievedOffset, reachedFull := a.ring.flush(triggerBoot)
	shortfall = !reachedFull && len(frames) > 0
	if a.ownFrameRing == nil {
		return frames, achievedOffset, shortfall
	}
	own, ownOffset, ownReachedFull := a.ownFrameRing.flush(triggerBoot)
	if len(own) == 0 {
		return frames, achievedOffset, shortfall
	}
	if len(frames) == 0 || ownOffset > achievedOffset {
		achievedOffset = ownOffset
	}
	return mergeByReceipt(frames, own), achievedOffset, shortfall || !ownReachedFull
}

// mergeByReceipt keeps each ring's own order: coded video must be replayed in
// the order it arrived.
func mergeByReceipt(a, b []bufferedCameraFrame) []bufferedCameraFrame {
	merged := make([]bufferedCameraFrame, 0, len(a)+len(b))
	for len(a) > 0 && len(b) > 0 {
		if b[0].frame.receiptBootNanos < a[0].frame.receiptBootNanos {
			merged, b = append(merged, b[0]), b[1:]
		} else {
			merged, a = append(merged, a[0]), a[1:]
		}
	}
	return append(append(merged, a...), b...)
}

// preRollFlushDrainLimitBytes bounds the side buffer that holds live frames
// arriving while the pre-roll is written. It is a quarter of the ring's own
// cap: the flush is bounded work, so the buffer only has to cover a burst
// lasting as long as writing at most preRollCameraLimitBytes to disk, and a
// bound is still needed because a stalled disk must cost memory no faster than
// the ring itself does.
const preRollFlushDrainLimitBytes = preRollCameraLimitBytes / 4

// frameDrain is what a drain goroutine collected while a slow operation held
// the capture goroutine.
type frameDrain struct {
	frames []drainedFrame
	bytes  int
	// dropped counts frames the side buffer refused because it was full. They
	// are charged to the capture's drop total, exactly like a hub-side drop:
	// the frame existed and the episode does not have it.
	dropped uint64
	// closed records that the subscription ended during the drain. The live
	// loop discovers the same thing on its next receive (a closed channel
	// yields immediately), so nothing special has to be done with it here.
	closed bool
}

type drainedFrame struct {
	frame        *videoFrame
	nativeBeside bool
}

// drainDuring runs fn while a helper goroutine keeps consuming the capture's
// subscriptions into a bounded side buffer, and returns what it collected.
//
// This exists because of what the pre-roll flush is: activate hands the LIVE
// hub subscription straight to the capture, and run then spends the flush
// writing every buffered frame to disk before it reaches the live select. The
// subscriber channel is four frames deep, so on any real camera the frames
// immediately after the trigger, the ones the episode exists to record, were
// dropped by the hub while the flush ran, and the tail resumed mid group of
// pictures. Keeping a reader on the channel throughout is what removes that
// loss; the frames collected are replayed through the ordinary live path
// afterwards, in order, so they are written exactly as if the flush had been
// instantaneous.
//
// The goroutine owns the subscription channels for the duration and is joined
// before this returns, so the caller resumes as their sole reader with no data
// race. A native subscription that closes is left for the live loop, which
// reads the close again and handles it.
func (c *cameraCapture) drainDuring(fn func() error) (*frameDrain, error) {
	d := &frameDrain{}
	stop := make(chan struct{})
	done := make(chan struct{})
	primary, nativeBeside := c.frames, c.nativeFrames
	keep := func(frame *videoFrame, fromNativeBeside bool) {
		if d.bytes+len(frame.data) > preRollFlushDrainLimitBytes {
			d.dropped++
			return
		}
		d.frames = append(d.frames, drainedFrame{frame: frame, nativeBeside: fromNativeBeside})
		d.bytes += len(frame.data)
	}
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case frame, ok := <-primary:
				if !ok {
					d.closed = true
					return
				}
				keep(frame, false)
			case frame, ok := <-nativeBeside:
				if !ok {
					nativeBeside = nil
					continue
				}
				keep(frame, true)
			}
		}
	}()
	err := fn()
	close(stop)
	<-done
	return d, err
}

// flushPreRoll writes every frame of the activated standby ring. An
// undecodable leading frame (which should not happen: the ring only retains
// from a keyframe) is skipped rather than failing the episode.
func (c *cameraCapture) flushPreRoll() error {
	for i := range c.preRoll {
		if err := c.writeBufferedFrame(c.preRoll[i]); errors.Is(err, errAwaitCameraRandomAccess) {
			continue
		} else if err != nil {
			return err
		}
		if c.preRollFlushHook != nil {
			c.preRollFlushHook()
		}
	}
	c.preRoll = nil
	return nil
}

// saturatingSub subtracts b from a without wrapping. The drop baselines it
// guards are read from the same monotonic counter as the totals, so a > b
// always holds in practice; a counter that wrapped the other way would turn a
// small overcount into an astronomically wrong drop figure in the manifest,
// which is exactly the kind of number a reader would believe.
func saturatingSub(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
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
	hub               *deviceHub
	subID             int
	frames            chan *videoFrame
	ring              *cameraPreRollRing
	primaryKind       frameKind
	wantsNativeBeside bool
	nativeSubID       int
	nativeFrames      chan *videoFrame
	ownFrameRing      *cameraPreRollRing
	ownFramesAreVideo bool
	ownFramesCodec    agentpb.VideoCodec
	ownFramesRefused  bool
	storeNotes        []string
	monotonicMapping  func() (data.MonotonicMappingSample, error)
	// carriedDrops accumulates the hub drop counters of subscriptions this
	// armed source has already left, so a mid-arm producer restart does not
	// lose the count. activate hands the total to the capture, which reports
	// it as the episode's armed-period drops.
	carriedDrops uint64
	// armedAt is when the source was armed, and ownWait what it has waited
	// for the camera since.
	armedAt time.Time
	ownWait ownCameraWait
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
				if refusal := a.hub.subscriberErr(a.subID); a.primaryKind == nativeFrames && isNativeRefusal(refusal) {
					if err := a.bufferVideoInstead(refusal); err != nil {
						a.alive = false
						return
					}
					continue
				}
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
			if a.primaryKind == nativeFrames && encodedFrames.receives(frame) {
				a.ownFramesAreVideo, a.ownFramesCodec = true, frame.codec
			}
			a.ring.add(frame)
		case frame, ok := <-a.nativeFrames:
			switch {
			case !ok:
				a.nativeBesideEnded()
			case encodedFrames.receives(frame):
				a.ownFramesAreVideo, a.ownFramesCodec = true, frame.codec
				a.leaveNativeBeside()
				a.ownFrameRing = nil
				a.shareRingBytes()
			default:
				a.ownFrameRing.add(frame)
			}
		}
	}
}

func (a *armedCameraSource) join(ctx context.Context) error {
	atDefaults := &agentpb.StreamVideoRequest{DeviceId: a.devID}
	hub, subID, frames, kind, refusal, err := joinWithVideoFallback(a.primaryKind, a.video.refusingOwnFramesKnownAbsent(a.key, atDefaults, func(kind frameKind) (*deviceHub, int, chan *videoFrame, error) {
		return a.video.joinHubKind(ctx, a.key, atDefaults, kind, a.source.Capture.EncodePreset())
	}))
	if err != nil {
		return err
	}
	if now := time.Now(); a.hub == nil {
		a.armedAt, a.ownWait.since = now, now
	} else {
		a.ownWait.movedFrom(a.hub, now)
	}
	a.hub, a.subID, a.frames, a.primaryKind = hub, subID, frames, kind
	if refusal != nil {
		a.ownFramesNoLongerStored(refusal)
	}
	if a.wantsNativeBeside && !a.ownFramesRefused && !a.ownFramesAreVideo && a.primaryKind == encodedFrames {
		if id, ch, err := a.hub.subscribeKind(nativeFrames); err != nil {
			a.ownFramesNoLongerStored(err)
		} else {
			a.nativeSubID, a.nativeFrames = id, ch
			if a.ownFrameRing == nil {
				a.ownFrameRing = &cameraPreRollRing{buffer: a.buffer}
			}
		}
	}
	a.shareRingBytes()
	return nil
}

func (a *armedCameraSource) shareRingBytes() {
	if a.ownFrameRing == nil {
		a.ring.limitBytes = preRollCameraLimitBytes
		return
	}
	a.ring.limitBytes = preRollCameraLimitBytes / 2
	a.ownFrameRing.limitBytes = preRollCameraLimitBytes - a.ring.limitBytes
	a.ring.evict()
	a.ownFrameRing.evict()
}

func (a *armedCameraSource) leaveNativeBeside() {
	if a.nativeFrames == nil {
		return
	}
	a.nativeFrames = nil
	a.carriedDrops += a.hub.unsubscribe(a.nativeSubID)
}

func (a *armedCameraSource) nativeBesideEnded() {
	refusal := a.hub.subscriberErr(a.nativeSubID)
	a.leaveNativeBeside()
	if refusal != nil {
		a.ownFramesNoLongerStored(refusal)
		a.ownFrameRing = nil
		a.shareRingBytes()
	}
}

func (a *armedCameraSource) bufferVideoInstead(refusal error) error {
	id, ch, drops, err := a.hub.resubscribeAs(a.subID, encodedFrames)
	if err != nil {
		return err
	}
	a.carriedDrops += drops
	a.subID, a.frames, a.primaryKind = id, ch, encodedFrames
	a.ring = &cameraPreRollRing{buffer: a.buffer, limitBytes: preRollCameraLimitBytes}
	a.ownFramesNoLongerStored(refusal)
	return nil
}

// ownFramesNoLongerStored is final for the armed period and the episode it
// opens, as it is for a running capture.
func (a *armedCameraSource) ownFramesNoLongerStored(cause error) {
	if a.ownFramesRefused {
		return
	}
	a.ownFramesRefused = true
	a.storeNotes = append(a.storeNotes, ownFramesUnavailableNote(a.source.Capture, cause, 0))
}

func (a *armedCameraSource) reattach(ctx context.Context) error {
	// The counts still belong to this armed source even when the rejoin
	// fails: activate reports them whether or not a live tail follows.
	a.leaveNativeBeside()
	a.carriedDrops += a.hub.unsubscribe(a.subID)
	if err := a.join(ctx); err != nil {
		return err
	}
	a.ring.markStreamReset()
	if a.ownFrameRing != nil {
		a.ownFrameRing.markStreamReset()
	}
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
	triggered := time.Now()
	a.cancel()
	<-a.done

	a.mu.Lock()
	if a.disarmed {
		a.mu.Unlock()
		return nil, false, errors.New("camera pre-roll source was disarmed before the trigger")
	}
	a.consumed = true
	a.mu.Unlock()

	preRoll, achievedOffset, shortfall := a.flushRings(session.RequestBootNanos)

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
		return nil, false, errors.Join(err, index.Close())
	}

	// If arming lost its producer (the camera faulted or stopped mid-arm), the
	// retained ring still opens the episode, but the live tail needs a fresh
	// subscription rather than the dead channel.
	if !a.alive || a.hub == nil {
		a.nativeFrames = nil
		if joinErr := a.join(context.Background()); joinErr != nil && len(preRoll) == 0 {
			return nil, false, errors.Join(joinErr, index.Close(), mappings.Close())
		}
	}

	var rateCap float64
	if a.source.Capture != nil && a.source.Capture.Rate > 0 {
		rateCap = a.source.Capture.Rate
	}
	// A wait for the camera that was over by the trigger is on record here, or
	// kept nothing of the episode waiting: the episode starts its own count.
	// It cost the episode what the pre-roll would have held before it ended.
	ownWait := a.ownWait
	var waitInPreRoll string
	windowOpens := triggered.Add(-a.buffer)
	if from, until, state := a.ownWait.of(a.hub, triggered); state == ownWaitOver {
		ownWait = ownCameraWait{since: triggered, hadCamera: true}
		if until.After(windowOpens) {
			waitInPreRoll = fmt.Sprintf(cameraWaitInPreRoll, roundedWait(from, until), roundedWait(until, triggered))
		}
	}
	armedShort := shortfall && (waitInPreRoll == "" || a.armedAt.After(windowOpens))
	notes := a.preRollNotes(preRoll, achievedOffset, armedShort)
	if waitInPreRoll != "" {
		notes = append(notes, waitInPreRoll)
	}
	notes = append(notes, a.storeNotes...)

	captureCtx, cancel := context.WithCancel(context.Background())
	devID := a.devID
	rejoin := func(ctx context.Context, kind frameKind) (*deviceHub, int, chan *videoFrame, error) {
		atDefaults := &agentpb.StreamVideoRequest{DeviceId: devID}
		return a.video.refusingOwnFramesKnownAbsent(a.key, atDefaults, func(kind frameKind) (*deviceHub, int, chan *videoFrame, error) {
			return a.video.joinHubKind(ctx, a.key, atDefaults, kind, a.source.Capture.EncodePreset())
		})(kind)
	}
	// Everything the hub dropped for this subscription up to now happened
	// during the ARMED period. The capture subtracts it from its own total and
	// reports it separately, so the episode's Drops describe the recording
	// rather than the whole standby watch that preceded it.
	// baseline is what the INHERITED subscription's own counter already holds;
	// it is the part the capture's teardown will read back and must subtract.
	// The armed total additionally carries the counters of subscriptions this
	// armed source already left across a mid-arm producer restart, which the
	// capture will never see.
	var baseline uint64
	if a.hub != nil {
		baseline = a.hub.drops(a.subID)
		if a.nativeFrames != nil {
			baseline += a.hub.drops(a.nativeSubID)
		}
	}
	armedDrops := a.carriedDrops + baseline
	c := &cameraCapture{
		source: a.source, session: session, dir: dir, hub: a.hub, subID: a.subID, frames: a.frames,
		primaryKind: a.primaryKind, wantsNativeBeside: a.wantsNativeBeside, nativeSubID: a.nativeSubID, nativeFrames: a.nativeFrames,
		monotonicMapping:  a.monotonicMapping,
		ownFramesAreVideo: a.ownFramesAreVideo, ownFramesCodec: a.ownFramesCodec, ownFramesRefused: a.ownFramesRefused,
		rejoin: rejoin, logger: a.video.logger, index: index, mappingFile: mappings, ctx: captureCtx, cancel: cancel,
		done: make(chan struct{}), ready: make(chan error, 1), mode: "continuous", rateCap: rateCap,
		notes: notes, ownWait: ownWait, lastSnapshotIdx: -1, preRoll: preRoll, armed: true,
		dropBaseline: baseline, armedDrops: armedDrops,
		presetNotApplied: a.video.whyPresetNotApplied(a.devID, a.source.Capture),
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
func (a *armedCameraSource) preRollNotes(preRoll []bufferedCameraFrame, achievedOffset int64, armedShort bool) []string {
	var notes []string
	if len(preRoll) == 0 {
		notes = append(notes, fmt.Sprintf("camera pre-roll: requested %s buffer, but no decodable frame was buffered before the trigger; the episode opens at the trigger", a.buffer))
	} else {
		note := fmt.Sprintf("camera pre-roll: requested %s buffer, flushed %d frame(s) reaching %s before the trigger", a.buffer, len(preRoll), time.Duration(-achievedOffset))
		if armedShort {
			note += "; armed less than the buffer before the trigger, so achieved pre-roll is shorter than requested"
		}
		if a.ring.droppedForBytes || (a.ownFrameRing != nil && a.ownFrameRing.droppedForBytes) {
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
		a.leaveNativeBeside()
		a.hub.unsubscribe(a.subID)
	}
}
