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
	"syscall"
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

// Removing a share node safely. v4l2loopback 0.15.4's CTL_REMOVE checks only
// the node's open count under its control mutex, and open() does not take that
// mutex, so an open() racing a removal can succeed on a node that is then
// freed; its close() then dereferences NULL and the kernel panics (it reset a
// Jetson). Every new node draws such openers: udev's v4l_id runs on the add
// event (opening ~6-7 ms after CTL_ADD), and WirePlumber opens nodes that
// announce capture, as share nodes do, ~3 ms after udev's add. So a share node
// is removed only once udev has processed its add event and it is at least
// shareNodeSettleAge old (waiting at most shareNodeSettleBound for both), and
// then only when no other process has it open. The wait runs in the
// background, so it never holds up a container's create, start or delete.
const (
	shareNodeSettleAge   = time.Second
	shareNodeSettleBound = 2 * time.Second
	shareNodeSettlePoll  = 20 * time.Millisecond
)

// shareClock is the share node removal's clock; a seam so tests run the waits
// on a fake one.
type shareClock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realShareClock struct{}

func (realShareClock) Now() time.Time                         { return time.Now() }
func (realShareClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Seams for the removal of share nodes: its clock, whether udev has processed
// a node's add event, and which other processes hold a device open.
var (
	shareRemovalClock shareClock = realShareClock{}
	shareNodeUdevDone            = udevProcessedVideoNode
	deviceHolders                = procDeviceHolders
)

// shareRemoval is a share node waiting to be removed. nr, name, made and
// checkName never change once it is queued; running and holder are guarded by
// cameraShares.mu.
type shareRemoval struct {
	nr   int
	name string // the label it was made with: the camera's name
	// made is when this agent made the node; zero for a node an earlier agent
	// made, which counts as settled.
	made time.Time
	// checkName: the node was neither made nor opened by this agent, so its
	// sysfs name is checked before it is removed.
	checkName bool
	running   bool // an attempt is in flight
	holder    int  // the pid last logged as holding it
}

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

	nr, made, reason := s.makeShareNode(ctx, dev.GetName())
	if reason != "" {
		s.logRealCamera(true, containerID, dev, reason)
		return
	}
	path := shareNodePath(nr)
	node, err := openShareNode(s.logger, path, decision.Mode)
	if err != nil {
		s.unmakeShareNode(nr, dev.GetName(), made)
		s.logRealCamera(true, containerID, dev, err.Error())
		return
	}
	if err := ctx.Err(); err != nil {
		// Made, but after the budget: the camera counts as not done.
		s.closeShareWriter(path, node)
		s.unmakeShareNode(nr, dev.GetName(), made)
		s.logRealCamera(true, containerID, dev, shareSetupStopped(err))
		return
	}

	r := &shareRecord{cameraName: dev.GetName(), cameraPath: dev.GetPath(), mode: decision.Mode, node: nr, camera: id, nodeMade: made}
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
// unmakeShareNode, and made is when the node was made. A non-empty reason
// means no node is left behind, or one is queued for removal.
func (s *VideoService) makeShareNode(ctx context.Context, label string) (nr int, made time.Time, reason string) {
	s.twoPlaneStartMu.Lock()
	defer s.twoPlaneStartMu.Unlock()
	nr, err := s.loopback.AllocateAuxNodeNumber()
	if errors.Is(err, ipcam.ErrBandExhausted) {
		return 0, time.Time{}, fmt.Sprintf("no free v4l2loopback node number (%d-%d are in use)", ipcam.IDBandStart, ipcam.IDBandEnd)
	}
	if err != nil {
		return 0, time.Time{}, "allocating a v4l2loopback node failed: " + err.Error()
	}
	s.shares.mu.Lock()
	if s.shares.pending == nil {
		s.shares.pending = make(map[int]bool)
	}
	s.shares.pending[nr] = true
	s.shares.mu.Unlock()
	err = s.loopback.EnsureShareNode(ctx, nr, label)
	made = shareRemovalClock.Now()
	if err != nil {
		if _, exists := s.loopback.NodePath(uint32(nr)); !exists {
			s.forgetPendingShareNode(nr)
		} else {
			s.unmakeShareNode(nr, label, made)
		}
		return 0, time.Time{}, fmt.Sprintf("creating v4l2loopback node %d failed: %v", nr, err)
	}
	return nr, made, ""
}

// unmakeShareNode queues the removal of a node makeShareNode made whose share
// was never stored. The node stays a share node until it is gone.
func (s *VideoService) unmakeShareNode(nr int, label string, made time.Time) {
	s.shares.mu.Lock()
	starts := s.queueShareRemovalsLocked([]*shareRemoval{{nr: nr, name: label, made: made}})
	delete(s.shares.pending, nr)
	s.shares.mu.Unlock()
	s.persistCameraShares()
	for _, start := range starts {
		start()
	}
}

func (s *VideoService) forgetPendingShareNode(nr int) {
	s.shares.mu.Lock()
	delete(s.shares.pending, nr)
	s.shares.mu.Unlock()
}

// queueShareRemovalsLocked adds removals to the queue, one entry per node
// number (a node already queued keeps its entry, so it is never removed
// twice), and returns the functions that start the attempts not already in
// flight. Callers hold s.shares.mu, persist, then call the functions.
func (s *VideoService) queueShareRemovalsLocked(removals []*shareRemoval) []func() {
	if s.shares.removing == nil {
		s.shares.removing = make(map[int]*shareRemoval)
	}
	var starts []func()
	for _, rm := range removals {
		if cur := s.shares.removing[rm.nr]; cur != nil {
			rm = cur
		} else {
			s.shares.removing[rm.nr] = rm
		}
		if start := s.startShareRemovalLocked(rm); start != nil {
			starts = append(starts, start)
		}
	}
	return starts
}

// startShareRemovalLocked returns the function that starts an attempt at rm on
// its own tracked goroutine, or nil when one is in flight or Shutdown has run.
// Callers hold s.shares.mu.
func (s *VideoService) startShareRemovalLocked(rm *shareRemoval) func() {
	if rm.running || s.shares.closed {
		return nil
	}
	if s.shares.removalCtx == nil {
		s.shares.removalCtx, s.shares.removalCancel = context.WithCancel(s.serviceContext())
	}
	rm.running = true
	s.shares.removalWG.Add(1)
	ctx := s.shares.removalCtx
	return func() {
		go func() {
			defer s.shares.removalWG.Done()
			if !s.waitShareNodeSettled(ctx, rm) {
				s.shares.mu.Lock()
				rm.running = false
				s.shares.mu.Unlock()
				return
			}
			s.attemptShareNodeRemoval(rm)
		}()
	}
}

// waitShareNodeSettled waits until udev has processed the node's add event and
// the node is at least shareNodeSettleAge old, for at most
// shareNodeSettleBound; past the bound it goes ahead anyway, because the
// holder check still guards the removal. It reports false when ctx ends first.
func (s *VideoService) waitShareNodeSettled(ctx context.Context, rm *shareRemoval) bool {
	clock := shareRemovalClock
	deadline := clock.Now().Add(shareNodeSettleBound)
	for {
		now := clock.Now()
		if (rm.made.IsZero() || now.Sub(rm.made) >= shareNodeSettleAge) && shareNodeUdevDone(rm.nr) {
			return true
		}
		if !now.Before(deadline) {
			s.shareLogger().Info(fmt.Sprintf("camera share: %s is not settled after %s (udev's add event, %s of age); removing it once nothing holds it",
				shareNodePath(rm.nr), shareNodeSettleBound, shareNodeSettleAge))
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-clock.After(shareNodeSettlePoll):
		}
	}
}

// attemptShareNodeRemoval removes a queued node unless another process holds
// it open; then the node stays, still a share node and its number not reused,
// and the next SyncCameraShares tries again. A node already gone, or (for one
// this agent never made or opened) now another node, just leaves the queue.
func (s *VideoService) attemptShareNodeRemoval(rm *shareRemoval) {
	path := shareNodePath(rm.nr)
	log := s.shareLogger()
	if s.loopback == nil {
		s.finishShareRemoval(rm)
		return
	}
	if _, exists := s.loopback.NodePath(uint32(rm.nr)); !exists {
		s.finishShareRemoval(rm)
		return
	}
	if rm.checkName && !s.nodeNamed(rm.nr, rm.name) {
		log.Warn(fmt.Sprintf("camera share: %s is no longer the share node of %s; it is left alone", path, rm.name))
		s.finishShareRemoval(rm)
		return
	}
	// Immediately before the removal: the writer is already closed, and the
	// agent's own pid is not counted.
	if holders := deviceHolders(path); len(holders) > 0 {
		h := holders[0]
		s.shares.mu.Lock()
		rm.running = false
		repeated := rm.holder == h.PID
		rm.holder = h.PID
		s.shares.mu.Unlock()
		if !repeated {
			log.Warn(fmt.Sprintf("camera share: left %s in place: it is open in pid %d (%s); it is removed later", path, h.PID, h.Comm))
		}
		return
	}
	s.removeShareNode(rm.nr)
	if _, exists := s.loopback.NodePath(uint32(rm.nr)); exists {
		// The driver refused (EBUSY: something opened it after the check).
		s.shares.mu.Lock()
		rm.running = false
		s.shares.mu.Unlock()
		log.Warn(fmt.Sprintf("camera share: left %s in place: removing it did not take; it is removed later", path))
		return
	}
	s.finishShareRemoval(rm)
	log.Info(fmt.Sprintf("camera share: removed %s, the share node of %s", path, rm.name))
}

// finishShareRemoval takes rm out of the queue and saves the state file.
func (s *VideoService) finishShareRemoval(rm *shareRemoval) {
	s.shares.mu.Lock()
	if s.shares.removing[rm.nr] == rm {
		delete(s.shares.removing, rm.nr)
	}
	rm.running = false
	s.shares.mu.Unlock()
	s.persistCameraShares()
}

// removeShareNode deletes share node nr. It is called only by
// attemptShareNodeRemoval, after the node has settled and nothing else holds
// it, so a change to how a node is removed has one place to land.
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

// RemoveCameraShares removes a container's shares: for each, it cancels the
// pump, waits for it and closes the writer; then it forgets the container's
// records and queues their nodes for removal (retireShares). The removals run
// in the background, so a delete is never held up by them; until a node is
// gone it stays a share node and its number is not reused, so a redeploy's
// new shares take other numbers meanwhile. A container with no shares is left
// alone.
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
	s.shareLogger().Info(fmt.Sprintf("camera share: removing %s's share nodes", containerID))
	s.retireShares(containerID, recs)
}

// retireShares stops recs, then takes them out of the container's shares and
// queues their nodes for removal in one step, so the state file never holds a
// node both as a share and as a removal; it then saves the file and starts the
// removals. Callers hold s.shares.ops.
func (s *VideoService) retireShares(containerID string, recs []*shareRecord) {
	var removals []*shareRemoval
	for _, r := range recs {
		if rm := s.stopShare(r); rm != nil {
			removals = append(removals, rm)
		}
	}
	s.shares.mu.Lock()
	s.forgetSharesLocked(containerID, recs)
	starts := s.queueShareRemovalsLocked(removals)
	s.shares.mu.Unlock()
	s.persistCameraShares()
	for _, start := range starts {
		start()
	}
}

// stopShare stops one share: it cancels the pump, waits for it and closes the
// writer. It returns the removal of the record's node, or nil when there is no
// node of this record's to remove: a record never opened in this agent's life
// (loaded, not adopted) whose node is gone (a reboot) or is now another node.
// Callers hold s.shares.ops.
func (s *VideoService) stopShare(r *shareRecord) *shareRemoval {
	s.shares.mu.Lock()
	cancel, done, node := r.cancel, r.done, r.io
	s.shares.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	if node != nil {
		s.closeShareWriter(shareNodePath(r.node), node)
		return &shareRemoval{nr: r.node, name: r.cameraName, made: r.nodeMade}
	}
	if !s.nodeIsShareFor(r) {
		return nil
	}
	return &shareRemoval{nr: r.node, name: r.cameraName, checkName: true}
}

func (s *VideoService) closeShareWriter(path string, node shareNodeIO) {
	if err := node.Close(); err != nil {
		s.shareLogger().Warn(fmt.Sprintf("camera share: closing the writer of %s failed: %v", path, err))
	}
}

// forgetSharesLocked takes recs out of the container's shares, and the
// container out of the index once it has none. Callers hold s.shares.mu.
func (s *VideoService) forgetSharesLocked(containerID string, recs []*shareRecord) {
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
// is gone (a reboot) is left for the container's next start. Every node still
// waiting to be removed (another process held it open) is tried again.
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
			s.retireShares(id, gone[id])
		}
	}

	s.shares.mu.Lock()
	var retries []func()
	for _, rm := range s.shares.removing {
		if start := s.startShareRemovalLocked(rm); start != nil {
			retries = append(retries, start)
		}
	}
	s.shares.mu.Unlock()
	for _, start := range retries {
		start()
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
	return s.nodeNamed(r.node, r.cameraName)
}

// nodeNamed reports whether node nr's sysfs name is the one a node labelled
// with label reports. An unreadable name is not a match.
func (s *VideoService) nodeNamed(nr int, label string) bool {
	if s.readDeviceName == nil {
		return true
	}
	name, err := s.readDeviceName(fmt.Sprintf("video%d", nr))
	return err == nil && shareNodeName(name) == shareNodeName(label)
}

// stopCameraSharePumps stops every pump and closes every writer, for Shutdown,
// then cancels the removals waiting for a node to settle and waits for their
// goroutines. Nodes, records and queued removals stay (the state file holds
// them): the next agent adopts the nodes and retries the removals.
func (s *VideoService) stopCameraSharePumps() {
	defer s.stopShareRemovals()
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

// stopShareRemovals starts no more removal attempts, cancels the waiting ones
// and waits for every attempt's goroutine. One already past its wait finishes
// its holder check and removal first, which takes milliseconds.
func (s *VideoService) stopShareRemovals() {
	s.shares.mu.Lock()
	s.shares.closed = true
	cancel := s.shares.removalCancel
	s.shares.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.shares.removalWG.Wait()
}

// IsShareNode reports whether path is an app's share node, so the agent's own
// camera code never treats one as a camera: share nodes announce CAPTURE
// (announce_all_caps), which a capability probe cannot tell from a camera.
// A node is a share node when a record names it (live, loaded from the state
// file and not yet adopted, being made, or waiting to be removed), or when it
// is in the band share nodes come from and its sysfs name is that of a camera
// a record shares: a node whose record never got written, say because the
// agent stopped between making it and saving the state file.
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
	if s.shares.pending[nr] || s.shares.removing[nr] != nil {
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

// The state file: {"version":1,"containers":{"<id>":{"app":…,"shares":[…]}}},
// plus "removing":[{"node":254,"name":…}] while a share node waits to be
// removed, so a removal a holder or Shutdown interrupted is retried by the next
// agent. An older agent ignores "removing".
type cameraShareState struct {
	Version    int                                  `json:"version"`
	Containers map[string]cameraShareStateContainer `json:"containers"`
	Removing   []cameraShareStateRemoval            `json:"removing,omitempty"`
}

type cameraShareStateRemoval struct {
	Node int    `json:"node"`
	Name string `json:"name"`
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
	for _, rm := range s.shares.removing {
		state.Removing = append(state.Removing, cameraShareStateRemoval{Node: rm.nr, Name: rm.name})
	}
	s.shares.mu.Unlock()
	sort.Slice(state.Removing, func(i, j int) bool { return state.Removing[i].Node < state.Removing[j].Node })

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
	// A queued removal from an earlier agent: its node is old, and its name is
	// checked before it is removed, since after a reboot the number may be
	// another node's.
	removing := make(map[int]*shareRemoval, len(state.Removing))
	for _, rm := range state.Removing {
		if rm.Node < ipcam.IDBandStart || rm.Node > ipcam.IDBandEnd {
			log.Warn(fmt.Sprintf("camera share: %s: dropping the removal of node %d; this agent could not have made it", path, rm.Node))
			continue
		}
		removing[rm.Node] = &shareRemoval{nr: rm.Node, name: rm.Name, checkName: true}
	}
	s.shares.mu.Lock()
	s.shares.containers = containers
	s.shares.removing = removing
	s.shares.mu.Unlock()
	if len(containers) > 0 || len(removing) > 0 {
		log.Info(fmt.Sprintf("camera share: loaded the shares of %d containers and %d nodes to remove from %s; the next sync adopts and removes them",
			len(containers), len(removing), path))
	}
}

// udevProcessedVideoNode reports whether udev has processed the add event of
// /dev/video<nr>: its database entry /run/udev/data/c<major>:<minor> exists. A
// node that is gone has nothing to wait for, and neither does a system without
// a udev database.
func udevProcessedVideoNode(nr int) bool {
	devNum, err := os.ReadFile(fmt.Sprintf("/sys/class/video4linux/video%d/dev", nr))
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	if _, err := os.Stat("/run/udev/data"); err != nil {
		return true
	}
	_, err = os.Stat("/run/udev/data/c" + strings.TrimSpace(string(devNum)))
	return err == nil
}

// deviceHolder is a process that holds a device node open.
type deviceHolder struct {
	PID     int
	Comm    string
	MountNS uint64 // the inode of /proc/<pid>/ns/mnt
}

// procRoot is /proc; a variable so tests can scan a fixture tree.
var procRoot = "/proc"

// procDeviceHolders lists the processes other than the agent that hold the
// character device at path open, by comparing each /proc/<pid>/fd/* entry's
// rdev with the device's.
func procDeviceHolders(path string) []deviceHolder {
	want, ok := charDeviceRdev(path)
	if !ok {
		return nil
	}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var out []deviceHolder
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		dir := filepath.Join(procRoot, e.Name())
		fds, err := os.ReadDir(filepath.Join(dir, "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			if rdev, ok := charDeviceRdev(filepath.Join(dir, "fd", fd.Name())); ok && rdev == want {
				h := deviceHolder{PID: pid}
				if comm, err := os.ReadFile(filepath.Join(dir, "comm")); err == nil {
					h.Comm = strings.TrimSpace(string(comm))
				}
				if fi, err := os.Stat(filepath.Join(dir, "ns", "mnt")); err == nil {
					if st, ok := fi.Sys().(*syscall.Stat_t); ok {
						h.MountNS = uint64(st.Ino)
					}
				}
				out = append(out, h)
				break
			}
		}
	}
	return out
}

// charDeviceRdev is the device number of the character device at path, after
// following links.
func charDeviceRdev(path string) (uint64, bool) {
	fi, err := os.Stat(path)
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Rdev), true
}
