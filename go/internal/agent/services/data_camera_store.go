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

// isOwnFormatFrame reports a whole frame in the camera's own format that is
// not the encoded video itself. Each one decodes alone.
func isOwnFormatFrame(frame *videoFrame) bool {
	return frame.native && !encodedFrames.receives(frame)
}

// subscriptionKindsFor maps what a campaign stores onto hub subscriptions:
// the primary one, and whether a native one runs beside an encoded primary.
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

// joinWithVideoFallback joins for kind. A native join the camera turns away is
// retried for encoded video, and the refusal is returned beside the
// subscription that replaced it.
func joinWithVideoFallback(kind frameKind, join hubJoin) (hub *deviceHub, id int, ch chan *videoFrame, joined frameKind, refusal, err error) {
	hub, id, ch, err = join(kind)
	if kind == nativeFrames && isNativeRefusal(err) {
		refusal, kind = err, encodedFrames
		hub, id, ch, err = join(kind)
	}
	return hub, id, ch, kind, refusal, err
}

// resubscribeAs replaces subscription id with one of another kind that keeps
// id's claim on the hub's stream parameters, and returns the drops id counted.
// The new subscription is made first so the hub never sees its last subscriber
// leave.
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

// ownFramesUnavailableNote is the manifest note for a source that was asked to
// store the camera's own frames and records video in their place.
func ownFramesUnavailableNote(capture *data.SourceCapture, cause error, storedBefore uint64) string {
	instead := "video is recorded instead"
	if capture.StoresVideo() {
		instead = "only video is recorded"
	}
	if storedBefore == 0 {
		return fmt.Sprintf("store: %s was requested, but this camera's own frames cannot be stored: %s; %s",
			capture.Store, nativeRefusalReason(cause), instead)
	}
	return fmt.Sprintf("store: %s was requested, but this camera's own frames stopped being available after %d stored frame(s): %s; %s from there on",
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

// cameraSegmentStream is one run of segment files of a single form, with the
// producer sequence it follows.
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

// endProducerStream closes the segment a replaced producer was writing.
// Sequence numbering belongs to the producer, so it starts over.
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

// perFrameRateCap keeps a frame once the cap's interval has passed since the
// last one kept. It suits only frames that decode alone.
type perFrameRateCap struct {
	started         bool
	first, lastKept int64
	kept, skipped   uint64
}

func (r *perFrameRateCap) admits(receipt int64, hertz float64) bool {
	if r.started && float64(receipt-r.lastKept) < float64(time.Second)/hertz {
		r.skipped++
		return false
	}
	if !r.started {
		r.started, r.first = true, receipt
	}
	r.lastKept = receipt
	r.kept++
	return true
}

func (r *perFrameRateCap) note(hertz float64, end int64) string {
	note := fmt.Sprintf("rate cap %.3g Hz applied per frame to the camera's own frames", hertz)
	span := end - r.first
	if end == 0 || span <= 0 {
		span = r.lastKept - r.first
	}
	if span > 0 {
		note += fmt.Sprintf("; achieved %.3g Hz", float64(r.kept)/(float64(span)/float64(time.Second)))
	}
	return note + fmt.Sprintf("; %d frame(s) skipped", r.skipped)
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
	if c.rateCap > 0 && !c.ownFrameRate.admits(receipt, c.rateCap) {
		c.trackSequence(stream, frame)
		return nil
	}
	return c.writeEncodedFrame(stream, frame, canonical, uncertainty, mappingID, receipt, stream.opensLiveTailWith(frame))
}

// takePrimaryFrame handles a frame from the primary subscription. A native
// primary that delivers the encoded video itself is the case store: frames
// records as video.
func (c *cameraCapture) takePrimaryFrame(frame *videoFrame) error {
	if c.primaryKind == nativeFrames && encodedFrames.receives(frame) {
		c.ownFramesAreVideo, c.ownFramesCodec = true, frame.codec
	}
	return c.handleFrame(frame)
}

// takeNativeBesideFrame handles a frame from the native subscription held
// beside an encoded primary. When it is the encoded video itself the primary
// already delivers it, so the subscription is given up and nothing is stored
// twice.
func (c *cameraCapture) takeNativeBesideFrame(frame *videoFrame) error {
	if encodedFrames.receives(frame) {
		c.ownFramesAreVideo, c.ownFramesCodec = true, frame.codec
		c.leaveNativeBeside()
		return nil
	}
	return c.handleFrame(frame)
}

// joinNativeBeside subscribes for the camera's own frames on the hub the
// encoded primary runs on. A camera that has none is noted, not failed.
func (c *cameraCapture) joinNativeBeside() {
	if !c.wantsNativeBeside || c.ownFramesAreVideo || c.primaryKind != encodedFrames || c.hub == nil {
		return
	}
	id, ch, err := c.hub.subscribeKind(nativeFrames)
	if err != nil {
		c.noteOwnFramesUnavailable(err)
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

// nativeBesideEnded handles the native subscription's channel closing: the
// producer turned it away, or the producer itself ended and the primary's
// handling rejoins both.
func (c *cameraCapture) nativeBesideEnded() {
	refusal := c.hub.subscriberErr(c.nativeSubID)
	c.carriedDrops += c.leaveNativeBeside()
	if refusal != nil {
		c.noteOwnFramesUnavailable(refusal)
	}
}

// recordVideoInstead swaps a native primary the producer turned away for an
// encoded one on the same hub.
func (c *cameraCapture) recordVideoInstead(refusal error) error {
	id, ch, drops, err := c.hub.resubscribeAs(c.subID, encodedFrames)
	if err != nil {
		return err
	}
	c.carriedDrops += drops
	c.subID, c.frames, c.primaryKind = id, ch, encodedFrames
	c.noteOwnFramesUnavailable(refusal)
	return nil
}

func (c *cameraCapture) noteOwnFramesUnavailable(cause error) {
	note := ownFramesUnavailableNote(c.source.Capture, cause, c.ownFrameStream.indexed)
	for _, existing := range c.notes {
		if existing == note {
			return
		}
	}
	c.notes = append(c.notes, note)
}

// rejoinSubscriptions replaces the subscriptions of a producer that ended.
// The caller has already left them.
func (c *cameraCapture) rejoinSubscriptions() error {
	hub, subID, frames, kind, refusal, err := joinWithVideoFallback(c.primaryKind, func(kind frameKind) (*deviceHub, int, chan *videoFrame, error) {
		return c.rejoin(c.ctx, kind)
	})
	if err != nil {
		return err
	}
	c.hub, c.subID, c.frames, c.primaryKind = hub, subID, frames, kind
	if refusal != nil {
		c.noteOwnFramesUnavailable(refusal)
	}
	c.joinNativeBeside()
	return nil
}

func (c *cameraCapture) storeNotes() []string {
	var notes []string
	capture := c.source.Capture
	if c.ownFramesAreVideo && capture.StoresFrames() {
		notes = append(notes, ownFramesAreVideoNote(capture, c.ownFramesCodec))
	}
	if video, own := c.cameraSegmentStream.indexed, c.ownFrameStream.indexed; video > 0 && own > 0 {
		notes = append(notes, fmt.Sprintf("%d video frame(s) and %d of the camera's own frame(s) are indexed; the source's frame and drop counts are sums over both", video, own))
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
