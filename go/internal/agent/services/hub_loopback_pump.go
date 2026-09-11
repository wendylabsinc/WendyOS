package services

import (
	"context"
	"errors"
	"fmt"
	"sync"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
)

// loopbackFrameWriter is the seam between the pump and the v4l2loopback node.
//
// It exists so the part of this feature that can be reasoned about and tested
// on any platform (which frames are bindable, what the pump records, how drops
// are accounted) is separated from the part that can only be exercised on a
// Linux device with the module loaded (the ioctls themselves). Tests inject a
// fake; the Linux build injects the real V4L2 OUTPUT writer.
type loopbackFrameWriter interface {
	// WriteFrame queues one whole access unit on the node's OUTPUT queue,
	// stamping it with boottimeNanos, and returns the sequence the KERNEL
	// assigned to it.
	//
	// The two directions are deliberate and not symmetric. Returning the
	// kernel's sequence rather than accepting one from the caller is the whole
	// basis of the binding: v4l2loopback overwrites any sequence a writer
	// supplies with its own write_position, and hands the result back on the
	// same ioctl. The timestamp goes the other way, because the module copies a
	// nonzero writer timestamp through to the reader verbatim and flags it as
	// writer-supplied. See hub_loopback_binding.go for the full reasoning.
	//
	// boottimeNanos is the frame's canonical CLOCK_BOOTTIME receipt, the same
	// value the binding and FrameIdentity carry, so the in-band stamp and the
	// control plane can never disagree about a frame.
	WriteFrame(data []byte, boottimeNanos int64) (sequence uint32, err error)
	// Close releases the node. The node itself outlives the writer.
	Close() error
}

// openLoopbackFrameWriter opens a node for writing. Implemented per platform;
// non-Linux builds return an error, which makes the whole data plane a no-op
// there rather than a compile break.
var openLoopbackFrameWriter = openLoopbackFrameWriterPlatform

// errLoopbackDataPlaneUnsupported is returned by the pump when the running build
// or the source cannot support a sound binding. It is deliberately a refusal
// rather than a degraded mode.
var errLoopbackDataPlaneUnsupported = errors.New("two-plane camera data path unavailable on this build")

// hubLoopbackPump feeds one v4l2loopback node from the producer hub and records
// the identity binding for every frame it writes.
//
// It is ONE MORE SUBSCRIBER of the existing hub, exactly like a gRPC sensor
// subscriber or the episode capture adapter. It never opens the camera itself.
// That is the property the whole two-plane design rests on: because the pixels
// on the node came off the same producer that fed episode capture, the frame an
// app scores is provably the frame the episode recorded, which is not true of
// the camera entitlement's independent second reader.
type hubLoopbackPump struct {
	logger *zap.Logger
	// sourceID is the canonical harness identifier of the camera being pumped,
	// used only for logging and to match a node to its producer source.
	sourceID string
	// nodePath is the v4l2loopback node being fed.
	nodePath string
	// bindings is the published mapping this pump maintains. The control plane
	// reads it concurrently.
	bindings *loopbackBindingTable

	// mu guards the identity subscriber set.
	mu     sync.Mutex
	subs   map[int]chan loopbackBinding
	nextID int
}

// identitySubscriberBuffer is the per-subscriber queue depth for the control
// plane. Identities are a few dozen bytes each, so this is cheap, and the depth
// is what lets a briefly stalled gRPC stream catch up without losing any.
const identitySubscriberBuffer = 256

// newHubLoopbackPump creates a pump for one source and node.
func newHubLoopbackPump(logger *zap.Logger, sourceID, nodePath string) *hubLoopbackPump {
	return &hubLoopbackPump{
		logger:   logger,
		sourceID: sourceID,
		nodePath: nodePath,
		bindings: newLoopbackBindingTable(loopbackBindingRetention),
		subs:     map[int]chan loopbackBinding{},
	}
}

// subscribeIdentities registers a control-plane subscriber and returns its
// channel plus a cancel function that unregisters and closes it.
func (p *hubLoopbackPump) subscribeIdentities() (<-chan loopbackBinding, func()) {
	ch := make(chan loopbackBinding, identitySubscriberBuffer)
	p.mu.Lock()
	id := p.nextID
	p.nextID++
	p.subs[id] = ch
	p.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			p.mu.Lock()
			delete(p.subs, id)
			p.mu.Unlock()
			close(ch)
		})
	}
}

// publishIdentity fans one binding out to the control-plane subscribers.
//
// Sends are non-blocking. A subscriber that cannot keep up loses identities
// rather than stalling the pump, because blocking here would stall the DATA
// plane: frames would stop reaching the node for every reader, so one slow gRPC
// client could freeze the camera for everyone. The cost of dropping is bounded
// and fails closed: the app simply cannot resolve those sequences and must treat
// the frames as unidentified, which is the correct outcome. It never yields a
// wrong identity.
func (p *hubLoopbackPump) publishIdentity(b loopbackBinding) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, ch := range p.subs {
		select {
		case ch <- b:
		default:
			p.logger.Warn("two-plane: frame identity subscriber is behind; dropping an identity",
				zap.String("source", p.sourceID), zap.Int("subscriber", id))
		}
	}
}

// Bindings exposes the pump's mapping so the control plane can resolve a
// sequence, and so tests can assert what was recorded.
func (p *hubLoopbackPump) Bindings() *loopbackBindingTable { return p.bindings }

// errLoopbackHubRestarted is pump's internal signal that the hub it was
// consuming was torn down by an episode-capture takeover, so Run should rejoin
// the replacement hub rather than stop the data plane.
var errLoopbackHubRestarted = errors.New("producer hub restarted by episode capture")

// Run joins the hub for src and pumps frames to the node until ctx is done or
// the producer stops.
//
// Stream parameters are deliberately NOT asserted, for the same reason
// SubscribeSensor does not assert them: a data-plane consumer must never take a
// running stream away from a viewer or from episode capture. An empty request
// also matches the parameters episode capture uses by default, so the common
// case joins the one existing hub rather than forcing a second producer.
//
// The reverse direction is allowed: episode capture with explicit campaign
// parameters may restart a producer this pump joined at defaults. The pump is
// a parameter-less subscriber like any other, so it survives the restart by
// rejoining the replacement hub: the stream it was writing to the node ends
// and a new one begins on a random-access unit, never a mid-stream splice of
// different sequence parameter sets into one timeline. The node itself carries
// its picture size in the bitstream's own parameter sets (see
// hub_loopback_writer.go), so the writer stays open across the restart.
func (p *hubLoopbackPump) Run(ctx context.Context, svc *VideoService, src videoSource, devID uint32) error {
	writer, err := openLoopbackFrameWriter(p.nodePath)
	if err != nil {
		return fmt.Errorf("opening loopback node %s: %w", p.nodePath, err)
	}
	defer func() {
		if cerr := writer.Close(); cerr != nil {
			p.logger.Warn("two-plane: closing loopback node failed",
				zap.String("node", p.nodePath), zap.Error(cerr))
		}
	}()

	awaitRandomAccess := false
	// carried accumulates losses the previous subscription could not report on
	// a binding of its own: frames the hub dropped after the last written
	// frame, plus frames the starting gate skipped. The first binding on the
	// replacement hub reports them, so the restart leaves no loss unreported.
	var carried uint64
	for {
		hub, subID, frames, err := svc.joinHub(ctx, src.key, &agentpb.StreamVideoRequest{DeviceId: devID})
		if err != nil {
			return fmt.Errorf("joining producer hub for %s: %w", p.sourceID, err)
		}
		carried, err = p.pumpFrom(ctx, hub, subID, frames, writer, awaitRandomAccess, carried)
		hub.unsubscribe(subID)
		if errors.Is(err, errLoopbackHubRestarted) {
			p.logger.Info("two-plane: producer restarted by episode capture; rejoining the replacement hub",
				zap.String("source", p.sourceID), zap.String("node", p.nodePath))
			awaitRandomAccess = true
			continue
		}
		return err
	}
}

// pump is the frame loop, split out from Run so tests can drive it with a fake
// hub subscription and a fake writer without any device or kernel module.
func (p *hubLoopbackPump) pump(ctx context.Context, hub *deviceHub, subID int, frames <-chan *videoFrame, writer loopbackFrameWriter) error {
	_, err := p.pumpFrom(ctx, hub, subID, frames, writer, false, 0)
	return err
}

// pumpFrom is pump with an explicit starting gate and a carried loss count.
// After a capture takeover the rejoined stream must begin on a random-access
// unit, so the node's new stream starts decodable. Frames skipped by the gate
// never reach the node and record no binding; the next written frame's
// HubDropsBefore carries them, the same way it carries hub-side drops, so no
// loss goes unreported. carried seeds that count with losses inherited from a
// subscription the pump already left (see Run); the value returned alongside
// errLoopbackHubRestarted hands the still-unreported total back for the next
// subscription, including hub drops that accrued after the last written frame.
// On every other return the count is meaningless and returned as zero.
func (p *hubLoopbackPump) pumpFrom(ctx context.Context, hub *deviceHub, subID int, frames <-chan *videoFrame, writer loopbackFrameWriter, awaitRandomAccess bool, carried uint64) (uint64, error) {
	// lastDrops is the hub's running drop total for this subscriber as of the
	// previously WRITTEN frame, so each binding reports the drops since it. This
	// is drop case 1 from hub_loopback_binding.go: those samples never reach the
	// node, so the loopback sequence does not gap and cannot report them. If the
	// pump did not carry this number across, a hub-side loss would be invisible
	// on both planes.
	//
	// One honest limit on the attribution, shared with the gRPC sensor path
	// (cameraSensorSubscription.Next does exactly the same thing): the counter is
	// read when the pump CONSUMES a frame, not when the hub queued it. The
	// subscriber channel is buffered, so a drop that happens while frames are
	// still sitting in that buffer is attributed to the next frame the pump
	// consumes rather than to the frame it actually preceded. The RUNNING TOTAL
	// is exact and no loss is ever unreported; only the frame a given loss is
	// charged to can be off by up to the channel depth. Reporting the drop
	// against the wrong neighbouring frame is tolerable; silently losing the
	// count would not be. Making it exact would mean stamping the drop count onto
	// the frame at broadcast, which is a change to shared hub behaviour that the
	// gRPC sensor path and episode capture would also have to absorb.
	var lastDrops uint64
	// unreported counts losses no binding has reported yet: the count carried
	// in from a previous subscription, plus frames the starting gate skipped
	// after a rejoin. None of them reached the node, so like hub-side drops
	// they are reported on the next frame that did.
	unreported := carried
	for {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case frame, ok := <-frames:
			if !ok {
				if err := hub.terminalErr(); err != nil {
					return 0, err
				}
				if hub.wasRestarted() {
					// Hand back everything still unreported, including hub
					// drops since the last written frame: this subscription
					// will never write another binding, so they must ride on
					// the replacement's first one.
					return unreported + hub.drops(subID) - lastDrops, errLoopbackHubRestarted
				}
				return 0, nil
			}
			if bindable, reason := frameBindableToLoopback(frame); !bindable {
				// Fail closed rather than writing something the sequence cannot
				// name. Publishing a node whose frames have no resolvable identity
				// would hand an app exactly the unprovable join the two-plane path
				// exists to eliminate.
				p.logger.Warn("two-plane: source cannot support a frame identity binding; stopping pump",
					zap.String("source", p.sourceID),
					zap.String("node", p.nodePath),
					zap.String("reason", reason))
				return 0, fmt.Errorf("source %s: %s", p.sourceID, reason)
			}

			if awaitRandomAccess {
				// The rejoined stream must begin decodable. frameBindableToLoopback
				// already guaranteed a whole H.264 access unit, so this only asks
				// whether it is a random-access one.
				if _, randomAccess := frameRandomAccess(frame); !randomAccess {
					unreported++
					continue
				}
				awaitRandomAccess = false
			}

			drops := hub.drops(subID)
			delta := drops - lastDrops + unreported

			// The stamp is the SAME canonical receipt the binding below
			// records and FrameIdentity publishes, read from the one frame
			// object, so the in-band value and the control-plane value cannot
			// drift apart.
			seq, err := writer.WriteFrame(frame.data, frame.receiptBootNanos)
			if err != nil {
				// A failed write is NOT a dropped frame: the kernel did not advance
				// its counter and we record no binding, so the sample is simply
				// absent from both planes rather than appearing as a loss. Keep
				// lastDrops unadvanced so the next successful write still reports
				// every hub drop since the last frame that actually reached the node.
				p.logger.Warn("two-plane: writing frame to loopback node failed",
					zap.String("node", p.nodePath),
					zap.Uint64("sample_id", frame.sampleID),
					zap.Error(err))
				continue
			}
			lastDrops = drops
			unreported = 0

			binding := loopbackBinding{
				LoopbackSequence: seq,
				SampleID:         frame.sampleID,
				BootNanos:        frame.receiptBootNanos,
				UncertaintyNanos: frame.receiptUncertaintyNanos,
				HubDropsBefore:   delta,
			}
			// Record before publishing, so a subscriber that reacts to the
			// identity can always resolve it in the table too.
			p.bindings.Record(binding)
			p.publishIdentity(binding)
		}
	}
}
