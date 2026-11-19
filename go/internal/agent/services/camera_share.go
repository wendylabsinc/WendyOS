package services

// The share manager. When a container with the camera entitlement is created,
// each shared camera present gets one v4l2loopback share node for it, which the
// agent keeps open as the writer (camera_share_node.go) and fills on demand
// (camera_share_pump.go). Nodes are removed with the container, kept in a state
// file, and adopted by number after an agent restart, so an app reading its
// node never sees it removed or recreated under it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// cameraSharesPath is the share state file. A variable so tests can move it.
var cameraSharesPath = "/var/lib/wendy/camera-shares.json"

// cameraShareCreateBudget bounds CreateCameraShares: a container create never
// waits longer than this for its shares.
const cameraShareCreateBudget = 2 * time.Second

// cameraShareSyncGrace protects shares this agent just made from a sync whose
// container listing was taken before their container existed: a container
// missing from a sync's list loses its shares only when they were loaded from
// the state file or made at least this long ago.
const cameraShareSyncGrace = time.Minute

// cameraShareStateVersion is the state file format this agent reads and writes.
const cameraShareStateVersion = 1

// v4l2NameBytes is how much of a node's label the driver keeps as its name.
const v4l2NameBytes = 31

// CameraShareContainer is one Wendy container as SyncCameraShares sees it.
type CameraShareContainer struct {
	ID      string
	Running bool
	PID     uint32
}

// CreateCameraShares gives a newly created container one share node for each
// shared camera present, within cameraShareCreateBudget. It logs why a camera
// is not shared and never fails: a camera it does not share is the real
// camera, exactly as before sharing existed.
func (s *VideoService) CreateCameraShares(ctx context.Context, containerID, appID string) {
	ctx, cancel := context.WithTimeout(ctx, cameraShareCreateBudget)
	defer cancel()
	log := s.shareLogger()
	if s.ctx != nil && s.ctx.Err() != nil {
		log.Info(fmt.Sprintf("camera share: %s gets the real cameras: the video service is shutting down", containerID))
		return
	}
	// Available can load the module on its first call, so it is timed too.
	unavailable, ok := withinBudget(ctx, func() bool { return s.loopback == nil || s.loopback.Available() != nil })
	switch {
	case !ok:
		log.Warn(fmt.Sprintf("camera share: %s gets the real cameras: %s", containerID, shareSetupStopped(ctx.Err())))
		return
	case unavailable:
		log.Info(fmt.Sprintf("camera share: %s gets the real cameras: no compatible v4l2loopback module on this device", containerID))
		return
	}

	type listing struct {
		devices []*agentpb.VideoDevice
		err     error
	}
	listed, ok := withinBudget(ctx, func() listing {
		devices, err := s.listCameras(ctx)
		return listing{devices, err}
	})
	switch {
	case !ok:
		log.Warn(fmt.Sprintf("camera share: %s gets the real cameras: %s", containerID, shareSetupStopped(ctx.Err())))
		return
	case listed.err != nil:
		log.Warn(fmt.Sprintf("camera share: %s gets the real cameras: listing cameras failed: %v", containerID, listed.err))
		return
	}

	var cams []*agentpb.VideoDevice
	for _, dev := range listed.devices {
		switch dev.GetTransport() {
		case agentpb.VideoTransport_VIDEO_TRANSPORT_ROS2, agentpb.VideoTransport_VIDEO_TRANSPORT_IP:
			continue
		}
		cams = append(cams, dev)
	}
	for i, dev := range cams {
		if err := ctx.Err(); err != nil {
			for _, rest := range cams[i:] {
				s.logRealCamera(true, containerID, rest, shareSetupStopped(err))
			}
			return
		}
		s.createCameraShare(ctx, containerID, appID, dev)
	}
}

// createCameraShare shares one camera with a container, or logs why not.
func (s *VideoService) createCameraShare(ctx context.Context, containerID, appID string, dev *agentpb.VideoDevice) {
	id, err := s.identityOf(filepath.Base(dev.GetPath()))
	if err != nil {
		s.logRealCamera(true, containerID, dev, "its sysfs identity could not be read: "+err.Error())
		return
	}
	if s.containerSharesCamera(containerID, id) {
		return
	}
	// shareDecisionFor can block well past the budget (its GStreamer element
	// listing is not context-aware), so it runs on its own goroutine.
	decision, ok := withinBudget(ctx, func() shareDecision { return s.shareDecisionFor(ctx, dev) })
	if !ok {
		s.logRealCamera(true, containerID, dev, shareSetupStopped(ctx.Err()))
		return
	}
	if !decision.Shared {
		s.logRealCamera(false, containerID, dev, decision.Reason)
		return
	}

	nr, reason := s.makeShareNode(ctx, dev.GetName())
	if reason != "" {
		s.logRealCamera(true, containerID, dev, reason)
		return
	}
	path := shareNodePath(nr)
	node, err := openShareNode(s.logger, path, decision.Mode)
	if err != nil {
		s.unmakeShareNode(nr)
		s.logRealCamera(true, containerID, dev, err.Error())
		return
	}
	if err := ctx.Err(); err != nil {
		// Made, but after the budget: the camera counts as not done.
		s.closeShareWriter(path, node)
		s.unmakeShareNode(nr)
		s.logRealCamera(true, containerID, dev, shareSetupStopped(err))
		return
	}

	r := &shareRecord{cameraName: dev.GetName(), cameraPath: dev.GetPath(), mode: decision.Mode, node: nr, camera: id}
	start := s.storeShare(containerID, appID, r, node)
	s.persistCameraShares()
	start()
	s.shareLogger().Info(fmt.Sprintf("camera share: %s shares %s (%s) through %s at %s",
		containerID, dev.GetName(), dev.GetPath(), path, decision.Mode))
}

// makeShareNode allocates a node number and creates the node, labelled with
// the camera's name. It holds twoPlaneStartMu across both steps: allocation
// does not reserve a number, so two allocators could otherwise be handed the
// same one. On success the number is pending until storeShare or
// unmakeShareNode. A non-empty reason means no node was left behind.
func (s *VideoService) makeShareNode(ctx context.Context, label string) (nr int, reason string) {
	s.twoPlaneStartMu.Lock()
	defer s.twoPlaneStartMu.Unlock()
	nr, err := s.loopback.AllocateAuxNodeNumber()
	if errors.Is(err, ipcam.ErrBandExhausted) {
		return 0, fmt.Sprintf("no free v4l2loopback node number (%d-%d are in use)", ipcam.IDBandStart, ipcam.IDBandEnd)
	}
	if err != nil {
		return 0, "allocating a v4l2loopback node failed: " + err.Error()
	}
	s.shares.mu.Lock()
	if s.shares.pending == nil {
		s.shares.pending = make(map[int]bool)
	}
	s.shares.pending[nr] = true
	s.shares.mu.Unlock()
	if err := s.loopback.EnsureShareNode(ctx, nr, label); err != nil {
		if _, exists := s.loopback.NodePath(uint32(nr)); !exists {
			s.forgetPendingShareNode(nr)
		} else {
			s.unmakeShareNode(nr)
		}
		return 0, fmt.Sprintf("creating v4l2loopback node %d failed: %v", nr, err)
	}
	return nr, ""
}

// unmakeShareNode removes a node makeShareNode made whose share was never
// stored.
func (s *VideoService) unmakeShareNode(nr int) {
	s.removeShareNode(nr)
	s.forgetPendingShareNode(nr)
}

func (s *VideoService) forgetPendingShareNode(nr int) {
	s.shares.mu.Lock()
	delete(s.shares.pending, nr)
	s.shares.mu.Unlock()
}

// removeShareNode deletes share node nr. Every share node removal goes through
// here, so a change to how one is removed has one place to land.
func (s *VideoService) removeShareNode(nr int) {
	if s.loopback != nil {
		s.loopback.RemoveAuxNode(nr)
	}
}

// storeShare adds r to the container's shares with its writer and a pump, and
// returns the function that starts the pump. The record is stored with its
// pump already set, so a sync never takes it for one to adopt.
func (s *VideoService) storeShare(containerID, appID string, r *shareRecord, node shareNodeIO) (start func()) {
	s.shares.mu.Lock()
	defer s.shares.mu.Unlock()
	if s.shares.containers == nil {
		s.shares.containers = make(map[string]*shareContainer)
	}
	c := s.shares.containers[containerID]
	if c == nil {
		c = &shareContainer{app: appID}
		s.shares.containers[containerID] = c
	}
	c.created = time.Now()
	c.shares = append(c.shares, r)
	delete(s.shares.pending, r.node)
	return s.attachSharePumpLocked(containerID, r, node, false)
}

// attachSharePumpLocked gives r its writer and a pump, and returns the function
// that starts the pump. The pump's context comes from the service's, so it
// outlives the call that made the share. Callers hold s.shares.mu.
func (s *VideoService) attachSharePumpLocked(containerID string, r *shareRecord, node shareNodeIO, adopted bool) (start func()) {
	ctx, cancel := context.WithCancel(s.serviceContext())
	pump := newSharePump(s.logger, containerID, r.cameraName, node, r.mode,
		func(ctx context.Context) (shareSubscription, error) { return s.joinShare(ctx, r) }, adopted)
	done := make(chan struct{})
	r.io, r.pump, r.cancel, r.done = node, pump, cancel, done
	return func() {
		go func() {
			defer close(done)
			pump.Run(ctx)
		}()
	}
}

// joinShare is a share pump's join: it finds where the record's camera is now
// and joins its hub at the shared mode.
func (s *VideoService) joinShare(ctx context.Context, r *shareRecord) (shareSubscription, error) {
	path, ok := s.currentSharePath(r)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "camera %s is not plugged in", r.cameraName)
	}
	devID, err := strconv.ParseUint(strings.TrimPrefix(filepath.Base(path), "video"), 10, 32)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "camera %s is at %s, which is not a video node", r.cameraName, path)
	}
	return s.joinHubForShare(ctx, path, uint32(devID), r.mode)
}

// currentSharePath resolves the record's camera to its present /dev path. When
// the camera has moved, the record takes the new path, so the shared-mode rules
// follow the camera, and the state file is saved.
func (s *VideoService) currentSharePath(r *shareRecord) (string, bool) {
	base, ok := s.resolveShareCamera(r.camera)
	if !ok {
		return "", false
	}
	path := "/dev/" + base
	s.shares.mu.Lock()
	moved := r.cameraPath != path
	r.cameraPath = path
	s.shares.mu.Unlock()
	if moved {
		s.persistCameraShares()
	}
	return path, true
}

// resolveShareCamera finds the present node of a stored camera identity with
// camera.Resolve. Share nodes are never candidates. A present camera is
// claimed when another record names it exactly, or when another record's
// camera of the same name and index is absent too, because then which of the
// two it is cannot be told: two same-model cameras never cross.
func (s *VideoService) resolveShareCamera(want camera.Identity) (string, bool) {
	present := s.presentIdentities()
	for base, id := range present {
		if nr, ok := videoNodeNumber(base); ok && s.isShareNodeNumber(nr, func() (string, bool) { return id.Name, true }) {
			delete(present, base)
		}
	}
	stored := s.storedShareIdentities()
	isPresent := make(map[camera.Identity]bool, len(present))
	for _, id := range present {
		isPresent[id] = true
	}
	claimed := func(id camera.Identity) bool {
		if stored[id] {
			return true
		}
		for other := range stored {
			if other != want && other.Name == id.Name && other.Index == id.Index && !isPresent[other] {
				return true
			}
		}
		return false
	}
	return camera.Resolve(want, present, claimed)
}

func (s *VideoService) storedShareIdentities() map[camera.Identity]bool {
	s.shares.mu.Lock()
	defer s.shares.mu.Unlock()
	out := map[camera.Identity]bool{}
	for _, c := range s.shares.containers {
		for _, r := range c.shares {
			out[r.camera] = true
		}
	}
	return out
}

func (s *VideoService) containerSharesCamera(containerID string, id camera.Identity) bool {
	s.shares.mu.Lock()
	defer s.shares.mu.Unlock()
	if c := s.shares.containers[containerID]; c != nil {
		for _, r := range c.shares {
			if r.camera == id {
				return true
			}
		}
	}
	return false
}

// RemoveCameraShares removes a container's share nodes: for each, it stops the
// pump, closes the writer and removes the node, then forgets the container and
// saves the state file. A container with no shares is left alone.
func (s *VideoService) RemoveCameraShares(containerID string) {
	s.shares.ops.Lock()
	defer s.shares.ops.Unlock()
	s.shares.mu.Lock()
	c := s.shares.containers[containerID]
	var recs []*shareRecord
	if c != nil {
		recs = append(recs, c.shares...)
	}
	s.shares.mu.Unlock()
	if len(recs) == 0 {
		return
	}
	for _, r := range recs {
		s.dropShare(containerID, r)
	}
	s.forgetShares(containerID, recs)
	s.persistCameraShares()
}

// dropShare stops one share and removes its node, in this order: cancel the
// pump, wait for it, close the writer, remove the node. A record that was never
// opened in this agent's life (loaded, not adopted) has its node removed only
// when it is still that record's share node. The record stays in the index
// until the caller forgets it, so IsShareNode holds until the node is gone.
// Callers hold s.shares.ops.
func (s *VideoService) dropShare(containerID string, r *shareRecord) {
	s.shares.mu.Lock()
	cancel, done, node := r.cancel, r.done, r.io
	s.shares.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	path := shareNodePath(r.node)
	if node != nil {
		s.closeShareWriter(path, node)
	}
	if node == nil && !s.nodeIsShareFor(r) {
		return
	}
	s.removeShareNode(r.node)
	s.shareLogger().Info(fmt.Sprintf("camera share: removed %s, %s's share of %s", path, containerID, r.cameraName))
}

func (s *VideoService) closeShareWriter(path string, node shareNodeIO) {
	if err := node.Close(); err != nil {
		s.shareLogger().Warn(fmt.Sprintf("camera share: closing the writer of %s failed: %v", path, err))
	}
}

// forgetShares takes recs out of the container's shares, and the container out
// of the index once it has none.
func (s *VideoService) forgetShares(containerID string, recs []*shareRecord) {
	s.shares.mu.Lock()
	defer s.shares.mu.Unlock()
	c := s.shares.containers[containerID]
	if c == nil {
		return
	}
	gone := make(map[*shareRecord]bool, len(recs))
	for _, r := range recs {
		gone[r] = true
	}
	kept := c.shares[:0]
	for _, r := range c.shares {
		if !gone[r] {
			kept = append(kept, r)
		}
	}
	c.shares = kept
	if len(c.shares) == 0 {
		delete(s.shares.containers, containerID)
	}
}

// SyncCameraShares squares the share records with the containers that exist.
// A container missing from the list loses its shares: it was deleted while the
// agent was down (or, for shares made by this agent, more than
// cameraShareSyncGrace ago without a RemoveCameraShares). A listed container's
// record without a pump, after an agent restart, is adopted when its node still
// exists: the node is reopened at the record's own mode with a pump that
// blanks it if idle, and nothing is removed or recreated. A record whose node
// is gone (a reboot) is left for the container's next start.
func (s *VideoService) SyncCameraShares(ctx context.Context, containers []CameraShareContainer) {
	s.shares.ops.Lock()
	defer s.shares.ops.Unlock()
	listed := make(map[string]bool, len(containers))
	for _, c := range containers {
		listed[c.ID] = true
	}

	type share struct {
		containerID string
		r           *shareRecord
	}
	var gone map[string][]*shareRecord
	var adopt []share
	now := time.Now()
	s.shares.mu.Lock()
	for id, c := range s.shares.containers {
		if !listed[id] {
			if c.created.IsZero() || now.Sub(c.created) >= cameraShareSyncGrace {
				if gone == nil {
					gone = map[string][]*shareRecord{}
				}
				gone[id] = append([]*shareRecord(nil), c.shares...)
			}
			continue
		}
		for _, r := range c.shares {
			if r.pump == nil {
				adopt = append(adopt, share{id, r})
			}
		}
	}
	s.shares.mu.Unlock()

	if len(gone) > 0 {
		ids := make([]string, 0, len(gone))
		for id := range gone {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			s.shareLogger().Info(fmt.Sprintf("camera share: %s no longer exists; removing its share nodes", id))
			for _, r := range gone[id] {
				s.dropShare(id, r)
			}
			s.forgetShares(id, gone[id])
		}
		s.persistCameraShares()
	}

	sort.SliceStable(adopt, func(i, j int) bool { return adopt[i].containerID < adopt[j].containerID })
	for _, a := range adopt {
		if ctx.Err() != nil {
			return
		}
		s.adoptShare(a.containerID, a.r)
	}
}

// adoptShare reopens the node of a record loaded from the state file and
// starts its pump. The node is opened at the record's stored mode: the earlier
// writer's keep_format locked the node's format for the node's life, so any
// other mode would be refused. Opening sets keep_format again. Callers hold
// s.shares.ops.
func (s *VideoService) adoptShare(containerID string, r *shareRecord) {
	if s.loopback == nil {
		return
	}
	if _, exists := s.loopback.NodePath(uint32(r.node)); !exists {
		return // a reboot: the container's next start makes the node again
	}
	path := shareNodePath(r.node)
	if !s.nodeIsShareFor(r) {
		s.noteAdoptFailure(containerID, r, fmt.Sprintf("%s is no longer its share node of %s; it is left alone", path, r.cameraName))
		return
	}
	node, err := openShareNode(s.logger, path, r.mode)
	if err != nil {
		s.noteAdoptFailure(containerID, r, fmt.Sprintf("adopting %s for %s failed: %v; the app's node gets no frames", path, r.cameraName, err))
		return
	}
	s.shares.mu.Lock()
	r.adoptErr = ""
	start := s.attachSharePumpLocked(containerID, r, node, true)
	s.shares.mu.Unlock()
	start()
	s.shareLogger().Info(fmt.Sprintf("camera share: %s adopted %s for %s at %s", containerID, path, r.cameraName, r.mode))
}

// noteAdoptFailure logs why a record could not be adopted, once per reason.
func (s *VideoService) noteAdoptFailure(containerID string, r *shareRecord, reason string) {
	s.shares.mu.Lock()
	repeated := r.adoptErr == reason
	r.adoptErr = reason
	s.shares.mu.Unlock()
	if !repeated {
		s.shareLogger().Warn(fmt.Sprintf("camera share: %s: %s", containerID, reason))
	}
}

// nodeIsShareFor reports whether the node a record names exists and is still
// the share node made for it, by its sysfs name: after a reboot the number may
// have gone to another node, which must be neither opened nor removed.
func (s *VideoService) nodeIsShareFor(r *shareRecord) bool {
	if s.loopback == nil {
		return false
	}
	if _, exists := s.loopback.NodePath(uint32(r.node)); !exists {
		return false
	}
	if s.readDeviceName == nil {
		return true
	}
	name, err := s.readDeviceName(fmt.Sprintf("video%d", r.node))
	return err == nil && shareNodeName(name) == shareNodeName(r.cameraName)
}

// stopCameraSharePumps stops every pump and closes every writer, for Shutdown.
// Nodes and records stay: the next agent adopts them.
func (s *VideoService) stopCameraSharePumps() {
	s.shares.ops.Lock()
	defer s.shares.ops.Unlock()
	type running struct {
		r      *shareRecord
		cancel context.CancelFunc
		done   chan struct{}
		io     shareNodeIO
	}
	var pumps []running
	s.shares.mu.Lock()
	for _, c := range s.shares.containers {
		for _, r := range c.shares {
			if r.cancel != nil {
				pumps = append(pumps, running{r, r.cancel, r.done, r.io})
			}
		}
	}
	s.shares.mu.Unlock()
	for _, p := range pumps {
		p.cancel()
	}
	for _, p := range pumps {
		<-p.done
		s.closeShareWriter(shareNodePath(p.r.node), p.io)
	}
	// The records are as a restarted agent finds them: no writer, no pump.
	s.shares.mu.Lock()
	for _, p := range pumps {
		p.r.io, p.r.pump, p.r.cancel, p.r.done = nil, nil, nil, nil
	}
	s.shares.mu.Unlock()
}

// IsShareNode reports whether path is an app's share node, so the agent's own
// camera code never treats one as a camera: share nodes announce CAPTURE
// (announce_all_caps), which a capability probe cannot tell from a camera.
// A node is a share node when a record names it (live, loaded from the state
// file and not yet adopted, or being made), or when it is in the band share
// nodes come from and its sysfs name is that of a camera a record shares: a
// node whose record never got written, say because the agent stopped between
// making it and saving the state file.
func (s *VideoService) IsShareNode(path string) bool {
	nr, ok := videoNodeNumber(filepath.Base(path))
	if !ok {
		return false
	}
	return s.isShareNodeNumber(nr, func() (string, bool) {
		if s.readDeviceName == nil {
			return "", false
		}
		name, err := s.readDeviceName(fmt.Sprintf("video%d", nr))
		return name, err == nil
	})
}

// isShareNodeNumber is IsShareNode for node nr; name reads the node's sysfs
// name and is called only for a node in the band that no record names.
func (s *VideoService) isShareNodeNumber(nr int, name func() (string, bool)) bool {
	s.shares.mu.Lock()
	if s.shares.pending[nr] {
		s.shares.mu.Unlock()
		return true
	}
	names := map[string]bool{}
	for _, c := range s.shares.containers {
		for _, r := range c.shares {
			if r.node == nr {
				s.shares.mu.Unlock()
				return true
			}
			names[shareNodeName(r.cameraName)] = true
		}
	}
	s.shares.mu.Unlock()
	if nr < ipcam.IDBandStart || nr > ipcam.IDBandEnd || len(names) == 0 {
		return false
	}
	got, ok := name()
	return ok && names[shareNodeName(got)]
}

// shareNodeName is a camera name as a node labelled with it reports it.
func shareNodeName(name string) string {
	name = strings.TrimSpace(name)
	if len(name) > v4l2NameBytes {
		name = name[:v4l2NameBytes]
	}
	return name
}

// videoNodeNumber parses "video<N>".
func videoNodeNumber(base string) (int, bool) {
	num, ok := strings.CutPrefix(base, "video")
	if !ok {
		return 0, false
	}
	nr, err := strconv.ParseUint(num, 10, 16)
	if err != nil {
		return 0, false
	}
	return int(nr), true
}

func shareNodePath(nr int) string { return fmt.Sprintf("/dev/video%d", nr) }

// withinBudget runs f on its own goroutine and returns its result, or ok false
// when ctx ends first. f then keeps running and its result is dropped, so it
// is only for work that changes nothing a caller depends on.
func withinBudget[T any](ctx context.Context, f func() T) (T, bool) {
	ch := make(chan T, 1)
	go func() { ch <- f() }()
	select {
	case v := <-ch:
		return v, true
	case <-ctx.Done():
		select {
		case v := <-ch:
			return v, true
		default:
		}
		var zero T
		return zero, false
	}
}

// shareSetupStopped is the reason a camera got no share when CreateCameraShares
// stopped before reaching it.
func shareSetupStopped(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("camera sharing setup ran out of time (%s)", cameraShareCreateBudget)
	}
	return "camera sharing setup was cancelled"
}

// logRealCamera logs why a container gets a camera itself rather than a share;
// warn marks a failure rather than a policy.
func (s *VideoService) logRealCamera(warn bool, containerID string, dev *agentpb.VideoDevice, reason string) {
	msg := fmt.Sprintf("camera share: %s gets the real camera %s (%s): %s", containerID, dev.GetName(), dev.GetPath(), reason)
	if warn {
		s.shareLogger().Warn(msg)
	} else {
		s.shareLogger().Info(msg)
	}
}

func (s *VideoService) shareLogger() *zap.Logger {
	if s.logger == nil {
		return zap.NewNop()
	}
	return s.logger
}

// serviceContext is s.ctx, or Background for a service built as a struct
// literal.
func (s *VideoService) serviceContext() context.Context {
	if s.ctx == nil {
		return context.Background()
	}
	return s.ctx
}

func (s *VideoService) identityOf(base string) (camera.Identity, error) {
	if s.cameraIdentity != nil {
		return s.cameraIdentity(base)
	}
	return camera.IdentityOf(base)
}

func (s *VideoService) presentIdentities() map[string]camera.Identity {
	if s.presentCameras != nil {
		return s.presentCameras()
	}
	return camera.PresentIdentities()
}

// The state file: {"version":1,"containers":{"<id>":{"app":…,"shares":[…]}}}.
type cameraShareState struct {
	Version    int                                  `json:"version"`
	Containers map[string]cameraShareStateContainer `json:"containers"`
}

type cameraShareStateContainer struct {
	App    string                  `json:"app"`
	Shares []cameraShareStateShare `json:"shares"`
}

type cameraShareStateShare struct {
	Camera cameraShareStateCamera `json:"camera"`
	Path   string                 `json:"path"`
	Mode   cameraShareStateMode   `json:"mode"`
	Node   int                    `json:"node"`
}

type cameraShareStateCamera struct {
	Device string `json:"device"`
	Index  string `json:"index"`
	Name   string `json:"name"`
}

type cameraShareStateMode struct {
	Width        uint32 `json:"width"`
	Height       uint32 `json:"height"`
	Fourcc       string `json:"fourcc"`
	BytesPerLine uint32 `json:"bytesPerLine"`
	FPS          uint32 `json:"fps"`
}

// sharesStatePathLocked is the state file's path, fixed at first use. Callers
// hold s.shares.fileMu.
func (s *VideoService) sharesStatePathLocked() string {
	if s.shares.statePath == "" {
		s.shares.statePath = cameraSharesPath
	}
	return s.shares.statePath
}

// persistCameraShares writes every record to the state file. A failure is
// logged: the shares work, but an agent restart would not adopt them.
func (s *VideoService) persistCameraShares() {
	s.shares.fileMu.Lock()
	defer s.shares.fileMu.Unlock()
	path := s.sharesStatePathLocked()

	state := cameraShareState{Version: cameraShareStateVersion, Containers: map[string]cameraShareStateContainer{}}
	s.shares.mu.Lock()
	for id, c := range s.shares.containers {
		sc := cameraShareStateContainer{App: c.app, Shares: make([]cameraShareStateShare, 0, len(c.shares))}
		for _, r := range c.shares {
			sc.Shares = append(sc.Shares, cameraShareStateShare{
				Camera: cameraShareStateCamera{Device: r.camera.Device, Index: r.camera.Index, Name: r.camera.Name},
				Path:   r.cameraPath,
				Mode: cameraShareStateMode{Width: r.mode.Width, Height: r.mode.Height, Fourcc: r.mode.Fourcc,
					BytesPerLine: r.mode.BytesPerLine, FPS: r.mode.FPS},
				Node: r.node,
			})
		}
		state.Containers[id] = sc
	}
	s.shares.mu.Unlock()

	data, err := json.Marshal(state)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(path), 0o755)
	}
	if err == nil {
		err = atomicfile.Write(path, data, 0o644)
	}
	if err != nil {
		s.shareLogger().Warn(fmt.Sprintf("camera share: saving %s failed: %v; after an agent restart these shares would not be adopted", path, err))
	}
}

// loadCameraShares reads the state file into the index, as records without a
// writer or pump: SyncCameraShares adopts them. A file that does not parse is
// moved aside to <path>.corrupt and logged. A record that could not have been
// written by this agent (a node outside the band, a mode without a known
// format) is dropped, so it never hides a real camera from IsShareNode.
func (s *VideoService) loadCameraShares() {
	log := s.shareLogger()
	s.shares.fileMu.Lock()
	defer s.shares.fileMu.Unlock()
	path := s.sharesStatePathLocked()

	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		log.Warn(fmt.Sprintf("camera share: reading %s failed: %v; shares made before this agent started are not adopted", path, err))
		return
	}
	var state cameraShareState
	err = json.Unmarshal(data, &state)
	if err == nil && state.Version != cameraShareStateVersion {
		err = fmt.Errorf("version %d, this agent reads version %d", state.Version, cameraShareStateVersion)
	}
	if err != nil {
		corrupt := path + ".corrupt"
		if rerr := os.Rename(path, corrupt); rerr != nil {
			log.Warn(fmt.Sprintf("camera share: %s does not parse (%v), and moving it to %s failed: %v", path, err, corrupt, rerr))
		} else {
			log.Warn(fmt.Sprintf("camera share: %s does not parse (%v); moved it to %s and starting with no shares", path, err, corrupt))
		}
		return
	}

	containers := make(map[string]*shareContainer, len(state.Containers))
	for id, c := range state.Containers {
		sc := &shareContainer{app: c.App}
		for _, sh := range c.Shares {
			r := &shareRecord{
				cameraName: sh.Camera.Name,
				cameraPath: sh.Path,
				mode: sharedMode{Width: sh.Mode.Width, Height: sh.Mode.Height, Fourcc: sh.Mode.Fourcc,
					BytesPerLine: sh.Mode.BytesPerLine, FPS: sh.Mode.FPS},
				node:   sh.Node,
				camera: camera.Identity{Device: sh.Camera.Device, Index: sh.Camera.Index, Name: sh.Camera.Name},
			}
			if r.node < ipcam.IDBandStart || r.node > ipcam.IDBandEnd || r.mode.pixelFormat() == 0 || r.mode.frameBytes() <= 0 {
				log.Warn(fmt.Sprintf("camera share: %s: dropping %s's share of %q at node %d with mode %s; this agent could not have made it",
					path, id, sh.Camera.Name, sh.Node, r.mode))
				continue
			}
			sc.shares = append(sc.shares, r)
		}
		if len(sc.shares) > 0 {
			containers[id] = sc
		}
	}
	s.shares.mu.Lock()
	s.shares.containers = containers
	s.shares.mu.Unlock()
	if len(containers) > 0 {
		log.Info(fmt.Sprintf("camera share: loaded the shares of %d containers from %s; the next sync adopts them", len(containers), path))
	}
}
