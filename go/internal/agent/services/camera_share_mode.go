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
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// cameraShares holds the share records of every container that shares a
// camera. Its zero value shares nothing; containers and pending are made on
// first use. Lock order: ops, then fileMu, then mu; mu is never held with the
// service's mu.
type cameraShares struct {
	mu         sync.Mutex
	containers map[string]*shareContainer // by container ID
	// pending holds the numbers of share nodes being made, so IsShareNode
	// knows them before their record exists.
	pending map[int]bool

	// ops serialises removal, adoption and shutdown (camera_share.go), so a
	// record is never stopped twice or adopted while it is being removed.
	ops sync.Mutex
	// fileMu orders state file writes; statePath is cameraSharesPath, read
	// once under it.
	fileMu    sync.Mutex
	statePath string
}

// shareContainer is one container's shares.
type shareContainer struct {
	app    string
	shares []*shareRecord
	// created is when this agent last made a share for the container; zero
	// for shares loaded from the state file. See SyncCameraShares.
	created time.Time
}

// shareRecord is one camera shared with one container through one node.
// cameraName, camera, mode and node never change once the record is stored;
// cameraPath follows the camera and is guarded by cameraShares.mu, as are io,
// pump, cancel, done and adoptErr.
type shareRecord struct {
	cameraName, cameraPath string
	mode                   sharedMode
	node                   int
	camera                 camera.Identity

	// The node's writer and the pump feeding it; nil for a record loaded from
	// the state file until it is adopted, and after Shutdown.
	io     shareNodeIO
	pump   *sharePump
	cancel context.CancelFunc
	done   chan struct{} // closed when the pump's Run has returned
	// adoptErr is the last reason adoption failed, so it is logged once.
	adoptErr string
}

// errCameraShared is the named refusal for a recording whose explicit
// parameters are not the shared mode of a camera apps share. The campaign
// records it in the manifest (isCameraCaptureRefusal) instead of failing.
var errCameraShared = errors.New("camera is shared with apps and runs only at its shared mode")

// shareRestartCause is what a stream ended by a share takeover is told
// restarted its camera, in place of the episode capture's wording.
const shareRestartCause = "an app started sharing the camera, so its producer restarted at the camera's shared mode"

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

// runsAtSharedMode reports whether a hub's producer runs at mode: whatever the
// hub's request named, the mode admits, exactly as applySharedMode admits a
// viewer's. A hub at defaults runs at the shared mode, which is the camera's
// default mode; so does one a viewer started with only --fps 30.
func runsAtSharedMode(h *deviceHub, mode sharedMode) bool {
	return mode.admits(&agentpb.StreamVideoRequest{Width: h.width, Height: h.height, Framerate: h.framerate})
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

// captureFreeLocked returns a channel closed once no episode capture holds h's
// parameters (releaseCaptureHoldLocked). Callers hold h.mu and have seen
// heldByCaptureLocked report true.
func (h *deviceHub) captureFreeLocked() <-chan struct{} {
	if h.captureFree == nil {
		h.captureFree = make(chan struct{})
	}
	return h.captureFree
}

// releaseCaptureHoldLocked closes captureFree when the last explicit episode
// capture has left. unsubscribe calls it after every removal. Callers hold h.mu.
func (h *deviceHub) releaseCaptureHoldLocked() {
	if h.captureFree != nil && !h.heldByCaptureLocked() {
		close(h.captureFree)
		h.captureFree = nil
	}
}

// firstClosed returns a channel closed as soon as either a or b is. Its
// goroutine ends then too; for a share wait that is when the recording leaves
// or the hub ends, whichever is first.
func firstClosed(a, b <-chan struct{}) <-chan struct{} {
	out := make(chan struct{})
	go func() {
		select {
		case <-a:
		case <-b:
		}
		close(out)
	}()
	return out
}

// joinHubForShare is a share node's join. It subscribes for pixels at
// defaults; a hub that already runs at the shared mode is joined as it is. A
// hub that runs at another mode is waited for when a recording holds it
// (*shareWaitError, at once) and otherwise taken over: its explicit viewers'
// streams end with a message that names the shared mode, its parameter-less
// subscribers rejoin, and the camera restarts at defaults.
//
// It never waits on a recording, but it is not instant: each of its
// maxHubRetries attempts can wait on a dying hub inside getOrCreateHubKind (up
// to maxHubRetries × hubTeardownTimeout), and a takeover waits up to one more
// hubTeardownTimeout for the old producer to let go. With today's constants
// the worst case is about 6 s; the usual case is no wait, or one takeover.
func (s *VideoService) joinHubForShare(ctx context.Context, path string, devID uint32, mode sharedMode) (shareSubscription, error) {
	req := &agentpb.StreamVideoRequest{DeviceId: devID}
	for attempt := 0; attempt < maxHubRetries; attempt++ {
		h, id, ch, err := s.getOrCreateHubKind(ctx, path, req, hubHolderStreamClient, true)
		switch {
		case err == nil && runsAtSharedMode(h, mode):
			return &hubShareSubscription{hub: h, id: id, ch: ch}, nil
		case err == nil:
			h.unsubscribe(id)
		case errors.Is(err, errPixelsUnavailable) && s.liveHubAtOtherMode(path, mode):
			// The producer has no decoded tap at the mode a viewer chose: a
			// size MJPEG does not cover and whose raw frame is too large, say.
			// The shared mode has one (shareDecisionFor chose it from the tap),
			// so the camera is taken over exactly as for any other mode.
		default:
			// Any other failure, or a pixel refusal at the shared mode itself:
			// a takeover would change nothing, so the pump is told why.
			return nil, err
		}
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

// liveHubAtOtherMode reports whether a live hub for path runs at a mode the
// shared mode does not admit.
func (s *VideoService) liveHubAtOtherMode(path string, mode sharedMode) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.hubs[path]
	return ok && h.ctx.Err() == nil && !runsAtSharedMode(h, mode)
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
		// The camera is free to switch once the recording leaves, even when
		// a parameter-less consumer keeps the hub running at its mode; the
		// pump then joins again and takes the camera over.
		wait := &shareWaitError{
			Holder: hubHolderEpisodeCapture,
			Params: describeStreamParams(old.width, old.height, old.framerate),
			Done:   firstClosed(old.captureFreeLocked(), old.done),
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
	old.restartedForShare = true
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
