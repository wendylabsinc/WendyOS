package services

// Calibrated frames: the agent publishing a measurement instead of a picture.
//
// WendyVideoService multiplexes one camera to many subscribers, and since the
// raw tap it hands the untouched capture bytes to analytic consumers as well as
// H.264 to viewers. That is what lets two apps share one webcam. It carries
// pictures, though, and an RGB-D camera does not produce a picture: it produces
// colour, a distance per colour pixel, and the intrinsics that turn both into
// metres. An app that needs those has had to open the device through the vendor
// SDK itself and lock every other app off the camera.
//
// This service is the other thing the agent can publish. Three properties, none
// of which a consumer can reconstruct for itself:
//
//   - ONE capture instant. Both planes carry the same frame id and timestamp,
//     because pairing is the source's job; two independently timestamped
//     streams are not a frame.
//   - Alignment done ONCE, at the source, from the module's factory extrinsics,
//     which exist nowhere else.
//   - A REFUSAL when a declared requirement cannot be met — before the first
//     frame, and again the moment it stops holding. Never a silent downgrade:
//     losing depth must produce an outage, not confident nonsense.
//
// Fan-out is latest-wins per subscriber, which is the one thing it does
// differently from deviceHub. A viewer that falls behind should see the frames
// it missed; a depth consumer should not. A measurement's value is that it
// describes NOW, so a slow analytics subscriber is given the newest frame
// rather than the head of an aging backlog — and never stalls the subscriber
// sharing its camera.
//
// See specs/2026-09-20-calibrated-frame-sensor-source-design.md.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/framesource"
	"github.com/wendylabsinc/wendy/go/internal/shared/streamreason"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// defaultGRPCRecvBytes is grpc-go's default per-message receive limit. One
// calibrated frame is one message, so a client that has not raised its limit
// can only be served sources whose frames fit inside it.
const defaultGRPCRecvBytes = 4 * 1024 * 1024

// maxCalibratedSubscribers caps concurrent subscribers on one source, for the
// same reason maxSubscribersPerHub does on a camera: bounded per-subscriber
// memory and bounded publish work.
const maxCalibratedSubscribers = 16

// CalibratedFrameService serves WendyCalibratedFrameService.
type CalibratedFrameService struct {
	agentpbv2.UnimplementedWendyCalibratedFrameServiceServer
	logger   *zap.Logger
	registry *framesource.Registry

	mu   sync.Mutex
	hubs map[string]*frameHub
}

// NewCalibratedFrameService builds the service over a source registry.
func NewCalibratedFrameService(logger *zap.Logger, registry *framesource.Registry) *CalibratedFrameService {
	return &CalibratedFrameService{logger: logger, registry: registry, hubs: map[string]*frameHub{}}
}

func (s *CalibratedFrameService) ListCalibratedSources(ctx context.Context, _ *agentpbv2.ListCalibratedSourcesRequest) (*agentpbv2.ListCalibratedSourcesResponse, error) {
	sources, err := s.registry.Describe(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing calibrated frame sources: %v", err)
	}
	return &agentpbv2.ListCalibratedSourcesResponse{Sources: sources}, nil
}

func (s *CalibratedFrameService) StreamCalibratedFrames(req *agentpbv2.StreamCalibratedFramesRequest, stream grpc.ServerStreamingServer[agentpbv2.CalibratedFrame]) error {
	ctx := stream.Context()
	require := framesource.Normalise(req.GetRequire())

	src, desc, err := s.registry.Lookup(ctx, req.GetSource(), require)
	if err != nil {
		switch {
		case errors.Is(err, framesource.ErrNoSuchSource):
			return status.Error(codes.NotFound, err.Error())
		case errors.Is(err, framesource.ErrAmbiguousSource):
			return status.Error(codes.InvalidArgument, err.Error())
		default:
			return status.Errorf(codes.Internal, "resolving calibrated frame source: %v", err)
		}
	}

	// A source that is present but cannot be opened — in practice a RealSense
	// on an image with no capture helper — is refused by name. Reporting it as
	// "no depth camera" is the class of silence this service exists to remove.
	if !desc.GetAvailable() {
		return errCalibratedSourceUnavailable(desc)
	}
	if err := refuseUnmetRequirements(desc, require); err != nil {
		return err
	}
	opts := framesource.Options{Width: req.GetWidth(), Height: req.GetHeight(), Framerate: req.GetFramerate()}
	if err := refuseOversizedFrames(desc, opts, req.GetMaxFrameBytes()); err != nil {
		return err
	}

	hub, id, sub, err := s.getOrCreateHub(desc.GetSource(), src, opts, require)
	if err != nil {
		return err
	}
	defer hub.unsubscribe(id)

	return pumpCalibratedFrames(ctx, stream, hub, sub)
}

// getOrCreateHub joins the capture already running for this source, or starts
// one. One capture per source, however many subscribers: taking the camera
// twice is what this service exists to stop.
func (s *CalibratedFrameService) getOrCreateHub(name string, src framesource.Source, opts framesource.Options, require []agentpbv2.FrameRequirement) (*frameHub, int, *frameSub, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if hub, ok := s.hubs[name]; ok {
		// A request that asks for nothing joins whatever is playing, exactly as
		// a bare `camera view` does. One that asks for a DIFFERENT geometry is
		// refused with what is running, rather than silently served frames of
		// another size or restarting a capture other subscribers depend on.
		if !opts.IsDefault() && opts != hub.opts {
			return nil, 0, nil, status.Errorf(codes.FailedPrecondition,
				"%s is already capturing at %s for another subscriber; request that geometry or omit it to join",
				name, describeOptions(hub.opts))
		}
		// The descriptor the capture actually negotiated is stricter than the
		// listing's promise. Check against it when there is one.
		if negotiated := hub.negotiatedDesc(); negotiated != nil {
			if err := refuseUnmetRequirements(negotiated, require); err != nil {
				return nil, 0, nil, err
			}
		}
		id, sub, err := hub.subscribe(require)
		if err != nil {
			return nil, 0, nil, err
		}
		return hub, id, sub, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	hub := &frameHub{
		subs:   map[int]*frameSub{},
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
		opts:   opts,
		source: name,
	}
	id, sub, err := hub.subscribe(require)
	if err != nil {
		cancel()
		return nil, 0, nil, err
	}
	s.hubs[name] = hub
	go s.runProducer(hub, src, name, opts)
	return hub, id, sub, nil
}

func (s *CalibratedFrameService) dropHub(name string, hub *frameHub) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hubs[name] == hub {
		delete(s.hubs, name)
	}
}

// runProducer owns the capture for one source: it opens it, sanitises each
// frame once before fan-out, and tears the hub down when it ends.
func (s *CalibratedFrameService) runProducer(hub *frameHub, src framesource.Source, name string, opts framesource.Options) {
	defer close(hub.done)
	defer s.dropHub(name, hub)

	stream, err := src.Open(hub.ctx, opts)
	if err != nil {
		hub.finish(openFailure(name, err))
		return
	}
	// Close on cancellation as well as on return: a helper blocked in a read
	// does not notice a context until the pipe under it goes away.
	closed := make(chan struct{})
	go func() {
		select {
		case <-hub.ctx.Done():
			_ = stream.Close()
		case <-closed:
		}
	}()
	defer func() {
		close(closed)
		_ = stream.Close()
	}()

	if n, ok := stream.(interface {
		Negotiated() *agentpbv2.CalibratedSource
	}); ok {
		hub.setNegotiated(n.Negotiated())
	}

	warned := false
	for {
		frame, err := stream.Next(hub.ctx)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
				hub.finish(nil)
				return
			}
			hub.finish(status.Errorf(codes.Unavailable, "calibrated frame source %s ended: %v", name, err))
			return
		}
		// Once, in the producer, so every subscriber sees the same frame and no
		// consumer re-derives the rules.
		if dropped := framesource.Sanitise(frame); len(dropped) > 0 && !warned {
			warned = true
			s.logger.Warn("calibrated frame source sent a plane that does not describe itself; dropping it",
				zap.String("source", name), zap.Strings("dropped", dropped))
		}
		if frame.GetSource() == "" {
			frame.Source = name
		}
		if !hub.publish(frame) {
			return // nobody left
		}
	}
}

// openFailure turns a source that would not open into an error naming the fix.
func openFailure(name string, err error) error {
	if errors.Is(err, framesource.ErrHelperNotInstalled) {
		return streamreason.New(codes.FailedPrecondition,
			fmt.Sprintf("calibrated frame source %s cannot be opened: %v", name, err),
			streamreason.CalibratedSourceUnavailable,
			map[string]string{"source": name, "helper": framesource.HelperName})
	}
	return status.Errorf(codes.FailedPrecondition, "calibrated frame source %s could not be opened: %v", name, err)
}

// pumpCalibratedFrames delivers this subscriber's latest frame until it is
// closed. It never blocks the producer: publish overwrites the slot.
func pumpCalibratedFrames(ctx context.Context, stream grpc.ServerStreamingServer[agentpbv2.CalibratedFrame], hub *frameHub, sub *frameSub) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-sub.notify:
			if !ok {
				// Closed by the producer: either the capture ended or this
				// subscriber's requirements stopped being met.
				return hub.subscriberErr(sub)
			}
			frame := hub.take(sub)
			if frame == nil {
				continue
			}
			if err := stream.Send(frame); err != nil {
				return err
			}
		}
	}
}

// --- the hub ---

// frameHub fans one capture out to many subscribers, latest-wins.
type frameHub struct {
	mu         sync.Mutex
	subs       map[int]*frameSub
	nextID     int
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	err        error
	finished   bool
	opts       framesource.Options
	source     string
	negotiated *agentpbv2.CalibratedSource
}

// frameSub is one subscriber's depth-1 slot. latest is overwritten on arrival —
// that IS the latest-wins policy. notify is a single-slot doorbell, not a
// queue: it says "there is something new", never how much.
type frameSub struct {
	latest  *agentpbv2.CalibratedFrame
	notify  chan struct{}
	require []agentpbv2.FrameRequirement
	err     error
	closed  bool
}

func (h *frameHub) subscribe(require []agentpbv2.FrameRequirement) (int, *frameSub, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.finished {
		if h.err != nil {
			return 0, nil, h.err
		}
		return 0, nil, status.Error(codes.Unavailable, "calibrated frame capture is shutting down")
	}
	if len(h.subs) >= maxCalibratedSubscribers {
		return 0, nil, status.Errorf(codes.ResourceExhausted,
			"too many concurrent calibrated frame streams for this source (max %d)", maxCalibratedSubscribers)
	}
	id := h.nextID
	h.nextID++
	sub := &frameSub{notify: make(chan struct{}, 1), require: require}
	h.subs[id] = sub
	return id, sub, nil
}

func (h *frameHub) unsubscribe(id int) {
	h.mu.Lock()
	delete(h.subs, id)
	empty := len(h.subs) == 0
	h.mu.Unlock()
	if empty {
		h.cancel()
	}
}

// publish hands a frame to every subscriber that can still use it, and ends the
// ones that cannot. Returns false when nobody is left.
func (h *frameHub) publish(frame *agentpbv2.CalibratedFrame) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) == 0 {
		return false
	}
	live := 0
	for _, sub := range h.subs {
		if sub.closed {
			continue
		}
		// The rule that turns a silent degradation into a visible outage: a
		// property this subscriber requires that this frame does not carry ends
		// the stream, rather than continuing without it.
		if missing := framesource.MissingFromFrame(frame, sub.require); len(missing) > 0 {
			h.closeSubLocked(sub, errRequirementLost(h.source, frame, missing))
			continue
		}
		live++
		sub.latest = frame
		select {
		case sub.notify <- struct{}{}:
		default: // the doorbell is already ringing; latest is what changed
		}
	}
	return live > 0
}

// take reads and clears a subscriber's slot.
func (h *frameHub) take(sub *frameSub) *agentpbv2.CalibratedFrame {
	h.mu.Lock()
	defer h.mu.Unlock()
	frame := sub.latest
	sub.latest = nil
	return frame
}

func (h *frameHub) subscriberErr(sub *frameSub) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if sub.err != nil {
		return sub.err
	}
	return h.err
}

func (h *frameHub) closeSubLocked(sub *frameSub, err error) {
	if sub.closed {
		return
	}
	sub.closed = true
	sub.err = err
	close(sub.notify)
}

// finish ends every subscriber with err (nil for a clean end of capture).
func (h *frameHub) finish(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.finished = true
	if h.err == nil {
		h.err = err
	}
	for _, sub := range h.subs {
		h.closeSubLocked(sub, err)
	}
}

func (h *frameHub) setNegotiated(desc *agentpbv2.CalibratedSource) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.negotiated = desc
}

func (h *frameHub) negotiatedDesc() *agentpbv2.CalibratedSource {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.negotiated
}

// --- refusals ---

// refuseUnmetRequirements is the up-front check: every requirement, named, with
// a sentence each saying why this source cannot provide it.
func refuseUnmetRequirements(desc *agentpbv2.CalibratedSource, require []agentpbv2.FrameRequirement) error {
	missing := framesource.MissingFromSource(desc, require)
	if len(missing) == 0 {
		return nil
	}
	metadata := map[string]string{
		"source":  desc.GetSource(),
		"missing": framesource.Slugs(missing),
	}
	reasons := make([]string, 0, len(missing))
	for _, r := range missing {
		why := framesource.WhySourceLacks(desc, r)
		metadata[framesource.Slug(r)] = why
		reasons = append(reasons, fmt.Sprintf("%s (%s)", framesource.Slug(r), why))
	}
	return streamreason.New(codes.FailedPrecondition,
		fmt.Sprintf("calibrated frame source %s cannot provide: %s", desc.GetSource(), joinSentences(reasons)),
		streamreason.RequirementUnmet, metadata)
}

// errRequirementLost is the same refusal for a property that STOPPED holding.
// The wording distinguishes the two: this is not a camera that never could, it
// is one that stopped, and the operator's next move is different.
func errRequirementLost(source string, frame *agentpbv2.CalibratedFrame, missing []agentpbv2.FrameRequirement) error {
	metadata := map[string]string{
		"source":   source,
		"missing":  framesource.Slugs(missing),
		"frame_id": strconv.FormatUint(frame.GetFrameId(), 10),
	}
	reasons := make([]string, 0, len(missing))
	for _, r := range missing {
		why := framesource.WhyFrameLacks(frame, r)
		metadata[framesource.Slug(r)] = why
		reasons = append(reasons, fmt.Sprintf("%s (%s)", framesource.Slug(r), why))
	}
	return streamreason.New(codes.FailedPrecondition,
		fmt.Sprintf("calibrated frame source %s stopped providing: %s", source, joinSentences(reasons)),
		streamreason.RequirementUnmet, metadata)
}

func errCalibratedSourceUnavailable(desc *agentpbv2.CalibratedSource) error {
	metadata := map[string]string{"source": desc.GetSource()}
	reason := desc.GetUnavailableReason()
	if reason == "" {
		reason = "the source is attached but cannot be opened"
	}
	if desc.GetKind() == framesource.KindRealSense {
		metadata["helper"] = framesource.HelperName
	}
	return streamreason.New(codes.FailedPrecondition,
		fmt.Sprintf("calibrated frame source %s is not available: %s", desc.GetSource(), reason),
		streamreason.CalibratedSourceUnavailable, metadata)
}

// refuseOversizedFrames turns "this stream dies on its first Recv" into a
// refusal at subscribe time, with both numbers. One frame is one gRPC message
// and the default receive limit is 4 MiB, which a 1080p colour plane alone
// exceeds — so this is a real case, not a defensive one.
func refuseOversizedFrames(desc *agentpbv2.CalibratedSource, opts framesource.Options, clientLimit uint64) error {
	limit := clientLimit
	if limit == 0 {
		limit = defaultGRPCRecvBytes
	}
	frameBytes := framesource.FrameBytesFor(desc, opts)
	if frameBytes == 0 || frameBytes <= limit {
		return nil
	}
	return streamreason.New(codes.FailedPrecondition,
		fmt.Sprintf("one %s frame at %s is up to %d bytes and this client accepts %d; "+
			"raise the receive limit (grpc.MaxCallRecvMsgSize) or ask for a smaller capture size",
			desc.GetSource(), describeGeometry(desc, opts), frameBytes, limit),
		streamreason.FrameTooLarge,
		map[string]string{
			"source":      desc.GetSource(),
			"frame_bytes": strconv.FormatUint(frameBytes, 10),
			"limit_bytes": strconv.FormatUint(limit, 10),
		})
}

func describeGeometry(desc *agentpbv2.CalibratedSource, opts framesource.Options) string {
	if opts.Width == 0 || opts.Height == 0 {
		return fmt.Sprintf("%dx%d", desc.GetColourWidth(), desc.GetColourHeight())
	}
	return fmt.Sprintf("%dx%d", opts.Width, opts.Height)
}

func describeOptions(o framesource.Options) string {
	if o.IsDefault() {
		return "the source's default geometry"
	}
	return fmt.Sprintf("%dx%d@%d", o.Width, o.Height, o.Framerate)
}

func joinSentences(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	default:
		out := parts[0]
		for _, p := range parts[1:] {
			out += "; " + p
		}
		return out
	}
}
