package services

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// The two-plane data path's node and pump lifecycle.
//
// This deliberately mirrors the shape of the network-camera loopback wiring
// (EnsureCameraNodes / SetCameraContainerConsumers, backed by ipcam.Loopback)
// rather than introducing a second, differently-shaped mechanism: demand comes
// from container truth, is replaced wholesale rather than incrementally, and is
// entirely best-effort so a data-plane problem can never break container
// management.
//
// What is new is where the frames come from. The network-camera pumps source an
// RTSP stream through GStreamer; these source the agent's OWN producer hub, so
// the node carries exactly the frames episode capture and the gRPC sensor
// subscribers see.

// FrameIdentitySample is one frame identity handed to the control plane. It is
// the harness-internal form of appspbv1.FrameIdentity and carries no pixels.
type FrameIdentitySample struct {
	SourceID         string
	SampleID         uint64
	BootNanos        int64
	UncertaintyNanos int64
	// LoopbackSequence is the kernel-assigned v4l2_buffer.sequence for this
	// frame on the node. It is the join key an app matches against the buffer it
	// dequeued.
	LoopbackSequence uint32
	DroppedBefore    uint64
	NodePath         string
}

// frameIdentitySubscription is one live subscription to a source's frame
// identities.
type frameIdentitySubscription interface {
	Next(ctx context.Context) (FrameIdentitySample, error)
	Close()
}

// errNoDataPlane reports that a source has no v4l2loopback node, which is the
// normal state rather than a fault: a node exists only while an app holding the
// camera entitlement is running, and only for a source whose frames can be
// identified frame-for-frame.
var errNoDataPlane = errors.New("source has no two-plane data path")

// twoPlaneNode is one running node plus the pump feeding it.
type twoPlaneNode struct {
	pump   *hubLoopbackPump
	cancel context.CancelFunc
	done   chan struct{}
	nodeNr int
	path   string
}

// SetTwoPlaneContainerConsumers replaces, wholesale, the set of running
// containers entitled to the two-plane camera path, starting or stopping nodes
// and pumps to match.
//
// The caller decides entitlement (see containerd.twoPlaneConsumerNames, which
// requires BOTH sensors and camera). This end only honours the resulting demand.
// Demand is not per-source, matching the existing camera loopback behaviour: the
// entitlements are device-wide, not per-device-node.
func (s *VideoService) SetTwoPlaneContainerConsumers(ctx context.Context, containerIDs []string) {
	fingerprint := consumerFingerprint(containerIDs)
	s.twoPlaneMu.Lock()
	s.twoPlaneDemand = len(containerIDs) > 0
	demand := s.twoPlaneDemand
	if s.twoPlaneConsumers != fingerprint {
		// A different set of entitled containers is a different question, so
		// earlier refusals stop counting: the new consumer deserves one honest
		// attempt at every source rather than inheriting a verdict reached for
		// somebody else.
		s.twoPlaneConsumers = fingerprint
		s.twoPlaneRefused = nil
	}
	s.twoPlaneMu.Unlock()

	if !demand {
		s.stopAllTwoPlane()
		return
	}
	s.ensureTwoPlaneForLocalCameras(ctx)
}

// ensureTwoPlaneForLocalCameras starts a node and pump for every local camera
// that does not already have one.
//
// Only local V4L2 cameras are candidates. A network camera already reaches
// containers through the existing ipcam loopback path, and its producer
// delivers an unaligned byte stream that frameBindableToLoopback refuses
// anyway, so it could not carry a frame identity binding.
func (s *VideoService) ensureTwoPlaneForLocalCameras(ctx context.Context) {
	if s.loopback == nil {
		return
	}
	devices, err := s.listCameras(ctx)
	if err != nil {
		s.logger.Warn("two-plane: listing cameras failed", zap.Error(err))
		return
	}
	local := make([]*agentpb.VideoDevice, 0, len(devices))
	candidates := make([]string, 0, len(devices))
	for _, dev := range devices {
		if dev.GetId() >= uint32(ipcam.IDBandStart) {
			continue // a network camera, served by the existing loopback path
		}
		local = append(local, dev)
		candidates = append(candidates, "v4l2:"+dev.GetPath())
	}
	s.forgetStaleTwoPlaneRefusals(candidates)

	for _, dev := range local {
		sourceID := "v4l2:" + dev.GetPath()
		if s.twoPlaneSourceRefused(sourceID) {
			// The pump already refused this source's frames as unbindable, and
			// that is a property of the producer's pipeline rather than a
			// transient fault. Recreating the node every sweep would start and
			// tear down the camera pipeline forever, producing nothing.
			continue
		}
		if err := s.ensureTwoPlaneNode(ctx, sourceID, dev.GetId()); err != nil {
			s.logger.Warn("two-plane: starting data path failed",
				zap.String("source", sourceID), zap.Error(err))
		}
	}
}

// consumerFingerprint reduces a set of container IDs to one comparable string,
// order-independent, so a re-ordered but unchanged set is not read as a change.
func consumerFingerprint(containerIDs []string) string {
	ids := append([]string(nil), containerIDs...)
	sort.Strings(ids)
	return strings.Join(ids, "\x00")
}

// twoPlaneSourceRefused reports whether the pump has already refused this
// source's frames as unbindable.
func (s *VideoService) twoPlaneSourceRefused(sourceID string) bool {
	s.twoPlaneMu.Lock()
	defer s.twoPlaneMu.Unlock()
	return s.twoPlaneRefused[sourceID]
}

// noteTwoPlaneRefusal records that a source's pump refused its frames, so
// later sweeps skip it instead of rebuilding a data path that cannot work.
func (s *VideoService) noteTwoPlaneRefusal(sourceID string) {
	s.twoPlaneMu.Lock()
	defer s.twoPlaneMu.Unlock()
	if s.twoPlaneRefused == nil {
		s.twoPlaneRefused = map[string]bool{}
	}
	s.twoPlaneRefused[sourceID] = true
}

// forgetStaleTwoPlaneRefusals drops refusals for sources the current device
// list no longer offers. A camera that was unplugged and plugged back in, or
// replaced by a different one on the same node, is a new producer and gets a
// fresh attempt; a source still present keeps its verdict.
func (s *VideoService) forgetStaleTwoPlaneRefusals(present []string) {
	s.twoPlaneMu.Lock()
	defer s.twoPlaneMu.Unlock()
	if len(s.twoPlaneRefused) == 0 {
		return
	}
	live := make(map[string]bool, len(present))
	for _, id := range present {
		live[id] = true
	}
	for id := range s.twoPlaneRefused {
		if !live[id] {
			delete(s.twoPlaneRefused, id)
		}
	}
}

// ensureTwoPlaneNode allocates a node and starts its pump for one source, if it
// does not already have one running.
//
// Node creation is serialised across sources by twoPlaneStartMu. The aux node
// number comes from the loopback module's own free-number search, which cannot
// see a number this service has picked but not yet created: two concurrent
// sweeps would be handed the SAME number, both create it, and the second one
// to fail its map insertion would remove the node the first is pumping into.
// Serialising the allocate-and-create step is what makes the number reservation
// atomic. The lock is never held while twoPlaneMu is held, and node creation is
// rare and off the frame path, so the cost is nil.
func (s *VideoService) ensureTwoPlaneNode(ctx context.Context, sourceID string, devID uint32) error {
	s.twoPlaneStartMu.Lock()
	defer s.twoPlaneStartMu.Unlock()

	s.twoPlaneMu.Lock()
	if s.twoPlane == nil {
		s.twoPlane = map[string]*twoPlaneNode{}
	}
	if _, running := s.twoPlane[sourceID]; running {
		s.twoPlaneMu.Unlock()
		return nil
	}
	s.twoPlaneMu.Unlock()

	src, err := s.resolveSource(devID)
	if err != nil {
		return err
	}

	nodeNr, err := s.loopback.AllocateAuxNodeNumber()
	if err != nil {
		return fmt.Errorf("allocating a loopback node: %w", err)
	}
	label := fmt.Sprintf("Wendy sensor %d", devID)
	if err := s.loopback.EnsureAuxNode(ctx, nodeNr, label); err != nil {
		return fmt.Errorf("creating loopback node %d: %w", nodeNr, err)
	}
	path := fmt.Sprintf("/dev/video%d", nodeNr)

	// The pump's context is derived from the service's, not from the caller's:
	// the caller here is a container lifecycle nudge whose context ends with the
	// nudge, while the pump must outlive it and stop only on explicit demand
	// withdrawal or service shutdown.
	pumpCtx, cancel := context.WithCancel(s.ctx)
	pump := newHubLoopbackPump(s.logger, sourceID, path)
	node := &twoPlaneNode{pump: pump, cancel: cancel, done: make(chan struct{}), nodeNr: nodeNr, path: path}

	s.twoPlaneMu.Lock()
	if !s.twoPlaneDemand {
		// Demand was withdrawn while the node was being created, and
		// stopAllTwoPlane has already passed over a map this node was not in
		// yet. Undo the creation here rather than leaving an unowned node.
		s.twoPlaneMu.Unlock()
		cancel()
		s.loopback.RemoveAuxNode(nodeNr)
		return nil
	}
	s.twoPlane[sourceID] = node
	s.twoPlaneMu.Unlock()

	go func() {
		defer close(node.done)
		err := pump.Run(pumpCtx, s, src, devID)
		if err != nil && !errors.Is(err, context.Canceled) {
			// A pump that stops on its own is not retried here. The usual reason
			// is frameBindableToLoopback refusing the source, which will refuse
			// it again on every retry, so a retry loop would spin producing
			// nothing. The node and its pump are torn down below and the source
			// reports no data plane, which is the honest state; a refusal is
			// remembered so the next sweep does not rebuild the whole thing.
			s.logger.Warn("two-plane: pump stopped",
				zap.String("source", sourceID), zap.String("node", path), zap.Error(err))
			if errors.Is(err, errSourceNotBindable) {
				s.noteTwoPlaneRefusal(sourceID)
			}
		}
		s.twoPlaneMu.Lock()
		if cur, ok := s.twoPlane[sourceID]; ok && cur == node {
			delete(s.twoPlane, sourceID)
		}
		s.twoPlaneMu.Unlock()
		s.loopback.RemoveAuxNode(nodeNr)
	}()
	return nil
}

// stopAllTwoPlane cancels every running pump and waits for each to release its
// node, so a subsequent allocation cannot hand out a number still in use.
func (s *VideoService) stopAllTwoPlane() {
	s.twoPlaneMu.Lock()
	nodes := make([]*twoPlaneNode, 0, len(s.twoPlane))
	for _, n := range s.twoPlane {
		nodes = append(nodes, n)
	}
	s.twoPlaneMu.Unlock()

	for _, n := range nodes {
		n.cancel()
	}
	for _, n := range nodes {
		<-n.done
	}
}

// TwoPlaneNodePath reports the v4l2loopback node carrying a source's frames on
// the data path, if one is running.
func (s *VideoService) TwoPlaneNodePath(sourceID string) (string, bool) {
	s.twoPlaneMu.Lock()
	defer s.twoPlaneMu.Unlock()
	node, ok := s.twoPlane[sourceID]
	if !ok {
		return "", false
	}
	return node.path, true
}

// SubscribeFrameIdentity streams the identity of frames written to a source's
// node. It is the control plane half of the two-plane path.
//
// It does NOT start a data plane: a source without a running node returns
// FailedPrecondition rather than quietly creating one, because creating a node
// is a device-node grant question that belongs to the entitlement wiring, not to
// whoever happens to subscribe.
func (s *VideoService) SubscribeFrameIdentity(ctx context.Context, sourceID string) (frameIdentitySubscription, error) {
	if _, err := s.resolveSensorSource(sourceID); err != nil {
		return nil, err
	}
	s.twoPlaneMu.Lock()
	node, ok := s.twoPlane[sourceID]
	s.twoPlaneMu.Unlock()
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition,
			"source %q has no two-plane data path; subscribe to samples instead", sourceID)
	}
	ch, cancel := node.pump.subscribeIdentities()
	return &pumpIdentitySubscription{sourceID: sourceID, nodePath: node.path, ch: ch, cancel: cancel}, nil
}

// pumpIdentitySubscription adapts one pump identity subscription to the
// control-plane contract.
type pumpIdentitySubscription struct {
	sourceID string
	nodePath string
	ch       <-chan loopbackBinding
	cancel   func()
}

func (p *pumpIdentitySubscription) Next(ctx context.Context) (FrameIdentitySample, error) {
	select {
	case <-ctx.Done():
		return FrameIdentitySample{}, ctx.Err()
	case b, ok := <-p.ch:
		if !ok {
			return FrameIdentitySample{}, errNoDataPlane
		}
		return FrameIdentitySample{
			SourceID:         p.sourceID,
			SampleID:         b.SampleID,
			BootNanos:        b.BootNanos,
			UncertaintyNanos: b.UncertaintyNanos,
			LoopbackSequence: b.LoopbackSequence,
			DroppedBefore:    b.HubDropsBefore,
			NodePath:         p.nodePath,
		}, nil
	}
}

func (p *pumpIdentitySubscription) Close() { p.cancel() }

// twoPlaneState is the VideoService state this file owns. It is embedded rather
// than declared inline in VideoService so the two-plane path's state stays
// visibly separable from the rest of the service.
type twoPlaneState struct {
	twoPlaneMu     sync.Mutex
	twoPlane       map[string]*twoPlaneNode
	twoPlaneDemand bool
	// twoPlaneRefused names sources whose pump refused their frames as
	// unbindable. Guarded by twoPlaneMu. Cleared when the entitled consumer set
	// changes or when a source leaves the device list, the two events that can
	// make the verdict wrong; otherwise it stands, because the reason for a
	// refusal is a fixed property of the producer's pipeline.
	twoPlaneRefused map[string]bool
	// twoPlaneConsumers fingerprints the entitled container set the refusals
	// above were reached under. Guarded by twoPlaneMu.
	twoPlaneConsumers string
	// twoPlaneStartMu serialises node allocation and creation. See
	// ensureTwoPlaneNode for why the allocation cannot be left unsynchronised.
	twoPlaneStartMu sync.Mutex
}
