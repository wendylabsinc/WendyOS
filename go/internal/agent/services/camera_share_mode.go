package services

// Shared-mode rules. While any container holds a share node for a camera, that
// camera runs only at its shared mode: viewers and recordings that name no
// parameters, or exactly the shared mode, join as a default join; any other
// explicit request is refused with a message that names the mode. A share
// node's own capture joins the hub through joinHubForShare, which ends a
// viewer holding another mode and waits for a recording holding one.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// cameraShares holds the share records of every container that shares a
// camera. Its zero value shares nothing; containers is made on first use.
type cameraShares struct {
	mu         sync.Mutex
	containers map[string]*shareContainer // by container ID
}

// shareContainer is one container's shares.
type shareContainer struct {
	app    string
	shares []*shareRecord
}

// shareRecord is one camera shared with one container through one node.
type shareRecord struct {
	cameraName, cameraPath string
	mode                   sharedMode
	node                   int
}

// errCameraShared is the named refusal for a recording whose explicit
// parameters are not the shared mode of a camera apps share. The campaign
// records it in the manifest (isCameraCaptureRefusal) instead of failing.
var errCameraShared = errors.New("camera is shared with apps and runs only at its shared mode")

// sharedModeOf reports whether any container shares the camera at path, its
// shared mode, and the sorted IDs of the containers that share it. It reads the
// live records on every call, so the rules lift as soon as the last record for
// a camera goes.
func (s *VideoService) sharedModeOf(path string) (mode sharedMode, apps []string, ok bool) {
	s.shares.mu.Lock()
	defer s.shares.mu.Unlock()
	modes := make(map[string]sharedMode)
	for id, c := range s.shares.containers {
		if c == nil {
			continue
		}
		for _, r := range c.shares {
			if r != nil && r.cameraPath == path {
				modes[id] = r.mode
				break
			}
		}
	}
	if len(modes) == 0 {
		return sharedMode{}, nil, false
	}
	for id := range modes {
		apps = append(apps, id)
	}
	slices.Sort(apps)
	// Every record of one camera carries the mode the planner chose for it;
	// taking the first container's keeps the answer deterministic regardless.
	return modes[apps[0]], apps, true
}

// withoutStreamParams returns a copy of req with no width, height or
// framerate, so it joins as a default join and never restarts the camera.
func withoutStreamParams(req *agentpb.StreamVideoRequest) *agentpb.StreamVideoRequest {
	out := &agentpb.StreamVideoRequest{}
	if req != nil {
		out = proto.Clone(req).(*agentpb.StreamVideoRequest)
	}
	out.Width, out.Height, out.Framerate = 0, 0, 0
	return out
}

// applySharedMode is the viewer rule. For a camera no container shares it
// returns req untouched. For a shared camera, a request the shared mode admits
// comes back as a copy with its parameters cleared; any other is refused with
// InvalidArgument, the code a busy camera's refusal has always had.
func (s *VideoService) applySharedMode(path string, req *agentpb.StreamVideoRequest) (*agentpb.StreamVideoRequest, error) {
	mode, apps, ok := s.sharedModeOf(path)
	if !ok {
		return req, nil
	}
	if !mode.admits(req) {
		return nil, status.Errorf(codes.InvalidArgument,
			"camera %s is shared with %s and runs only at its shared mode, %s; ask for no size or rate, or for exactly that mode",
			path, strings.Join(apps, ", "), mode)
	}
	return withoutStreamParams(req), nil
}

// applySharedModeToCapture is the recording rule. For a shared camera, a
// request the shared mode admits is cleared, and admitted is the mode when the
// request named parameters, so the caller can report what a hub at defaults
// really runs at; any other explicit request is refused with errCameraShared.
func (s *VideoService) applySharedModeToCapture(path string, req *agentpb.StreamVideoRequest) (out *agentpb.StreamVideoRequest, admitted *sharedMode, err error) {
	mode, apps, ok := s.sharedModeOf(path)
	if !ok || !streamRequestAssertsParams(req) {
		return req, nil, nil
	}
	if !mode.admits(req) {
		return nil, nil, fmt.Errorf("%w: camera %s is shared with %s and runs only at its shared mode, %s; the campaign requested %s",
			errCameraShared, path, strings.Join(apps, ", "), mode,
			describeStreamParams(req.GetWidth(), req.GetHeight(), req.GetFramerate()))
	}
	return withoutStreamParams(req), &mode, nil
}

// runsAtSharedMode reports whether a hub's producer runs at mode: at defaults,
// which is what the shared mode is, or at the mode's size with its rate or none.
func runsAtSharedMode(h *deviceHub, mode sharedMode) bool {
	if h.width == 0 && h.height == 0 && h.framerate == 0 {
		return true
	}
	return h.width == mode.Width && h.height == mode.Height && (h.framerate == 0 || h.framerate == mode.FPS)
}

// heldByCaptureLocked reports whether any current subscriber is an episode
// capture that asserted the hub's parameters. Every explicit holder is
// checked, not only the one heldExplicitly names: a viewer and a recording
// can hold one hub at the same parameters. Callers hold h.mu.
func (h *deviceHub) heldByCaptureLocked() bool {
	for _, label := range h.subExplicit {
		if label == hubHolderEpisodeCapture {
			return true
		}
	}
	return false
}

// joinHubForShare is a share node's join. It subscribes for pixels at
// defaults; a hub that already runs at the shared mode is joined as it is. A
// hub that runs at another mode is waited for when a recording holds it
// (*shareWaitError, at once) and otherwise taken over: its explicit viewers'
// streams end with a message that names the shared mode, its parameter-less
// subscribers rejoin, and the camera restarts at defaults. It never blocks for
// longer than one hubTeardownTimeout.
func (s *VideoService) joinHubForShare(ctx context.Context, path string, devID uint32, mode sharedMode) (shareSubscription, error) {
	req := &agentpb.StreamVideoRequest{DeviceId: devID}
	for attempt := 0; attempt < maxHubRetries; attempt++ {
		h, id, ch, err := s.getOrCreateHubKind(ctx, path, req, hubHolderStreamClient, true)
		if err != nil {
			return nil, err
		}
		if runsAtSharedMode(h, mode) {
			return &hubShareSubscription{hub: h, id: id, ch: ch}, nil
		}
		h.unsubscribe(id)
		sub, retry, err := s.takeOverHubForShare(ctx, path, req, mode)
		if err != nil {
			return nil, err
		}
		if !retry {
			return sub, nil
		}
		// The hub went, or now runs at the shared mode; join from the top.
	}
	return nil, status.Error(codes.Unavailable, "video device hub churned during subscription")
}

// takeOverHubForShare is takeOverDefaultedHub with the roles swapped: the
// share node's parameter-less capture replaces a hub a viewer holds at another
// mode. It returns retry when the hub is gone or no longer conflicts, and a
// *shareWaitError when a recording holds it. The check and the takeover happen
// under s.mu and the old hub's mu, so no recording can join between them.
func (s *VideoService) takeOverHubForShare(ctx context.Context, path string, req *agentpb.StreamVideoRequest, mode sharedMode) (sub shareSubscription, retry bool, err error) {
	// Read before s.mu: the share records have their own lock, never nested.
	_, apps, _ := s.sharedModeOf(path)
	ended := status.Errorf(codes.InvalidArgument,
		"video stream ended: camera %s is now shared with %s and runs only at its shared mode, %s; reconnect without a size or rate",
		path, strings.Join(apps, ", "), mode)

	s.mu.Lock()
	old, exists := s.hubs[path]
	if !exists || old.ctx.Err() != nil || runsAtSharedMode(old, mode) {
		s.mu.Unlock()
		return nil, true, nil
	}
	old.mu.Lock()
	if old.heldByCaptureLocked() {
		wait := &shareWaitError{
			Holder: hubHolderEpisodeCapture,
			Params: describeStreamParams(old.width, old.height, old.framerate),
			Done:   old.done,
		}
		old.mu.Unlock()
		s.mu.Unlock()
		return nil, false, wait
	}

	hctx, cancel := context.WithCancel(s.ctx)
	h := &deviceHub{
		subs:      make(map[int]*hubSubscriber),
		subDrops:  make(map[int]uint64),
		ctx:       hctx,
		cancel:    cancel,
		done:      make(chan struct{}),
		sampleSeq: s.sampleSeqLocked(path),
	}
	id, ch, _ := h.subscribePixels()
	s.hubs[path] = h
	// End each viewer that asked for its mode, with the reason; it is told
	// before the producer stops, so pumpFrames reads this, not a restart.
	for subID, label := range old.subExplicit {
		if label != hubHolderStreamClient {
			continue
		}
		if v := old.subs[subID]; v != nil && !v.closed {
			v.err = ended
			v.closed = true
			close(v.ch)
		}
	}
	old.restarted = true
	old.mu.Unlock()
	old.cancel()
	s.mu.Unlock()

	s.logger.Info("camera share restarting camera producer at its shared mode",
		zap.String("device", path), zap.Strings("containers", apps), zap.Stringer("mode", mode))

	if err := s.startReplacementHub(ctx, path, old, h, req, "camera share takeover"); err != nil {
		return nil, false, err
	}
	return &hubShareSubscription{hub: h, id: id, ch: ch}, false, nil
}

// hubShareSubscription is a share pump's pixel subscription on a hub.
type hubShareSubscription struct {
	hub   *deviceHub
	id    int
	ch    chan *videoFrame
	leave sync.Once
}

func (s *hubShareSubscription) Frames() <-chan *videoFrame { return s.ch }

func (s *hubShareSubscription) Restarted() bool { return s.hub.wasRestarted() }

// Err is the reason the producer turned this subscriber away (no tap), or the
// producer's terminal error.
func (s *hubShareSubscription) Err() error {
	if err := s.hub.subscriberErr(s.id); err != nil {
		return err
	}
	return s.hub.terminalErr()
}

func (s *hubShareSubscription) Leave() {
	s.leave.Do(func() { s.hub.unsubscribe(s.id) })
}

var _ shareSubscription = (*hubShareSubscription)(nil)
