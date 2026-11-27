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

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
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
	// monotonicMapping is a test seam over data.SampleMonotonicMapping, which
	// only Linux implements.
	monotonicMapping func() (data.MonotonicMappingSample, error)
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
			// listCameras lists only paired network cameras, which have a
			// stored login, so being offline is the one reason left to report.
			healthy := dev.GetOnline()
			detail := strings.TrimSpace(dev.GetName() + " " + dev.GetAddress())
			reason := ""
			if !healthy {
				detail += " (offline)"
				reason = "camera offline"
			}
			out = append(out, data.Source{ID: fmt.Sprintf("ipcamera:%d", dev.GetId()), Kind: "camera", ClockDomain: "IP_CAMERA_STREAM/AGENT_RECEIPT", Healthy: healthy, Detail: detail, Name: ipCameraSourceName(dev.GetName()), UnusableReason: reason})
			continue
		}
		domain := "V4L2_BUFFER_TIMESTAMP"
		if dev.GetTransport() == agentpb.VideoTransport_VIDEO_TRANSPORT_CSI {
			domain = "GSTREAMER_PIPELINE/AGENT_RECEIPT"
		}
		// Healthy is not a check here: listCameras lists a local camera only
		// after it answers a V4L2 capture query, so a listed camera is present,
		// and nothing tests whether it delivers a picture.
		out = append(out, data.Source{ID: "v4l2:" + dev.GetPath(), Kind: "camera", ClockDomain: domain, Healthy: true, Detail: strings.TrimSpace(dev.GetName() + " " + dev.GetTransport().String()), Name: cameraSourceName(dev.GetName(), dev.GetPath())})
	}
	return out
}

// cameraSourceName is the friendly name of a local camera. V4L2 card names are
// capped at 31 bytes, and uvcvideo fills them as "<product>: <product>", so a
// C920 reads "HD Pro Webcam C920: HD Pro Webc"; the truncated repeat after the
// colon is dropped.
func cameraSourceName(name, path string) string {
	name = strings.TrimSpace(name)
	if head, tail, ok := strings.Cut(name, ": "); ok && tail != "" && strings.HasPrefix(head, tail) {
		name = head
	}
	if name == "" {
		return path
	}
	return name
}

// ipCameraSourceName is the friendly name of a network camera: its model, or
// "Network camera" when it reported none.
func ipCameraSourceName(model string) string {
	model = strings.TrimSpace(model)
	if model == "" || model == "network camera" {
		return "Network camera"
	}
	return model
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
			if isCameraCaptureRefusal(err) {
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
		if isCameraCaptureRefusal(err) {
			// Another consumer holds this camera at explicitly requested
			// parameters that conflict with the campaign's, or apps share it
			// at another mode. Refusing this one source, with the named error
			// in its manifest entry, is the honest outcome: the episode
			// continues with its other sources and the manifest says exactly
			// who holds the camera and at what parameters, instead of
			// recording a capture the campaign did not ask for.
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
// consumer and keeps a keyframe-aligned ring of `buffer` seconds of frames, in
// each form the campaign stores, without writing an episode. Snapshot-mode
// sources are not armed:
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
		if !armsStandbyRing(source) {
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
		primaryKind, nativeBeside := subscriptionKindsFor(source.Capture)
		as := &armedCameraSource{
			video: a.video, source: source, key: src.key, devID: devID, buffer: buffer,
			primaryKind: primaryKind, wantsNativeBeside: nativeBeside, monotonicMapping: a.monotonicMapping,
			ring: &cameraPreRollRing{buffer: buffer}, done: make(chan struct{}),
		}
		// A parameter-less request: arming must never take a running stream from
		// a viewer, and must never register an explicit holder that would let it
		// force the parameter-precedence takeover.
		if err := as.join(context.Background()); err != nil {
			continue
		}
		as.alive = true
		ctx, cancel := context.WithCancel(context.Background())
		as.cancel = cancel
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
	var notes []string
	capture := source.Capture
	interval := snapshotInterval(capture)
	if interval > 0 {
		mode = captureModeSnapshot
		notes = append(notes, fmt.Sprintf("snapshot mode: one still per %s", interval))
	} else if capture.EffectiveMode() == captureModeSnapshot {
		notes = append(notes, "snapshot mode requested without a valid interval; recording continuously")
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
		return nil, errors.Join(err, index.Close())
	}
	primaryKind, nativeBeside := subscriptionKindsFor(capture)
	if mode == captureModeSnapshot && a.video.ownFramesAreKnownToBeVideo(src.key, req) {
		primaryKind = encodedFrames
	}
	var achievedW, achievedH, achievedFPS uint32
	var restarted bool
	hub, subID, frames, primaryKind, ownFramesRefusal, err := joinWithVideoFallback(primaryKind, a.video.refusingOwnFramesKnownAbsent(src.key, req, func(kind frameKind) (hub *deviceHub, id int, ch chan *videoFrame, err error) {
		hub, id, ch, achievedW, achievedH, achievedFPS, restarted, err = a.subscribeHub(ctx, src.key, req, kind, capture.EncodePreset())
		return hub, id, ch, err
	}))
	if err != nil {
		return nil, errors.Join(err, index.Close(), mappings.Close())
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
	rejoin := func(ctx context.Context, kind frameKind) (*deviceHub, int, chan *videoFrame, error) {
		atDefaults := &agentpb.StreamVideoRequest{DeviceId: devID}
		return a.video.refusingOwnFramesKnownAbsent(src.key, atDefaults, func(kind frameKind) (*deviceHub, int, chan *videoFrame, error) {
			return a.video.joinHubKind(ctx, src.key, atDefaults, kind, capture.EncodePreset())
		})(kind)
	}
	c := &cameraCapture{source: source, session: session, dir: dir, hub: hub, subID: subID, frames: frames, primaryKind: primaryKind, wantsNativeBeside: nativeBeside, monotonicMapping: a.monotonicMapping, rejoin: rejoin, logger: a.video.logger, index: index, mappingFile: mappings, ctx: captureCtx, cancel: cancel, done: make(chan struct{}), ready: make(chan error, 1), mode: mode, interval: interval.Nanoseconds(), rateCap: rateCap, notes: notes, lastSnapshotIdx: -1,
		presetNotApplied: a.video.whyPresetNotApplied(devID, capture)}
	if ownFramesRefusal != nil {
		c.ownFramesNoLongerStored(ownFramesRefusal)
	}
	c.joinNativeBeside()
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
		return nil, errors.New("timed out waiting for the first camera frame")
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
// means the cap cannot be requested at the source; the adapter-side gates
// (gateGOP for encoded video, perFrameRateCap for a camera's own frames) still
// enforce it.
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

// subscribeHub joins the device's frame hub, for frames of the given kind, with
// episode capture's parameter priority (see joinHubForCapture): explicit
// campaign parameters win over a hub running at defaults for parameter-less
// subscribers (the producer is restarted, reported via the restarted return),
// an explicit conflict with another explicit-parameter consumer is a named
// error rather than a silent downgrade, and a capture that asserts no
// parameters joins whatever runs and reports requested-vs-achieved. preset is
// what the source's capture block states for the encoder.
func (a *cameraDataAdapter) subscribeHub(ctx context.Context, key string, req *agentpb.StreamVideoRequest, kind frameKind, preset appconfig.EncodePreset) (*deviceHub, int, chan *videoFrame, uint32, uint32, uint32, bool, error) {
	return a.video.joinHubForCapture(ctx, key, req, kind, preset)
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
	source            data.Source
	session           data.CaptureSession
	dir               string
	hub               *deviceHub
	subID             int
	frames            chan *videoFrame
	primaryKind       frameKind
	wantsNativeBeside bool
	nativeSubID       int
	nativeFrames      chan *videoFrame
	ownFramesAreVideo bool
	ownFramesCodec    agentpb.VideoCodec
	ownFramesRefused  bool
	// rejoin resubscribes to the device's hub, asserting no parameters. A
	// capture that itself asserted none is a parameter-less subscriber, so
	// when a concurrent episode's explicit campaign parameters restart the
	// producer (takeOverDefaultedHub) this capture reattaches to the restarted
	// stream instead of ending. A capture with explicit parameters never
	// observes such a restart: its own explicit subscription refuses the
	// takeover. Nil only in unit tests that drive a bare hub.
	rejoin func(ctx context.Context, kind frameKind) (*deviceHub, int, chan *videoFrame, error)
	// carriedDrops accumulates subscriber-channel drops from hub
	// subscriptions this capture already left when reattaching.
	carriedDrops uint64
	// dropBaseline is the hub drop total already accrued on the subscription
	// this capture inherited, at the moment it inherited it. Only an armed
	// capture has one: its subscription was delivering throughout the armed
	// period, and those drops belong to the armed period rather than to the
	// episode. Subtracted from the final total in run's teardown.
	dropBaseline uint64
	// armedDrops is that same baseline, reported in the manifest as the
	// source's armed-period drops so the number is separated rather than lost.
	armedDrops uint64
	// deathRejoins counts recoveries from a producer that exited under this
	// capture, bounding how often one episode chases a failing camera.
	deathRejoins int
	// logger is the video service's logger, so a mid-episode failure is said
	// when it happens rather than only appearing in the manifest at Stop. Nil in
	// unit tests that drive a bare capture.
	logger             *zap.Logger
	index, mappingFile *os.File
	cameraSegmentStream
	ownFrameStream     cameraSegmentStream
	ownFrameRate       perFrameRateCap
	videoRateGated     bool
	refusedCodecs      map[agentpb.VideoCodec]uint64
	sampleTimes        sampleTimes
	lastStoredReceipt  int64
	nothingStoredSince int64
	ctx                context.Context
	cancel             context.CancelFunc
	done               chan struct{}
	ready              chan error
	readyOnce          sync.Once
	stopOnce           sync.Once
	result             data.CaptureResult
	runErr             error
	// lastSampleID is the hub identity of the previous VP8 frame this capture
	// handled, written or not; a jump in it is a loss (see sampleGap).
	// webmResync is set from such a loss until the next segment opens, and
	// webmBreaks counts the segments ended that way.
	lastSampleID uint64
	webmResync   bool
	webmBreaks   int
	// sawSequence records that this capture observed at least one frame carrying
	// a valid producer sequence number, at any point in its life. Unlike
	// haveSequence it is never reset by a producer restart: it decides which of
	// the two loss accountings the capture result reports (see run's teardown).
	sawSequence       bool
	lastMapping       data.MonotonicMappingSample
	haveMapping       bool
	mappingNumber     int
	mappingStart      int64
	mappingMaxError   int64
	mappingSamples    uint64
	mappingSummaries  []data.ClockMapping
	maxCanonicalError int64
	observedDomains   map[string]bool
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
	// monotonicMapping is the same over data.SampleMonotonicMapping.
	monotonicMapping func() (data.MonotonicMappingSample, error)
	// notes accumulates honesty notes folded into the result's source detail.
	notes []string
	// videoStoredAt counts the video frames stored by the preset the agent's
	// encoder made each at (videoFrame.encodedAt), and lastVideoAt is that of
	// the last one taken. awaitNewEncoderSegment is set from a video frame made
	// at another preset than the one before it until a frame that decoding can
	// start on opens a segment for that encoder's video; the frames between
	// decode from neither file and are left out.
	videoStoredAt          map[appconfig.EncodePreset]uint64
	lastVideoAt            appconfig.EncodePreset
	awaitNewEncoderSegment bool
	// presetNotApplied is why this device does not apply the preset the
	// source states to the camera's video (VideoService.forecastVideoOf).
	presetNotApplied string
	// Snapshot state: the interval index of the last written snapshot (-1
	// before the first) and intervals that produced no still.
	lastSnapshotIdx int64
	snapshotNumber  int
	missedIntervals uint64
	// stillsAwaitVideo is set from the moment a source that stored stills goes
	// on recording continuously until its first video frame is stored.
	stillsAwaitVideo bool
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
	// preRollFlushHook is a test seam over the pre-roll write loop, in the
	// style of captureReceipt above: it is called after each buffered frame is
	// written, so a test can make the flush slow enough to prove the live
	// frames arriving during it are not lost.
	preRollFlushHook func()
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
		// One loss model, three cases, and each branch below reports under
		// exactly one of them:
		//
		//   snapshot   the loss unit is an interval that produced no still.
		//              Discarding frames between intervals is the workflow, so
		//              subscriber-channel drops are not loss here at all.
		//   sequenced  the producer's own frame counter is the single witness.
		//              A frame the hub dropped for this subscriber also leaves
		//              a gap in that counter, so adding the subscriber total on
		//              top would report the same lost frame twice.
		//   neither    the transport carries no sequence numbers, so a hub-side
		//              drop leaves no trace in the stream and the subscriber
		//              counter is the only witness there is.
		//
		// subscriberDrops is accumulated for every capture but reported only in
		// the third case. The armed-period baseline is netted out of it once,
		// here, rather than at each use, so that the episode's drops and the
		// armed period's drops can never both claim the same frame nor both
		// disclaim it.
		c.carriedDrops += c.leaveNativeBeside()
		subscriberDrops := c.hub.unsubscribe(c.subID) + c.carriedDrops
		// An armed capture inherited a subscription that had been delivering
		// since the campaign armed, so its hub drop counter already held the
		// armed period's losses when the trigger arrived. Those frames were
		// never candidates for this episode (the ring keeps only the last
		// `buffer` seconds), so they are reported separately instead of being
		// folded into the episode's own drop figure. Losses inside the
		// pre-roll window do reach Drops, through the sequence gaps between
		// the frames the ring retained; that is the sequenced case above, and
		// it is why netting the baseline out here loses nothing.
		subscriberDrops = saturatingSub(subscriberDrops, c.dropBaseline)
		if c.armed {
			armed := c.armedDrops
			c.result.ArmedDrops = &armed
		}
		end := int64(0)
		if _, receipt, _, err := c.receiptNow(); err == nil {
			end = receipt
		}
		if c.stillsAwaitVideo {
			c.closeStillsAccount(end, false)
		}
		if c.mode == captureModeSnapshot {
			// Discarding frames between intervals is the normal snapshot
			// workflow, so subscriber-channel drops are not data loss here;
			// the honest loss unit is an interval that produced no still.
			c.finishSnapshotAccounting(end)
		} else if c.sawSequence {
			// The producer's own sequence counter already covers a frame the hub
			// dropped for this subscriber: the frame was produced, it never
			// reached the capture, and the next frame that did arrives with a
			// gap in its sequence. Adding the hub's subscriber-drop counter on
			// top counted every such frame twice, so a camera that dropped five
			// frames reported ten. When sequences are available they are the
			// single loss count, and the accounting label says so.
			drops := uint64(0)
			if c.result.Drops != nil {
				drops = *c.result.Drops
			}
			c.result.Drops = &drops
			c.result.DropAccounting = "driver_sequence_gaps_include_subscriber_drops"
		} else {
			// No sequence numbers from this transport, so a hub-side drop leaves
			// no trace in the stream: the subscriber counter is the only loss
			// signal there is, and it sees only what the hub itself discarded.
			drops := subscriberDrops
			c.result.DropAccounting = "subscriber_drops_only_no_source_sequence"
			if c.result.Drops != nil {
				// Sequence gaps counted before the recorded stream changed to
				// one without sequence numbers (see recordedStreamChanged).
				drops += *c.result.Drops
				c.result.DropAccounting = "driver_sequence_gaps_then_subscriber_drops"
			}
			c.result.Drops = &drops
		}
		if c.mode != captureModeSnapshot && c.snapshotNumber > 0 {
			*c.result.Drops += c.missedIntervals
			c.result.DropAccounting = "missed_snapshot_intervals_then_" + c.result.DropAccounting
		}
		c.finishResultDetail(end)
		c.finishMapping(c.result.Count)
		c.result.Mappings = append([]data.ClockMapping(nil), c.mappingSummaries...)
		if c.maxCanonicalError > 0 {
			maxError := c.maxCanonicalError
			c.result.MappingError = &maxError
		}
		_ = c.closeSegment()
		_ = c.ownFrameStream.closeSegment()
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
	if len(c.preRoll) > 0 {
		// The flush is disk work (MkdirAll, OpenFile, per-segment Sync) taking
		// far longer than a frame interval, and the subscriber channel it is
		// not reading during that time is four frames deep. Draining it into a
		// bounded side buffer from a helper goroutine is what keeps the frames
		// immediately after the trigger, which are the most interesting ones in
		// the episode, out of the hub's drop counter.
		drain, err := c.drainDuring(c.flushPreRoll)
		if err != nil {
			c.runErr = err
			c.signalReady(err)
			return
		}
		c.carriedDrops += drain.dropped
		c.awaitLiveSegmentReset = true
		c.ownFrameStream.awaitLiveSegmentReset = true
		// The armed subscription was already producing while the ring filled,
		// so the capture is ready as soon as its pre-roll is on disk; the live
		// loop then continues.
		if c.armed {
			c.signalReady(nil)
		}
		for _, drained := range drain.frames {
			take := c.takePrimaryFrame
			if drained.nativeBeside {
				take = c.takeNativeBesideFrame
			}
			if err := take(drained.frame); errors.Is(err, errAwaitCameraRandomAccess) {
				continue
			} else if err != nil {
				c.runErr = err
				c.signalReady(err)
				return
			}
		}
	} else if c.armed {
		// An armed capture whose ring held nothing still reports ready here
		// rather than waiting for a first live frame.
		c.signalReady(nil)
	}

	for {
		select {
		case <-c.ctx.Done():
			c.signalReady(c.ctx.Err())
			return
		case frame, ok := <-c.nativeFrames:
			if !ok {
				c.nativeBesideEnded()
				continue
			}
			if err := c.takeNativeBesideFrame(frame); errors.Is(err, errAwaitCameraRandomAccess) {
				continue
			} else if err != nil {
				c.runErr = err
				c.signalReady(err)
				return
			}
		case frame, ok := <-c.frames:
			if !ok {
				if refusal := c.hub.subscriberErr(c.subID); c.primaryKind == nativeFrames && isNativeRefusal(refusal) {
					if err := c.recordVideoInstead(refusal); err != nil {
						c.runErr = err
						c.signalReady(err)
						return
					}
					continue
				}
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
				// The producer died mid-episode: a GStreamer crash, a USB
				// disconnect, a camera that faulted. Say so at the moment it
				// happens rather than letting the episode carry on marked
				// "recording" with no camera data until Stop, and try to get the
				// camera back.
				c.warn("camera capture: producer stopped mid-episode",
					zap.String("source", c.source.ID),
					zap.Uint64("frames_captured", c.result.Count))
				if c.rejoin == nil || c.deathRejoins >= maxCameraProducerRejoins {
					c.runErr = errors.New("camera producer stopped")
					c.signalReady(c.runErr)
					return
				}
				if err := c.rejoinAfterProducerDeath(); err != nil {
					c.warn("camera capture: producer could not be rejoined; the source is failed for the rest of the episode",
						zap.String("source", c.source.ID), zap.Error(err))
					c.runErr = fmt.Errorf("camera producer stopped and could not be rejoined: %w", err)
					c.signalReady(c.runErr)
					return
				}
				continue
			}
			if err := c.takePrimaryFrame(frame); errors.Is(err, errAwaitCameraRandomAccess) {
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

// Bounds on recovering from a producer that died mid-episode. Three attempts
// half a second apart covers a GStreamer pipeline respawn and a camera that
// re-enumerates, without turning a genuinely unplugged camera into a capture
// that spins for the rest of the episode.
const (
	maxCameraProducerRejoins = 3
	cameraProducerRejoinWait = 500 * time.Millisecond
)

// warn logs one line about this capture, if the adapter was given a logger.
// Unit tests that drive a bare cameraCapture leave it nil.
func (c *cameraCapture) warn(msg string, fields ...zap.Field) {
	if c.logger == nil {
		return
	}
	c.logger.Warn(msg, fields...)
}

// rejoinAfterProducerDeath tries to get the camera back after its producer
// exited under the capture.
//
// What it can and cannot say honestly is the whole point. Rejoining starts a
// NEW stream, so the recording has a real discontinuity there and the next write
// opens a fresh segment. How many frames the gap swallowed is not knowable: the
// producer is gone, its sequence counter restarts, and nothing counted what the
// camera would have delivered. So the note records the wall time of the gap,
// which is knowable, and says the frame count is not, instead of publishing a
// zero that would read as "nothing was lost".
func (c *cameraCapture) rejoinAfterProducerDeath() error {
	_, start, _, _ := c.receiptNow()
	c.carriedDrops += c.leaveNativeBeside()
	c.carriedDrops += c.hub.unsubscribe(c.subID)
	c.deathRejoins++
	var lastErr error
	for attempt := 1; attempt <= maxCameraProducerRejoins; attempt++ {
		err := c.rejoinSubscriptions()
		if err == nil {
			// Sequence numbering and access-unit alignment are properties of the
			// producer, so both classifications reset with the new one.
			if err := c.endProducerStreams(); err != nil {
				return err
			}
			c.result.Discontinuities++
			_, end, _, _ := c.receiptNow()
			c.notes = append(c.notes, fmt.Sprintf(
				"camera producer stopped mid-episode and was rejoined after %s on attempt %d of %d; the recording has a discontinuity there and continues in a new segment, and the number of frames lost across the gap is not knowable because the producer that would have counted them is the one that died",
				time.Duration(end-start).Round(time.Millisecond), attempt, maxCameraProducerRejoins))
			c.warn("camera capture: rejoined the producer after it stopped",
				zap.String("source", c.source.ID), zap.Int("attempt", attempt),
				zap.Duration("gap", time.Duration(end-start)))
			return nil
		}
		lastErr = err
		select {
		case <-c.ctx.Done():
			return c.ctx.Err()
		case <-time.After(cameraProducerRejoinWait):
		}
	}
	return lastErr
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
	c.carriedDrops += c.leaveNativeBeside()
	c.carriedDrops += c.hub.unsubscribe(c.subID)
	if err := c.rejoinSubscriptions(); err != nil {
		return err
	}
	if err := c.endProducerStreams(); err != nil {
		return err
	}
	c.notes = append(c.notes, "camera producer was restarted at another episode's explicit capture parameters; this parameter-less capture reattached and continues in a new segment on the new stream")
	return nil
}

// handleFrame dispatches one hub frame to the active capture mode and to the
// stream its form is stored in. A frame in a codec cameraSegmentExtensions has
// no row for is counted and left out.
//
// Frame-boundary-sensitive behavior (snapshot stills, GOP-granularity rate
// capping) requires access-unit-aligned frames, which native V4L2 capture and
// the derived layer deliver; the GStreamer and IP camera producers emit
// arbitrary byte-stream chunks (see videoFrame.auAligned). A producer's
// alignment is a property of its pipeline and never changes mid-stream, so the
// first frame decides: on a chunked transport snapshot mode degrades to
// continuous recording and the rate cap is disabled, each with an honesty note
// that ends up in the manifest's source detail.
func (c *cameraCapture) handleFrame(frame *videoFrame) error {
	if _, storable := cameraSegmentExtensions[frame.codec]; !storable {
		c.refuseCodec(frame.codec)
		return nil
	}
	if !c.alignmentKnown {
		c.alignmentKnown = true
		if !frame.auAligned {
			if c.mode == captureModeSnapshot {
				c.mode = "continuous"
				c.notes = append(c.notes, "snapshot stills require a camera that delivers whole frames (one the agent captures natively); this stream arrives as byte chunks, so the source records continuously instead")
				c.stillsGiveWayToVideo()
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
	if isOwnFormatFrame(frame) {
		return c.writeOwnFrame(frame)
	}
	return c.writeFrame(frame)
}

// computeCanonicalTime stamps one frame on the canonical CLOCK_BOOTTIME episode
// timeline, maintaining the native-clock mapping segments as a side effect.
//
// The receipt bracket is the one the HUB took when it broadcast the frame
// (frame.receiptBootNanos), not a fresh read taken here. The hub stamps it once
// so that every consumer of a frame agrees on it, and the pre-roll replay path
// already uses it; reading the clock again in the adapter made the capture index
// and the model-input ledger publish two different receipts for the same
// sample_id, which is exactly the join those two records exist to support. A
// zero receipt means the hub's clock read failed (or a unit test drove the
// capture with a bare frame), and only then is a fresh read taken.
func (c *cameraCapture) computeCanonicalTime(frame *videoFrame) (canonical, uncertainty int64, mappingID string, receipt int64, err error) {
	if c.observedDomains == nil {
		c.observedDomains = make(map[string]bool)
	}
	c.observedDomains[frame.nativeClock] = true
	if frame.receiptBootNanos != 0 {
		receipt, uncertainty = frame.receiptBootNanos, frame.receiptUncertaintyNanos
	} else {
		var before, after int64
		before, receipt, after, err = c.receiptNow()
		if err != nil {
			return 0, 0, "", 0, err
		}
		uncertainty = (after - before + 1) / 2
	}
	canonical = receipt
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
// unit. A VP8 frame is a pipe read of one WebM container, so its own bytes say
// nothing: the unit is a Cluster opening on a key frame, which the hub found
// (see webmJoins), and it decodes only behind that join point's prefix. Any
// other codec reaches the capture only as a camera's own whole frame (see
// isOwnFormatFrame), which decodes alone.
func frameRandomAccess(frame *videoFrame) (int, bool) {
	switch frame.codec {
	case agentpb.VideoCodec_VIDEO_CODEC_H264:
		return h264RandomAccessOffset(frame.data)
	case agentpb.VideoCodec_VIDEO_CODEC_VP8:
		if frame.webmJoin == nil {
			return 0, false
		}
		return frame.webmJoin.offset, true
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

// trackSequence follows the producer's own frame counter and accumulates the
// gaps in it as driver-side losses. It must be called for EVERY frame the
// capture takes off the hub channel that carries a valid sequence, written or
// deliberately skipped, or a skipped run is later read as a gap.
func (c *cameraCapture) trackSequence(stream *cameraSegmentStream, frame *videoFrame) {
	if !frame.sequenceValid {
		return
	}
	c.sawSequence = true
	if stream.haveSequence && frame.sequence > stream.lastSequence+1 {
		d := uint64(frame.sequence - stream.lastSequence - 1)
		if c.result.Drops == nil {
			c.result.Drops = new(uint64)
		}
		*c.result.Drops += d
	}
	stream.lastSequence, stream.haveSequence = frame.sequence, true
}

func (c *cameraCapture) writeFrame(frame *videoFrame) error {
	canonical, uncertainty, mappingID, receipt, err := c.canonicalTime(frame)
	if err != nil {
		return err
	}
	stream := &c.cameraSegmentStream
	c.videoRateGated = c.videoRateGated || c.rateCap > 0
	if c.rateCap > 0 && c.gateGOP(frame, receipt) {
		// A group of pictures skipped to honour the rate cap is not a loss, but
		// its frames still advance the driver's sequence counter. Tracking them
		// here is what makes that true: the gate used to return before any
		// sequence bookkeeping, so the next admitted keyframe saw a sequence jump
		// spanning the whole skipped group and counted every deliberately
		// discarded frame as a driver drop.
		c.trackSequence(stream, frame)
		return nil
	}
	if err := c.writeEncodedFrame(stream, frame, canonical, uncertainty, mappingID, receipt, stream.opensLiveTailWith(frame)); err != nil {
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
	if _, storable := cameraSegmentExtensions[frame.codec]; !storable {
		c.refuseCodec(frame.codec)
		return nil
	}
	if c.observedDomains == nil {
		c.observedDomains = make(map[string]bool)
	}
	c.observedDomains[frame.nativeClock] = true
	uncertainty := frame.receiptUncertaintyNanos
	if uncertainty > c.maxCanonicalError {
		c.maxCanonicalError = uncertainty
	}
	if c.indexesSamplesTwice() && frame.sampleID != 0 {
		c.sampleTimes.keep(frame.sampleID, sampleTime{canonical: frame.receiptBootNanos, uncertainty: uncertainty, receipt: frame.receiptBootNanos, mappingID: "receipt-bracket-v1"})
	}
	return c.writeEncodedFrame(c.streamFor(frame), frame, frame.receiptBootNanos, uncertainty, "receipt-bracket-v1", frame.receiptBootNanos, bf.resetSegment)
}

// writeEncodedFrame is the shared write core for both live capture and pre-roll
// replay, into either stream: it rotates the segment on random-access
// boundaries, appends the payload, records the auditable index entry that ties
// the harness sample_id to its byte range, and accounts the frame as captured.
// canonical is the frame's CLOCK_BOOTTIME episode time (negative for
// pre-roll); newStream forces a fresh segment before the write so a producer
// restart never splices two sequence parameter sets into one file.
//
// A VP8 segment also ends wherever frames were lost on the way here. The stream
// is one WebM container, so the bytes after a loss cannot be appended: the file
// would carry a hole in the middle of a Cluster, which matroskademux does not
// get past. The frames that follow are discarded until ensureSegment can open
// the next file at a join point, and count as lost with the ones before them.
func (c *cameraCapture) writeEncodedFrame(stream *cameraSegmentStream, frame *videoFrame, canonical, uncertainty int64, mappingID string, receipt int64, newStream bool) error {
	if frame.codec == agentpb.VideoCodec_VIDEO_CODEC_VP8 {
		if stream.segment != nil && sampleGap(c.lastSampleID, frame.sampleID) {
			newStream, c.webmResync = true, true
			c.webmBreaks++
		}
		c.lastSampleID = frame.sampleID
	}
	video := stream == &c.cameraSegmentStream
	if video && c.encoderChangedAt(frame) {
		// The producer is the same and its numbering runs on, so the segment
		// ends without endProducerStream.
		if err := stream.closeSegment(); err != nil {
			return err
		}
		c.awaitNewEncoderSegment = true
	}
	if newStream && stream.segment != nil {
		if err := stream.endProducerStream(); err != nil {
			return err
		}
	}
	trim, err := c.ensureSegment(stream, frame, canonical)
	if err != nil {
		switch {
		case !errors.Is(err, errAwaitCameraRandomAccess):
		case c.webmResync:
			c.carriedDrops++
		case video && c.awaitNewEncoderSegment:
			c.countLeftOut(stream, frame)
		}
		return err
	}
	c.webmResync = false
	if video {
		c.awaitNewEncoderSegment = false
	}
	payload := frame.data[trim:]
	offset := stream.segmentOffset
	n, err := stream.segment.Write(payload)
	if err != nil {
		return err
	}
	if n != len(payload) {
		return ioErrShortWrite(n, len(payload))
	}
	stream.segmentOffset += int64(n)
	stream.indexed++
	if video {
		c.videoStoredAt[frame.encodedAt]++
	}
	if c.stillsAwaitVideo {
		c.closeStillsAccount(receipt, true)
	}
	if c.nothingStoredSince != 0 {
		c.noteStorageResumed(receipt)
	}
	c.lastStoredReceipt = receipt
	c.trackSequence(stream, frame)
	var seq *uint32
	if frame.sequenceValid {
		v := frame.sequence
		seq = &v
	}
	record := cameraIndexRecord{SampleID: frame.sampleID, CanonicalEpisodeNanos: canonical - c.session.RequestBootNanos, CanonicalUncertaintyNanos: uncertainty, NativeTimestampNanos: frame.nativeNs, NativeClockDomain: frame.nativeClock, NativeTimestampFlags: frame.nativeFlags, AgentCaptureRealtimeNanos: frame.tsNs, AgentReceiptBootNanos: receipt, MappingSegment: mappingID, Segment: stream.segmentRel, ByteOffset: offset, ByteSize: n, Codec: codecIndexName(frame.codec), Sequence: seq}
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
// The capture asks the hub for the camera's own frames (subscriptionKindsFor).
// A camera captured as JPEG delivers whole pictures, and each still is one of
// them, byte for byte; the source asks for no encoding. A camera whose own
// frames are H.264, or whose own frames are refused so that its video is taken
// instead, yields key frame stills.
//
// Decodability of an H.264 still rests on two checks, both required.
// handleFrame only routes access-unit-aligned frames here (byte-chunked
// transports degrade to continuous recording), and frameRandomAccess then
// verifies the unit carries its own SPS/PPS/IDR trio.
// Together they guarantee a written still is a complete, self-contained
// access unit; a keyframe that still lacks parameter sets (the camera did not
// inline them and the producer had none to add, see h264ParameterSets) is
// skipped and its interval is reported as missed rather than filled with an
// undecodable file.
//
// Between intervals the capture stays subscribed and discards frames instead
// of unsubscribing: receiving and dropping a shared frame pointer is a channel
// receive, while hub churn closes the device file descriptor, restarts the
// producer pipeline, and rejoining waits for the device to be released and
// then a full group of pictures for the next keyframe.
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
	name := fmt.Sprintf("snapshot-%06d%s", c.snapshotNumber, cameraStillExtension(frame.codec))
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
	record := cameraIndexRecord{SampleID: frame.sampleID, CanonicalEpisodeNanos: canonical - c.session.RequestBootNanos, CanonicalUncertaintyNanos: uncertainty, NativeTimestampNanos: frame.nativeNs, NativeClockDomain: frame.nativeClock, NativeTimestampFlags: frame.nativeFlags, AgentCaptureRealtimeNanos: frame.tsNs, AgentReceiptBootNanos: receipt, MappingSegment: mappingID, Segment: rel, ByteOffset: 0, ByteSize: n, Codec: codecIndexName(frame.codec), Sequence: seq}
	b, _ := json.Marshal(record)
	if _, err := c.index.Write(append(b, '\n')); err != nil {
		return err
	}
	c.result.Count++
	if c.result.ActualOffset == nil {
		actual := canonical - c.session.RequestBootNanos
		c.result.ActualOffset = &actual
	}
	if c.nothingStoredSince != 0 {
		c.noteStorageResumed(receipt)
	}
	c.lastStoredReceipt = receipt
	return nil
}

// countIntervalsMissedBefore adds every fully elapsed interval after the last
// still and before end, a receipt on CLOCK_BOOTTIME.
func (c *cameraCapture) countIntervalsMissedBefore(end int64) {
	if c.interval > 0 && end > c.session.RequestBootNanos {
		endIdx := (end - c.session.RequestBootNanos) / c.interval
		if tail := endIdx - 1 - c.lastSnapshotIdx; tail > 0 {
			c.missedIntervals += uint64(tail)
		}
	}
}

// finishSnapshotAccounting reports missed snapshot intervals as the capture's
// drop count: every fully elapsed interval that produced no still, including
// the tail after the last snapshot up to end (zero skips the tail when no end
// receipt is available).
func (c *cameraCapture) finishSnapshotAccounting(end int64) {
	c.countIntervalsMissedBefore(end)
	missed := c.missedIntervals
	c.result.Drops = &missed
	c.result.DropAccounting = "missed_snapshot_intervals"
}

// finishResultDetail folds accumulated honesty notes, including the achieved
// rate under a capture rate cap and what became of the campaign's store
// choice, into the reported source detail.
//
// The achieved rate is computed over the span from the first written frame to
// the capture's end receipt, not to the last written frame: frames arrive in
// GOP bursts, so a capture that admitted a single GOP and then skipped to the
// end would otherwise report the encoder's intra-GOP rate (say 30 Hz) rather
// than the delivered rate the cap actually produced.
func (c *cameraCapture) finishResultDetail(end int64) {
	if c.rateCap > 0 && (c.videoRateGated || !c.ownFrameRate.started) {
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
	if c.ownFrameRate.started {
		c.notes = append(c.notes, c.ownFrameRate.note())
	}
	if c.webmBreaks > 0 {
		c.notes = append(c.notes, fmt.Sprintf("reads of the WebM stream were lost before reaching this capture %d time(s); a WebM file cannot continue across missing bytes, so each time the recording resumed in a new segment at the next key frame, and the reads discarded on the way are counted as drops", c.webmBreaks))
	}
	c.notes = append(c.notes, c.storeNotes()...)
	if len(c.notes) > 0 {
		c.result.SourceDetail = strings.TrimSpace(c.source.Detail + " (" + strings.Join(c.notes, "; ") + ")")
	}
}

// ensureSegment makes sure the stream has a segment file open for the frame
// about to be written, and returns how many of the frame's leading bytes to
// leave out. A segment only opens on a random-access unit, so every file
// decodes from its first byte, and rotates on one every cameraSegmentDuration.
// The file's extension is the frame codec's row in cameraSegmentExtensions.
//
// A VP8 segment opens at a join point: the join prefix (the stream's
// initialization, and any of the Cluster's opening bytes from earlier reads)
// is written first, so the file is a WebM file whether or not the capture saw
// the stream begin. Those bytes belong to no sample and no index entry covers
// them; the opening frame's entry starts after them. VP8 is one container and
// does not rotate.
func (c *cameraCapture) ensureSegment(stream *cameraSegmentStream, frame *videoFrame, canonical int64) (int, error) {
	codec := frame.codec
	extension, storable := cameraSegmentExtensions[codec]
	if !storable {
		return 0, fmt.Errorf("no segment file format is defined for codec %s", codecIndexName(codec))
	}
	randomAccessOffset, randomAccess := frameRandomAccess(frame)
	if stream.segment == nil && !randomAccess {
		return 0, errAwaitCameraRandomAccess
	}
	rotates := codec != agentpb.VideoCodec_VIDEO_CODEC_VP8
	rotate := stream.segment == nil || (rotates && canonical-stream.segmentStart >= int64(cameraSegmentDuration) && randomAccess)
	if !rotate {
		return 0, nil
	}
	if err := stream.closeSegment(); err != nil {
		return 0, err
	}
	stream.segmentNumber++
	name := fmt.Sprintf("segment-%06d%s", stream.segmentNumber, extension)
	f, err := os.OpenFile(filepath.Join(c.dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return 0, err
	}
	stream.segment, stream.segmentRel = f, filepath.ToSlash(filepath.Join("cameras", safeCaptureName(c.source.ID), name))
	stream.segmentStart, stream.segmentOffset = canonical, 0
	if codec == agentpb.VideoCodec_VIDEO_CODEC_VP8 {
		n, err := f.Write(frame.webmJoin.prefix)
		if err != nil {
			return 0, err
		}
		stream.segmentOffset = int64(n)
	}
	return randomAccessOffset, nil
}

// h264RandomAccessOffset reports an Annex-B payload position where SPS, PPS,
// and IDR NALs appear in order. The GStreamer pipelines emit this trio at
// every keyframe (h264parse config-interval=-1) and the native V4L2 path
// completes it for a camera that does not (h264ParameterSets), so starting
// segments only here makes each file begin independently decodable. Note this
// inspects only the bytes it is given: on a byte-chunked transport a reported
// unit may still be truncated at the payload's end, which is why snapshot
// stills additionally require an access-unit-aligned frame (see handleFrame).
func h264RandomAccessOffset(payload []byte) (offset int, found bool) {
	spsOffset, ppsOffset := -1, -1
	for start, header, ok := h264NextNAL(payload, 0); ok; start, header, ok = h264NextNAL(payload, header+1) {
		switch payload[header] & 0x1f {
		case h264NALIDR:
			if spsOffset >= 0 && ppsOffset >= spsOffset {
				offset, found = spsOffset, true
			}
		case h264NALSPS:
			spsOffset, ppsOffset = start, -1
		case h264NALPPS:
			if spsOffset >= 0 {
				ppsOffset = start
			}
		}
	}
	return offset, found
}

// The H.264 NAL unit types (nal_unit_type, the low five bits of the header
// byte) that the agent tells apart.
const (
	h264NALIDR = 5 // coded slice of an IDR picture
	h264NALSPS = 7 // sequence parameter set
	h264NALPPS = 8 // picture parameter set
	h264NALAUD = 9 // access unit delimiter
)

// h264NextNAL finds the next NAL unit of an Annex-B payload at or after from.
// start is the offset of its start code, three bytes or four, and header that
// of the NAL header byte behind it; a start code with nothing after it is not
// a unit. The unit runs to the start of the following one, or to the end of
// the payload.
func h264NextNAL(payload []byte, from int) (start, header int, ok bool) {
	for i := from; i+3 < len(payload); i++ {
		if payload[i] == 0 && payload[i+1] == 0 && payload[i+2] == 1 {
			return i, i + 3, true
		}
		if i+4 < len(payload) && payload[i] == 0 && payload[i+1] == 0 && payload[i+2] == 0 && payload[i+3] == 1 {
			return i, i + 4, true
		}
	}
	return 0, 0, false
}

func (c *cameraCapture) sampleMapping(receipt int64) error {
	sampleMonotonicMapping := data.SampleMonotonicMapping
	if c.monotonicMapping != nil {
		sampleMonotonicMapping = c.monotonicMapping
	}
	sample, err := sampleMonotonicMapping()
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
