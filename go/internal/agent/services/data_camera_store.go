package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// cameraSegmentExtensions is the only place a codec is given a segment file
// format. A frame in a codec without a row is not written.
var cameraSegmentExtensions = map[agentpb.VideoCodec]string{
	agentpb.VideoCodec_VIDEO_CODEC_H264: ".h264",
	agentpb.VideoCodec_VIDEO_CODEC_VP8:  ".webm",
	videoCodecMJPEG:                     ".mjpeg",
}

// cameraStillExtensions names the file format of one frame stored alone, for
// the codecs whose segment format is a stream of frames.
var cameraStillExtensions = map[agentpb.VideoCodec]string{
	videoCodecMJPEG: ".jpg",
}

func cameraStillExtension(codec agentpb.VideoCodec) string {
	if extension, alone := cameraStillExtensions[codec]; alone {
		return extension
	}
	return cameraSegmentExtensions[codec]
}

func isOwnFormatFrame(frame *videoFrame) bool {
	return frame.native && !encodedFrames.receives(frame)
}

func snapshotInterval(capture *data.SourceCapture) time.Duration {
	if capture.EffectiveMode() != captureModeSnapshot {
		return 0
	}
	return capture.IntervalDuration()
}

func subscriptionKindsFor(capture *data.SourceCapture) (primary frameKind, nativeBeside bool) {
	switch {
	case snapshotInterval(capture) > 0:
		return nativeFrames, false
	case capture.EffectiveMode() == captureModeSnapshot || !capture.StoresFrames():
		return encodedFrames, false
	case capture.StoresVideo():
		return encodedFrames, true
	}
	return nativeFrames, false
}

type hubJoin func(kind frameKind) (*deviceHub, int, chan *videoFrame, error)

// ownFramesForecast asks the native capture sources what the producer of a
// camera would hand on for req, without starting it. asked is false for the
// cameras runProducer and captureNode never give to those sources, whose
// producer alone answers.
func (s *VideoService) ownFramesForecast(hubKey string, req *agentpb.StreamVideoRequest) (own ownFrames, declined string, native, asked bool) {
	if !strings.HasPrefix(hubKey, "/dev/video") || (s.sensorFrames != nil && isSensorLinkCameraPath(hubKey)) {
		return ownFrames{}, "", false, false
	}
	if s.classifyTransport != nil {
		if transport, _ := s.classifyTransport(filepath.Base(hubKey)); transport == camera.TransportCSI {
			return ownFrames{}, "", false, false
		}
	}
	own, declined, native = s.ownFramesFor(hubKey, req)
	return own, declined, native, true
}

// videoForecast is how a default capture would make the video of a camera, as
// far as can be told without running it.
type videoForecast struct {
	// softwareEncoder names the software element that encodes the video. It is
	// "" for a camera whose video needs no encoder here, for one whose capture
	// is known only once it runs (a SensorLink camera, a native source that
	// cannot describe its frames), and for an encoder not known to be software.
	softwareEncoder string
	// ownFrames says the capture also hands on the camera's own frames: a
	// native source derives the video from them, and every other local camera
	// is encoded by the GStreamer pipeline, which hands on none.
	ownFrames bool
	// presetNotApplied is why a preset stated for the video is not applied,
	// and "" where it is applied and where that is not known.
	presetNotApplied string
}

func (s *VideoService) forecastVideoOf(src videoSource) videoForecast {
	switch src.kind {
	case sourceV4L2:
	case sourceIP:
		return videoForecast{presetNotApplied: presetIgnoredOwnH264}
	default:
		return videoForecast{}
	}
	_, available, err := s.gstElements()
	if err != nil {
		return videoForecast{}
	}
	transport := camera.TransportUnknown
	if s.classifyTransport != nil {
		transport, _ = s.classifyTransport(filepath.Base(src.key))
	}
	// A feed of JPEG frames and Argus are launched once, at the default preset.
	unshaped := s.sensorFrames != nil && isSensorLinkCameraPath(src.key)
	var forecast videoForecast
	var encoder string
	own, declined, native, asked := s.ownFramesForecast(src.key, &agentpb.StreamVideoRequest{})
	switch {
	case asked && native && declined == "" && encodedFrames.receives(&videoFrame{codec: own.codec}):
		return videoForecast{ownFrames: true, presetNotApplied: presetIgnoredOwnH264}
	case asked && native && declined == "":
		encoder, forecast.ownFrames = h264EncoderFrom(own.codec, available), true
		forecast.softwareEncoder = softwareEncoderAmong(encoder)
	case asked && native:
		return videoForecast{}
	default:
		chosen, err := findGStreamerEncoderFromSet(available)
		if err != nil {
			return videoForecast{}
		}
		encoder = chosen.element
		if chosen.codec == agentpb.VideoCodec_VIDEO_CODEC_H264 && !unshaped {
			forecast.softwareEncoder = softwareEncoderAmong(encoder)
		}
		unshaped = unshaped || useArgusSource(transport, s.hostIsJetson(), available)
	}
	switch {
	case encoder == "":
	case encoderPreset(encoder, noPreset) == noPreset:
		forecast.presetNotApplied = fmt.Sprintf(presetIgnoredByEncoder, encoder)
	case unshaped:
		forecast.presetNotApplied = fmt.Sprintf(presetIgnoredUnshaped, encoderPreset(encoder, noPreset))
	}
	return forecast
}

// whyPresetNotApplied is forecastVideoOf's reason for the camera a source
// with this capture block records, and "" for a source that states no preset.
func (s *VideoService) whyPresetNotApplied(devID uint32, capture *data.SourceCapture) string {
	if capture.EncodePreset() == noPreset {
		return ""
	}
	src, err := s.resolveSource(devID)
	if err != nil {
		return ""
	}
	return s.forecastVideoOf(src).presetNotApplied
}

// asksForEncodedVideo reports whether a recording or a pre-roll ring of a
// camera source with this capture block subscribes to its encoded video.
func asksForEncodedVideo(capture *data.SourceCapture) bool {
	primary, _ := subscriptionKindsFor(capture)
	return primary == encodedFrames
}

// armsStandbyRing reports whether Arm would keep a pre-roll ring for the
// listing: it keeps none for a snapshot source. A ring also takes a buffer
// and a camera Arm can join, which only Arm finds out.
func armsStandbyRing(source data.Source) bool {
	return source.Capture == nil || source.Capture.EffectiveMode() != captureModeSnapshot
}

// DeployWarnings names each camera of the campaign present now whose video
// this device encodes in software for the campaign, with the preset where the
// encoder takes one: the one the camera's recording states, since inference
// states none. It also names each camera whose recording states a preset that
// this device does not apply to its video, with the reason. It lists the cameras for
// any plan with a camera source, and resolves the plan's selectors with
// data.Campaign.CamerasAmong. A camera is read as its recording asks; with a
// buffer, the last listing Arm keeps a ring for decides instead, because the
// episode goes on with that ring's subscription. A campaign's inference reads
// every camera as video whatever its source stores. It stops between cameras
// once ctx is done: nobody waits for the answer by then.
func (a *cameraDataAdapter) DeployWarnings(ctx context.Context, campaign data.Campaign) []string {
	if !slices.ContainsFunc(campaign.Sources, func(source data.CampaignSource) bool { return source.Camera != "" }) {
		return nil
	}
	recorded, listed := campaign.CamerasAmong(a.Discover(ctx))
	readAs := make(map[string]*data.SourceCapture, len(recorded))
	for _, source := range recorded {
		readAs[source.ID] = source.Capture
	}
	asksForFramesAlone := map[string]bool{}
	for _, listing := range listed {
		if campaign.BufferDuration() > 0 && armsStandbyRing(listing) {
			readAs[listing.ID] = listing.Capture
		}
		if !listing.Capture.StoresVideo() {
			asksForFramesAlone[listing.ID] = true
		}
	}
	inference := campaign.Inference.IsEnabled()
	var warnings []string
	for _, source := range recorded {
		if ctx.Err() != nil {
			return warnings
		}
		devID, ok := cameraDeviceID(source.ID)
		if !ok {
			continue
		}
		src, err := a.video.resolveSource(devID)
		if err != nil {
			continue
		}
		forecast := a.video.forecastVideoOf(src)
		stated := readAs[source.ID].EncodePreset()
		video := asksForEncodedVideo(readAs[source.ID])
		if encoder := forecast.softwareEncoder; encoder != "" && (inference || video || !forecast.ownFrames) {
			willRunAt := ""
			if forecast.presetNotApplied == "" {
				recording := noPreset
				if !inference {
					recording = stated
				}
				willRunAt = betweenCommas(describePresetWhenRead(encoder, recording))
			}
			encoded := fmt.Sprintf("encoded in software (%s)%s on this device while ", encoder, willRunAt)
			switch {
			case inference:
				encoded = "its video is " + encoded + "the campaign's inference runs"
			case !video:
				encoded = "it hands on no frames of its own, so what its source asks to store is recorded as video, " + encoded + "the campaign records or buffers"
			case forecast.ownFrames && !asksForFramesAlone[source.ID]:
				encoded = "its video is " + encoded + "the campaign records or buffers; store: frames keeps the camera's own frames without encoding"
			default:
				encoded = "its video is " + encoded + "the campaign records or buffers"
			}
			warnings = append(warnings, fmt.Sprintf("camera %s: %s", source.ID, encoded))
		}
		if ignored := describePresetIgnored(stated, forecast.presetNotApplied); ignored != "" {
			warnings = append(warnings, fmt.Sprintf("camera %s: %s", source.ID, ignored))
		}
	}
	return warnings
}

// ownFramesAreKnownToBeVideo is asked of a camera the join may start. One
// that is running hands on what it hands on and answers at the join.
func (s *VideoService) ownFramesAreKnownToBeVideo(hubKey string, req *agentpb.StreamVideoRequest) bool {
	if s.joinsCameraAsItRuns(hubKey, req) {
		return false
	}
	own, declined, native, asked := s.ownFramesForecast(hubKey, req)
	return asked && native && (declined != "" || encodedFrames.receives(&videoFrame{codec: own.codec}))
}

// joinsCameraAsItRuns reports that a join for req starts no producer: the
// request names no parameters and the camera is running.
func (s *VideoService) joinsCameraAsItRuns(hubKey string, req *agentpb.StreamVideoRequest) bool {
	if streamRequestAssertsParams(req) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	hub, running := s.hubs[hubKey]
	return running && hub.ctx.Err() == nil
}

// refusingOwnFramesKnownAbsent turns a join for the camera's own frames away
// before it is made when the join may start the camera and no native source
// would capture it. Made as the camera's first subscription, that join starts
// its pipeline without the encoder, is refused once the pipeline runs, and
// costs a second launch with the encoder. A camera that is running answers
// for itself at the join.
func (s *VideoService) refusingOwnFramesKnownAbsent(hubKey string, req *agentpb.StreamVideoRequest, join hubJoin) hubJoin {
	return func(kind frameKind) (*deviceHub, int, chan *videoFrame, error) {
		if kind == nativeFrames && !s.joinsCameraAsItRuns(hubKey, req) {
			if _, declined, native, asked := s.ownFramesForecast(hubKey, req); asked && !native {
				return nil, 0, nil, errNativeUnavailable(declined)
			}
		}
		return join(kind)
	}
}

func joinWithVideoFallback(kind frameKind, join hubJoin) (hub *deviceHub, id int, ch chan *videoFrame, joined frameKind, refusal, err error) {
	hub, id, ch, err = join(kind)
	if kind == nativeFrames && isNativeRefusal(err) {
		refusal, kind = err, encodedFrames
		hub, id, ch, err = join(kind)
	}
	return hub, id, ch, kind, refusal, err
}

// resubscribeAs subscribes before it unsubscribes, because a hub whose last
// subscriber leaves stops its producer.
func (h *deviceHub) resubscribeAs(id int, kind frameKind) (newID int, ch chan *videoFrame, drops uint64, err error) {
	h.mu.Lock()
	holder, preset := h.subExplicit[id], noPreset
	if sub := h.subs[id]; sub != nil {
		preset = sub.preset
	}
	h.mu.Unlock()
	newID, ch, err = h.subscribeAsKind(holder, kind, preset)
	if err != nil {
		return 0, nil, 0, err
	}
	return newID, ch, h.unsubscribe(id), nil
}

func nativeRefusalReason(err error) string {
	var refusal nativeUnavailable
	if errors.As(err, &refusal) {
		return refusal.reason
	}
	return err.Error()
}

func ownFramesUnavailableNote(capture *data.SourceCapture, cause error, storedBefore uint64) string {
	asked, stored, instead := "store: "+capture.Store+" was requested", "frame(s)", "video is recorded instead"
	switch {
	case snapshotInterval(capture) > 0:
		asked, stored, instead = "snapshot mode stores a camera's own frames as stills", "still(s)", "its video is used instead"
	case capture.StoresVideo():
		instead = "only video is recorded"
	}
	if storedBefore == 0 {
		return fmt.Sprintf("%s, but this camera's own frames cannot be stored: %s; %s for the whole episode",
			asked, nativeRefusalReason(cause), instead)
	}
	return fmt.Sprintf("%s, but this camera's own frames stopped being available after %d stored %s: %s; %s for the rest of the episode, even if the camera offers them again",
		asked, storedBefore, stored, nativeRefusalReason(cause), instead)
}

func ownFramesAreVideoNote(capture *data.SourceCapture, codec agentpb.VideoCodec) string {
	if capture.StoresVideo() {
		return fmt.Sprintf("store: %s: this camera's own frames are its encoded video (%s), so there is one stream and it is stored once",
			capture.Store, codecIndexName(codec))
	}
	return fmt.Sprintf("store: %s: this camera's own frames are its encoded video (%s), stored in the same files and index as store: %s",
		capture.Store, codecIndexName(codec), data.CaptureStoreVideo)
}

type cameraSegmentStream struct {
	segment       *os.File
	segmentRel    string
	segmentStart  int64
	segmentNumber int
	segmentOffset int64
	lastSequence  uint32
	haveSequence  bool
	// awaitLiveSegmentReset makes the first random-access frame of the LIVE
	// tail open a fresh segment. Set once the pre-roll has been written: the
	// flush is a burst of disk work during which the hub may have dropped
	// frames despite the drain (see drainDuring), so any gap between the
	// pre-roll and the tail is made to land on a segment boundary rather than
	// inside a file a decoder reads as one continuous timeline.
	awaitLiveSegmentReset bool
	indexed               uint64
}

func (s *cameraSegmentStream) closeSegment() error {
	if s.segment == nil {
		return nil
	}
	if err := s.segment.Sync(); err != nil {
		return err
	}
	if err := s.segment.Close(); err != nil {
		return err
	}
	s.segment = nil
	return nil
}

// endProducerStream also restarts sequence tracking: the numbering belongs to
// the producer that is gone.
func (s *cameraSegmentStream) endProducerStream() error {
	if err := s.closeSegment(); err != nil {
		return err
	}
	s.haveSequence = false
	return nil
}

func (s *cameraSegmentStream) opensLiveTailWith(frame *videoFrame) bool {
	if !s.awaitLiveSegmentReset {
		return false
	}
	if _, randomAccess := frameRandomAccess(frame); !randomAccess {
		return false
	}
	s.awaitLiveSegmentReset = false
	return true
}

// perFrameRateCap keeps frames on a schedule that advances by exactly one cap
// interval per kept frame. Measuring from the last kept frame instead would
// put every decision on the threshold when the camera runs at the cap or a
// multiple of it, and timestamp jitter would then halve the achieved rate.
// It suits only frames that decode alone.
type perFrameRateCap struct {
	hertz           float64
	started         bool
	first, lastKept int64
	lastSeen, due   int64
	kept, skipped   uint64
}

// perFrameRateEarliness is how far before its scheduled time a frame is still
// kept, as a divisor of the cap interval: enough for timestamp jitter, and
// well short of the previous frame of a camera running at twice the cap.
const perFrameRateEarliness = 8

func (r *perFrameRateCap) admits(captureTime int64, hertz float64) bool {
	interval := int64(float64(time.Second) / hertz)
	wentBackwards := captureTime < r.lastSeen
	r.lastSeen = captureTime
	if r.started && !wentBackwards && captureTime < r.due-interval/perFrameRateEarliness {
		r.skipped++
		return false
	}
	if !r.started {
		r.hertz, r.started, r.first, r.due = hertz, true, captureTime, captureTime
	}
	r.due += interval
	// A frame later than a whole interval restarts the schedule from itself,
	// so a stall is not made up for with a burst.
	if wentBackwards || captureTime > r.due {
		r.due = captureTime + interval
	}
	r.lastKept = captureTime
	r.kept++
	return true
}

func (r *perFrameRateCap) note() string {
	note := fmt.Sprintf("rate cap %.3g Hz applied per frame to the camera's own frames", r.hertz)
	if span := r.lastKept - r.first; r.kept > 1 && span > 0 {
		note += fmt.Sprintf("; achieved %.3g Hz", float64(r.kept-1)/(float64(span)/float64(time.Second)))
	}
	return note + fmt.Sprintf("; %d frame(s) skipped", r.skipped)
}

// frameCaptureTime is the camera's own timestamp when the frame has one, which
// is steadier than the agent's receipt of it.
func frameCaptureTime(frame *videoFrame, receipt int64) int64 {
	if frame.nativeNs != 0 {
		return frame.nativeNs
	}
	return receipt
}

type sampleTime struct {
	canonical, uncertainty, receipt int64
	mappingID                       string
}

// sampleTimes holds the time fields first given to recent sample IDs. A source
// stored in two forms indexes each sample twice, and the two records arrive
// apart, possibly on either side of a clock mapping refresh.
type sampleTimes struct {
	byID   map[uint64]sampleTime
	recent []uint64
	oldest int
}

// sampleTimesKept bounds the table far above the few frames by which derived
// video trails the frames it is made from.
const sampleTimesKept = 256

func (s *sampleTimes) of(sampleID uint64) (sampleTime, bool) {
	known, ok := s.byID[sampleID]
	return known, ok
}

func (s *sampleTimes) keep(sampleID uint64, t sampleTime) {
	if s.byID == nil {
		s.byID = make(map[uint64]sampleTime, sampleTimesKept)
		s.recent = make([]uint64, 0, sampleTimesKept)
	}
	if _, known := s.byID[sampleID]; known {
		return
	}
	if len(s.recent) < sampleTimesKept {
		s.recent = append(s.recent, sampleID)
	} else {
		delete(s.byID, s.recent[s.oldest])
		s.recent[s.oldest] = sampleID
		s.oldest = (s.oldest + 1) % sampleTimesKept
	}
	s.byID[sampleID] = t
}

func (c *cameraCapture) canonicalTime(frame *videoFrame) (canonical, uncertainty int64, mappingID string, receipt int64, err error) {
	if !c.indexesSamplesTwice() || frame.sampleID == 0 {
		return c.computeCanonicalTime(frame)
	}
	if known, ok := c.sampleTimes.of(frame.sampleID); ok {
		return known.canonical, known.uncertainty, known.mappingID, known.receipt, nil
	}
	canonical, uncertainty, mappingID, receipt, err = c.computeCanonicalTime(frame)
	if err == nil {
		c.sampleTimes.keep(frame.sampleID, sampleTime{canonical: canonical, uncertainty: uncertainty, receipt: receipt, mappingID: mappingID})
	}
	return canonical, uncertainty, mappingID, receipt, err
}

func (c *cameraCapture) indexesSamplesTwice() bool {
	return c.wantsNativeBeside && !c.ownFramesRefused && !c.ownFramesAreVideo
}

func (c *cameraCapture) endProducerStreams() error {
	if err := c.endProducerStream(); err != nil {
		return err
	}
	if err := c.ownFrameStream.endProducerStream(); err != nil {
		return err
	}
	c.alignmentKnown = false
	return nil
}

func (c *cameraCapture) streamFor(frame *videoFrame) *cameraSegmentStream {
	if isOwnFormatFrame(frame) {
		return &c.ownFrameStream
	}
	return &c.cameraSegmentStream
}

func (c *cameraCapture) refuseCodec(codec agentpb.VideoCodec) {
	if c.refusedCodecs == nil {
		c.refusedCodecs = make(map[agentpb.VideoCodec]uint64)
	}
	c.refusedCodecs[codec]++
}

func (c *cameraCapture) writeOwnFrame(frame *videoFrame) error {
	canonical, uncertainty, mappingID, receipt, err := c.canonicalTime(frame)
	if err != nil {
		return err
	}
	stream := &c.ownFrameStream
	if c.rateCap > 0 && !c.ownFrameRate.admits(frameCaptureTime(frame, receipt), c.rateCap) {
		c.trackSequence(stream, frame)
		return nil
	}
	return c.writeEncodedFrame(stream, frame, canonical, uncertainty, mappingID, receipt, stream.opensLiveTailWith(frame))
}

func (c *cameraCapture) takePrimaryFrame(frame *videoFrame) error {
	if c.primaryKind == nativeFrames && encodedFrames.receives(frame) {
		c.ownFramesAreVideo, c.ownFramesCodec = true, frame.codec
	}
	return c.handleFrame(frame)
}

func (c *cameraCapture) takeNativeBesideFrame(frame *videoFrame) error {
	if encodedFrames.receives(frame) {
		c.ownFramesAreVideo, c.ownFramesCodec = true, frame.codec
		c.leaveNativeBeside()
		return nil
	}
	return c.handleFrame(frame)
}

func (c *cameraCapture) joinNativeBeside() {
	if !c.wantsNativeBeside || c.ownFramesRefused || c.ownFramesAreVideo || c.primaryKind != encodedFrames || c.hub == nil {
		return
	}
	id, ch, err := c.hub.subscribeKind(nativeFrames)
	if err != nil {
		c.ownFramesNoLongerStored(err)
		return
	}
	c.nativeSubID, c.nativeFrames = id, ch
}

func (c *cameraCapture) leaveNativeBeside() uint64 {
	if c.nativeFrames == nil {
		return 0
	}
	c.nativeFrames = nil
	return c.hub.unsubscribe(c.nativeSubID)
}

func (c *cameraCapture) nativeBesideEnded() {
	refusal := c.hub.subscriberErr(c.nativeSubID)
	c.carriedDrops += c.leaveNativeBeside()
	if refusal == nil {
		// The producer itself ended; the primary's close rejoins both.
		return
	}
	c.ownFramesNoLongerStored(refusal)
	// The producer that turned the subscription away is a different pipeline
	// from the one that served it, so its video does not continue the file the
	// earlier one was writing.
	c.awaitLiveSegmentReset = true
	c.recordedStreamChanged()
}

func (c *cameraCapture) recordVideoInstead(refusal error) error {
	id, ch, drops, err := c.hub.resubscribeAs(c.subID, encodedFrames)
	if err != nil {
		return err
	}
	c.carriedDrops += drops
	c.subID, c.frames, c.primaryKind = id, ch, encodedFrames
	c.ownFramesNoLongerStored(refusal)
	if c.result.Count > 0 {
		c.result.Discontinuities++
		c.nothingStoredSince = c.lastStoredReceipt
	}
	c.recordedStreamChanged()
	return nil
}

// ownFramesNoLongerStored is final for the episode: a camera that offers its
// own frames again later does not get them stored, so one episode never
// alternates between forms.
func (c *cameraCapture) ownFramesNoLongerStored(cause error) {
	if c.ownFramesRefused {
		return
	}
	c.ownFramesRefused = true
	c.notes = append(c.notes, ownFramesUnavailableNote(c.source.Capture, cause, c.ownFrameStream.indexed+uint64(c.snapshotNumber)))
}

// recordedStreamChanged forgets what was learned from the stream recorded so
// far: whether frames are whole access units, and whether they are numbered.
// Losses already counted from sequence gaps stay counted; what the hub dropped
// up to now left gaps too, so it is not counted a second time from here.
func (c *cameraCapture) recordedStreamChanged() {
	c.alignmentKnown = false
	c.haveSequence, c.ownFrameStream.haveSequence = false, false
	if !c.sawSequence {
		return
	}
	c.sawSequence = false
	c.carriedDrops = 0
	c.dropBaseline = c.hub.drops(c.subID)
}

func (c *cameraCapture) noteStorageResumed(receipt int64) {
	gap := time.Duration(receipt - c.nothingStoredSince).Round(time.Millisecond)
	if c.snapshotNumber > 0 {
		c.notes = append(c.notes, fmt.Sprintf(
			"nothing was stored for %s between the last still of the camera's own frames and the first video frame; the recording has a discontinuity there", gap))
	} else {
		c.notes = append(c.notes, fmt.Sprintf(
			"nothing was stored for %s between the camera's last own frame and the first video frame; the recording has a discontinuity there, and the number of frames lost across the gap is not knowable", gap))
	}
	c.nothingStoredSince = 0
}

// stillsGiveWayToVideo marks a source that stored stills and goes on recording
// continuously. Its snapshot accounting stays open until the first video
// frame is stored, or the episode ends. The subscription's drops up to here
// are frames discarded between stills, and do not become losses of the video.
func (c *cameraCapture) stillsGiveWayToVideo() {
	if c.snapshotNumber == 0 {
		return
	}
	c.stillsAwaitVideo = true
	if c.hub != nil {
		c.carriedDrops = 0
		c.dropBaseline = c.hub.drops(c.subID)
	}
}

// closeStillsAccount counts the intervals without a still up to until, the
// receipt of the first video frame stored or of the episode's end, and says
// what the source's counts now add up.
func (c *cameraCapture) closeStillsAccount(until int64, videoStored bool) {
	c.stillsAwaitVideo = false
	c.countIntervalsMissedBefore(until)
	note := fmt.Sprintf("%d still(s) were stored and %d snapshot interval(s) passed without one before ", c.snapshotNumber, c.missedIntervals)
	if videoStored {
		note += "the source went on recording continuously; its frame count adds the video frames stored since to those stills, and its drop count adds the frames lost since to those intervals"
	} else {
		note += "the episode ended; the source was to go on recording continuously, and its drop count adds the frames lost since the stills to those intervals"
	}
	c.notes = append(c.notes, note)
}

func (c *cameraCapture) rejoinSubscriptions() error {
	hub, subID, frames, kind, refusal, err := joinWithVideoFallback(c.primaryKind, func(kind frameKind) (*deviceHub, int, chan *videoFrame, error) {
		return c.rejoin(c.ctx, kind)
	})
	if err != nil {
		return err
	}
	c.noteCameraWaitOnLeaving()
	c.hub, c.subID, c.frames, c.primaryKind = hub, subID, frames, kind
	if refusal != nil {
		c.ownFramesNoLongerStored(refusal)
	}
	c.joinNativeBeside()
	return nil
}

// encoderChangedAt reports that an encoder run at another preset than the
// video frame before it made this one, with parameter sets of its own.
func (c *cameraCapture) encoderChangedAt(frame *videoFrame) bool {
	first := c.videoStoredAt == nil
	if first {
		c.videoStoredAt = make(map[appconfig.EncodePreset]uint64)
	}
	changed := !first && frame.encodedAt != c.lastVideoAt
	c.lastVideoAt = frame.encodedAt
	return changed
}

// countLeftOut counts a frame the recording took and could not store. Where
// frames are numbered it follows the numbering first, so the frames lost
// before this one are counted with it and not again at the next one stored.
func (c *cameraCapture) countLeftOut(stream *cameraSegmentStream, frame *videoFrame) {
	if !frame.sequenceValid {
		c.carriedDrops++
		return
	}
	c.trackSequence(stream, frame)
	if c.result.Drops == nil {
		c.result.Drops = new(uint64)
	}
	*c.result.Drops++
}

// presetNotAppliedNote says how many of a recording's video frames were not
// encoded at the preset its source states, or "".
func presetNotAppliedNote(stated appconfig.EncodePreset, storedAt map[appconfig.EncodePreset]uint64) string {
	if stated == noPreset {
		return ""
	}
	var atOthers []string
	for _, name := range appconfig.EncodePresets {
		preset := appconfig.ParseEncodePreset(name)
		if frames := storedAt[preset]; frames > 0 && preset != stated {
			atOthers = append(atOthers, fmt.Sprintf("%d at %s", frames, name))
		}
	}
	var said []string
	if len(atOthers) > 0 {
		first, rest, _ := strings.Cut(strings.Join(atOthers, " and "), " at ")
		said = append(said, fmt.Sprintf("%s video frame(s) were encoded at %s", first, rest))
	}
	if frames := storedAt[noPreset]; frames > 0 {
		said = append(said, fmt.Sprintf("%d video frame(s) were not encoded at a preset: they are the camera's own or come from an encoder that takes none", frames))
	}
	if len(said) == 0 {
		return ""
	}
	return fmt.Sprintf("preset: %s was requested, but %s", stated, strings.Join(said, "; "))
}

func (c *cameraCapture) storeNotes() []string {
	var notes []string
	capture := c.source.Capture
	if note := describePresetIgnored(capture.EncodePreset(), c.presetNotApplied); note != "" {
		notes = append(notes, note)
	} else if note := presetNotAppliedNote(capture.EncodePreset(), c.videoStoredAt); note != "" {
		notes = append(notes, note)
	}
	if c.nothingStoredSince != 0 {
		notes = append(notes, "no video frame was stored after the camera's last own frame before the episode ended")
	}
	if c.ownFramesAreVideo && capture.StoresFrames() {
		notes = append(notes, ownFramesAreVideoNote(capture, c.ownFramesCodec))
	}
	if video, own := c.cameraSegmentStream.indexed, c.ownFrameStream.indexed; video > 0 && own > 0 {
		notes = append(notes, fmt.Sprintf("%d video frame(s) and %d of the camera's own frame(s) are indexed; the source's frame and drop counts are sums over both", video, own))
	}
	if c.videoRateGated && c.ownFrameRate.started {
		notes = append(notes, "under the rate cap the two streams hold different samples: video keeps whole groups of pictures, and the camera's own frames are kept one by one at the cap's spacing")
	}
	codecs := make([]agentpb.VideoCodec, 0, len(c.refusedCodecs))
	for codec := range c.refusedCodecs {
		codecs = append(codecs, codec)
	}
	sort.Slice(codecs, func(i, j int) bool { return codecs[i] < codecs[j] })
	for _, codec := range codecs {
		notes = append(notes, fmt.Sprintf("%d frame(s) in codec %s were not stored: no segment file format is defined for that codec",
			c.refusedCodecs[codec], codecIndexName(codec)))
	}
	return notes
}
