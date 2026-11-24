package services

// Pixel subscribers and encoder demand.
//
// A hub has had two audiences: viewers and recordings, which get encoded video,
// and gRPC raw subscribers, which get the bytes the camera produced. Camera
// sharing adds a third that lives inside the agent: a consumer that wants
// decoded frames from the same tap, to hand on to apps. It is not a raw
// subscriber, because the promise differs. A raw subscriber was promised the
// camera's own bytes, so decoded MJPEG is refused to it; a pixel consumer only
// needs a picture it can read, so decoded MJPEG serves it.
//
// Pixel consumers do not need the H.264 encoder either. A pipeline that runs it
// for nobody spends an edge device's CPU encoding frames nobody receives, so
// the hub reports whether anyone wants encoded video and signals when that may
// have changed. The producer decides what to do about it; the hub only counts.

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// errPixelsUnavailable is what a pixel subscriber is told by a camera whose
// producer has no tap. It wraps the producer's own reason, so the consumer that
// gives up can say why.
var errPixelsUnavailable = errors.New("camera offers no uncompressed frames")

// errPixelsCameraHeld is wrapped into a pixel refusal when the camera had no
// tap because another client held it: the agent could not open it and read it
// through PipeWire instead (captureLocalCamera), which gives no uncompressed
// frames. The refusal's reason then names PipeWire, but the cause is the
// holder, so a share pump says that instead. Raw subscribers' refusals do not
// carry it.
var errPixelsCameraHeld = errors.New("another client holds the camera")

// heldCameraSink is the part of a hub a producer tells, before the start that
// reads a held camera through PipeWire, that the start's refusal is because of
// the holder. Reached by type assertion on the sink, like pixelSink.
type heldCameraSink interface {
	pixelsFromHeldCamera()
}

// pixelSink is the part of a hub a producer tells that its tap carries frames
// in-agent consumers may read. rawSink stays as it is, so producers and fakes
// that know nothing of pixels keep compiling; a producer reaches this by type
// assertion on its sink.
type pixelSink interface {
	pixelsOffered(format *agentpb.RawFormat)
}

// encodeDemand is what a producer reads to decide whether its pipeline needs
// the encoder, and what it reports back when it restarts. Reached by type
// assertion on the sink, like pixelSink.
type encodeDemand interface {
	wantEncoded() bool
	wantRaw() bool
	wantNative() bool
	demandChanged() <-chan struct{}
	encodedStreamRestarted()
	setPipelineEncodes(bool)
}

var (
	_ rawSink        = (*deviceHub)(nil)
	_ pixelSink      = (*deviceHub)(nil)
	_ heldCameraSink = (*deviceHub)(nil)
	_ encodeDemand   = (*deviceHub)(nil)
)

// hubDemand is the hub state behind pixel and native subscribers and encoder
// demand. Protected by the hub's mu, like everything else on deviceHub.
type hubDemand struct {
	// changed is poked, never blocking, whenever the answer to wantEncoded or
	// wantRaw may have changed. Capacity 1: a producer that was busy sees one
	// pending signal and re-reads the hub, rather than a backlog to work off.
	// Made on first use, so a hub built as a struct literal still has one.
	changed chan struct{}
	// tap is set by pixelsOffered and consumed by the raw decision that follows
	// it (rawOffered or rawNotOffered). A producer makes that decision on every
	// pipeline start, so a start that never said it had a tap is read as having
	// none, even when an earlier start in the same hub did.
	tap bool
	// format is the layout of the tap's frames, as last offered.
	format *agentpb.RawFormat
	// refused is non-nil once the producer turned pixel subscribers away: it is
	// the raw refusal's reason, evaluated only if a pixel consumer asks.
	refused func() string

	nativeRefused func() string
	// held is set by pixelsFromHeldCamera and consumed, like tap, by the raw
	// decision that follows it; refusedHeld keeps it with refused, so the pixel
	// refusal wraps errPixelsCameraHeld.
	held, refusedHeld bool
	// encoderOff mirrors setPipelineEncodes, inverted so that the zero value
	// matches every producer that never calls it: they all encode.
	encoderOff bool
}

// subscribePixels adds an in-agent consumer of the tap's uncompressed frames.
// A hub whose producer already said it has no tap refuses the join up front,
// as rawRefusal does for a raw subscriber, rather than leaving the consumer
// waiting on frames that will never come.
func (h *deviceHub) subscribePixels() (int, chan *videoFrame, error) {
	return h.subscribeAsKind("", pixelFrames)
}

// pixelRefusal builds the error a refused pixel subscriber is given. held says
// the refusal is because another client holds the camera (pixelsFromHeldCamera).
func pixelRefusal(reason func() string, held bool) error {
	if held {
		return fmt.Errorf("%w: %s (%w)", errPixelsUnavailable, reason(), errPixelsCameraHeld)
	}
	return fmt.Errorf("%w: %s", errPixelsUnavailable, reason())
}

// pixelsFromHeldCamera records that the start about to run reads a camera
// another client holds, through PipeWire. Call it before that start; its raw
// decision consumes it.
func (h *deviceHub) pixelsFromHeldCamera() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.demand.held = true
}

func (h *deviceHub) wantEncoded() bool {
	return h.wantKind(encodedFrames)
}

func (h *deviceHub) wantNative() bool {
	return h.wantKind(nativeFrames)
}

func (h *deviceHub) wantKind(kind frameKind) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, sub := range h.subs {
		if sub.kind == kind && !sub.closed {
			return true
		}
	}
	return false
}

func errNativeUnavailable(reason string) error {
	return status.Error(codes.FailedPrecondition, "native frames are not available for this camera: "+reason)
}

func (h *deviceHub) nativeNotOffered(reason func() string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.demand.nativeRefused = memoiseReason(reason)
	closed := false
	for _, sub := range h.subs {
		if sub.kind == nativeFrames && !sub.closed {
			sub.err = errNativeUnavailable(h.demand.nativeRefused())
			sub.closed = true
			close(sub.ch)
			closed = true
		}
	}
	if closed {
		h.pokeDemandLocked()
	}
}

// demandChanged returns the channel poked whenever wantEncoded or wantRaw may
// have changed: on every subscribe and unsubscribe, and when the producer closes
// a subscriber early. A receive means "read the hub again", not what changed.
func (h *deviceHub) demandChanged() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.demandChangedLocked()
}

func (h *deviceHub) demandChangedLocked() chan struct{} {
	if h.demand.changed == nil {
		h.demand.changed = make(chan struct{}, 1)
	}
	return h.demand.changed
}

// pokeDemandLocked signals demandChanged without blocking. Callers hold h.mu,
// which is why it must never block: a producer waiting on that same mutex
// would never get to receive.
func (h *deviceHub) pokeDemandLocked() {
	select {
	case h.demandChangedLocked() <- struct{}{}:
	default: // a signal is already pending; the reader re-reads the hub anyway
	}
}

// pixelsOffered records that the pipeline about to start tees frames in-agent
// consumers may read, in this layout. Call it before rawOffered or
// rawNotOffered for the same start: that decision consumes it, and
// rawNotOffered turns pixel subscribers away unless it came first. It also
// lifts a refusal an earlier start made, as rawOffered does for raw.
func (h *deviceHub) pixelsOffered(format *agentpb.RawFormat) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.demand.tap = true
	h.demand.format = format
	h.demand.refused = nil
	h.demand.refusedHeld = false
}

// pixelsNotOfferedLocked is rawNotOffered's pixel rule. Unless pixelsOffered
// came first for this start, the producer has no tap at all, so every pixel
// subscriber waiting on it is closed with the same reason the raw ones get, and
// later pixel joins are refused up front. Callers hold h.mu. Reports whether it
// closed any subscriber.
func (h *deviceHub) pixelsNotOfferedLocked(reason func() string) bool {
	held := h.demand.held
	h.demand.held = false
	if h.demand.tap {
		h.demand.tap = false
		return false
	}
	h.demand.refused = reason
	h.demand.refusedHeld = held
	closed := false
	for _, sub := range h.subs {
		if sub.kind == pixelFrames && !sub.closed {
			sub.err = pixelRefusal(reason, held)
			sub.closed = true
			close(sub.ch)
			closed = true
		}
	}
	return closed
}

// encodedStreamRestarted forgets the encoded stream's WebM state. A restarted
// pipeline begins a new WebM stream with its own initialization, and a viewer
// joining after the restart must be handed that one, not the previous stream's.
func (h *deviceHub) encodedStreamRestarted() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.webmInit = webmInitialization{}
	h.webmJoins = webmJoins{}
}

// setPipelineEncodes records whether the producer's current pipeline runs the
// encoder.
func (h *deviceHub) setPipelineEncodes(encodes bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.demand.encoderOff = !encodes
}

// pipelineEncodes reports whether the producer's current pipeline runs the
// encoder. True until a producer says otherwise.
func (h *deviceHub) pipelineEncodes() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.demand.encoderOff
}

// joinHubPixels is joinHub for a pixel subscriber: it joins the device's hub, or
// starts one, without insisting on the stream parameters in req, falling back to
// the running hub as it is when that hub runs at other parameters. A camera
// whose producer has no tap answers with errPixelsUnavailable.
func (s *VideoService) joinHubPixels(ctx context.Context, key string, req *agentpb.StreamVideoRequest) (*deviceHub, int, chan *videoFrame, error) {
	for attempt := 0; attempt < maxHubRetries; attempt++ {
		hub, id, ch, err := s.getOrCreateHubKind(ctx, key, req, hubHolderStreamClient, pixelFrames)
		if err == nil {
			return hub, id, ch, nil
		}
		if status.Code(err) != codes.InvalidArgument {
			return nil, 0, nil, err
		}
		s.mu.Lock()
		hub, exists := s.hubs[key]
		if !exists || hub.ctx.Err() != nil {
			// The conflicting hub disappeared between the two calls; retry.
			s.mu.Unlock()
			continue
		}
		id, ch, err = hub.subscribePixels()
		s.mu.Unlock()
		if err == nil {
			return hub, id, ch, nil
		}
		if status.Code(err) != codes.Unavailable {
			return nil, 0, nil, err
		}
		// The hub began shutting down between the lookup and subscribe; retry.
	}
	return nil, 0, nil, status.Error(codes.Unavailable, "video device hub churned during subscription")
}
