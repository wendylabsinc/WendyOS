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
	"strings"
	"sync"
	"time"

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

	// ctx is cancelled by Shutdown and every hub's context derives from it, so
	// a SIGTERM ends each capture helper instead of waiting on subscribers that
	// may never disconnect. wg tracks the producers, so Shutdown returns only
	// once every camera has actually been released.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu   sync.Mutex
	hubs map[string]*frameHub
}

// NewCalibratedFrameService builds the service over a source registry. Captures
// are tied to ctx; call Shutdown to end them and wait for their helpers to exit.
func NewCalibratedFrameService(ctx context.Context, logger *zap.Logger, registry *framesource.Registry) *CalibratedFrameService {
	svcCtx, cancel := context.WithCancel(ctx)
	return &CalibratedFrameService{
		logger:   logger,
		registry: registry,
		ctx:      svcCtx,
		cancel:   cancel,
		hubs:     map[string]*frameHub{},
	}
}

// Shutdown ends every capture and waits until each has released its camera.
// It mirrors VideoService.Shutdown for the same reason: GracefulStop waits for
// open streams, and a subscriber that never disconnects would otherwise hold
// the agent -- and the helper holding the camera -- up indefinitely.
func (s *CalibratedFrameService) Shutdown() {
	s.cancel()
	s.wg.Wait()
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
	opts := framesource.Options{Width: req.GetWidth(), Height: req.GetHeight(), Framerate: req.GetFramerate()}
	limit := req.GetMaxFrameBytes()

	// A named source whose capture is already running is joined without
	// enumerating. Enumeration forks the capture helper, and a second helper
	// querying a camera the first is streaming from is exactly the disturbance
	// the Provider contract forbids; the running hub already holds everything
	// a join needs.
	if name := req.GetSource(); name != "" {
		if hub, id, sub, err := s.joinRunning(name, opts, require, limit); hub != nil {
			if err != nil {
				return err
			}
			defer hub.unsubscribe(id)
			return pumpCalibratedFrames(ctx, stream, hub, sub)
		}
	}

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
	// Against the listing, before the camera is touched. The running capture's
	// own descriptor is checked again on join, and by the producer once the
	// helper has reported what it actually opened.
	if err := refuseUnmetRequirements(desc, require); err != nil {
		return err
	}

	hub, id, sub, err := s.getOrCreateHub(ctx, desc, src, opts, require, limit)
	if err != nil {
		return err
	}
	defer hub.unsubscribe(id)

	return pumpCalibratedFrames(ctx, stream, hub, sub)
}

// joinRunning admits a subscriber to the live capture for name, if there is
// one. A nil hub means there is nothing to join -- no capture, or one that is
// ending -- and the caller resolves the source the long way.
func (s *CalibratedFrameService) joinRunning(name string, opts framesource.Options, require []agentpbv2.FrameRequirement, clientLimit uint64) (*frameHub, int, *frameSub, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hub, ok := s.hubs[name]
	if !ok {
		return nil, 0, nil, nil
	}
	id, sub, err := hub.join(opts, require, clientLimit)
	if errors.Is(err, errHubEnding) {
		return nil, 0, nil, nil
	}
	return hub, id, sub, err
}

// getOrCreateHub joins the capture already running for this source, or starts
// one. One capture per source, however many subscribers: taking the camera
// twice is what this service exists to stop.
//
// A hub whose last subscriber just left, or whose capture just failed, is
// still in the map until its producer returns. Joining it would attach the
// newcomer to a capture that is ending: the producer's next Next returns
// Canceled, finish closes the newcomer with no error, and the client sees a
// clean EOF with zero frames. So an ending hub is evicted, its producer waited
// for (the helper must exit before a fresh one can claim the camera), and the
// lookup retried -- the same dance VideoService.getOrCreateHub does.
func (s *CalibratedFrameService) getOrCreateHub(ctx context.Context, desc *agentpbv2.CalibratedSource, src framesource.Source, opts framesource.Options, require []agentpbv2.FrameRequirement, clientLimit uint64) (*frameHub, int, *frameSub, error) {
	name := desc.GetSource()
	for retries := 0; ; retries++ {
		if retries >= maxHubRetries {
			s.logger.Warn("calibrated hub retry limit exceeded", zap.String("source", name))
			return nil, 0, nil, status.Errorf(codes.Unavailable,
				"calibrated frame source %s is temporarily unavailable, please retry", name)
		}
		s.mu.Lock()
		hub, exists := s.hubs[name]
		if !exists {
			break // s.mu is held: the hub is created below
		}
		id, sub, err := hub.join(opts, require, clientLimit)
		if !errors.Is(err, errHubEnding) {
			s.mu.Unlock()
			if err != nil {
				return nil, 0, nil, err
			}
			return hub, id, sub, nil
		}
		delete(s.hubs, name)
		s.mu.Unlock()
		if err := s.awaitTeardown(ctx, hub); err != nil {
			return nil, 0, nil, err
		}
	}

	hctx, cancel := context.WithCancel(s.ctx)
	hub := &frameHub{
		subs:    map[int]*frameSub{},
		ctx:     hctx,
		cancel:  cancel,
		done:    make(chan struct{}),
		opts:    opts,
		source:  name,
		listing: desc,
	}
	// The same admission as a joiner gets, against the listing: a fresh hub
	// runs exactly what was asked for, so the checks reduce to the size gate
	// and the requirement check -- and to "is the agent shutting down".
	id, sub, err := hub.join(opts, require, clientLimit)
	if err != nil {
		cancel()
		s.mu.Unlock()
		if errors.Is(err, errHubEnding) {
			return nil, 0, nil, status.Error(codes.Unavailable, "the agent is shutting down")
		}
		return nil, 0, nil, err
	}
	s.hubs[name] = hub
	s.mu.Unlock()

	s.wg.Add(1)
	go s.runProducer(hub, src, name, opts)
	return hub, id, sub, nil
}

// awaitTeardown waits for an ending hub's producer to return, which is when its
// helper has exited and the camera is free. Bounded: a producer that will not
// die must not hold every new subscriber hostage, and the open that follows
// reports its own failure if the camera is still held.
func (s *CalibratedFrameService) awaitTeardown(ctx context.Context, hub *frameHub) error {
	timer := time.NewTimer(hubTeardownTimeout)
	defer timer.Stop()
	select {
	case <-hub.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		s.logger.Warn("timed out waiting for calibrated capture teardown", zap.String("source", hub.source))
		return nil
	}
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
	defer s.wg.Done()
	defer close(hub.done)
	defer s.dropHub(name, hub)
	// Whichever path returns, nobody is left waiting on a doorbell that will
	// never ring: a subscriber that slipped in after the last one left and
	// before the hub was dropped is closed here rather than hung forever.
	defer hub.finish(nil)

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
		if desc := n.Negotiated(); desc != nil {
			hub.setNegotiated(desc)
		}
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
		// A colour plane that does not carry the bytes its geometry claims is
		// not a frame with a flaw in it; it is not a frame. Every consumer
		// slices colour by that geometry. Ending the capture is the loud answer:
		// silently skipping such frames would leave every subscriber waiting on
		// a stream that never delivers, which is the failure this service exists
		// to remove.
		if why := framesource.ColourIsUnusable(frame); why != "" {
			hub.finish(status.Errorf(codes.Unavailable,
				"calibrated frame source %s sent a colour plane that does not describe itself (%s); the capture was stopped",
				name, why))
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
			return // nobody left; publish marked the hub finished under its lock
		}
	}
}

// openFailure turns a source that would not open into an error naming the fix.
//
// On a RealSense the commonest reason after a missing helper is that
// StreamVideo already holds the camera's colour node: the helper's pipeline
// claims that node, and nothing coordinates the two (§14 of the design spec).
// librealsense's own message for that is not something an operator can act
// on, so the refusal names the other stream, as errCameraInUse names this one.
func openFailure(name string, err error) error {
	if errors.Is(err, framesource.ErrHelperNotInstalled) {
		return streamreason.New(codes.FailedPrecondition,
			fmt.Sprintf("calibrated frame source %s cannot be opened: %v", name, err),
			streamreason.CalibratedSourceUnavailable,
			map[string]string{"source": name, "helper": framesource.HelperName})
	}
	msg := fmt.Sprintf("calibrated frame source %s could not be opened: %v", name, err)
	if strings.HasPrefix(name, framesource.KindRealSense+":") {
		msg += ". If a StreamVideo subscriber (`wendy device camera view`, the companion app) is streaming from this camera's " +
			"colour node, that stream holds it: a RealSense cannot yet be shared between StreamVideo and calibrated frames. " +
			"Stop that stream, then retry"
	}
	return status.Error(codes.FailedPrecondition, msg)
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

// errHubEnding is join's answer for a hub whose capture is ending: its last
// subscriber left, its producer finished, or the service is shutting down.
// Internal -- the caller either waits for the teardown and starts afresh, or
// turns it into a status.
var errHubEnding = errors.New("calibrated frame hub is ending")

// frameHub fans one capture out to many subscribers, latest-wins.
type frameHub struct {
	mu       sync.Mutex
	subs     map[int]*frameSub
	nextID   int
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	err      error
	finished bool
	opts     framesource.Options
	source   string
	// listing is what the source promised when it was enumerated; negotiated is
	// what the capture actually opened, once the producer has been told. A
	// subscriber is checked against negotiated whenever there is one: it is the
	// stricter of the two, and the one the frames will actually match.
	listing    *agentpbv2.CalibratedSource
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

// join admits a subscriber, checked against what is RUNNING rather than what
// the listing promised: a geometry other than the running one is refused with
// the running one named; requirements are checked against the negotiated
// descriptor once the capture has reported it; and the size gate uses the
// running capture's frame size, because that is what this subscriber will be
// sent -- a default-geometry request joining a 1080p capture is a 1080p
// subscriber whatever the listing's default says.
func (h *frameHub) join(opts framesource.Options, require []agentpbv2.FrameRequirement, clientLimit uint64) (int, *frameSub, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.finished || h.ctx.Err() != nil {
		return 0, nil, errHubEnding
	}
	// A request that asks for nothing joins whatever is playing, exactly as a
	// bare `camera view` does. One that asks for a DIFFERENT geometry is
	// refused with what is running, rather than silently served frames of
	// another size or restarting a capture other subscribers depend on.
	if !opts.IsDefault() && !h.runsLocked(opts) {
		return 0, nil, status.Errorf(codes.FailedPrecondition,
			"%s is already capturing at %s for another subscriber; %s",
			h.source, h.describeRunningLocked(), h.joinAdviceLocked())
	}
	desc, runningOpts := h.runningLocked()
	if err := refuseUnmetRequirements(desc, require); err != nil {
		return 0, nil, err
	}
	if err := refuseOversizedFrames(desc, runningOpts, clientLimit); err != nil {
		return 0, nil, err
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

// runningLocked is the descriptor and geometry the capture is actually
// producing. The negotiated descriptor describes it exactly -- its size fields
// and max_frame_bytes are the capture's own -- so it needs no scaling; before
// the capture has reported, it is the listing scaled by the geometry the
// capture was started with.
func (h *frameHub) runningLocked() (*agentpbv2.CalibratedSource, framesource.Options) {
	if h.negotiated != nil {
		return h.negotiated, framesource.Options{}
	}
	return h.listing, h.opts
}

// runsLocked reports whether a named geometry is the one this capture runs. A
// capture started with an explicit geometry runs exactly that. One started
// with defaults runs whatever the source chose, which the negotiated descriptor
// names -- width and height, that is. The descriptor carries no framerate, so
// a joiner naming one on a default-started capture is asking for something
// that cannot be confirmed, and is told to leave it out.
func (h *frameHub) runsLocked(opts framesource.Options) bool {
	if !h.opts.IsDefault() {
		return opts == h.opts
	}
	n := h.negotiated
	return n != nil && opts.Framerate == 0 &&
		opts.Width == n.GetColourWidth() && opts.Height == n.GetColourHeight()
}

func (h *frameHub) describeRunningLocked() string {
	if !h.opts.IsDefault() {
		return describeOptions(h.opts)
	}
	if n := h.negotiated; n != nil {
		return fmt.Sprintf("%dx%d at the source's default framerate", n.GetColourWidth(), n.GetColourHeight())
	}
	return "the source's default geometry"
}

func (h *frameHub) joinAdviceLocked() string {
	switch {
	case !h.opts.IsDefault():
		return "request that geometry or omit it to join"
	case h.negotiated != nil:
		return "request that size without a framerate, or omit the geometry, to join"
	default:
		return "omit the geometry to join"
	}
}

func (h *frameHub) unsubscribe(id int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, id)
	if len(h.subs) == 0 {
		// Under the lock, so a join that comes next sees a cancelled context
		// rather than attaching to a capture that is about to stop.
		h.cancel()
	}
}

// publish hands a frame to every subscriber that can still use it, and ends the
// ones that cannot. Returns false when nobody is left.
func (h *frameHub) publish(frame *agentpbv2.CalibratedFrame) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
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
	if live == 0 {
		// Nobody left, and the producer is about to return. Marked here, under
		// the same lock a join takes, so a subscriber arriving between now and
		// dropHub is told to wait for the teardown instead of ringing a bell
		// nobody will answer.
		h.finished = true
		return false
	}
	return true
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

// setNegotiated records what the capture actually opened and re-checks every
// subscriber against it. The first subscriber was admitted on the listing's
// promise, before the helper had said anything; a helper that opened a mode
// without depth would otherwise hand that subscriber a depth-less first frame
// and an error saying the source STOPPED providing depth, when the truth is
// that this capture never could.
func (h *frameHub) setNegotiated(desc *agentpbv2.CalibratedSource) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.negotiated = desc
	for _, sub := range h.subs {
		if sub.closed {
			continue
		}
		if err := refuseUnmetRequirements(desc, sub.require); err != nil {
			h.closeSubLocked(sub, err)
		}
	}
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
