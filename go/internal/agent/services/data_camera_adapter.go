package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

const cameraSegmentDuration = 10 * time.Second

// captureModeSnapshot is the camera capture mode producing one standalone
// decodable still per campaign-declared interval. Every other declared mode
// records continuously (deployment already warned for the unimplemented ones).
const captureModeSnapshot = "snapshot"

var errAwaitCameraRandomAccess = errors.New("waiting for a decodable random-access unit")

type cameraDataAdapter struct {
	video *VideoService
	// armed holds the standby pre-roll rings a campaign armed for its camera
	// sources, keyed by campaign concurrency key then by source ID. It is
	// populated by Arm and consumed by Start when the campaign triggers.
	armedMu sync.Mutex
	armed   map[string]map[string]*armedCameraSource
}

func newCameraDataAdapter(video *VideoService) dataCaptureAdapter {
	if video == nil {
		return nil
	}
	return &cameraDataAdapter{video: video}
}

func (a *cameraDataAdapter) Discover(ctx context.Context) []data.Source {
	devices, err := a.video.listCameras(ctx)
	if err != nil {
		return nil
	}
	out := make([]data.Source, 0, len(devices))
	for _, dev := range devices {
		if dev.GetTransport() == agentpb.VideoTransport_VIDEO_TRANSPORT_IP {
			healthy := dev.GetOnline() && dev.GetHasCredentials()
			detail := strings.TrimSpace(dev.GetName() + " " + dev.GetAddress())
			if !dev.GetHasCredentials() {
				detail += " (credentials required)"
			} else if !dev.GetOnline() {
				detail += " (offline)"
			}
			out = append(out, data.Source{ID: fmt.Sprintf("ipcamera:%d", dev.GetId()), Kind: "camera", ClockDomain: "IP_CAMERA_STREAM/AGENT_RECEIPT", Healthy: healthy, Detail: detail})
			continue
		}
		domain := "V4L2_BUFFER_TIMESTAMP"
		if dev.GetTransport() == agentpb.VideoTransport_VIDEO_TRANSPORT_CSI {
			domain = "GSTREAMER_PIPELINE/AGENT_RECEIPT"
		}
		out = append(out, data.Source{ID: "v4l2:" + dev.GetPath(), Kind: "camera", ClockDomain: domain, Healthy: true, Detail: strings.TrimSpace(dev.GetName() + " " + dev.GetTransport().String())})
	}
	return out
}

func cameraDeviceID(sourceID string) (uint32, bool) {
	var raw string
	if strings.HasPrefix(sourceID, "ipcamera:") {
		raw = strings.TrimPrefix(sourceID, "ipcamera:")
	} else if strings.HasPrefix(sourceID, "v4l2:/dev/video") {
		raw = strings.TrimPrefix(sourceID, "v4l2:/dev/video")
	} else {
		return 0, false
	}
	n, err := strconv.ParseUint(raw, 10, 32)
	return uint32(n), err == nil
}

func (a *cameraDataAdapter) Start(ctx context.Context, session data.CaptureSession, selected []data.Source) (runningDataCapture, error) {
	group := &cameraCaptureGroup{}
	armed := a.takeArmed(session.CampaignKey)
	for _, source := range selected {
		devID, ok := cameraDeviceID(source.ID)
		if !ok {
			continue
		}
		// A campaign that armed this camera for pre-roll flushes its standby ring
		// into the opening frames and continues live on the same subscription.
		if pre := armed[source.ID]; pre != nil {
			delete(armed, source.ID)
			capture, _, err := pre.activate(session)
			if err != nil {
				// Pre-roll activation failed (for example the camera faulted mid
				// arm). Fall back to a fresh live capture so the episode still
				// records this camera from the trigger rather than losing it.
				capture, err = a.startOne(ctx, session, source, devID)
			}
			if errors.Is(err, errCameraHeldExplicitly) {
				group.refused = append(group.refused, data.CaptureResult{
					SourceID:     source.ID,
					SourceDetail: strings.TrimSpace(source.Detail + " (not captured: " + err.Error() + ")"),
				})
				continue
			}
			if err != nil {
				_, _ = group.Stop(context.Background())
				a.disarmAll(armed)
				return nil, fmt.Errorf("%s: %w", source.ID, err)
			}
			group.captures = append(group.captures, capture)
			continue
		}
		capture, err := a.startOne(ctx, session, source, devID)
		if errors.Is(err, errCameraHeldExplicitly) {
			// Another consumer holds this camera at explicitly requested
			// parameters that conflict with the campaign's. Refusing this one
			// source, with the named error in its manifest entry, is the
			// honest outcome: the episode continues with its other sources and
			// the manifest says exactly who holds the camera and at what
			// parameters, instead of recording a capture the campaign did not
			// ask for.
			group.refused = append(group.refused, data.CaptureResult{
				SourceID:     source.ID,
				SourceDetail: strings.TrimSpace(source.Detail + " (not captured: " + err.Error() + ")"),
			})
			continue
		}
		if err != nil {
			_, _ = group.Stop(context.Background())
			return nil, fmt.Errorf("%s: %w", source.ID, err)
		}
		group.captures = append(group.captures, capture)
	}
	// Any armed ring for this campaign whose source was not among the episode's
	// selected sources is released rather than left subscribed.
	a.disarmAll(armed)
	if len(group.captures) == 0 && len(group.refused) == 0 {
		return nil, nil
	}
	return group, nil
}

// Arm puts each continuous-mode camera source into standby for a campaign that
// requested a buffer: it subscribes to the source's hub as a non-owning
// consumer and keeps a keyframe-aligned ring of `buffer` seconds of encoded
// frames without writing an episode. Snapshot-mode sources are not armed:
// snapshot capture writes sparse independent stills, not a continuous ring.
// Arm is idempotent per campaign key when paired with Disarm; a source that
// cannot be resolved or subscribed is skipped so the campaign still arms its
// other cameras.
func (a *cameraDataAdapter) Arm(campaignKey string, buffer time.Duration, selected []data.Source) {
	if a.video == nil || buffer <= 0 {
		return
	}
	for _, source := range selected {
		devID, ok := cameraDeviceID(source.ID)
		if !ok {
			continue
		}
		if source.Capture != nil && source.Capture.EffectiveMode() == captureModeSnapshot {
			continue
		}
		src, err := a.video.resolveSource(devID)
		if err != nil {
			continue
		}
		if src.kind == sourceIP {
			if err := a.video.preflightIPCamera(src.camera); err != nil {
				continue
			}
		}
		// A parameter-less request: arming must never take a running stream from
		// a viewer, and must never register an explicit holder that would let it
		// force the parameter-precedence takeover.
		hub, subID, frames, err := a.video.joinHub(context.Background(), src.key, &agentpb.StreamVideoRequest{DeviceId: devID})
		if err != nil {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		as := &armedCameraSource{
			video: a.video, source: source, key: src.key, devID: devID, buffer: buffer,
			hub: hub, subID: subID, frames: frames, alive: true,
			ring:   &cameraPreRollRing{buffer: buffer, limitBytes: preRollCameraLimitBytes},
			cancel: cancel, done: make(chan struct{}),
		}
		go as.fill(ctx)
		a.store(campaignKey, source.ID, as)
	}
}

// Disarm stops and releases every armed source for a campaign key.
func (a *cameraDataAdapter) Disarm(campaignKey string) {
	a.disarmAll(a.takeArmed(campaignKey))
}

func (a *cameraDataAdapter) store(campaignKey, sourceID string, as *armedCameraSource) {
	a.armedMu.Lock()
	if a.armed == nil {
		a.armed = make(map[string]map[string]*armedCameraSource)
	}
	byKey := a.armed[campaignKey]
	if byKey == nil {
		byKey = make(map[string]*armedCameraSource)
		a.armed[campaignKey] = byKey
	}
	old := byKey[sourceID]
	byKey[sourceID] = as
	a.armedMu.Unlock()
	// Replace any stale armed source for the same key+source (re-arm), disarming
	// the old one so its subscription is not leaked. Done outside the lock: disarm
	// waits for the old fill goroutine to stop.
	if old != nil {
		old.disarm()
	}
}

// takeArmed removes and returns the armed sources for a campaign key.
func (a *cameraDataAdapter) takeArmed(campaignKey string) map[string]*armedCameraSource {
	a.armedMu.Lock()
	defer a.armedMu.Unlock()
	byKey := a.armed[campaignKey]
	delete(a.armed, campaignKey)
	return byKey
}

func (a *cameraDataAdapter) disarmAll(sources map[string]*armedCameraSource) {
	for _, as := range sources {
		as.disarm()
	}
}

func (a *cameraDataAdapter) startOne(ctx context.Context, session data.CaptureSession, source data.Source, devID uint32) (*cameraCapture, error) {
	src, err := a.video.resolveSource(devID)
	if err != nil {
		return nil, err
	}
	if src.kind == sourceIP {
		if err := a.video.preflightIPCamera(src.camera); err != nil {
			return nil, err
		}
	}
	mode := "continuous"
	var interval time.Duration
	var notes []string
	capture := source.Capture
	if capture.EffectiveMode() == captureModeSnapshot {
		if d := capture.IntervalDuration(); d > 0 {
			mode, interval = captureModeSnapshot, d
			notes = append(notes, fmt.Sprintf("snapshot mode: one still per %s", d))
		} else {
			notes = append(notes, "snapshot mode requested without a valid interval; recording continuously")
		}
	}
	var rateCap float64
	if capture != nil && capture.Rate > 0 {
		rateCap = capture.Rate
	}
	req, requestNotes := buildStreamRequest(devID, src, capture)
	notes = append(notes, requestNotes...)
	dir := filepath.Join(session.Directory, "cameras", safeCaptureName(source.ID))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	index, err := os.OpenFile(filepath.Join(dir, "index.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, err
	}
	mappings, err := os.OpenFile(filepath.Join(dir, "clock_samples.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		index.Close()
		return nil, err
	}
	hub, subID, frames, achievedW, achievedH, achievedFPS, restarted, err := a.subscribeHub(ctx, src.key, req)
	if err != nil {
		index.Close()
		mappings.Close()
		return nil, err
	}
	if restarted {
		notes = append(notes, fmt.Sprintf("campaign capture parameters took over the camera: restarted the producer, previously running at producer-default parameters for parameter-less subscribers, at the requested %s; those subscribers reattached to the new stream",
			describeStreamParams(achievedW, achievedH, achievedFPS)))
	}
	if achievedW != req.GetWidth() || achievedH != req.GetHeight() || achievedFPS != req.GetFramerate() {
		notes = append(notes, fmt.Sprintf("stream already active with different parameters; requested %s, capturing at %s",
			describeStreamParams(req.GetWidth(), req.GetHeight(), req.GetFramerate()),
			describeStreamParams(achievedW, achievedH, achievedFPS)))
	}
	captureCtx, cancel := context.WithCancel(context.Background())
	rejoin := func(ctx context.Context) (*deviceHub, int, chan *videoFrame, error) {
		return a.video.joinHub(ctx, src.key, &agentpb.StreamVideoRequest{DeviceId: devID})
	}
	c := &cameraCapture{source: source, session: session, dir: dir, hub: hub, subID: subID, frames: frames, rejoin: rejoin, index: index, mappingFile: mappings, ctx: captureCtx, cancel: cancel, done: make(chan struct{}), ready: make(chan error, 1), mode: mode, interval: interval.Nanoseconds(), rateCap: rateCap, notes: notes, lastSnapshotIdx: -1}
	go c.run()
	select {
	case err := <-c.ready:
		if err != nil {
			cancel()
			<-c.done
			return nil, err
		}
		return c, nil
	case <-ctx.Done():
		cancel()
		<-c.done
		return nil, ctx.Err()
	case <-time.After(20 * time.Second):
		cancel()
		<-c.done
		return nil, errors.New("timed out waiting for first encoded camera frame")
	}
}

// buildStreamRequest translates a campaign capture policy into stream
// parameters the video pipeline will accept. Caps that cannot be requested at
// the source are recorded as honesty notes and reconciled adapter-side (rate)
// or reported (resolution) instead of failing the episode.
func buildStreamRequest(devID uint32, src videoSource, capture *data.SourceCapture) (*agentpb.StreamVideoRequest, []string) {
	req := &agentpb.StreamVideoRequest{DeviceId: devID}
	if capture == nil {
		return req, nil
	}
	var notes []string
	if w, h, ok := capture.MaxResolutionPixels(); ok {
		if src.kind == sourceV4L2 {
			req.Width, req.Height = w, h
		} else {
			// On network cameras, width selects which pre-configured stream to
			// open rather than sizing frames, so the cap is not requestable.
			notes = append(notes, fmt.Sprintf("max_resolution %s cannot be requested from a network camera; recording its configured stream", capture.MaxResolution))
		}
	}
	if capture.Rate > 0 && src.kind == sourceV4L2 {
		req.Framerate = sourceFramerateAtOrBelow(capture.Rate)
	}
	if src.kind == sourceV4L2 && req.GetWidth() != 0 {
		// The framerate always comes from the accepted set, so a validation
		// failure here can only be the resolution.
		if err := validateStreamParams(src.path, req); err != nil {
			notes = append(notes, fmt.Sprintf("max_resolution %s is not a mode this camera advertises; recording at the stream default", capture.MaxResolution))
			req.Width, req.Height = 0, 0
		}
	}
	return req, notes
}

// sourceFramerateAtOrBelow maps a capture rate cap onto the discrete
// framerates the stream pipeline accepts (see validateStreamParams). Zero
// means the cap cannot be requested at the source; the adapter-side
// group-of-pictures gate still enforces it.
func sourceFramerateAtOrBelow(rate float64) uint32 {
	best := uint32(0)
	for _, fps := range []uint32{15, 24, 25, 30, 60, 90, 120} {
		if float64(fps) <= rate && fps > best {
			best = fps
		}
	}
	return best
}

func describeStreamParams(w, h, fps uint32) string {
	size := "default resolution"
	if w != 0 || h != 0 {
		size = fmt.Sprintf("%dx%d", w, h)
	}
	if fps != 0 {
		return fmt.Sprintf("%s at %d fps", size, fps)
	}
	return size + " at default rate"
}

// subscribeHub joins the device's frame hub with episode capture's parameter
// priority (see joinHubForCapture): explicit campaign parameters win over a
// hub running at defaults for parameter-less subscribers (the producer is
// restarted, reported via the restarted return), an explicit conflict with
// another explicit-parameter consumer is a named error rather than a silent
// downgrade, and a capture that asserts no parameters joins whatever runs and
// reports requested-vs-achieved.
func (a *cameraDataAdapter) subscribeHub(ctx context.Context, key string, req *agentpb.StreamVideoRequest) (*deviceHub, int, chan *videoFrame, uint32, uint32, uint32, bool, error) {
	return a.video.joinHubForCapture(ctx, key, req)
}

type cameraCaptureGroup struct {
	captures []*cameraCapture
	// refused carries the manifest entries of sources this episode did not
	// capture because another consumer held the camera at conflicting explicit
	// parameters. Delivered at Stop like every other capture result.
	refused []data.CaptureResult
}

func (g *cameraCaptureGroup) Stop(ctx context.Context) ([]data.CaptureResult, error) {
	results := append([]data.CaptureResult(nil), g.refused...)
	var errs []error
	for i := len(g.captures) - 1; i >= 0; i-- {
		r, err := g.captures[i].Stop(ctx)
		results = append(results, r...)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return results, errors.Join(errs...)
}

type cameraCapture struct {
	source  data.Source
	session data.CaptureSession
	dir     string
	hub     *deviceHub
	subID   int
	frames  chan *videoFrame
	// rejoin resubscribes to the device's hub, asserting no parameters. A
	// capture that itself asserted none is a parameter-less subscriber, so
	// when a concurrent episode's explicit campaign parameters restart the
	// producer (takeOverDefaultedHub) this capture reattaches to the restarted
	// stream instead of ending. A capture with explicit parameters never
	// observes such a restart: its own explicit subscription refuses the
	// takeover. Nil only in unit tests that drive a bare hub.
	rejoin func(ctx context.Context) (*deviceHub, int, chan *videoFrame, error)
	// carriedDrops accumulates subscriber-channel drops from hub
	// subscriptions this capture already left when reattaching.
	carriedDrops       uint64
	index, mappingFile *os.File
	segment            *os.File
	segmentRel         string
	segmentStart       int64
	segmentNumber      int
	segmentOffset      int64
	ctx                context.Context
	cancel             context.CancelFunc
	done               chan struct{}
	ready              chan error
	readyOnce          sync.Once
	stopOnce           sync.Once
	result             data.CaptureResult
	runErr             error
	lastSequence       uint32
	haveSequence       bool
	lastMapping        data.MonotonicMappingSample
	haveMapping        bool
	mappingNumber      int
	mappingStart       int64
	mappingMaxError    int64
	mappingSamples     uint64
	mappingSummaries   []data.ClockMapping
	maxCanonicalError  int64
	observedDomains    map[string]bool
	// mode is "continuous" or captureModeSnapshot; interval is the snapshot
	// period in nanoseconds; rateCap is the campaign's continuous-mode capture
	// rate cap in hertz (0 when uncapped). alignmentKnown records that the
	// first frame has classified the transport as access-unit aligned or
	// byte-chunked (see handleFrame).
	mode           string
	interval       int64
	rateCap        float64
	alignmentKnown bool
	// captureReceipt is a test seam over data.CaptureReceipt (see receiptNow).
	captureReceipt func() (int64, int64, int64, error)
	// notes accumulates honesty notes folded into the result's source detail.
	notes []string
	// Snapshot state: the interval index of the last written snapshot (-1
	// before the first) and intervals that produced no still.
	lastSnapshotIdx int64
	snapshotNumber  int
	missedIntervals uint64
	// Rate-cap state: receipts of the first and last written frames, and
	// whether the current group of pictures is being skipped. rateWritten counts
	// only frames the live loop wrote (not flushed pre-roll), so a campaign that
	// both pre-rolls and rate-caps enforces the cap over the live capture rather
	// than being thrown off by the pre-roll frames already on disk.
	rateStart     int64
	rateLast      int64
	rateWritten   uint64
	haveRateStart bool
	skipGOP       bool
	// preRoll holds the standby ring an armed campaign accumulated before the
	// trigger. run() replays it first, writing each buffered frame at its real
	// (negative-offset) capture time, before the live loop continues on the same
	// hub subscription. Empty for captures that were not armed.
	preRoll []bufferedCameraFrame
	// armed marks a capture activated from a standby ring: it is already
	// subscribed and producing, so run() reports ready once its pre-roll is
	// flushed rather than waiting for a first live frame.
	armed bool
}

func (c *cameraCapture) receiptNow() (int64, int64, int64, error) {
	if c.captureReceipt != nil {
		return c.captureReceipt()
	}
	return data.CaptureReceipt()
}

type cameraIndexRecord struct {
	// SampleID is the harness-wide identity of this frame within its source,
	// assigned by the producer hub. It is the join key between the frame a
	// model consumed (recorded in the episode's model-input ledger) and the
	// bytes this episode kept for it: given a sample_id from the ledger, the
	// index entry with the same sample_id names the segment file and byte
	// offset holding the payload. Absent (zero) only for frames produced
	// before the hub assigned identities, which cannot happen in production.
	SampleID                  uint64  `json:"sample_id,omitempty"`
	CanonicalEpisodeNanos     int64   `json:"canonical_episode_nanos"`
	CanonicalUncertaintyNanos int64   `json:"canonical_uncertainty_nanos"`
	NativeTimestampNanos      int64   `json:"native_timestamp_nanos,omitempty"`
	NativeClockDomain         string  `json:"native_clock_domain"`
	NativeTimestampFlags      uint32  `json:"native_timestamp_flags,omitempty"`
	AgentCaptureRealtimeNanos uint64  `json:"agent_capture_realtime_nanos,omitempty"`
	AgentReceiptBootNanos     int64   `json:"agent_receipt_boottime_nanos"`
	MappingSegment            string  `json:"mapping_segment"`
	Segment                   string  `json:"segment"`
	ByteOffset                int64   `json:"byte_offset"`
	ByteSize                  int     `json:"byte_size"`
	Codec                     string  `json:"codec"`
	Sequence                  *uint32 `json:"sequence,omitempty"`
}

func (c *cameraCapture) signalReady(err error) { c.readyOnce.Do(func() { c.ready <- err }) }

func (c *cameraCapture) run() {
	defer close(c.done)
	defer func() {
		c.result.SourceID = c.source.ID
		if len(c.observedDomains) == 1 {
			for domain := range c.observedDomains {
				c.result.ClockDomain = domain
			}
		} else if len(c.observedDomains) > 1 {
			c.result.ClockDomain = "MULTIPLE_NATIVE_DOMAINS/AGENT_RECEIPT"
		}
		if c.result.ClockDomain == "CLOCK_REALTIME_AGENT_CAPTURE" {
			c.result.ClockDomain = "GSTREAMER_PIPE/AGENT_RECEIPT"
			c.notes = append([]string{"pipeline PTS unavailable; canonical time uses bounded agent receipt"}, c.notes...)
		}
		subscriberDrops := c.hub.unsubscribe(c.subID) + c.carriedDrops
		end := int64(0)
		if _, receipt, _, err := c.receiptNow(); err == nil {
			end = receipt
		}
		if c.mode == captureModeSnapshot {
			// Discarding frames between intervals is the normal snapshot
			// workflow, so subscriber-channel drops are not data loss here;
			// the honest loss unit is an interval that produced no still.
			c.finishSnapshotAccounting(end)
		} else {
			drops := subscriberDrops
			if c.result.Drops != nil {
				drops += *c.result.Drops
			}
			c.result.Drops = &drops
			c.result.DropAccounting = "partial_known_driver_and_subscriber"
		}
		c.finishResultDetail(end)
		c.finishMapping(c.result.Count)
		c.result.Mappings = append([]data.ClockMapping(nil), c.mappingSummaries...)
		if c.maxCanonicalError > 0 {
			maxError := c.maxCanonicalError
			c.result.MappingError = &maxError
		}
		if c.segment != nil {
			_ = c.segment.Sync()
			_ = c.segment.Close()
		}
		_ = c.index.Sync()
		_ = c.mappingFile.Sync()
		_ = c.index.Close()
		_ = c.mappingFile.Close()
	}()

	// Flush the armed pre-roll ring first: these frames were captured BEFORE the
	// trigger, so they open the episode with real negative-offset canonical
	// times before the live loop takes over on the same subscription. An
	// undecodable leading frame (should not happen: the ring only retains from a
	// keyframe) is skipped rather than failing the episode.
	for i := range c.preRoll {
		if err := c.writeBufferedFrame(c.preRoll[i]); errors.Is(err, errAwaitCameraRandomAccess) {
			continue
		} else if err != nil {
			c.runErr = err
			c.signalReady(err)
			return
		}
	}
	c.preRoll = nil
	// The armed subscription was already producing while the ring filled, so the
	// capture is ready as soon as its pre-roll is on disk; the live loop then
	// continues. A never-armed capture leaves preRoll empty and signals ready on
	// its first live frame below, exactly as before.
	if c.armed {
		c.signalReady(nil)
	}

	for {
		select {
		case <-c.ctx.Done():
			c.signalReady(c.ctx.Err())
			return
		case frame, ok := <-c.frames:
			if !ok {
				if err := c.hub.terminalErr(); err != nil {
					c.runErr = err
					c.signalReady(err)
					return
				}
				if c.rejoin != nil && c.hub.wasRestarted() {
					if err := c.reattachAfterRestart(); err != nil {
						c.runErr = err
						c.signalReady(err)
						return
					}
					continue
				}
				c.runErr = errors.New("camera producer stopped")
				c.signalReady(c.runErr)
				return
			}
			if err := c.handleFrame(frame); errors.Is(err, errAwaitCameraRandomAccess) {
				continue
			} else if err != nil {
				c.runErr = err
				c.signalReady(err)
				return
			}
			c.signalReady(nil)
		}
	}
}

// reattachAfterRestart rejoins the device's hub after an explicit-parameter
// capture takeover replaced the producer this parameter-less capture was
// consuming. The old stream has ended; the new one must not be spliced into
// the current segment, because a segment holds one sequence parameter set and
// downstream reads it as one timeline. Closing the segment forces the next
// write through ensureSegment's random-access gate
// (errAwaitCameraRandomAccess), so the new stream starts a fresh,
// independently decodable segment. Driver sequence numbers and access-unit
// alignment are properties of the producer, so both classifications reset.
func (c *cameraCapture) reattachAfterRestart() error {
	c.carriedDrops += c.hub.unsubscribe(c.subID)
	hub, subID, frames, err := c.rejoin(c.ctx)
	if err != nil {
		return err
	}
	c.hub, c.subID, c.frames = hub, subID, frames
	if c.segment != nil {
		if err := c.segment.Sync(); err != nil {
			return err
		}
		if err := c.segment.Close(); err != nil {
			return err
		}
		c.segment = nil
	}
	c.haveSequence = false
	c.alignmentKnown = false
	c.notes = append(c.notes, "camera producer was restarted at another episode's explicit capture parameters; this parameter-less capture reattached and continues in a new segment on the new stream")
	return nil
}

// handleFrame dispatches one hub frame to the active capture mode.
//
// Frame-boundary-sensitive behavior (snapshot stills, GOP-granularity rate
// capping) requires access-unit-aligned frames, which only the native V4L2
// producer delivers; the GStreamer and IP camera producers emit arbitrary
// byte-stream chunks (see videoFrame.auAligned). A producer's alignment is a
// property of its pipeline and never changes mid-stream, so the first frame
// decides: on a chunked transport snapshot mode degrades to continuous
// recording and the rate cap is disabled, each with an honesty note that ends
// up in the manifest's source detail.
func (c *cameraCapture) handleFrame(frame *videoFrame) error {
	if !c.alignmentKnown {
		c.alignmentKnown = true
		if !frame.auAligned {
			if c.mode == captureModeSnapshot {
				c.mode = "continuous"
				c.notes = append(c.notes, "snapshot stills require a transport that delivers whole access units (native V4L2 H.264); this stream arrives as byte chunks, so the source records continuously instead")
			}
			if c.rateCap > 0 {
				c.notes = append(c.notes, fmt.Sprintf("rate cap %.3g Hz cannot be enforced at GOP granularity on this transport's byte-chunked stream without corrupting it; recording at the stream rate", c.rateCap))
				c.rateCap = 0
			}
		}
	}
	if c.mode == captureModeSnapshot {
		return c.writeSnapshot(frame)
	}
	return c.writeFrame(frame)
}

// canonicalTime stamps one frame on the canonical CLOCK_BOOTTIME episode
// timeline, maintaining the native-clock mapping segments as a side effect.
func (c *cameraCapture) canonicalTime(frame *videoFrame) (canonical, uncertainty int64, mappingID string, receipt int64, err error) {
	if c.observedDomains == nil {
		c.observedDomains = make(map[string]bool)
	}
	c.observedDomains[frame.nativeClock] = true
	before, receipt, after, err := c.receiptNow()
	if err != nil {
		return 0, 0, "", 0, err
	}
	canonical = receipt
	uncertainty = (after - before + 1) / 2
	mappingID = "receipt-bracket-v1"
	if frame.nativeClock == "CLOCK_MONOTONIC_V4L2" {
		if !c.haveMapping || receipt-c.lastMapping.BootAfterNanos >= int64(time.Second) {
			if err := c.sampleMapping(receipt); err != nil {
				return 0, 0, "", 0, err
			}
		}
		offset := c.lastMapping.OffsetLowerNanos + (c.lastMapping.OffsetUpperNanos-c.lastMapping.OffsetLowerNanos)/2
		mapped := frame.nativeNs + offset
		mapError := (c.lastMapping.OffsetUpperNanos - c.lastMapping.OffsetLowerNanos + 1) / 2
		// A wildly implausible driver timestamp is preserved but not promoted to
		// canonical time. Receipt order remains valid.
		if absCapture(mapped-receipt) <= int64(5*time.Second) {
			canonical = mapped
			uncertainty = mapError
			mappingID = fmt.Sprintf("monotonic-boottime-%d", c.mappingNumber)
			if mapError > c.mappingMaxError {
				c.mappingMaxError = mapError
			}
		}
	}
	if uncertainty > c.maxCanonicalError {
		c.maxCanonicalError = uncertainty
	}
	return canonical, uncertainty, mappingID, receipt, nil
}

// frameRandomAccess reports whether a frame can start decoding on its own, and
// the byte offset of the decodable unit. H.264 requires an SPS/PPS/IDR access
// unit; VP8 marks keyframes in the frame tag (bit 0 of the first byte is zero
// for keyframes). Other codecs are treated as independently decodable per
// frame, matching how the segmenter already treats them.
func frameRandomAccess(frame *videoFrame) (int, bool) {
	switch frame.codec {
	case agentpb.VideoCodec_VIDEO_CODEC_H264:
		return h264RandomAccessOffset(frame.data)
	case agentpb.VideoCodec_VIDEO_CODEC_VP8:
		return 0, len(frame.data) > 0 && frame.data[0]&0x01 == 0
	default:
		return 0, len(frame.data) > 0
	}
}

// gateGOP applies the campaign's capture rate cap adapter-side by admitting or
// skipping whole groups of pictures (GOPs). H.264 inter frames reference
// earlier frames, so dropping below GOP granularity would corrupt the stream:
// the cap is therefore best-effort at GOP granularity, and the achieved rate
// is recorded honestly in the capture result (see finishResultDetail).
// Intentionally skipped GOPs are not counted as drops.
//
// The admit/skip decision toggles only on whole random-access units: the cap
// is disabled outright on byte-chunked transports (see handleFrame), and a
// frame truncated by the maxFrameBytes cap keeps the current decision, since
// its parsed prefix does not prove a GOP boundary.
func (c *cameraCapture) gateGOP(frame *videoFrame, receipt int64) (skip bool) {
	if _, randomAccess := frameRandomAccess(frame); randomAccess && frame.auAligned {
		if c.rateWritten == 0 {
			// Always admit the first live GOP so the capture starts promptly.
			// Pre-roll frames already on disk are counted separately and never
			// gate the live cap.
			c.skipGOP = false
		} else {
			elapsed := float64(receipt-c.rateStart) / float64(time.Second)
			c.skipGOP = elapsed <= 0 || float64(c.rateWritten) > c.rateCap*elapsed
		}
	}
	return c.skipGOP
}

func (c *cameraCapture) writeFrame(frame *videoFrame) error {
	canonical, uncertainty, mappingID, receipt, err := c.canonicalTime(frame)
	if err != nil {
		return err
	}
	if c.rateCap > 0 && c.gateGOP(frame, receipt) {
		return nil
	}
	if err := c.writeEncodedFrame(frame, canonical, uncertainty, mappingID, receipt, false); err != nil {
		return err
	}
	if !c.haveRateStart {
		c.rateStart, c.haveRateStart = receipt, true
	}
	c.rateWritten++
	c.rateLast = receipt
	return nil
}

// writeBufferedFrame replays one frame from an armed campaign's standby ring.
// Unlike a live frame it is stamped with the bracketed CLOCK_BOOTTIME receipt
// the hub took when it broadcast the frame (frame.receiptBootNanos), not a
// fresh receipt: the frame was captured seconds before this replay, so its real
// capture time is the only honest canonical time, and it lands at a negative
// episode offset. The native-monotonic refinement live capture applies is not
// available retroactively, so the receipt bracket is the canonical mapping,
// exactly as the sensor path reports the same frames to a model. The rate cap
// never applies to pre-roll: the ring's byte and window bounds already bound it.
func (c *cameraCapture) writeBufferedFrame(bf bufferedCameraFrame) error {
	frame := bf.frame
	if c.observedDomains == nil {
		c.observedDomains = make(map[string]bool)
	}
	c.observedDomains[frame.nativeClock] = true
	uncertainty := frame.receiptUncertaintyNanos
	if uncertainty > c.maxCanonicalError {
		c.maxCanonicalError = uncertainty
	}
	return c.writeEncodedFrame(frame, frame.receiptBootNanos, uncertainty, "receipt-bracket-v1", frame.receiptBootNanos, bf.resetSegment)
}

// writeEncodedFrame is the shared write core for both live capture and pre-roll
// replay: it rotates the segment on random-access boundaries, appends the
// payload, records the auditable index entry that ties the harness sample_id to
// its byte range, and accounts the frame as captured. canonical is the frame's
// CLOCK_BOOTTIME episode time (negative for pre-roll); newStream forces a fresh
// segment before the write so a producer restart never splices two sequence
// parameter sets into one file.
func (c *cameraCapture) writeEncodedFrame(frame *videoFrame, canonical, uncertainty int64, mappingID string, receipt int64, newStream bool) error {
	if newStream && c.segment != nil {
		if err := c.segment.Sync(); err != nil {
			return err
		}
		if err := c.segment.Close(); err != nil {
			return err
		}
		c.segment = nil
		c.haveSequence = false
	}
	trim, err := c.ensureSegment(frame.codec, canonical, frame.data)
	if err != nil {
		return err
	}
	payload := frame.data[trim:]
	offset := c.segmentOffset
	n, err := c.segment.Write(payload)
	if err != nil {
		return err
	}
	if n != len(payload) {
		return ioErrShortWrite(n, len(payload))
	}
	c.segmentOffset += int64(n)
	var seq *uint32
	if frame.sequenceValid {
		v := frame.sequence
		seq = &v
		if c.haveSequence && frame.sequence > c.lastSequence+1 {
			d := uint64(frame.sequence - c.lastSequence - 1)
			if c.result.Drops == nil {
				c.result.Drops = new(uint64)
			}
			*c.result.Drops += d
		}
		c.lastSequence, c.haveSequence = frame.sequence, true
	}
	record := cameraIndexRecord{SampleID: frame.sampleID, CanonicalEpisodeNanos: canonical - c.session.RequestBootNanos, CanonicalUncertaintyNanos: uncertainty, NativeTimestampNanos: frame.nativeNs, NativeClockDomain: frame.nativeClock, NativeTimestampFlags: frame.nativeFlags, AgentCaptureRealtimeNanos: frame.tsNs, AgentReceiptBootNanos: receipt, MappingSegment: mappingID, Segment: c.segmentRel, ByteOffset: offset, ByteSize: n, Codec: frame.codec.String(), Sequence: seq}
	b, _ := json.Marshal(record)
	if _, err := c.index.Write(append(b, '\n')); err != nil {
		return err
	}
	c.result.Count++
	if c.result.ActualOffset == nil {
		actual := canonical - c.session.RequestBootNanos
		c.result.ActualOffset = &actual
	}
	return nil
}

// writeSnapshot implements snapshot capture: one standalone decodable still
// per campaign interval.
//
// Format choice: snapshots are written as single H.264 random-access units
// (snapshot-<n>.h264) rather than JPEG, so keyframe stills need no new
// GStreamer JPEG pipeline; the episode file inventory already classifies
// .h264 payloads.
//
// Decodability rests on two checks, both required. handleFrame only routes
// access-unit-aligned frames here (native V4L2 H.264, one whole encoded frame
// per buffer — byte-chunked transports degrade to continuous recording), and
// frameRandomAccess then verifies the unit carries its own SPS/PPS/IDR trio.
// Together they guarantee a written still is a complete, self-contained
// access unit; a keyframe whose parameter sets the camera firmware did not
// inline is skipped and its interval is reported as missed rather than filled
// with an undecodable file.
//
// Between intervals the capture stays subscribed and discards frames instead
// of unsubscribing: receiving and dropping a shared frame pointer is a channel
// receive, while hub churn closes the device file descriptor, restarts the
// producer pipeline, and rejoining races hub teardown (bounded by
// maxHubRetries * hubTeardownTimeout) and then waits a full group of pictures
// for the next keyframe.
func (c *cameraCapture) writeSnapshot(frame *videoFrame) error {
	offset, randomAccess := frameRandomAccess(frame)
	// A frame truncated by the maxFrameBytes cap loses its alignment promise
	// mid-stream; it must not become a still even when its head parses as one.
	if !randomAccess || !frame.auAligned {
		if c.result.Count == 0 {
			return errAwaitCameraRandomAccess
		}
		return nil
	}
	canonical, uncertainty, mappingID, receipt, err := c.canonicalTime(frame)
	if err != nil {
		return err
	}
	idx := (receipt - c.session.RequestBootNanos) / c.interval
	if idx <= c.lastSnapshotIdx {
		// This interval already has its still; discard cheaply.
		return nil
	}
	// Intervals that elapsed without a decodable unit are honest losses.
	c.missedIntervals += uint64(idx - c.lastSnapshotIdx - 1)
	c.lastSnapshotIdx = idx
	c.snapshotNumber++
	ext := ".h264"
	if frame.codec == agentpb.VideoCodec_VIDEO_CODEC_VP8 {
		ext = ".webm"
	}
	name := fmt.Sprintf("snapshot-%06d%s", c.snapshotNumber, ext)
	f, err := os.OpenFile(filepath.Join(c.dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	payload := frame.data[offset:]
	n, err := f.Write(payload)
	if err == nil && n != len(payload) {
		err = ioErrShortWrite(n, len(payload))
	}
	if syncErr := f.Sync(); err == nil {
		err = syncErr
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	var seq *uint32
	if frame.sequenceValid {
		v := frame.sequence
		seq = &v
	}
	rel := filepath.ToSlash(filepath.Join("cameras", safeCaptureName(c.source.ID), name))
	record := cameraIndexRecord{SampleID: frame.sampleID, CanonicalEpisodeNanos: canonical - c.session.RequestBootNanos, CanonicalUncertaintyNanos: uncertainty, NativeTimestampNanos: frame.nativeNs, NativeClockDomain: frame.nativeClock, NativeTimestampFlags: frame.nativeFlags, AgentCaptureRealtimeNanos: frame.tsNs, AgentReceiptBootNanos: receipt, MappingSegment: mappingID, Segment: rel, ByteOffset: 0, ByteSize: n, Codec: frame.codec.String(), Sequence: seq}
	b, _ := json.Marshal(record)
	if _, err := c.index.Write(append(b, '\n')); err != nil {
		return err
	}
	c.result.Count++
	if c.result.ActualOffset == nil {
		actual := canonical - c.session.RequestBootNanos
		c.result.ActualOffset = &actual
	}
	return nil
}

// finishSnapshotAccounting reports missed snapshot intervals as the capture's
// drop count: every fully elapsed interval that produced no still, including
// the tail after the last snapshot up to end (a receipt on CLOCK_BOOTTIME;
// zero skips the tail when no end receipt is available).
func (c *cameraCapture) finishSnapshotAccounting(end int64) {
	if c.interval > 0 && end > c.session.RequestBootNanos {
		endIdx := (end - c.session.RequestBootNanos) / c.interval
		if tail := endIdx - 1 - c.lastSnapshotIdx; tail > 0 {
			c.missedIntervals += uint64(tail)
		}
	}
	missed := c.missedIntervals
	c.result.Drops = &missed
	c.result.DropAccounting = "missed_snapshot_intervals"
}

// finishResultDetail folds accumulated honesty notes, including the achieved
// rate under a capture rate cap, into the reported source detail.
//
// The achieved rate is computed over the span from the first written frame to
// the capture's end receipt, not to the last written frame: frames arrive in
// GOP bursts, so a capture that admitted a single GOP and then skipped to the
// end would otherwise report the encoder's intra-GOP rate (say 30 Hz) rather
// than the delivered rate the cap actually produced.
func (c *cameraCapture) finishResultDetail(end int64) {
	if c.rateCap > 0 {
		note := fmt.Sprintf("rate cap %.3g Hz applied at GOP granularity (best effort)", c.rateCap)
		span := end - c.rateStart
		if end == 0 || span <= 0 {
			// No end receipt: fall back to the written-frame span.
			span = c.rateLast - c.rateStart
		}
		if c.rateWritten > 0 && span > 0 {
			note += fmt.Sprintf("; achieved %.3g Hz", float64(c.rateWritten)/(float64(span)/float64(time.Second)))
		}
		c.notes = append(c.notes, note)
	}
	if len(c.notes) > 0 {
		c.result.SourceDetail = strings.TrimSpace(c.source.Detail + " (" + strings.Join(c.notes, "; ") + ")")
	}
}

func (c *cameraCapture) ensureSegment(codec agentpb.VideoCodec, canonical int64, payload []byte) (int, error) {
	randomAccessOffset, randomAccess := h264RandomAccessOffset(payload)
	if codec == agentpb.VideoCodec_VIDEO_CODEC_H264 && c.segment == nil && !randomAccess {
		return 0, errAwaitCameraRandomAccess
	}
	rotate := c.segment == nil || (codec == agentpb.VideoCodec_VIDEO_CODEC_H264 && canonical-c.segmentStart >= int64(cameraSegmentDuration) && randomAccess)
	if !rotate {
		return 0, nil
	}
	if c.segment != nil {
		if err := c.segment.Sync(); err != nil {
			return 0, err
		}
		if err := c.segment.Close(); err != nil {
			return 0, err
		}
	}
	c.segmentNumber++
	ext := ".h264"
	if codec == agentpb.VideoCodec_VIDEO_CODEC_VP8 {
		ext = ".webm"
	}
	name := fmt.Sprintf("segment-%06d%s", c.segmentNumber, ext)
	f, err := os.OpenFile(filepath.Join(c.dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return 0, err
	}
	c.segment, c.segmentRel = f, filepath.ToSlash(filepath.Join("cameras", safeCaptureName(c.source.ID), name))
	c.segmentStart, c.segmentOffset = canonical, 0
	if codec == agentpb.VideoCodec_VIDEO_CODEC_H264 {
		return randomAccessOffset, nil
	}
	return 0, nil
}

// h264RandomAccessOffset reports an Annex-B payload position where SPS, PPS,
// and IDR NALs appear in order. The GStreamer pipelines emit this trio at
// every keyframe (h264parse config-interval=-1) and UVC H.264 firmware
// commonly inlines it too, so starting segments only here makes each file
// begin independently decodable. Note this inspects only the bytes it is
// given: on a byte-chunked transport a reported unit may still be truncated
// at the payload's end, which is why snapshot stills additionally require an
// access-unit-aligned frame (see handleFrame).
func h264RandomAccessOffset(payload []byte) (offset int, found bool) {
	spsOffset, ppsOffset := -1, -1
	for i := 0; i+3 < len(payload); {
		start, prefix := -1, 0
		for ; i+3 < len(payload); i++ {
			if payload[i] == 0 && payload[i+1] == 0 && payload[i+2] == 1 {
				start, prefix = i, 3
				break
			}
			if i+4 < len(payload) && payload[i] == 0 && payload[i+1] == 0 && payload[i+2] == 0 && payload[i+3] == 1 {
				start, prefix = i, 4
				break
			}
		}
		if start < 0 || start+prefix >= len(payload) {
			break
		}
		switch payload[start+prefix] & 0x1f {
		case 5:
			if spsOffset >= 0 && ppsOffset >= spsOffset {
				offset, found = spsOffset, true
			}
		case 7:
			spsOffset, ppsOffset = start, -1
		case 8:
			if spsOffset >= 0 {
				ppsOffset = start
			}
		}
		i = start + prefix + 1
	}
	return offset, found
}

func (c *cameraCapture) sampleMapping(receipt int64) error {
	sample, err := data.SampleMonotonicMapping()
	if err != nil {
		return err
	}
	mid := sample.OffsetLowerNanos + (sample.OffsetUpperNanos-sample.OffsetLowerNanos)/2
	newSegment := !c.haveMapping
	if c.haveMapping {
		oldMid := c.lastMapping.OffsetLowerNanos + (c.lastMapping.OffsetUpperNanos-c.lastMapping.OffsetLowerNanos)/2
		if absCapture(mid-oldMid) > int64(time.Millisecond) {
			c.finishMapping(c.result.Count)
			c.result.Discontinuities++
			c.mappingStart = receipt - c.session.RequestBootNanos
			newSegment = true
		}
	}
	if !c.haveMapping {
		c.mappingStart = receipt - c.session.RequestBootNanos
	}
	if newSegment {
		c.mappingNumber++
	}
	c.haveMapping = true
	c.lastMapping = sample
	c.mappingSamples++
	b, _ := json.Marshal(struct {
		EpisodeNanos   int64                       `json:"episode_nanos"`
		MappingSegment string                      `json:"mapping_segment"`
		Sample         data.MonotonicMappingSample `json:"sample"`
	}{receipt - c.session.RequestBootNanos, fmt.Sprintf("monotonic-boottime-%d", c.mappingNumber), sample})
	_, err = c.mappingFile.Write(append(b, '\n'))
	return err
}

func (c *cameraCapture) finishMapping(_ uint64) {
	if !c.haveMapping || c.mappingSamples == 0 {
		return
	}
	end, _, _, _ := data.CaptureReceipt()
	c.mappingSummaries = append(c.mappingSummaries, data.ClockMapping{ID: fmt.Sprintf("monotonic-boottime-%d", c.mappingNumber), SourceClockDomain: "CLOCK_MONOTONIC_V4L2", CanonicalClock: "CLOCK_BOOTTIME", StartedEpisodeNS: c.mappingStart, EndedEpisodeNS: end - c.session.RequestBootNanos, MaxErrorNanos: c.mappingMaxError, Samples: c.mappingSamples, Algorithm: "boottime-monotonic-sandwich-v1"})
	c.mappingSamples = 0
	c.mappingMaxError = 0
}

func (c *cameraCapture) Stop(context.Context) ([]data.CaptureResult, error) {
	c.stopOnce.Do(func() { c.cancel(); <-c.done })
	return []data.CaptureResult{c.result}, c.runErr
}

func safeCaptureName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '_'
	}, s)
}

func absCapture(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
func ioErrShortWrite(got, want int) error {
	return fmt.Errorf("short media write: wrote %d of %d bytes", got, want)
}
