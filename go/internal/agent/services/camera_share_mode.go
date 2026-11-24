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
	"github.com/wendylabsinc/wendy/go/internal/agent/devcover"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// cameraShares holds the share records of every container that shares a
// camera. Its zero value shares nothing; its maps are made on first use.
//
// Lock order: ops or nodeMu (never both: node allocation and removal hold
// nodeMu, removal, adoption and shutdown of shares hold ops), then fileMu, then
// mu. mu is never held with the service's mu, and never across I/O or a wait.
type cameraShares struct {
	mu         sync.Mutex
	containers map[string]*shareContainer // by container ID
	// Latest module capability observed while opening a share. A later
	// successful open clears it after a driver update.
	freshStartUnsupported bool
	// pending holds the numbers of share nodes being made, so IsShareNode
	// knows them before their record exists.
	pending map[int]bool
	// removing holds the share nodes waiting to be removed, by number
	// (camera_share.go, queueShareRemovalsLocked). removalWG tracks their
	// goroutines, which run on removalCtx; closed (set by Shutdown) starts no
	// more.
	removing      map[int]*shareRemoval
	removalWG     sync.WaitGroup
	removalCtx    context.Context
	removalCancel context.CancelFunc
	closed        bool
	// stopped is set when Shutdown begins: from then on no share is stored
	// and no record is adopted.
	stopped bool
	// creating holds the CreateCameraShares calls in flight, by container;
	// RemoveCameraShares marks them removed, so they store nothing more.
	creating map[string][]*shareCreation
	// coverWG tracks the goroutines of CoverCameraShares calls, which can
	// outlive the call, and nsWG the closes of namespaces handed off the start
	// path; both are added to only before stopped is set, and Shutdown waits
	// for them.
	coverWG sync.WaitGroup
	nsWG    sync.WaitGroup

	// nodeMu serialises making and removing share nodes. The loopback module's
	// search for a free node number cannot see a number picked and not yet
	// made, so two allocations at once would be handed the same one.
	nodeMu sync.Mutex
	// ops serialises removal, adoption and shutdown (camera_share.go), so a
	// record is never stopped twice or adopted while it is being removed. A
	// start's cover never takes it, because it must not wait: it claims a
	// record it makes a node for with shareRecord.opening instead.
	ops sync.Mutex
	// fileMu orders state file writes; statePath is cameraSharesPath, read
	// once under it. stateNewer, also under it, is set when the file was
	// written by a newer agent in a version this one does not read: it is
	// left as it is and never written over (loadCameraShares).
	fileMu     sync.Mutex
	statePath  string
	stateNewer bool
}

// shareContainer is one container's shares.
type shareContainer struct {
	app    string
	shares []*shareRecord
	// created is when this agent last made a share for the container; zero
	// for shares loaded from the state file. See SyncCameraShares.
	created time.Time

	// ns pins the mount namespace of the container's running task, opened at
	// its start (CoverCameraShares) or by a sync after an agent restart; nil
	// while the container is not running, so a replug is covered again only
	// for running containers. nsInode identifies it, nsPID is the pid it was
	// opened through and nsAt when. nsGen counts every change, so a start's
	// cover still running from before never stores its namespace over a newer
	// one or a release. All guarded by cameraShares.mu.
	ns      *devcover.Namespace
	nsInode uint64
	nsPID   uint32
	nsAt    time.Time
	nsGen   uint64
}

// shareRecord is one camera shared with one container through one node.
// cameraName, mode, stamp, askedFormat and askedWhyNot never change once the
// record is stored. Everything else
// is guarded by cameraShares.mu: cameraPath follows the camera, and
// cameraAbsent says its last resolve did not find it; camera moves to the
// camera's new port when it is followed there; node and nodeMade change when
// a start makes a node lost in a reboot again; and siblings, io, pump, cancel,
// done, adoptErr, tookRealCamera, coverFailed, modeNote, settingsNote, opening
// and retired.
type shareRecord struct {
	cameraName, cameraPath string
	// cameraAbsent is set when the record's camera was not among the present
	// cameras at its last resolve, and cleared when it is found again.
	// cameraPath then still names where it was last (for logs and the state
	// file), but nothing at that path is this record's camera: the shared-mode
	// rules (sharedModeOf) and the camera listing (shareStatesOf) skip it, so a
	// different camera that takes the path does not inherit them.
	cameraAbsent bool
	mode         sharedMode
	stamp        string
	// askedFormat is the format the app's entitlement asked for when the
	// camera could not be declared in it and the node serves another format
	// instead; askedWhyNot is why it could not. Both are empty otherwise.
	askedFormat, askedWhyNot string
	node                     int
	camera                   camera.Identity
	// siblings are the ports (Identity.Device) of cameras of the same name and
	// index seen present while this record's camera was present too, sorted.
	// A camera on one of them is another camera, so the record never follows
	// it when its own camera is gone.
	siblings []string
	// nodeMade is when this agent made the node (shareRemovalClock); zero for
	// a node an earlier agent made.
	nodeMade time.Time

	// The node's writer and the pump feeding it; nil for a record loaded from
	// the state file until it is adopted, and after Shutdown.
	io     shareNodeIO
	pump   *sharePump
	cancel context.CancelFunc
	done   chan struct{} // closed when the pump's Run has returned
	// adoptErr is the last reason adoption failed, so it is logged once.
	adoptErr string
	// tookRealCamera is set when the container's app opened the real camera in
	// the moment between a replug and its cover (checkWindowLosers): it holds
	// the real camera, not its node, until it reopens the camera. It says
	// which process; the pump clears it when it next reads for the app.
	tookRealCamera string
	// coverFailed is why the cover of the container's last start failed, so
	// the app holds the real camera for its run: set by reportStartCover, and
	// cleared by the next cover that is there (a recheck's, a replug's, or the
	// next start's), when the pump next reads for the app, or when the run
	// ends (ReleaseCameraShares).
	coverFailed string
	// modeNote tells the user to redeploy the app: an adopted record's node
	// keeps the mode an earlier agent stored (keep_format locks it), and a
	// share of its camera made today would get another (checkAdoptedMode).
	modeNote string
	// settingsNote says which of the node's settings the app's entitlement
	// entries disagree on for this camera (appconfig.CameraNodeSettings), as
	// of the container's last create; empty when they agree, and when that
	// create did not see the camera. It quotes allowlist entries as written,
	// which says nothing of where the camera is now.
	settingsNote string
	// opening is set while a writer is being opened for the record (a start
	// making its node again, or a sync adopting it), so only one ever is.
	// retired is set when stopShare has run: nothing attaches to it after.
	opening, retired bool
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
// a camera goes. A record whose camera was absent at its last resolve is not
// the camera at path, whatever its cameraPath says: another camera that took
// the path is not held to its mode. It does no I/O (it is on the StreamVideo
// path); the records are resolved on every camera node that appears, on every
// pump join, by every sync and by the camera listing.
func (s *VideoService) sharedModeOf(path string) (mode sharedMode, apps []string, ok bool) {
	s.shares.mu.Lock()
	defer s.shares.mu.Unlock()
	modes := make(map[string]sharedMode)
	for id, c := range s.shares.containers {
		if c == nil {
			continue
		}
		for _, r := range c.shares {
			if r != nil && r.cameraPath == path && !r.cameraAbsent {
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
	// Records of one camera can differ in format; their size and rate, which
	// is all the rules compare, are the camera's. Taking the first container's
	// keeps the answer deterministic.
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

var shareFrameKinds = map[string]frameKind{
	appconfig.CameraFormatPixels: pixelFrames,
	appconfig.CameraFormatNative: nativeFrames,
	appconfig.CameraFormatH264:   encodedFrames,
}

func hubLacksFramesFor(err error) bool {
	return errors.Is(err, errPixelsUnavailable) || isNativeRefusal(err)
}

// joinHubForShare is a share node's join. It subscribes for the frames of the
// mode's format at defaults; a hub that already runs at the shared mode is
// joined as it is. A hub that runs at another mode is waited for when a
// recording holds it (*shareWaitError, at once) and otherwise taken over: its
// explicit viewers' streams end with a message that names the shared mode, its
// parameter-less subscribers rejoin, and the camera restarts at defaults.
//
// It never waits on a recording, but it is not instant: each of its
// maxHubRetries attempts can wait on a dying hub inside getOrCreateHubKind (up
// to maxHubRetries × hubTeardownTimeout), and a takeover waits up to one more
// hubTeardownTimeout for the old producer to let go. With today's constants
// the worst case is about 6 s; the usual case is no wait, or one takeover.
func (s *VideoService) joinHubForShare(ctx context.Context, path string, devID uint32, mode sharedMode) (shareSubscription, error) {
	kind, known := shareFrameKinds[mode.Format]
	if !known {
		return nil, status.Errorf(codes.Internal, "camera share has no subscription for format %q", mode.Format)
	}
	req := &agentpb.StreamVideoRequest{DeviceId: devID}
	for attempt := 0; attempt < maxHubRetries; attempt++ {
		h, id, ch, err := s.getOrCreateHubKind(ctx, path, req, hubHolderStreamClient, kind)
		switch {
		case err == nil && runsAtSharedMode(h, mode):
			return &hubShareSubscription{hub: h, id: id, ch: ch}, nil
		case err == nil:
			h.unsubscribe(id)
		case hubLacksFramesFor(err) && s.liveHubAtOtherMode(path, mode):
			// The producer has none of the node's frames at the mode a viewer
			// chose: no decoded tap at a size MJPEG does not cover, say, or
			// no whole frames of the camera's own. The shared mode has them
			// (shareDecisionFor chose it from them), so the camera is taken
			// over exactly as for any other mode.
		default:
			// Any other failure, or a refusal at the shared mode itself: a
			// takeover would change nothing, so the pump is told why.
			return nil, err
		}
		sub, retry, err := s.takeOverHubForShare(ctx, path, req, mode, kind)
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
func (s *VideoService) takeOverHubForShare(ctx context.Context, path string, req *agentpb.StreamVideoRequest, mode sharedMode, kind frameKind) (sub shareSubscription, retry bool, err error) {
	// Read before s.mu: the share records have their own lock, never nested.
	_, apps, _ := s.sharedModeOf(path)
	seqKey := s.sampleSeqKey(path)
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
		sampleSeq: s.sampleSeqLocked(seqKey),
	}
	id, ch, _ := h.subscribeKind(kind)
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

type hubShareSubscription struct {
	hub   *deviceHub
	id    int
	ch    chan *videoFrame
	leave sync.Once
}

func (s *hubShareSubscription) Frames() <-chan *videoFrame { return s.ch }

func (s *hubShareSubscription) Restarted() bool { return s.hub.wasRestarted() }

// Err is the reason the producer turned this subscriber away (it does not
// offer the subscription's frames), or the producer's terminal error.
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
