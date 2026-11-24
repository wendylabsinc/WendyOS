package services

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// cameraSegmentExtensions is the only place a codec is given a segment file
// format. A frame in a codec without a row is not written.
var cameraSegmentExtensions = map[agentpb.VideoCodec]string{
	agentpb.VideoCodec_VIDEO_CODEC_H264: ".h264",
	agentpb.VideoCodec_VIDEO_CODEC_VP8:  ".webm",
	videoCodecMJPEG:                     ".mjpeg",
}

func isOwnFormatFrame(frame *videoFrame) bool {
	return frame.native && !encodedFrames.receives(frame)
}

func subscriptionKindsFor(capture *data.SourceCapture) (primary frameKind, nativeBeside bool) {
	switch {
	case capture.EffectiveMode() == captureModeSnapshot || !capture.StoresFrames():
		return encodedFrames, false
	case capture.StoresVideo():
		return encodedFrames, true
	}
	return nativeFrames, false
}

type hubJoin func(kind frameKind) (*deviceHub, int, chan *videoFrame, error)

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
	holder := h.subExplicit[id]
	h.mu.Unlock()
	newID, ch, err = h.subscribeAsKind(holder, kind)
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
	instead := "video is recorded instead"
	if capture.StoresVideo() {
		instead = "only video is recorded"
	}
	if storedBefore == 0 {
		return fmt.Sprintf("store: %s was requested, but this camera's own frames cannot be stored: %s; %s for the whole episode",
			capture.Store, nativeRefusalReason(cause), instead)
	}
	return fmt.Sprintf("store: %s was requested, but this camera's own frames stopped being available after %d stored frame(s): %s; %s for the rest of the episode, even if the camera offers them again",
		capture.Store, storedBefore, nativeRefusalReason(cause), instead)
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
	c.notes = append(c.notes, ownFramesUnavailableNote(c.source.Capture, cause, c.ownFrameStream.indexed))
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
	c.notes = append(c.notes, fmt.Sprintf(
		"nothing was stored for %s between the camera's last own frame and the first video frame; the recording has a discontinuity there, and the number of frames lost across the gap is not knowable",
		time.Duration(receipt-c.nothingStoredSince).Round(time.Millisecond)))
	c.nothingStoredSince = 0
}

func (c *cameraCapture) rejoinSubscriptions() error {
	hub, subID, frames, kind, refusal, err := joinWithVideoFallback(c.primaryKind, func(kind frameKind) (*deviceHub, int, chan *videoFrame, error) {
		return c.rejoin(c.ctx, kind)
	})
	if err != nil {
		return err
	}
	c.hub, c.subID, c.frames, c.primaryKind = hub, subID, frames, kind
	if refusal != nil {
		c.ownFramesNoLongerStored(refusal)
	}
	c.joinNativeBeside()
	return nil
}

func (c *cameraCapture) storeNotes() []string {
	var notes []string
	capture := c.source.Capture
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
