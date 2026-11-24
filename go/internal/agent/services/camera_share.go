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
	"os/user"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/devcover"
	"github.com/wendylabsinc/wendy/go/internal/agent/devwatch"
	"github.com/wendylabsinc/wendy/go/internal/agent/hardware"
	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
	"github.com/wendylabsinc/wendy/go/internal/shared/camerashare"
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

// The state file formats this agent reads and writes. Version 1 has no format
// or stamp: its shares are pixels with capture times. An agent that knows only
// version 1 ignores a version 2 file, and the nodes of running apps then get no
// writer, so version 2 is written only while some share needs it.
const (
	cameraShareStateVersion                = 2
	cameraShareStateVersionWithoutSettings = 1
)

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
// a node's add event, and which other processes hold a node open (an error
// means that could not be told). deviceHolders is the path-based scan the
// replug check uses (checkWindowLosers).
var (
	shareRemovalClock shareClock = realShareClock{}
	shareNodeUdevDone            = udevProcessedVideoNode
	shareNodeHolders             = sysfsShareNodeHolders
	deviceHolders                = procDeviceHolders
)

// Seams for a share node's /dev entry: giving a new node udev's owner group
// and mode; the kernel's device number for a node (from sysfs, which a missing
// /dev entry does not change; fs.ErrNotExist once the kernel has no such
// node); taking the entry away just before a removal, only while it is that
// device; and putting it back when the removal does not take.
var (
	setShareNodeAccess    = setVideoNodeAccess
	shareNodeRdev         = sysfsVideoRdev
	unlinkShareNodeEntry  = unlinkVideoNodeEntry
	restoreShareNodeEntry = restoreVideoNodeEntry
)

// shareRemovalShutdownWait bounds how long Shutdown waits for removal attempts
// still in flight; a variable so tests can shorten it.
var shareRemovalShutdownWait = 3 * time.Second

// shareRemoval is a share node waiting to be removed. nr, name and made never
// change once it is queued; running and noted are guarded by cameraShares.mu.
type shareRemoval struct {
	nr   int
	name string // the label it was made with: the camera's name
	// made is when this agent made the node; zero for a node an earlier agent
	// made, which counts as settled.
	made    time.Time
	running bool            // an attempt is in flight
	noted   map[string]bool // what was logged about it, so each is said once
	// restored is the device number of a /dev entry this agent made again
	// (mknod) after a removal did not take, or zero. The kernel never removes
	// such an entry itself, so the next attempt checks it is still the node's
	// (settleRestoredEntry). Only attemptShareNodeRemoval touches it, under
	// twoPlaneStartMu, one attempt at a time.
	restored uint64
}

// CameraShareContainer is one Wendy container as SyncCameraShares sees it.
type CameraShareContainer struct {
	ID      string
	Running bool
	PID     uint32
}

// CreateCameraShares gives a newly created container one share node for each
// shared camera present, within cameraShareCreateBudget. Each node's format
// and stamp are what the container's entitlements say for its camera. It logs
// why a camera is not shared and never fails: a camera it does not share is
// the real camera, exactly as before sharing existed.
func (s *VideoService) CreateCameraShares(ctx context.Context, containerID, appID string, entitlements []appconfig.Entitlement) {
	ctx, cancel := context.WithTimeout(ctx, cameraShareCreateBudget)
	defer cancel()
	log := s.shareLogger()
	creation, ok := s.beginShareCreation(containerID)
	if !ok || (s.ctx != nil && s.ctx.Err() != nil) {
		log.Info(fmt.Sprintf("camera share: %s gets the real cameras: %s", containerID, shareRefusedStopping))
		if ok {
			s.endShareCreation(containerID, creation)
		}
		return
	}
	defer s.endShareCreation(containerID, creation)
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
		s.createCameraShare(ctx, containerID, appID, dev, entitlements, creation)
	}
}

// shareCreation is one CreateCameraShares call in flight. removed (guarded by
// cameraShares.mu) is set by a RemoveCameraShares of the same container, after
// which the call stores nothing more.
type shareCreation struct{ removed bool }

// Why a share made in time is not stored.
const (
	shareRefusedStopping = "the video service is shutting down"
	shareRefusedRemoved  = "the container was removed while its shares were being made"
)

// beginShareCreation registers a CreateCameraShares call, or reports false
// once Shutdown has begun.
func (s *VideoService) beginShareCreation(containerID string) (*shareCreation, bool) {
	s.shares.mu.Lock()
	defer s.shares.mu.Unlock()
	if s.shares.stopped {
		return nil, false
	}
	if s.shares.creating == nil {
		s.shares.creating = make(map[string][]*shareCreation)
	}
	c := &shareCreation{}
	s.shares.creating[containerID] = append(s.shares.creating[containerID], c)
	return c, true
}

func (s *VideoService) endShareCreation(containerID string, c *shareCreation) {
	s.shares.mu.Lock()
	defer s.shares.mu.Unlock()
	list := s.shares.creating[containerID]
	for i, x := range list {
		if x == c {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(s.shares.creating, containerID)
	} else {
		s.shares.creating[containerID] = list
	}
}

// openCameraShareNode also records whether listings can offer fresh captures.
func (s *VideoService) openCameraShareNode(path string, mode sharedMode) (shareNodeIO, error) {
	node, err := openShareNode(s.logger, path, mode)
	s.noteFreshStartSupport(err)
	return node, err
}

func (s *VideoService) noteFreshStartSupport(err error) {
	s.shares.mu.Lock()
	if errors.Is(err, errShareFreshStartUnsupported) {
		s.shares.freshStartUnsupported = true
	} else if err == nil {
		s.shares.freshStartUnsupported = false
	}
	s.shares.mu.Unlock()
}

// createCameraShare shares one camera with a container, or logs why not.
func (s *VideoService) createCameraShare(ctx context.Context, containerID, appID string, dev *agentpb.VideoDevice, entitlements []appconfig.Entitlement, creation *shareCreation) {
	format, stamp := appconfig.CameraNodeSettings(entitlements, dev.GetPath(), dev.GetName())
	// The policy goes first, so a camera it refuses (CSI, thermal, depth) is
	// logged with that reason. shareDecisionFor can block well past the
	// budget (its GStreamer element listing is not context-aware), so it runs
	// on its own goroutine.
	decision, ok := withinBudget(ctx, func() shareDecision { return s.shareDecisionFor(ctx, dev, format) })
	if !ok {
		s.logRealCamera(true, containerID, dev, shareSetupStopped(ctx.Err()))
		return
	}
	if !decision.Shared {
		s.logRealCamera(false, containerID, dev, decision.Reason)
		return
	}
	// A decision that arrives at the deadline makes nothing.
	if err := ctx.Err(); err != nil {
		s.logRealCamera(true, containerID, dev, shareSetupStopped(err))
		return
	}
	id, err := s.identityOf(filepath.Base(dev.GetPath()))
	if err != nil {
		s.logRealCamera(true, containerID, dev, "its sysfs identity could not be read: "+err.Error())
		return
	}
	if existing := s.shareOfCamera(containerID, id); existing != nil {
		if existing.mode.asksWhat(decision.Mode) && existing.stamp == stamp && existing.askedFormat == decision.AskedFormat {
			return
		}
		// A record left from an earlier container of this name: this
		// container's entitlements decide its node.
		if _, ok := withinBudget(ctx, func() struct{} { s.retireStaleShare(containerID, existing); return struct{}{} }); !ok {
			s.logRealCamera(true, containerID, dev, shareSetupStopped(ctx.Err()))
			return
		}
		s.shareLogger().Info(fmt.Sprintf("camera share: %s was created again: its share of %s was %s and is made again as %s",
			containerID, dev.GetName(), describeShareNode(existing.mode, existing.stamp), describeShareNode(decision.Mode, stamp)))
	}

	nr, made, reason := s.makeShareNode(ctx, dev.GetName())
	if reason != "" {
		s.logRealCamera(true, containerID, dev, reason)
		return
	}
	path := shareNodePath(nr)
	node, err := s.openCameraShareNode(path, decision.Mode)
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

	decision.Mode = node.Mode()
	r := &shareRecord{cameraName: dev.GetName(), cameraPath: dev.GetPath(), mode: decision.Mode, stamp: stamp,
		askedFormat: decision.AskedFormat, askedWhyNot: decision.AskedWhyNot, node: nr, camera: id, nodeMade: made,
		siblings: sameModelPorts(id, s.presentCameraIdentities())}
	start, refused := s.storeShare(containerID, appID, r, node, creation)
	if refused != "" {
		s.closeShareWriter(path, node)
		s.unmakeShareNode(nr, dev.GetName(), made)
		s.logRealCamera(true, containerID, dev, refused)
		return
	}
	s.persistCameraShares()
	start()
	s.shareLogger().Info(fmt.Sprintf("camera share: %s shares %s (%s) through %s at %s",
		containerID, dev.GetName(), dev.GetPath(), path, describeShareNode(decision.Mode, stamp)))
	if decision.AskedFormat != "" {
		s.shareLogger().Warn(fmt.Sprintf("camera share: %s: %s (%s): %s", containerID, dev.GetName(), dev.GetPath(),
			describeServedInstead(decision.AskedFormat, decision.Mode.Format, decision.AskedWhyNot)))
	}
}

// makeShareNode allocates a node number and creates the node, labelled with
// the camera's name. It holds twoPlaneStartMu across both steps: allocation
// does not reserve a number, so two allocators could otherwise be handed the
// same one. On success the number is pending until storeShare or
// unmakeShareNode, and made is when the node was made. A non-empty reason
// means no node is left behind, or one is queued for removal.
//
// Every number a share holds is passed over, whether or not its node exists
// now: a record keeps its number across a reboot (which removes every node)
// for the container's next start, and two records of one camera on one number
// cannot be told apart by the node's name.
func (s *VideoService) makeShareNode(ctx context.Context, label string) (nr int, made time.Time, reason string) {
	return s.makeShareNodeFor(ctx, label, nil)
}

// makeShareNodeFor is makeShareNode for owner's node lost in a reboot, when
// owner is not nil: owner's own number is used again when it is free
// (reusableShareNumber), and another is allocated when it is not.
func (s *VideoService) makeShareNodeFor(ctx context.Context, label string, owner *shareRecord) (nr int, made time.Time, reason string) {
	s.twoPlaneStartMu.Lock()
	defer s.twoPlaneStartMu.Unlock()
	if owner != nil {
		// The lock can be held by a node removal's /proc scan: what a start
		// saw before it may no longer hold.
		s.shares.mu.Lock()
		retired := owner.retired
		s.shares.mu.Unlock()
		switch {
		case retired:
			return 0, time.Time{}, shareRefusedRemoved
		case ctx.Err() != nil:
			return 0, time.Time{}, coverStopped(ctx).Error()
		}
	}
	nr, reuse := s.reusableShareNumber(owner)
	if !reuse {
		held := s.heldShareNumbers()
		var err error
		// The allocator stats /dev; a node the kernel still has without its
		// /dev entry (see attemptShareNodeRemoval) is skipped too, or CTL_ADD
		// would answer EEXIST and the share would open nothing.
		nr, err = s.loopback.AllocateAuxNodeNumberSkipping(func(nr int) bool { return held[nr] || shareNumberInKernel(nr) })
		if errors.Is(err, ipcam.ErrBandExhausted) {
			return 0, time.Time{}, fmt.Sprintf("no free v4l2loopback node number (%d-%d are in use)", ipcam.IDBandStart, ipcam.IDBandEnd)
		}
		if err != nil {
			return 0, time.Time{}, "allocating a v4l2loopback node failed: " + err.Error()
		}
	}
	s.shares.mu.Lock()
	if s.shares.pending == nil {
		s.shares.pending = make(map[int]bool)
	}
	s.shares.pending[nr] = true
	s.shares.mu.Unlock()
	err := s.loopback.EnsureShareNode(ctx, nr, label)
	made = shareRemovalClock.Now()
	if err != nil {
		if _, exists := s.loopback.NodePath(uint32(nr)); !exists {
			s.forgetPendingShareNode(nr)
		} else {
			s.unmakeShareNode(nr, label, made)
		}
		return 0, time.Time{}, fmt.Sprintf("creating v4l2loopback node %d failed: %v", nr, err)
	}
	// What udev does about 85 ms later, now: an app that is not root can open
	// a node a start made again from the start's first instruction.
	s.giveShareNodeAccess(shareNodePath(nr))
	return nr, made, ""
}

// reusableShareNumber reports owner's own node number when its node can be
// made there again: no other record names it, no node is being made or waits
// to be removed at it, the kernel has no node there (even one without a /dev
// entry), and the allocator, asked about that number alone, would hand it out
// (it is in the band, no registered camera has it, and no /dev entry is there
// now). heldShareNumbers counts owner's own number, so it is no
// use here. A nil owner has no number. Callers hold twoPlaneStartMu.
func (s *VideoService) reusableShareNumber(owner *shareRecord) (int, bool) {
	if owner == nil {
		return 0, false
	}
	s.shares.mu.Lock()
	nr := owner.node
	held := s.shares.pending[nr] || s.shares.removing[nr] != nil
	for _, c := range s.shares.containers {
		for _, r := range c.shares {
			if r != owner && r.node == nr {
				held = true
			}
		}
	}
	s.shares.mu.Unlock()
	if held || shareNumberInKernel(nr) {
		return 0, false
	}
	got, err := s.loopback.AllocateAuxNodeNumberSkipping(func(n int) bool { return n != nr })
	return nr, err == nil && got == nr
}

// heldShareNumbers is every node number a share holds: each record's (live,
// loaded, or kept across a reboot without its node), each node being made, and
// each node waiting to be removed.
func (s *VideoService) heldShareNumbers() map[int]bool {
	s.shares.mu.Lock()
	defer s.shares.mu.Unlock()
	held := make(map[int]bool)
	for _, c := range s.shares.containers {
		for _, r := range c.shares {
			held[r.node] = true
		}
	}
	for nr := range s.shares.pending {
		held[nr] = true
	}
	for nr := range s.shares.removing {
		held[nr] = true
	}
	return held
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
			s.noteShareRemoval(rm, false, fmt.Sprintf("camera share: %s is not settled after %s (udev's add event, %s of age); removing it once nothing holds it",
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
// it open, or who does cannot be told; then the node stays, still a share node
// and its number not reused, and the next SyncCameraShares tries again. A node
// already gone, a node that is now another node (its sysfs name is not the
// camera's), or a number a share's record holds just leaves the queue.
//
// It holds twoPlaneStartMu throughout, as node allocation does, so no node can
// be made at this number between its checks and the removal: one that was
// would be removed by mistake, possibly while udev or the agent opens it.
func (s *VideoService) attemptShareNodeRemoval(rm *shareRemoval) {
	s.twoPlaneStartMu.Lock()
	defer s.twoPlaneStartMu.Unlock()
	path := shareNodePath(rm.nr)
	keep := func(warn bool, msg string) {
		s.shares.mu.Lock()
		rm.running = false
		s.shares.mu.Unlock()
		s.noteShareRemoval(rm, warn, msg)
	}
	if s.loopback == nil {
		s.finishShareRemoval(rm)
		return
	}
	s.settleRestoredEntry(rm)
	if s.recordHoldsShareNumber(rm.nr) {
		s.noteShareRemoval(rm, true, fmt.Sprintf("camera share: not removing %s: a share's record holds it", path))
		s.finishShareRemoval(rm)
		return
	}
	// Gone means gone from the kernel (sysfs), not from /dev: removeShareNode
	// unlinks the entry before CTL_REMOVE, so a node whose removal was refused
	// and whose entry could not be made again, or one an agent that died
	// mid-attempt left without one, is still a node, and the safest one to
	// remove, since nothing can open it by its path.
	if !shareNumberInKernel(rm.nr) {
		s.finishShareRemoval(rm)
		return
	}
	if !s.nodeNamed(rm.nr, rm.name) {
		s.noteShareRemoval(rm, true, fmt.Sprintf("camera share: %s is no longer the share node of %s; it is left alone", path, rm.name))
		s.finishShareRemoval(rm)
		return
	}
	// Immediately before the removal: the writer is already closed, and the
	// agent's own pid is not counted.
	holders, err := shareNodeHolders(rm.nr)
	if err != nil {
		keep(true, fmt.Sprintf("camera share: left %s in place: what has it open could not be read (%v); it is removed later", path, err))
		return
	}
	if len(holders) > 0 {
		h := holders[0]
		keep(true, fmt.Sprintf("camera share: left %s in place: it is open in pid %d (%s); it is removed later", path, h.PID, h.Comm))
		return
	}
	if !s.removeShareNode(rm) {
		// The driver refused (EBUSY: something opened it after the check).
		keep(true, fmt.Sprintf("camera share: left %s in place: removing it did not take; it is removed later", path))
		return
	}
	s.finishShareRemoval(rm)
	s.shareLogger().Info(fmt.Sprintf("camera share: removed %s, the share node of %s", path, rm.name))
}

// settleRestoredEntry checks, before another attempt, a /dev entry an earlier
// attempt made again (rm.restored). The kernel removes only the entries it
// made itself, so if the node went some other way meanwhile the entry would
// stay, naming a device number that may be another device's by now: it is
// removed. If a new node holds the number with another device number (the
// kernel could not make its entry while this one was in the way), the entry is
// made again with the new node's number. Either way the attempt then goes on
// as for any node. rm.restored is cleared only once the entry it names is gone
// or replaced, so an attempt that stops early (a holder) checks it again next
// time; while it is still the node's own, removeShareNode unlinks it.
func (s *VideoService) settleRestoredEntry(rm *shareRemoval) {
	restored := rm.restored
	if restored == 0 {
		return
	}
	path := shareNodePath(rm.nr)
	now, err := shareNodeRdev(rm.nr)
	switch {
	case err == nil && now == restored:
		return // still the node's own entry
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		s.shareLogger().Warn(fmt.Sprintf("camera share: the entry %s that the agent made again could not be checked: %v", path, err))
		return
	}
	if uerr := unlinkShareNodeEntry(rm.nr, restored); uerr != nil {
		if errors.Is(uerr, fs.ErrNotExist) {
			rm.restored = 0 // already gone
		} else {
			s.shareLogger().Warn(fmt.Sprintf("camera share: removing the stale entry %s that the agent made again failed: %v", path, uerr))
		}
		return
	}
	rm.restored = 0
	if err != nil {
		return // the node is gone, and now so is its entry
	}
	if err := restoreShareNodeEntry(rm.nr, now); err != nil {
		s.shareLogger().Warn(fmt.Sprintf("camera share: making %s again for the node now at that number failed: %v", path, err))
		return
	}
	rm.restored = now
	s.giveShareNodeAccess(path)
}

// shareNumberInKernel reports whether the kernel has a node at nr, whatever
// its /dev entry: a number whose sysfs entry cannot be read counts as taken.
func shareNumberInKernel(nr int) bool {
	_, err := shareNodeRdev(nr)
	return !errors.Is(err, fs.ErrNotExist)
}

// giveShareNodeAccess is setShareNodeAccess, logged when it fails.
func (s *VideoService) giveShareNodeAccess(path string) {
	if err := setShareNodeAccess(path); err != nil {
		s.shareLogger().Warn(fmt.Sprintf("camera share: giving %s udev's owner group and mode (root:video 0660) failed: %v; "+
			"an app that is not root can open it once udev has", path, err))
	}
}

// removeShareNode deletes share node rm.nr and reports whether the kernel no
// longer has it. It is called only by attemptShareNodeRemoval, under
// twoPlaneStartMu, after the node has settled and nothing else holds it, so a
// change to how a node is removed has one place to land.
//
// The node's /dev entry is unlinked first. v4l2loopback 0.15.4's CTL_REMOVE
// can free a node that an open() racing it gets (the kernel panic described at
// shareNodeSettleAge). The openers seen doing that (udev's v4l_id, WirePlumber)
// open the node by its host path, and with the entry gone such an open fails
// with ENOENT before it reaches the driver; only an open already past its path
// lookup can still race. A bind of the node elsewhere (a container's cover)
// holds the inode and still opens it, but a node is removed only once its
// container is gone. If CTL_REMOVE does not take (EBUSY: something opened it
// after the holder scan), the node and its kernel device stay, so the entry is
// made again (mknod, the node's device number, root:video 0660 as udev leaves
// it): the node stays usable and its removal stays queued for the next attempt.
func (s *VideoService) removeShareNode(rm *shareRemoval) (gone bool) {
	if s.loopback == nil {
		return true
	}
	nr := rm.nr
	rdev, err := shareNodeRdev(nr)
	if err == nil {
		err = unlinkShareNodeEntry(nr, rdev)
	}
	unlinked := err == nil
	if unlinked || errors.Is(err, fs.ErrNotExist) {
		rm.restored = 0 // no entry is left for it to name
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.shareLogger().Debug(fmt.Sprintf("camera share: not unlinking %s before removing it: %v", shareNodePath(nr), err))
	}
	s.loopback.RemoveAuxNode(nr)
	if _, err := shareNodeRdev(nr); errors.Is(err, fs.ErrNotExist) {
		return true
	}
	if unlinked {
		if err := restoreShareNodeEntry(nr, rdev); err != nil {
			s.shareLogger().Warn(fmt.Sprintf("camera share: making %s again after its removal did not take failed: %v; "+
				"its removal is tried again, and apps cannot open it by its path meanwhile", shareNodePath(nr), err))
		} else {
			rm.restored = rdev
			s.giveShareNodeAccess(shareNodePath(nr))
		}
	}
	return false
}

// sysfsVideoRdev is video<nr>'s device number, from sysfs; fs.ErrNotExist
// once the kernel has no video<nr>.
func sysfsVideoRdev(nr int) (uint64, error) {
	devNum, err := sysfsVideoDevNum(nr)
	if err != nil {
		return 0, err
	}
	var major, minor uint32
	if _, err := fmt.Sscanf(devNum, "%d:%d", &major, &minor); err != nil {
		return 0, fmt.Errorf("video%d's device number %q: %w", nr, devNum, err)
	}
	return unix.Mkdev(major, minor), nil
}

// unlinkVideoNodeEntry removes /dev/video<nr> when it is the character device
// rdev; anything else at the path is left alone, and a missing entry is
// fs.ErrNotExist.
func unlinkVideoNodeEntry(nr int, rdev uint64) error {
	path := shareNodePath(nr)
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return fs.ErrNotExist
		}
		return err
	}
	if uint32(st.Mode)&unix.S_IFMT != unix.S_IFCHR || uint64(st.Rdev) != rdev {
		return fmt.Errorf("%s is not the character device %d:%d", path, unix.Major(rdev), unix.Minor(rdev))
	}
	return unix.Unlink(path)
}

// restoreVideoNodeEntry makes /dev/video<nr> again for device number rdev,
// root:root 0600 as the kernel makes one; the caller then gives it udev's
// access (setShareNodeAccess).
func restoreVideoNodeEntry(nr int, rdev uint64) error {
	return unix.Mknod(shareNodePath(nr), unix.S_IFCHR|0o600, int(rdev))
}

// setVideoNodeAccess gives a video node the owner group and mode udev's rules
// give every video node, root:video 0660. The kernel makes a node's /dev entry
// root:root 0600 and udev changes it about 85 ms later, so a share node an app
// that is not root opens at its start (a node made again after a reboot) would
// otherwise refuse it.
func setVideoNodeAccess(path string) error {
	group, err := user.LookupGroup("video")
	if err != nil {
		return fmt.Errorf("looking up the video group: %w", err)
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		return fmt.Errorf("the video group's id %q: %w", group.Gid, err)
	}
	if err := os.Chown(path, 0, gid); err != nil {
		return err
	}
	return os.Chmod(path, 0o660)
}

// noteShareRemoval logs msg about rm unless it was logged about it before, so
// a removal retried every sync says each thing once.
func (s *VideoService) noteShareRemoval(rm *shareRemoval, warn bool, msg string) {
	s.shares.mu.Lock()
	repeated := rm.noted[msg]
	if rm.noted == nil {
		rm.noted = make(map[string]bool)
	}
	rm.noted[msg] = true
	s.shares.mu.Unlock()
	switch {
	case repeated:
	case warn:
		s.shareLogger().Warn(msg)
	default:
		s.shareLogger().Info(msg)
	}
}

// recordHoldsShareNumber reports whether any share's record names nr.
func (s *VideoService) recordHoldsShareNumber(nr int) bool {
	s.shares.mu.Lock()
	defer s.shares.mu.Unlock()
	return s.recordHoldsShareNumberLocked(nr)
}

func (s *VideoService) recordHoldsShareNumberLocked(nr int) bool {
	for _, c := range s.shares.containers {
		for _, r := range c.shares {
			if r.node == nr {
				return true
			}
		}
	}
	return false
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

// storeShare adds r to the container's shares with its writer and a pump, and
// returns the function that starts the pump. The record is stored with its
// pump already set, so a sync never takes it for one to adopt. Nothing is
// stored, and refused says why, once Shutdown has begun or when a
// RemoveCameraShares of the container ran while this creation was in flight:
// the check and the store share one lock with Remove's snapshot of the
// container's records, so a racing Remove either takes this record or makes
// the creation refuse it.
func (s *VideoService) storeShare(containerID, appID string, r *shareRecord, node shareNodeIO, creation *shareCreation) (start func(), refused string) {
	s.shares.mu.Lock()
	defer s.shares.mu.Unlock()
	switch {
	case s.shares.stopped:
		return nil, shareRefusedStopping
	case creation != nil && creation.removed:
		return nil, shareRefusedRemoved
	}
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
	return s.attachSharePumpLocked(containerID, r, node), ""
}

// attachSharePumpLocked gives r its writer and a pump, and returns the function
// that starts the pump. The pump's context comes from the service's, so it
// outlives the call that made the share. Callers hold s.shares.mu.
func (s *VideoService) attachSharePumpLocked(containerID string, r *shareRecord, node shareNodeIO) (start func()) {
	ctx, cancel := context.WithCancel(s.serviceContext())
	pump := newSharePump(s.logger, containerID, r.cameraName, node, r.mode, r.stamp,
		func(ctx context.Context) (shareSubscription, error) { return s.joinShare(ctx, r) })
	// Reading for the app again means it reopened the camera: it shares it
	// now, whatever it took after a replug or a failed start cover.
	pump.onReading = func() {
		s.shares.mu.Lock()
		r.tookRealCamera, r.coverFailed = "", ""
		s.shares.mu.Unlock()
	}
	pump.unserved = func() string { return s.shareUnserved(r) }
	done := make(chan struct{})
	r.io, r.pump, r.cancel, r.done = node, pump, cancel, done
	return func() {
		go func() {
			defer close(done)
			pump.Run(ctx)
		}()
	}
}

// A camera that is not plugged in gives "": the join says that.
func (s *VideoService) shareUnserved(r *shareRecord) string {
	if !nodeNeedsOwnFrames(r.mode, r.stamp) {
		return ""
	}
	path, ok := s.currentSharePath(r)
	if !ok {
		return ""
	}
	return s.nodeUnservedAt(path, r.mode, r.stamp)
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
	path, ok, changed := s.resolveSharePath(r)
	if changed {
		s.persistCameraShares()
	}
	return path, ok
}

// resolveSharePath is currentSharePath without the save: changed reports that
// the record changed and the state file needs saving, which a caller on the
// start or replug path does once its covers are done.
func (s *VideoService) resolveSharePath(r *shareRecord) (path string, ok, changed bool) {
	return s.resolveSharePathIn(r, s.presentCameraIdentities())
}

// resolveSharePathIn is resolveSharePath among the present cameras given. A
// record whose camera is not among them is marked absent (cameraAbsent), so a
// different camera now at its old path never inherits its rules or its
// sharers; finding it again clears the mark. A resolve that fails because
// another one followed the camera elsewhere meanwhile marks nothing.
func (s *VideoService) resolveSharePathIn(r *shareRecord, present map[string]camera.Identity) (path string, ok, changed bool) {
	s.shares.mu.Lock()
	want := r.camera
	s.shares.mu.Unlock()
	base, ok, learned := s.resolveShareCamera(r, present)
	if !ok {
		s.shares.mu.Lock()
		if r.camera == want {
			r.cameraAbsent = true
		}
		s.shares.mu.Unlock()
		return "", false, learned
	}
	path = "/dev/" + base
	s.shares.mu.Lock()
	moved := r.cameraPath != path
	r.cameraPath, r.cameraAbsent = path, false
	s.shares.mu.Unlock()
	return path, true, moved || learned
}

// resolveAllShares resolves every record among the cameras present now, so
// each one's path (and whether its camera is present) is current; the state
// file is saved when a record changed. The sync and the camera listing run
// it, so a camera unplugged or swapped while no /dev watch ran is seen within
// one sync.
func (s *VideoService) resolveAllShares() {
	s.shares.mu.Lock()
	var recs []*shareRecord
	for _, c := range s.shares.containers {
		recs = append(recs, c.shares...)
	}
	s.shares.mu.Unlock()
	if len(recs) == 0 {
		return
	}
	present := s.presentCameraIdentities()
	changed := false
	for _, r := range recs {
		if _, _, ch := s.resolveSharePathIn(r, present); ch {
			changed = true
		}
	}
	if changed {
		s.persistCameraShares()
	}
}

// presentCameraIdentities is presentIdentities without share nodes: a share
// node is labelled with its camera's name, and must never pass for it.
func (s *VideoService) presentCameraIdentities() map[string]camera.Identity {
	present := s.presentIdentities()
	for base, id := range present {
		if nr, ok := videoNodeNumber(base); ok && s.isShareNodeNumber(nr, func() (string, bool) { return id.Name, true }) {
			delete(present, base)
		}
	}
	return present
}

// sameModelPorts lists the ports of the present cameras with want's name and
// index other than want's own port, sorted.
func sameModelPorts(want camera.Identity, present map[string]camera.Identity) []string {
	var ports []string
	for _, id := range present {
		if id.Name == want.Name && id.Index == want.Index && id.Device != want.Device && !slices.Contains(ports, id.Device) {
			ports = append(ports, id.Device)
		}
	}
	sort.Strings(ports)
	return ports
}

// resolveShareCamera finds the present node of a record's camera among present
// (presentCameraIdentities, so share nodes are never candidates) with
// camera.Resolve.
//
// When the record's own camera is present, every other present camera of the
// same name and index is learned as a sibling (learned reports a change, for
// the caller to save). When it is absent, a moved camera is followed only if
// it is the one candidate that is not:
//   - on a sibling's port: a camera seen beside this one is another camera;
//   - another record's camera, named exactly;
//   - possibly another record's absent camera of the same name and index,
//     because then which of the two it is cannot be told.
//
// So two same-model cameras never cross. Every record of one camera (the same
// identity: one per sharing container) is one camera, so their siblings count
// together, and a camera followed to another port is the new identity of all
// of them (learned, for the caller to save): a camera of the same model
// plugged into the old port later is then never taken for it by an exact
// match.
func (s *VideoService) resolveShareCamera(r *shareRecord, present map[string]camera.Identity) (base string, ok, learned bool) {
	s.shares.mu.Lock()
	want := r.camera
	s.shares.mu.Unlock()
	for b, id := range present {
		if id == want {
			ports := sameModelPorts(want, present)
			s.shares.mu.Lock()
			learned = mergeSiblingsLocked(r, ports)
			s.shares.mu.Unlock()
			return b, true, learned
		}
	}
	siblings := map[string]bool{}
	stored := map[camera.Identity]bool{}
	s.shares.mu.Lock()
	for _, port := range r.siblings {
		siblings[port] = true
	}
	for _, c := range s.shares.containers {
		for _, other := range c.shares {
			stored[other.camera] = true
			if other.camera == want {
				for _, port := range other.siblings {
					siblings[port] = true
				}
			}
		}
	}
	s.shares.mu.Unlock()
	isPresent := make(map[camera.Identity]bool, len(present))
	for _, id := range present {
		isPresent[id] = true
	}
	claimed := func(id camera.Identity) bool {
		if siblings[id.Device] || stored[id] {
			return true
		}
		for other := range stored {
			if other != want && other.Name == id.Name && other.Index == id.Index && !isPresent[other] {
				return true
			}
		}
		return false
	}
	base, ok = camera.Resolve(want, present, claimed)
	if !ok {
		return "", false, false
	}
	moved := present[base]
	ports := sameModelPorts(moved, present)
	(*afterShareResolveRead.Load())()
	s.shares.mu.Lock()
	defer s.shares.mu.Unlock()
	if r.camera == moved {
		// Another resolve read the same old camera and followed it here
		// first, for every record of it: the answer is the same, so this one
		// is resolved too, with nothing new to learn.
		return base, true, false
	}
	if r.camera != want {
		// It moved somewhere else meanwhile; this one's view is stale.
		return "", false, false
	}
	follow := func(rec *shareRecord) {
		rec.camera = moved
		mergeSiblingsLocked(rec, ports) // the cameras beside it at its new port
	}
	follow(r)
	for _, c := range s.shares.containers {
		for _, other := range c.shares {
			if other.camera == want {
				follow(other)
			}
		}
	}
	return base, true, true
}

// afterShareResolveRead runs in resolveShareCamera between choosing the camera
// to follow and storing it; a variable so a test can run a second resolve
// there. Atomic, because a background resolve may read it while a test swaps it.
var afterShareResolveRead atomic.Pointer[func()]

func init() {
	f := func() {}
	afterShareResolveRead.Store(&f)
}

// learnSameModelPorts is the sibling half of resolveShareCamera for every
// record at once: each record whose camera is present learns the ports of the
// other present cameras of its model. It runs on every camera node that
// appears, so a camera plugged in beside a shared one is known to be another
// camera even when no app reads its share. It reports whether any record
// learned a port, for the caller to save.
func (s *VideoService) learnSameModelPorts(present map[string]camera.Identity) bool {
	here := make(map[camera.Identity]bool, len(present))
	for _, id := range present {
		here[id] = true
	}
	s.shares.mu.Lock()
	defer s.shares.mu.Unlock()
	learned := false
	for _, c := range s.shares.containers {
		for _, r := range c.shares {
			if here[r.camera] && mergeSiblingsLocked(r, sameModelPorts(r.camera, present)) {
				learned = true
			}
		}
	}
	return learned
}

// mergeSiblingsLocked adds ports to r's siblings and reports whether any was
// new. Callers hold s.shares.mu.
func mergeSiblingsLocked(r *shareRecord, ports []string) bool {
	changed := false
	for _, port := range ports {
		if !slices.Contains(r.siblings, port) {
			r.siblings = append(r.siblings, port)
			changed = true
		}
	}
	if changed {
		sort.Strings(r.siblings)
	}
	return changed
}

func (s *VideoService) shareOfCamera(containerID string, id camera.Identity) *shareRecord {
	s.shares.mu.Lock()
	defer s.shares.mu.Unlock()
	if c := s.shares.containers[containerID]; c != nil {
		for _, r := range c.shares {
			if r.camera == id {
				return r
			}
		}
	}
	return nil
}

func (s *VideoService) retireStaleShare(containerID string, r *shareRecord) {
	s.shares.ops.Lock()
	defer s.shares.ops.Unlock()
	s.shares.mu.Lock()
	retired := r.retired
	s.shares.mu.Unlock()
	if !retired {
		s.retireShares(containerID, []*shareRecord{r})
	}
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
	// A CreateCameraShares of this container still in flight stores nothing
	// more (storeShare): what it stored already is in this snapshot.
	for _, creation := range s.shares.creating[containerID] {
		creation.removed = true
	}
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
// removals. A container left with no shares lets go of its mount namespace.
// Callers hold s.shares.ops.
func (s *VideoService) retireShares(containerID string, recs []*shareRecord) {
	var removals []*shareRemoval
	for _, r := range recs {
		if rm := s.stopShare(r); rm != nil {
			removals = append(removals, rm)
		}
	}
	s.shares.mu.Lock()
	c := s.shares.containers[containerID]
	s.forgetSharesLocked(containerID, recs)
	var ns *devcover.Namespace
	if c != nil && s.shares.containers[containerID] == nil {
		ns = releaseNamespaceLocked(c)
	}
	starts := s.queueShareRemovalsLocked(removals)
	s.shares.mu.Unlock()
	s.closeNamespaceNow(ns)
	s.persistCameraShares()
	for _, start := range starts {
		start()
	}
}

// stopShare stops one share: it cancels the pump, waits for it and closes the
// writer. It returns the removal of the record's node, or nil when there is no
// node of this record's to remove: a record never opened in this agent's life
// (loaded, not adopted) whose node is gone (a reboot) or is now another node,
// or one whose node a start is making again, which removes that node itself
// once it sees the record retired. Callers hold s.shares.ops.
func (s *VideoService) stopShare(r *shareRecord) *shareRemoval {
	s.shares.mu.Lock()
	r.retired = true
	cancel, done, node, made, nr, opening := r.cancel, r.done, r.io, r.nodeMade, r.node, r.opening
	s.shares.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	if node != nil {
		s.closeShareWriter(shareNodePath(nr), node)
		return &shareRemoval{nr: nr, name: r.cameraName, made: made}
	}
	if opening || !s.nodeIsShareFor(r) {
		return nil
	}
	return &shareRemoval{nr: nr, name: r.cameraName}
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
// waiting to be removed (another process held it open) is tried again. Then
// each running listed container's mount namespace is held, and its cameras
// covered again if a replug uncovered them while the agent was down; a
// container listed as not running lets go of its namespace
// (syncShareNamespaces).
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

	// Retry every queued removal, except one whose number a record also names
	// (a state file can hold both): the record wins, and the removal is
	// dropped rather than ever removing a share's node.
	s.shares.mu.Lock()
	var retries []func()
	var overruled []int
	for nr, rm := range s.shares.removing {
		if s.recordHoldsShareNumberLocked(nr) {
			if !rm.running {
				delete(s.shares.removing, nr)
				overruled = append(overruled, nr)
			}
			continue
		}
		if start := s.startShareRemovalLocked(rm); start != nil {
			retries = append(retries, start)
		}
	}
	s.shares.mu.Unlock()
	if len(overruled) > 0 {
		sort.Ints(overruled)
		for _, nr := range overruled {
			s.shareLogger().Warn(fmt.Sprintf("camera share: dropped the queued removal of %s: a share's record holds it", shareNodePath(nr)))
		}
		s.persistCameraShares()
	}
	for _, start := range retries {
		start()
	}

	// Every record learns where its camera is now, or that it is absent, so a
	// camera swapped at a path while no /dev watch ran (or while the agent was
	// down) stops being held to another camera's rules.
	if ctx.Err() == nil {
		s.resolveAllShares()
	}

	sort.SliceStable(adopt, func(i, j int) bool { return adopt[i].containerID < adopt[j].containerID })
	for _, a := range adopt {
		if ctx.Err() != nil {
			return
		}
		s.adoptShare(a.containerID, a.r)
	}
	s.syncShareNamespaces(ctx, containers)
}

// adoptShare reopens the node of a record loaded from the state file and
// starts its pump. The node is opened at the record's stored mode: the earlier
// writer's keep_format locked the node's format for the node's life, so any
// other mode would be refused. Opening sets keep_format again. A record a
// start is making a node for is left to it. Callers hold s.shares.ops.
func (s *VideoService) adoptShare(containerID string, r *shareRecord) {
	if s.loopback == nil || s.sharesStopped() {
		return
	}
	s.shares.mu.Lock()
	nr := r.node
	s.shares.mu.Unlock()
	if _, exists := s.loopback.NodePath(uint32(nr)); !exists {
		return // a reboot: the container's next start makes the node again
	}
	path := shareNodePath(nr)
	if !s.nodeIsShareFor(r) {
		s.noteAdoptFailure(containerID, r, fmt.Sprintf("%s is no longer its share node of %s; it is left alone", path, r.cameraName))
		return
	}
	s.shares.mu.Lock()
	if r.opening || r.pump != nil || r.retired || r.node != nr {
		s.shares.mu.Unlock()
		return
	}
	r.opening = true
	s.shares.mu.Unlock()
	node, err := s.openCameraShareNode(path, r.mode)
	if err != nil {
		s.endShareOpening(r)
		s.noteAdoptFailure(containerID, r, fmt.Sprintf("adopting %s for %s failed: %v; the app's node gets no frames", path, r.cameraName, err))
		return
	}
	s.shares.mu.Lock()
	r.opening = false
	if s.shares.stopped {
		s.shares.mu.Unlock()
		s.closeShareWriter(path, node)
		return
	}
	r.adoptErr = ""
	start := s.attachSharePumpLocked(containerID, r, node)
	s.shares.mu.Unlock()
	start()
	s.shareLogger().Info(fmt.Sprintf("camera share: %s adopted %s for %s at %s", containerID, path, r.cameraName, describeShareNode(r.mode, r.stamp)))
	s.checkAdoptedModeLater(containerID, r)
}

// cameraShareAdoptCheckBudget bounds checkAdoptedMode: the camera listing and
// the share decision together.
const cameraShareAdoptCheckBudget = 3 * time.Second

// checkAdoptedModeLater runs checkAdoptedMode on its own goroutine, off the
// sync and outside every share lock (shareDecisionFor can block on a
// GStreamer listing). Shutdown waits for it. Its budget is its own, not cut
// short by Shutdown: a check cut short would leave its decision running past
// Shutdown, so Shutdown waits up to cameraShareAdoptCheckBudget instead.
func (s *VideoService) checkAdoptedModeLater(containerID string, r *shareRecord) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), cameraShareAdoptCheckBudget)
		defer cancel()
		s.checkAdoptedMode(ctx, containerID, r)
	}()
}

// checkAdoptedMode compares an adopted record's stored mode with the mode a
// share of its camera made now would get. The node keeps the stored mode for
// its life (the earlier writer's keep_format locked it), so when the two
// differ (the camera, its driver or the agent's policy changed while the
// agent was down) the hub's frames may never fit the node: that is logged,
// and the camera listing tells the user to redeploy the app, which makes the
// node again at today's mode. A camera that is not present, or a decision
// that is not ready within ctx, is not judged.
func (s *VideoService) checkAdoptedMode(ctx context.Context, containerID string, r *shareRecord) {
	path, ok := s.currentSharePath(r)
	if !ok {
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
	if !ok || listed.err != nil {
		return
	}
	var dev *agentpb.VideoDevice
	for _, d := range listed.devices {
		if d.GetPath() == path {
			dev = d
		}
	}
	if dev == nil {
		return
	}
	decision, ok := withinBudget(ctx, func() shareDecision { return s.shareDecisionsFor(dev, r.mode.Format)[r.mode.Format] })
	if !ok || !decision.Shared || decision.Mode.asksWhat(r.mode) {
		return
	}
	note := fmt.Sprintf("redeploy the app: its node keeps %s, and the camera's shared mode is now %s", r.mode, decision.Mode)
	s.shares.mu.Lock()
	retired := r.retired
	if !retired {
		r.modeNote = note
	}
	s.shares.mu.Unlock()
	if !retired {
		s.shareLogger().Warn(fmt.Sprintf("camera share: %s: adopted %s's node at the mode an earlier agent stored, %s, but a share made now would get %s; "+
			"the node cannot change while it exists, so redeploy the app to share the camera at %s",
			containerID, r.cameraName, r.mode, decision.Mode, decision.Mode))
	}
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

func (s *VideoService) sharesStopped() bool {
	s.shares.mu.Lock()
	defer s.shares.mu.Unlock()
	return s.shares.stopped
}

// endShareOpening gives up a record's opening claim.
func (s *VideoService) endShareOpening(r *shareRecord) {
	s.shares.mu.Lock()
	r.opening = false
	s.shares.mu.Unlock()
}

// nodeIsShareFor reports whether the node a record names exists and is still
// the share node made for it: after a reboot the number may have gone to
// another node, which must be neither opened nor removed. The node's sysfs
// name must be the camera's, and no other record with a live writer, and no
// queued removal, may name the number: every share of one camera has the same
// name, so the name alone cannot tell two records' nodes apart.
func (s *VideoService) nodeIsShareFor(r *shareRecord) bool {
	_, ok := s.shareNodeNumber(r)
	return ok
}

// shareNodeNumber is nodeIsShareFor that also returns the number it checked.
func (s *VideoService) shareNodeNumber(r *shareRecord) (int, bool) {
	if s.loopback == nil {
		return 0, false
	}
	nr, claimed := s.shareNumberClaimedElsewhere(r)
	if claimed {
		return nr, false
	}
	if _, exists := s.loopback.NodePath(uint32(nr)); !exists {
		return nr, false
	}
	return nr, s.nodeNamed(nr, r.cameraName)
}

// shareNumberClaimedElsewhere returns r's node number, and whether it belongs
// to another record with a live writer, or to a queued removal.
func (s *VideoService) shareNumberClaimedElsewhere(r *shareRecord) (nr int, claimed bool) {
	s.shares.mu.Lock()
	defer s.shares.mu.Unlock()
	nr = r.node
	if s.shares.removing[nr] != nil {
		return nr, true
	}
	for _, c := range s.shares.containers {
		for _, other := range c.shares {
			if other != r && other.node == nr && other.io != nil {
				return nr, true
			}
		}
	}
	return nr, false
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
	// From here on nothing is stored or adopted, so nothing outlives this.
	s.shares.mu.Lock()
	s.shares.stopped = true
	s.shares.mu.Unlock()
	defer s.stopShareRemovals()
	s.shares.ops.Lock()
	defer s.shares.ops.Unlock()
	type running struct {
		r      *shareRecord
		cancel context.CancelFunc
		done   chan struct{}
		io     shareNodeIO
		nr     int
	}
	var pumps []running
	s.shares.mu.Lock()
	for _, c := range s.shares.containers {
		for _, r := range c.shares {
			if r.cancel != nil {
				pumps = append(pumps, running{r, r.cancel, r.done, r.io, r.node})
			}
		}
	}
	s.shares.mu.Unlock()
	for _, p := range pumps {
		p.cancel()
	}
	for _, p := range pumps {
		<-p.done
		s.closeShareWriter(shareNodePath(p.nr), p.io)
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
// its holder check and removal first, which takes milliseconds; the wait is
// bounded by shareRemovalShutdownWait all the same, so a scan stuck on the
// system cannot hold Shutdown.
func (s *VideoService) stopShareRemovals() {
	s.shares.mu.Lock()
	s.shares.closed = true
	cancel := s.shares.removalCancel
	s.shares.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	done := make(chan struct{})
	go func() {
		s.shares.removalWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shareRemovalShutdownWait):
		s.shareLogger().Warn(fmt.Sprintf("camera share: a share node removal is still running after %s; shutting down without it", shareRemovalShutdownWait))
	}
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
	Format string                 `json:"format,omitempty"`
	Stamp  string                 `json:"stamp,omitempty"`
	// AskedFormat and AskedWhyNot are the record's; an agent that does not
	// know them ignores them, so they are written in either version.
	AskedFormat string `json:"askedFormat,omitempty"`
	AskedWhyNot string `json:"askedWhyNot,omitempty"`
	Node        int    `json:"node"`
	// SameModelPorts is the record's siblings; omitted when it has none, and
	// absent from files written before it existed.
	SameModelPorts []string `json:"sameModelPorts,omitempty"`
}

type cameraShareStateCamera struct {
	Device string `json:"device"`
	Index  string `json:"index"`
	Name   string `json:"name"`
}

type cameraShareStateMode struct {
	Width         uint32 `json:"width"`
	Height        uint32 `json:"height"`
	Fourcc        string `json:"fourcc"`
	BytesPerLine  uint32 `json:"bytesPerLine"`
	FPS           uint32 `json:"fps"`
	MaxFrameBytes uint32 `json:"maxFrameBytes,omitempty"`
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
// logged: the shares work, but an agent restart would not adopt them. A file a
// newer agent wrote is never written over (loadCameraShares said so once).
func (s *VideoService) persistCameraShares() {
	s.shares.fileMu.Lock()
	defer s.shares.fileMu.Unlock()
	if s.shares.stateNewer {
		return
	}
	path := s.sharesStatePathLocked()

	state := cameraShareState{Version: cameraShareStateVersionWithoutSettings, Containers: map[string]cameraShareStateContainer{}}
	s.shares.mu.Lock()
	for id, c := range s.shares.containers {
		sc := cameraShareStateContainer{App: c.app, Shares: make([]cameraShareStateShare, 0, len(c.shares))}
		for _, r := range c.shares {
			if r.mode.Format != appconfig.CameraFormatPixels || r.stamp != appconfig.CameraStampTime {
				state.Version = cameraShareStateVersion
			}
			sc.Shares = append(sc.Shares, cameraShareStateShare{
				Camera: cameraShareStateCamera{Device: r.camera.Device, Index: r.camera.Index, Name: r.camera.Name},
				Path:   r.cameraPath,
				Mode: cameraShareStateMode{Width: r.mode.Width, Height: r.mode.Height, Fourcc: r.mode.Fourcc,
					BytesPerLine: r.mode.BytesPerLine, FPS: r.mode.FPS, MaxFrameBytes: r.mode.MaxFrameBytes},
				Format:         r.mode.Format,
				Stamp:          r.stamp,
				AskedFormat:    r.askedFormat,
				AskedWhyNot:    r.askedWhyNot,
				Node:           r.node,
				SameModelPorts: append([]string(nil), r.siblings...),
			})
		}
		state.Containers[id] = sc
	}
	if state.Version == cameraShareStateVersionWithoutSettings {
		for _, sc := range state.Containers {
			for i := range sc.Shares {
				sc.Shares[i].Format, sc.Shares[i].Stamp = "", ""
			}
		}
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
// moved aside to <path>.corrupt and logged. A file in a version newer than
// this agent reads (a newer agent ran, then this one was put back) is left in
// place, ignored and never written over, so the newer agent finds its own
// records again; this agent then keeps its shares in memory only. A version 1
// file is read with every share as pixels with capture times, which is all
// that version could hold. A record that could not have been written by this
// agent (a node outside the band, a mode no node can declare, a format or
// stamp it does not know) is dropped, so it never hides a real camera from
// IsShareNode.
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
	var head struct {
		Version int `json:"version"`
	}
	if json.Unmarshal(data, &head) == nil && head.Version > cameraShareStateVersion {
		s.shares.stateNewer = true
		log.Warn(fmt.Sprintf("camera share: %s is version %d, written by a newer agent; this agent reads version %d, so it leaves the file as it is "+
			"and does not save its own shares to it: shares made before this agent started are not adopted, and shares it makes are not adopted after it restarts",
			path, head.Version, cameraShareStateVersion))
		return
	}
	var state cameraShareState
	err = json.Unmarshal(data, &state)
	if err == nil && state.Version != cameraShareStateVersion && state.Version != cameraShareStateVersionWithoutSettings {
		err = fmt.Errorf("version %d, this agent reads versions %d and %d", state.Version, cameraShareStateVersionWithoutSettings, cameraShareStateVersion)
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
			format, stamp := sh.Format, sh.Stamp
			if state.Version == cameraShareStateVersionWithoutSettings {
				format, stamp = appconfig.CameraFormatPixels, appconfig.CameraStampTime
			}
			r := &shareRecord{
				cameraName: sh.Camera.Name,
				cameraPath: sh.Path,
				mode: sharedMode{Width: sh.Mode.Width, Height: sh.Mode.Height, Fourcc: sh.Mode.Fourcc,
					BytesPerLine: sh.Mode.BytesPerLine, FPS: sh.Mode.FPS, Format: format, MaxFrameBytes: sh.Mode.MaxFrameBytes},
				stamp:       stamp,
				askedFormat: sh.AskedFormat,
				askedWhyNot: sh.AskedWhyNot,
				node:        sh.Node,
				camera:      camera.Identity{Device: sh.Camera.Device, Index: sh.Camera.Index, Name: sh.Camera.Name},
				siblings:    append([]string(nil), sh.SameModelPorts...),
			}
			_, formatKnown := shareFrameKinds[format]
			if r.node < ipcam.IDBandStart || r.node > ipcam.IDBandEnd || r.mode.pixelFormat() == 0 || r.mode.bufferBytes() <= 0 ||
				!formatKnown || !slices.Contains(appconfig.CameraStamps, stamp) {
				log.Warn(fmt.Sprintf("camera share: %s: dropping %s's share of %q at node %d with mode %s; this agent could not have made it",
					path, id, sh.Camera.Name, sh.Node, describeShareNode(r.mode, stamp)))
				continue
			}
			sc.shares = append(sc.shares, r)
		}
		if len(sc.shares) > 0 {
			containers[id] = sc
		}
	}
	// A queued removal from an earlier agent: its node is old. Like every
	// removal, its node's name is checked before it is removed, since after a
	// reboot the number may be another node's.
	removing := make(map[int]*shareRemoval, len(state.Removing))
	for _, rm := range state.Removing {
		if rm.Node < ipcam.IDBandStart || rm.Node > ipcam.IDBandEnd {
			log.Warn(fmt.Sprintf("camera share: %s: dropping the removal of node %d; this agent could not have made it", path, rm.Node))
			continue
		}
		removing[rm.Node] = &shareRemoval{nr: rm.Node, name: rm.Name}
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
	devNum, err := sysfsVideoDevNum(nr)
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	if _, err := os.Stat("/run/udev/data"); err != nil {
		return true
	}
	_, err = os.Stat("/run/udev/data/c" + devNum)
	return err == nil
}

// videoSysfsRoot is /sys/class/video4linux; a variable so tests can use a
// fixture tree.
var videoSysfsRoot = "/sys/class/video4linux"

// sysfsVideoDevNum is video<nr>'s "<major>:<minor>" from sysfs.
func sysfsVideoDevNum(nr int) (string, error) {
	b, err := os.ReadFile(filepath.Join(videoSysfsRoot, fmt.Sprintf("video%d", nr), "dev"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
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
// character device at path open. A path that is not a character device, or a
// scan that cannot be done, lists none.
func procDeviceHolders(path string) []deviceHolder {
	want, ok := charDeviceRdev(path)
	if !ok {
		return nil
	}
	holders, _ := procHolders(want)
	return holders
}

// sysfsShareNodeHolders lists the processes other than the agent that hold
// /dev/video<nr> open. The device number comes from sysfs, not from the /dev
// entry, so a missing or replaced /dev entry cannot hide a holder; an error
// means who holds the node could not be told.
func sysfsShareNodeHolders(nr int) ([]deviceHolder, error) {
	devNum, err := sysfsVideoDevNum(nr)
	if err != nil {
		return nil, err
	}
	var major, minor uint32
	if _, err := fmt.Sscanf(devNum, "%d:%d", &major, &minor); err != nil {
		return nil, fmt.Errorf("video%d's device number %q: %w", nr, devNum, err)
	}
	return procHolders(unix.Mkdev(major, minor))
}

// procHolders lists the processes other than the agent with an open file on
// the character device rdev, from /proc/<pid>/fd/*. Each link is read before
// anything is stat'ed, and only targets under /dev/ are stat'ed, so a file on
// a hung network or FUSE mount cannot stall the scan (a covered node inside a
// container shows its in-container /dev path). A process that went away is
// skipped; /proc or a process's fd directory that cannot be read is an error.
func procHolders(rdev uint64) ([]deviceHolder, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
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
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, fd := range fds {
			link := filepath.Join(dir, "fd", fd.Name())
			target, err := os.Readlink(link)
			if err != nil || !strings.HasPrefix(target, "/dev/") {
				continue
			}
			if got, ok := charDeviceRdev(link); ok && got == rdev {
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
	return out, nil
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

// Covers. At every start of a sharing container, between containerd's NewTask
// and Start, each of its share nodes is put over its camera's path inside the
// container (devcover), so the app sees its node at /dev/video0 from its first
// instruction. A replug drops a cover with the old device node, so the agent
// watches /dev and covers the camera's new node again, for every running
// container that shares it, as soon as the node appears.

// cameraShareCoverBudget bounds CoverCameraShares: a container start never
// waits longer than this for its covers.
const cameraShareCoverBudget = 250 * time.Millisecond

// cameraShareRecoverTimeout bounds each cover after a replug.
const cameraShareRecoverTimeout = 50 * time.Millisecond

// cameraShareLoserCheckDelay is how long after covering a replugged camera
// again the agent looks for an app that took the real camera just before.
const cameraShareLoserCheckDelay = 100 * time.Millisecond

// Seams for covering, so tests run without Linux or containers: putting a node
// over a path in a namespace and checking one is there; pinning a namespace,
// reading its inode and closing it; watching /dev; and the inodes of the
// host's mount namespaces (the agent's and init's).
var (
	coverPath         = devcover.Cover
	coveredPath       = devcover.Covered
	openNamespace     = devcover.Open
	namespaceInode    = (*devcover.Namespace).Inode
	closeNamespace    = (*devcover.Namespace).Close
	watchDevices      = devwatch.Watch
	hostMountNSInodes = readHostMountNSInodes
)

// errShareNodeMaking is why a record whose node a start is making (or a sync
// is opening) is not covered now: its number may not be its node yet.
var errShareNodeMaking = errors.New("its node is still being made")

// The watch's pacing: it starts again cameraShareWatchRetry after it failed,
// and cameraShareWatchSettle after each start, once inotify is surely in
// place, every held namespace is checked for a cover a replug took while the
// watch was not running. Variables so tests can shorten them.
var (
	cameraShareWatchRetry  = time.Second
	cameraShareWatchSettle = 100 * time.Millisecond
)

// readHostMountNSInodes returns the inodes of the agent's and init's mount
// namespaces, the host's: no container's namespace is either.
func readHostMountNSInodes() ([]uint64, error) {
	var out []uint64
	for _, path := range []string{"/proc/self/ns/mnt", "/proc/1/ns/mnt"} {
		fi, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return nil, fmt.Errorf("%s has no inode", path)
		}
		out = append(out, uint64(st.Ino))
	}
	return out, nil
}

// CoverCameraShares puts each of the container's share nodes over its camera's
// path inside the container whose new task is pid; it is called between
// containerd's NewTask and Start, at every start. Everything shares one
// deadline, cameraShareCoverBudget, and it never returns later: a cover still
// running then is devcover.ErrTimeout, and is never applied afterwards. A node
// lost in a reboot is made again first, within the same deadline. Each camera
// not covered is logged and its error returned for the caller's log; the app
// then starts with the real camera, and the start always goes ahead. A camera
// not plugged in is covered when it appears: the container's mount namespace
// stays held for that until ReleaseCameraShares. One held from an earlier
// start is closed off the start path, since closing a dead container's last
// reference tears its mounts down, which takes milliseconds.
func (s *VideoService) CoverCameraShares(ctx context.Context, containerID string, pid uint32) []error {
	deadline := time.Now().Add(cameraShareCoverBudget)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	run, refused := s.beginStartCover(containerID)
	if refused != nil {
		s.shareLogger().Info(refused.Error())
		return []error{refused}
	}
	if run == nil {
		return nil
	}
	go func() {
		defer s.shares.coverWG.Done()
		s.runStartCover(ctx, run, pid, deadline)
	}()
	select {
	case <-run.finished:
	case <-ctx.Done():
	}
	return s.reportStartCover(ctx, run)
}

// startCoverRun is one CoverCameraShares call. Its goroutine reports each
// record's outcome; once the call has returned (closed), an outcome is late
// and only noted.
type startCoverRun struct {
	containerID string
	c           *shareContainer
	gen         uint64 // c.nsGen when the call began
	recs        []*shareRecord

	mu       sync.Mutex
	outcomes []*startCover
	closed   bool
	// finished is closed once every record has an outcome, or the goroutine
	// stopped early.
	finished chan struct{}
	finish   sync.Once
}

func (run *startCoverRun) done() { run.finish.Do(func() { close(run.finished) }) }

// startCover is what became of one record at a start.
type startCover struct {
	node, path string // the share node, and the camera's path
	absent     bool   // the camera is not plugged in
	err        error
}

// beginStartCover registers a start's cover of the container's shares: it
// takes the namespace held before (closed in the background) and counts the
// call in coverWG. It returns no run for a container that shares nothing, and
// refused once Shutdown has begun.
func (s *VideoService) beginStartCover(containerID string) (*startCoverRun, error) {
	s.shares.mu.Lock()
	c := s.shares.containers[containerID]
	if c == nil || len(c.shares) == 0 {
		s.shares.mu.Unlock()
		return nil, nil
	}
	if s.shares.stopped {
		s.shares.mu.Unlock()
		return nil, fmt.Errorf("camera share: %s starts with the real cameras: %s", containerID, shareRefusedStopping)
	}
	closeOld := s.namespaceCloserLocked(releaseNamespaceLocked(c))
	run := &startCoverRun{containerID: containerID, c: c, gen: c.nsGen,
		recs: append([]*shareRecord(nil), c.shares...), finished: make(chan struct{})}
	run.outcomes = make([]*startCover, len(run.recs))
	s.shares.coverWG.Add(1)
	s.shares.mu.Unlock()
	closeOld()
	return run, nil
}

// runStartCover is a start's cover, on its own goroutine: the namespace, then
// each record, those whose node is there first, since making a node can wait
// behind a node removal's /proc scan. It stops at ctx's end; a node it is
// making then is finished and kept for the next start, and the state file is
// saved last, off the start path.
func (s *VideoService) runStartCover(ctx context.Context, run *startCoverRun, pid uint32, deadline time.Time) {
	defer run.done()
	ns, held, nsErr := s.holdStartNamespace(run, pid)
	if ns != nil && !held {
		defer s.closeNamespaceLater(ns)
	}
	var ready, remake []int
	for i, r := range run.recs {
		if _, err := s.shareNodeToCover(r); err == nil {
			ready = append(ready, i)
		} else {
			remake = append(remake, i)
		}
	}
	changed := false
	for _, i := range append(ready, remake...) {
		if ctx.Err() != nil {
			break
		}
		out, ch := s.coverAtStart(ctx, run.containerID, run.recs[i], ns, nsErr, deadline)
		changed = changed || ch
		s.reportCover(run, i, out)
	}
	run.done()
	if changed {
		s.persistCameraShares()
	}
}

// holdStartNamespace pins pid's mount namespace and makes it the container's,
// unless a release or a later start came first: then held is false, and the
// caller closes it once its covers are done.
func (s *VideoService) holdStartNamespace(run *startCoverRun, pid uint32) (ns *devcover.Namespace, held bool, err error) {
	ns, inode, err := s.openContainerNamespace(pid)
	if err != nil {
		return nil, false, fmt.Errorf("opening its mount namespace: %w", err)
	}
	s.shares.mu.Lock()
	held = s.holdNamespaceLocked(run.containerID, run.c, run.gen, ns, inode, pid)
	s.shares.mu.Unlock()
	return ns, held, nil
}

// holdNamespaceLocked makes ns the container's when c is still the container's
// entry and nothing changed its namespace since gen. Callers hold
// s.shares.mu.
func (s *VideoService) holdNamespaceLocked(containerID string, c *shareContainer, gen uint64, ns *devcover.Namespace, inode uint64, pid uint32) bool {
	if s.shares.stopped || s.shares.containers[containerID] != c || c.nsGen != gen {
		return false
	}
	c.ns, c.nsInode, c.nsPID, c.nsAt = ns, inode, pid, time.Now()
	c.nsGen++
	return true
}

// openContainerNamespace pins pid's mount namespace and reads its inode. A pid
// in the host's mount namespace (the agent's or init's) is refused, and its
// namespace closed: a cover there would put a share node over the host's
// camera. So is any pid when the host's namespaces cannot be read to rule
// that out.
func (s *VideoService) openContainerNamespace(pid uint32) (*devcover.Namespace, uint64, error) {
	ns, err := openNamespace(pid)
	if err != nil {
		return nil, 0, err
	}
	inode, err := namespaceInode(ns)
	if err == nil {
		host, hostErr := hostMountNSInodes()
		switch {
		case hostErr != nil:
			err = fmt.Errorf("the host's mount namespace could not be read to tell it from pid %d's: %w", pid, hostErr)
		case slices.Contains(host, inode):
			err = fmt.Errorf("pid %d is in the host's mount namespace (the agent's or init's)", pid)
		}
	}
	if err != nil {
		s.closeNamespaceLater(ns)
		return nil, 0, err
	}
	return ns, inode, nil
}

// coverAtStart makes sure r's node is there, finds r's camera and covers it
// before deadline. changed reports a record change for the state file.
func (s *VideoService) coverAtStart(ctx context.Context, containerID string, r *shareRecord, ns *devcover.Namespace, nsErr error, deadline time.Time) (out startCover, changed bool) {
	lastPath := func() string {
		s.shares.mu.Lock()
		defer s.shares.mu.Unlock()
		return r.cameraPath
	}
	nr, made, err := s.startShareNode(ctx, containerID, r)
	if err != nil {
		if !errors.Is(err, errShareNodeMaking) {
			err = fmt.Errorf("its node could not be made again: %w", err)
		}
		return startCover{path: lastPath(), err: err}, false
	}
	out.node = shareNodePath(nr)
	if nsErr != nil {
		out.path, out.err = lastPath(), nsErr
		return out, made
	}
	path, ok, ch := s.resolveSharePath(r)
	changed = made || ch
	if !ok {
		out.path, out.absent = lastPath(), true
		return out, changed
	}
	out.path = path
	timeout := time.Until(deadline)
	if timeout <= 0 || ctx.Err() != nil {
		out.err = coverStopped(ctx)
		return out, changed
	}
	out.err = coverPath(ns, out.node, path, timeout)
	return out, changed
}

// coverStopped is the error of a cover the start's deadline (or its
// cancellation) cut off.
func coverStopped(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return fmt.Errorf("the start's covers were cancelled: %w", ctx.Err())
	}
	return fmt.Errorf("%w (the start's %s budget ran out)", devcover.ErrTimeout, cameraShareCoverBudget)
}

// reportCover hands one record's outcome to the call, or, once it has
// returned, notes a cover that landed after all: devcover lets a cover that
// committed before its deadline finish its one move_mount. A late cover
// clears the record's failed-cover note under run.mu, so it always lands
// after the note reportStartCover set for the same record.
func (s *VideoService) reportCover(run *startCoverRun, i int, out startCover) {
	run.mu.Lock()
	late := run.closed
	if !late {
		run.outcomes[i] = &out
	} else if out.err == nil && !out.absent {
		s.setCoverFailed(run.recs[i], "")
	}
	run.mu.Unlock()
	if late && out.err == nil && !out.absent {
		s.shareLogger().Info(fmt.Sprintf("camera share: %s has its node %s at %s after all: the cover finished just after its start's budget",
			run.containerID, out.node, out.path))
	}
}

// setCoverFailed sets or clears (why "") a record's failed-cover note.
func (s *VideoService) setCoverFailed(r *shareRecord, why string) {
	s.shares.mu.Lock()
	r.coverFailed = why
	s.shares.mu.Unlock()
}

// reportStartCover closes the call, logs each record's outcome and returns
// the errors. A record without an outcome was cut off by the deadline. Each
// record's failed-cover note is set from this start's outcome (cleared for a
// cover, or a camera that is not plugged in), except while another start makes
// its node: that start's cover decides it.
func (s *VideoService) reportStartCover(ctx context.Context, run *startCoverRun) []error {
	run.mu.Lock()
	run.closed = true
	outcomes := make([]*startCover, len(run.recs))
	for i, r := range run.recs {
		out := run.outcomes[i]
		if out == nil {
			s.shares.mu.Lock()
			path := r.cameraPath
			s.shares.mu.Unlock()
			out = &startCover{path: path, err: coverStopped(ctx)}
		}
		outcomes[i] = out
		switch {
		case out.err == nil || out.absent:
			s.setCoverFailed(r, "")
		case !errors.Is(out.err, errShareNodeMaking):
			s.setCoverFailed(r, out.err.Error())
		}
	}
	run.mu.Unlock()
	log := s.shareLogger()
	var errs []error
	for i, r := range run.recs {
		out := outcomes[i]
		switch {
		case out.absent:
			log.Info(fmt.Sprintf("camera share: %s is not plugged in; %s's node covers it when it is", r.cameraName, run.containerID))
		case out.err != nil:
			msg := fmt.Sprintf("camera share: %s starts with the real camera %s (%s)", run.containerID, r.cameraName, out.path)
			log.Warn(fmt.Sprintf("%s: %v", msg, out.err))
			errs = append(errs, fmt.Errorf("%s: %w", msg, out.err))
		default:
			log.Info(fmt.Sprintf("camera share: %s starts with its node %s at %s (%s)", run.containerID, out.node, out.path, r.cameraName))
		}
	}
	return errs
}

// shareNodeToCover returns the path of r's node when that node is r's own
// share node, the only node a cover may put over a camera: r's writer holds
// it, or no start is making r's node and the node there is still the share
// node made for r (nodeIsShareFor) with fresh_start verified. After a reboot
// the number may hold another
// node, a two-plane node say, whose stream must never stand in for the app's
// camera; while a node is being made, the number may not be its yet
// (errShareNodeMaking).
func (s *VideoService) shareNodeToCover(r *shareRecord) (string, error) {
	nr, err := s.shareNodeNumberToCover(r)
	if err != nil {
		return "", err
	}
	return shareNodePath(nr), nil
}

func (s *VideoService) shareNodeNumberToCover(r *shareRecord) (int, error) {
	s.shares.mu.Lock()
	nr, writer, opening := r.node, r.io != nil, r.opening
	s.shares.mu.Unlock()
	switch {
	case writer:
		return nr, nil
	case opening:
		return 0, errShareNodeMaking
	}
	if got, ok := s.shareNodeNumber(r); ok {
		// Identity alone cannot establish freshness for a node retained from
		// an older agent. A read-only control query preserves its current
		// reader/writer ownership while proving that fresh starts are enabled.
		err := checkShareNodeFreshStart(shareNodePath(got))
		s.noteFreshStartSupport(err)
		if err != nil {
			return 0, fmt.Errorf("%w: %w", errShareFreshStartUnverified, err)
		}
		return got, nil
	}
	return 0, fmt.Errorf("its node %s is gone or is now another node", shareNodePath(nr))
}

// startShareNode makes sure r's node is there for a start's cover and returns
// its number; made reports a node made now (the record changed). A node that
// is r's own share node (shareNodeToCover) is used as it is; one with no
// writer (an agent restart) is adopted by the next sync. A node another start
// is still making is not covered (errShareNodeMaking). Otherwise (a reboot
// removed it, or its number now holds another node) the node is made again,
// at r's own number when that is free and at a new one otherwise, then opened
// at r's stored mode with a pump, as a share's node is at its create. r is
// claimed meanwhile (opening), since the start must not wait for ops: an
// adoption leaves it alone, and a removal leaves the node to this, which
// removes it once it sees r retired.
func (s *VideoService) startShareNode(ctx context.Context, containerID string, r *shareRecord) (nr int, made bool, err error) {
	s.shares.mu.Lock()
	retired := r.retired
	s.shares.mu.Unlock()
	switch {
	case retired:
		return 0, false, errors.New(shareRefusedRemoved)
	case s.loopback == nil:
		return 0, false, errors.New("the agent has no v4l2loopback node manager")
	}
	nr, err = s.shareNodeNumberToCover(r)
	if err == nil || errors.Is(err, errShareNodeMaking) || errors.Is(err, errShareFreshStartUnverified) {
		return nr, false, err
	}

	s.shares.mu.Lock()
	switch {
	case r.retired:
		s.shares.mu.Unlock()
		return 0, false, errors.New(shareRefusedRemoved)
	case r.opening:
		s.shares.mu.Unlock()
		return 0, false, errShareNodeMaking
	case r.io != nil:
		nr = r.node
		s.shares.mu.Unlock()
		return nr, false, nil
	}
	r.opening = true
	was := r.node
	s.shares.mu.Unlock()

	// EnsureShareNode makes nothing, and says nothing, without the module.
	if err := s.loopback.Available(); err != nil {
		s.endShareOpening(r)
		return 0, false, errors.New("no compatible v4l2loopback module on this device")
	}
	nr, at, reason := s.makeShareNodeFor(ctx, r.cameraName, r)
	if reason != "" {
		s.endShareOpening(r)
		return 0, false, errors.New(reason)
	}
	path := shareNodePath(nr)
	node, err := s.openCameraShareNode(path, r.mode)
	if err != nil {
		s.endShareOpening(r)
		s.unmakeShareNode(nr, r.cameraName, at)
		return 0, false, err
	}
	s.shares.mu.Lock()
	r.opening = false
	if r.retired {
		s.shares.mu.Unlock()
		s.closeShareWriter(path, node)
		s.unmakeShareNode(nr, r.cameraName, at)
		return 0, false, errors.New(shareRefusedRemoved)
	}
	r.node, r.nodeMade = nr, at
	delete(s.shares.pending, nr)
	var start func()
	if !s.shares.stopped {
		start = s.attachSharePumpLocked(containerID, r, node)
	}
	s.shares.mu.Unlock()
	if start == nil {
		// Shutting down: the node and its record stay for the next agent.
		s.closeShareWriter(path, node)
		return nr, true, nil
	}
	start()
	msg := fmt.Sprintf("camera share: made %s again for %s's share of %s", path, containerID, r.cameraName)
	if nr != was {
		msg += fmt.Sprintf(" (its number %d is taken)", was)
	}
	s.shareLogger().Info(msg)
	return nr, true, nil
}

// ReleaseCameraShares lets go of the container's mount namespace, for a task
// that exited or stopped: no replug is covered for it until its next start.
// The namespace is closed here, which takes milliseconds when the agent holds
// its last reference, so this is never for the start path.
func (s *VideoService) ReleaseCameraShares(containerID string) {
	s.shares.mu.Lock()
	var ns *devcover.Namespace
	if c := s.shares.containers[containerID]; c != nil {
		ns = releaseNamespaceLocked(c)
		// A failed start cover described the run that just ended.
		for _, r := range c.shares {
			r.coverFailed = ""
		}
	}
	s.shares.mu.Unlock()
	s.closeNamespaceNow(ns)
}

// releaseNamespaceLocked takes the container's namespace away, for the caller
// to close. Callers hold s.shares.mu.
func releaseNamespaceLocked(c *shareContainer) *devcover.Namespace {
	ns := c.ns
	c.ns, c.nsInode, c.nsPID, c.nsAt = nil, 0, 0, time.Time{}
	c.nsGen++
	return ns
}

func (s *VideoService) closeNamespaceNow(ns *devcover.Namespace) {
	if ns == nil {
		return
	}
	if err := closeNamespace(ns); err != nil {
		s.shareLogger().Debug(fmt.Sprintf("camera share: closing a container's mount namespace failed: %v", err))
	}
}

// closeNamespaceLater closes ns on a goroutine Shutdown waits for, or at once
// when Shutdown has begun.
func (s *VideoService) closeNamespaceLater(ns *devcover.Namespace) {
	s.shares.mu.Lock()
	closeIt := s.namespaceCloserLocked(ns)
	s.shares.mu.Unlock()
	closeIt()
}

// namespaceCloserLocked decides, under the lock that orders it with Shutdown,
// how ns is closed: the function it returns closes ns on a goroutine Shutdown
// waits for, or, once Shutdown has begun, in place. Callers hold s.shares.mu
// and call the function after unlocking it.
func (s *VideoService) namespaceCloserLocked(ns *devcover.Namespace) func() {
	switch {
	case ns == nil:
		return func() {}
	case s.shares.stopped:
		return func() { s.closeNamespaceNow(ns) }
	}
	s.shares.nsWG.Add(1)
	return func() {
		go func() {
			defer s.shares.nsWG.Done()
			s.closeNamespaceNow(ns)
		}()
	}
}

// syncShareNamespaces is the namespace half of SyncCameraShares. A running
// listed container whose namespace is not held (an agent restart), or was
// held through another pid long enough ago, gets it opened; and each of its
// cameras present but not covered (a replug while the agent was down) is
// covered again. A container listed as not running lets go of its namespace.
// Either change to a namespace held less than cameraShareSyncGrace is left
// alone: the listing may predate the start that holds it, and its pid may be
// another process's by now. Callers hold s.shares.ops.
func (s *VideoService) syncShareNamespaces(ctx context.Context, containers []CameraShareContainer) {
	listed := append([]CameraShareContainer(nil), containers...)
	sort.SliceStable(listed, func(i, j int) bool { return listed[i].ID < listed[j].ID })
	now := time.Now()
	for _, lc := range listed {
		if ctx.Err() != nil {
			return
		}
		if !lc.Running {
			s.shares.mu.Lock()
			var ns *devcover.Namespace
			if c := s.shares.containers[lc.ID]; c != nil && c.ns != nil && now.Sub(c.nsAt) >= cameraShareSyncGrace {
				ns = releaseNamespaceLocked(c)
			}
			s.shares.mu.Unlock()
			s.closeNamespaceNow(ns)
			continue
		}
		if lc.PID != 0 {
			s.recoverMissedReplugs(lc.ID, lc.PID, now)
		}
	}
}

// recoverMissedReplugs holds the running container's namespace through pid
// when none is held, or one was held through another pid at least
// cameraShareSyncGrace before now, and covers again each camera a replug
// uncovered. A namespace it keeps (the container's own) is used to cover
// again the cameras whose start cover failed (retryFailedStartCovers).
func (s *VideoService) recoverMissedReplugs(containerID string, pid uint32, now time.Time) {
	s.shares.mu.Lock()
	c := s.shares.containers[containerID]
	if c == nil || len(c.shares) == 0 || s.shares.stopped {
		s.shares.mu.Unlock()
		return
	}
	if c.ns != nil && (c.nsPID == pid || now.Sub(c.nsAt) < cameraShareSyncGrace) {
		ns, at := c.ns, c.nsAt
		var failed []*shareRecord
		for _, r := range c.shares {
			if r.coverFailed != "" {
				failed = append(failed, r)
			}
		}
		s.shares.mu.Unlock()
		s.retryFailedStartCovers(containerID, ns, at, failed, now)
		return
	}
	gen := c.nsGen
	recs := append([]*shareRecord(nil), c.shares...)
	s.shares.mu.Unlock()

	ns, inode, err := s.openContainerNamespace(pid)
	if err != nil {
		s.shareLogger().Debug(fmt.Sprintf("camera share: %s: opening the mount namespace of pid %d failed: %v; its cameras are covered again at its next start", containerID, pid, err))
		return
	}
	s.shares.mu.Lock()
	prev := c.ns
	held := s.holdNamespaceLocked(containerID, c, gen, ns, inode, pid)
	// Held: the namespace it replaced goes. Not: this one does.
	gone := ns
	if held {
		gone = prev
	}
	closeIt := s.namespaceCloserLocked(gone)
	s.shares.mu.Unlock()
	closeIt()
	if held && s.coverWhereUncovered(containerID, recs, ns, "the camera was replugged while the agent was down") {
		s.persistCameraShares()
	}
}

// cameraShareCoverRetryAge is how long a start must have held the container's
// namespace before a sync covers again in it a camera whose start cover
// failed: by then the start's own cover, which can land just after its
// budget, has finished, so the two never stack.
const cameraShareCoverRetryAge = 2 * cameraShareCoverBudget

// retryFailedStartCovers covers again, in ns (the container's namespace,
// held since at), each of recs, whose start cover failed, each within
// cameraShareRecoverTimeout. A cover there now clears the note.
func (s *VideoService) retryFailedStartCovers(containerID string, ns *devcover.Namespace, at time.Time, recs []*shareRecord, now time.Time) {
	if len(recs) == 0 || now.Sub(at) < cameraShareCoverRetryAge {
		return
	}
	if s.coverWhereUncovered(containerID, recs, ns, "its start's cover had failed") {
		s.persistCameraShares()
	}
}

// coverWhereUncovered covers again, in the container's namespace ns, each of
// recs' present cameras whose cover is not there; why says how the cover was
// lost, for the log. Only a record's own share node is ever put over a camera
// (shareNodeToCover). A cover that is there afterwards, made now or found,
// clears the record's failed-cover note. changed reports a record change for
// the state file.
func (s *VideoService) coverWhereUncovered(containerID string, recs []*shareRecord, ns *devcover.Namespace, why string) (changed bool) {
	log := s.shareLogger()
	for _, r := range recs {
		path, ok, ch := s.resolveSharePath(r)
		changed = changed || ch
		if !ok {
			continue
		}
		node, err := s.shareNodeToCover(r)
		if err != nil {
			msg := fmt.Sprintf("camera share: %s: not covering %s (%s) again: %v", containerID, path, r.cameraName, err)
			if errors.Is(err, errShareNodeMaking) {
				log.Debug(msg) // the start making it covers it
			} else {
				log.Warn(msg)
			}
			continue
		}
		covered, err := coveredPath(ns, node, path)
		if err != nil {
			log.Debug(fmt.Sprintf("camera share: %s: checking %s's cover of %s failed: %v", containerID, node, path, err))
			continue
		}
		if covered {
			s.setCoverFailed(r, "")
			continue
		}
		if err := coverPath(ns, node, path, cameraShareRecoverTimeout); err != nil {
			log.Warn(fmt.Sprintf("camera share: %s has the real camera %s (%s): covering it again failed (%s): %v",
				containerID, r.cameraName, path, why, err))
			continue
		}
		s.setCoverFailed(r, "")
		log.Info(fmt.Sprintf("camera share: covered %s for %s again; %s", path, containerID, why))
	}
	return changed
}

// coverWhatTheWatchMissed runs once each start of the watch is surely in
// place: a camera replugged while no watch ran gave no event, so every held
// namespace is checked for covers that are not there.
func (s *VideoService) coverWhatTheWatchMissed() {
	type running struct {
		id   string
		ns   *devcover.Namespace
		recs []*shareRecord
	}
	var all []running
	s.shares.mu.Lock()
	for id, c := range s.shares.containers {
		if c.ns != nil {
			all = append(all, running{id, c.ns, append([]*shareRecord(nil), c.shares...)})
		}
	}
	s.shares.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].id < all[j].id })
	changed := false
	for _, a := range all {
		if s.coverWhereUncovered(a.id, a.recs, a.ns, "it was replugged while /dev was not watched") {
			changed = true
		}
	}
	if changed {
		s.persistCameraShares()
	}
}

// watchCameraReplugs covers a camera's new node again for every running
// container that shares it, as soon as the node appears in /dev: a replug
// takes the cover away with the old node. It acts on the node's creation
// (inotify), never on udev's event, which comes about 90 ms later, and
// attribute changes are not watched. The watch starts again
// cameraShareWatchRetry after it fails, and cameraShareWatchSettle after each
// start the covers a replug took while no watch ran are made again
// (coverWhatTheWatchMissed). It runs until ctx ends.
func (s *VideoService) watchCameraReplugs(ctx context.Context) {
	q := &replugQueue{wake: make(chan struct{}, 1)}
	handled := make(chan struct{})
	go func() {
		defer close(handled)
		s.handleReplugs(ctx, q)
	}()
	defer func() { <-handled }()
	for {
		settled := time.AfterFunc(cameraShareWatchSettle, q.requestRecheck)
		err := watchDevices(ctx, "/dev", q.push)
		settled.Stop()
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, devwatch.ErrUnsupported) {
			s.shareLogger().Debug("camera share: not watching /dev for replugs: " + err.Error())
			return
		}
		if err == nil {
			err = errors.New("the watch ended")
		}
		s.shareLogger().Warn(fmt.Sprintf("camera share: watching /dev for cameras failed: %v; trying again in %s", err, cameraShareWatchRetry))
		select {
		case <-ctx.Done():
			return
		case <-time.After(cameraShareWatchRetry):
		}
	}
}

// replugQueue hands the watch's events to the handler without blocking the
// watch: each node that appeared waits once, with the time it appeared, and
// none is dropped. A node removed needs nothing: its cover goes with it.
// recheck asks for coverWhatTheWatchMissed; overflow says the kernel dropped
// events (its inotify queue overflowed), so a replug may have gone unseen.
type replugQueue struct {
	mu       sync.Mutex
	pending  []replugEvent
	recheck  bool
	overflow bool
	wake     chan struct{}
}

func (q *replugQueue) requestRecheck() {
	q.mu.Lock()
	q.recheck = true
	q.mu.Unlock()
	q.signal()
}

func (q *replugQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

type replugEvent struct {
	base string
	at   time.Time
}

func (q *replugQueue) push(ev devwatch.Event) {
	if ev.Overflow {
		q.mu.Lock()
		q.recheck, q.overflow = true, true
		q.mu.Unlock()
		q.signal()
		return
	}
	if !ev.Created {
		return
	}
	at := time.Now()
	q.mu.Lock()
	if !slices.ContainsFunc(q.pending, func(p replugEvent) bool { return p.base == ev.Name }) {
		q.pending = append(q.pending, replugEvent{ev.Name, at})
	}
	q.mu.Unlock()
	q.signal()
}

func (q *replugQueue) take() (events []replugEvent, recheck, overflow bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	events, recheck, overflow = q.pending, q.recheck, q.overflow
	q.pending, q.recheck, q.overflow = nil, false, false
	return events, recheck, overflow
}

func (s *VideoService) handleReplugs(ctx context.Context, q *replugQueue) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		}
		events, recheck, overflow := q.take()
		if overflow {
			s.shareLogger().Warn("camera share: the /dev watch lost events (its inotify queue overflowed); checking every shared camera's cover")
		}
		for _, ev := range events {
			if ctx.Err() != nil {
				return
			}
			s.recoverAfterReplug(ctx, ev.base, ev.at)
		}
		if recheck && ctx.Err() == nil {
			s.coverWhatTheWatchMissed()
		}
		if overflow && ctx.Err() == nil {
			// A camera that came back unseen: its pumps need not wait out a
			// backoff either.
			nudgeSharePumps(s, s.allShareRecords())
		}
	}
}

// replugTarget is one running container's record whose camera is a node that
// just appeared.
type replugTarget struct {
	containerID string
	r           *shareRecord
	ns          *devcover.Namespace
	inode       uint64
	node        string
	// skipped: node is not the record's own share node (err says why), so
	// nothing is put over the camera.
	skipped bool
	err     error
	took    time.Duration
}

// recoverAfterReplug handles /dev/<base> appearing at at. A share node, or a
// node without a camera identity, is not a camera. For a camera, first every
// record learns it as a sibling where it is of their model (whether or not
// its container runs), then every record is resolved, and each running
// container whose record resolves to it gets its node over it again, each
// within cameraShareRecoverTimeout: all covers first, then the log lines,
// since a line between two covers delays the second. A check for an app that
// took the real camera just before its cover follows
// cameraShareLoserCheckDelay later. The state file is saved after the covers
// and before the lines.
func (s *VideoService) recoverAfterReplug(ctx context.Context, base string, at time.Time) {
	path := "/dev/" + base
	log := s.shareLogger()
	if s.IsShareNode(path) {
		log.Debug(fmt.Sprintf("camera share: %s appeared; it is a share node", path))
		return
	}
	if _, err := s.identityOf(base); err != nil {
		log.Debug(fmt.Sprintf("camera share: %s appeared; it has no camera identity (%v)", path, err))
		return
	}
	present := s.presentCameraIdentities()
	changed := s.learnSameModelPorts(present)
	targets, here, ch := s.replugTargets(path, present)
	changed = changed || ch
	// After the covers, which must not wait: a pump backing off while its
	// camera was away joins now rather than up to a backoff step later.
	defer nudgeSharePumps(s, here)
	for i := range targets {
		t := &targets[i]
		if t.skipped {
			continue
		}
		t.err = coverPath(t.ns, t.node, path, cameraShareRecoverTimeout)
		t.took = time.Since(at)
	}
	if changed {
		s.persistCameraShares()
	}
	var covered []replugTarget
	for _, t := range targets {
		switch {
		case t.skipped:
			log.Warn(fmt.Sprintf("camera share: %s has the real camera %s (%s) after it came back: not covering it again: %v",
				t.containerID, t.r.cameraName, path, t.err))
			continue
		case t.err != nil:
			log.Warn(fmt.Sprintf("camera share: %s has the real camera %s (%s) after it came back: covering it again failed: %v",
				t.containerID, t.r.cameraName, path, t.err))
			continue
		}
		s.setCoverFailed(t.r, "")
		log.Info(fmt.Sprintf("camera share: covered %s for %s again in %.2f ms after the camera came back",
			path, t.containerID, float64(t.took.Microseconds())/1000))
		covered = append(covered, t)
	}
	if len(targets) == 0 {
		log.Debug(fmt.Sprintf("camera share: %s appeared; no running app shares it", path))
	}
	if len(covered) == 0 {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		select {
		case <-ctx.Done():
			return
		case <-time.After(cameraShareLoserCheckDelay):
		}
		s.checkWindowLosers(path, covered)
	}()
}

// nudgeSharePumps nudges the pumps of recs (sharePump.Nudge), read under the
// share lock and called outside it.
func nudgeSharePumps(s *VideoService, recs []*shareRecord) {
	var pumps []*sharePump
	s.shares.mu.Lock()
	for _, r := range recs {
		if r.pump != nil {
			pumps = append(pumps, r.pump)
		}
	}
	s.shares.mu.Unlock()
	for _, p := range pumps {
		p.Nudge()
	}
}

// allShareRecords is every record, for a pass over all of them.
func (s *VideoService) allShareRecords() []*shareRecord {
	s.shares.mu.Lock()
	defer s.shares.mu.Unlock()
	var recs []*shareRecord
	for _, c := range s.shares.containers {
		recs = append(recs, c.shares...)
	}
	return recs
}

// replugTargets resolves every record among present, so each follows its
// camera, and returns those of running containers whose camera is at path, in
// container order, and in here every record whose camera is at path, running
// or not. changed reports a record change for the state file.
func (s *VideoService) replugTargets(path string, present map[string]camera.Identity) (targets []replugTarget, here []*shareRecord, changed bool) {
	type share struct {
		id string
		c  *shareContainer
		r  *shareRecord
	}
	var all []share
	s.shares.mu.Lock()
	for id, c := range s.shares.containers {
		for _, r := range c.shares {
			all = append(all, share{id, c, r})
		}
	}
	s.shares.mu.Unlock()
	sort.SliceStable(all, func(i, j int) bool { return all[i].id < all[j].id })
	for _, a := range all {
		at, ok, ch := s.resolveSharePathIn(a.r, present)
		changed = changed || ch
		if !ok || at != path {
			continue
		}
		here = append(here, a.r)
		s.shares.mu.Lock()
		ns, inode := a.c.ns, a.c.nsInode
		s.shares.mu.Unlock()
		if ns == nil {
			continue
		}
		t := replugTarget{containerID: a.id, r: a.r, ns: ns, inode: inode}
		// Only the record's own share node ever goes over the camera.
		t.node, t.err = s.shareNodeToCover(a.r)
		t.skipped = t.err != nil
		targets = append(targets, t)
	}
	return targets, here, changed
}

// checkWindowLosers names each container whose app opened the real camera at
// path in the moment between the node appearing and its cover: a holder of
// the camera in the container's mount namespace. That app holds the real
// camera, not its node, so it does not share it until it reopens the camera,
// and the record says so until its pump next reads for the app.
func (s *VideoService) checkWindowLosers(path string, covered []replugTarget) {
	holders := deviceHolders(path)
	for _, t := range covered {
		for _, h := range holders {
			if h.MountNS == 0 || h.MountNS != t.inode {
				continue
			}
			who := fmt.Sprintf("pid %d, %s", h.PID, h.Comm)
			s.shares.mu.Lock()
			t.r.tookRealCamera = who
			s.shares.mu.Unlock()
			s.shareLogger().Warn(fmt.Sprintf("camera share: %s took the real camera %s while it was being covered after a replug, "+
				"and does not share it until it reopens the camera (%s)", t.containerID, path, who))
			break
		}
	}
}

// closeCameraShareNamespaces is Shutdown's end, once the replug watch has
// stopped: it waits for start covers still running, closes every namespace
// held and waits for the closes handed off earlier, each wait bounded. Covers
// already made stay; a mount in a container outlives the agent.
func (s *VideoService) closeCameraShareNamespaces() {
	s.waitShareGroup(&s.shares.coverWG, "a container start's cover")
	s.shares.mu.Lock()
	var held []*devcover.Namespace
	for _, c := range s.shares.containers {
		if ns := releaseNamespaceLocked(c); ns != nil {
			held = append(held, ns)
		}
	}
	s.shares.mu.Unlock()
	for _, ns := range held {
		s.closeNamespaceNow(ns)
	}
	s.waitShareGroup(&s.shares.nsWG, "closing a container's mount namespace")
}

// waitShareGroup waits for wg for at most shareRemovalShutdownWait.
func (s *VideoService) waitShareGroup(wg *sync.WaitGroup, what string) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shareRemovalShutdownWait):
		s.shareLogger().Warn(fmt.Sprintf("camera share: %s is still running after %s; shutting down without it", what, shareRemovalShutdownWait))
	}
}

// cameraShareReportBudget (a variable for tests) bounds one CameraShareReport. The hardware discoverer
// calls it on every camera listing, and shareDecisionFor can block on a
// GStreamer element listing that is not context-aware.
var cameraShareReportBudget = 3 * time.Second

var _ hardware.CameraShareReporter = (*VideoService)(nil)

// CameraShareReport describes how each local camera is shared, by device path,
// for the hardware listing (internal/shared/camerashare names the keys). A
// camera that is shared carries its mode in each node format it can be shared
// in; one that is not carries why. When containers share it, it also names
// them and each one's node settings and state.
//
// Nothing is cached: every call decides afresh with shareDecisionFor, which
// only reads (its GStreamer listing is memoised), on its own goroutine and
// inside cameraShareReportBudget shared with the camera listing, so a slow
// element listing costs the listing at most that. A camera whose decision does
// not arrive in time still reports its sharers, just not its mode or reason. No share lock is held while deciding or
// reading a pump: the records are copied out under mu first.
func (s *VideoService) CameraShareReport(ctx context.Context) map[string]map[string]string {
	ctx, cancel := context.WithTimeout(ctx, cameraShareReportBudget)
	defer cancel()
	type listing struct {
		devices []*agentpb.VideoDevice
		err     error
	}
	listed, ok := withinBudget(ctx, func() listing {
		devices, err := s.listCameras(ctx)
		return listing{devices, err}
	})
	if !ok || listed.err != nil {
		return nil
	}

	// The rows say who shares the camera at each path now, not who shared the
	// camera that was there last.
	s.resolveAllShares()
	report := make(map[string]map[string]string)
	for _, dev := range listed.devices {
		switch dev.GetTransport() {
		case agentpb.VideoTransport_VIDEO_TRANSPORT_ROS2, agentpb.VideoTransport_VIDEO_TRANSPORT_IP:
			continue
		}
		props := map[string]string{}
		// Who shares a camera comes from the records, not from a decision, so
		// a decision that misses the budget costs only the mode or reason.
		if with, state := s.shareStatesOf(dev.GetPath()); len(with) > 0 {
			props[camerashare.KeyWith] = strings.Join(with, ", ")
			props[camerashare.KeyState] = strings.Join(state, ", ")
		}
		if ctx.Err() == nil {
			if decisions, ok := withinBudget(ctx, func() map[string]shareDecision {
				return s.shareDecisionsFor(dev, appconfig.CameraFormats...)
			}); ok {
				s.shares.mu.Lock()
				unsupported := s.shares.freshStartUnsupported
				s.shares.mu.Unlock()
				// The default format's mode and refusal keep the keys an
				// older command line reads. Another format says why it is
				// not to be had only on a camera that is shared: an app that
				// asks for it gets the default format's node there.
				defaultFormat, otherFormats := appconfig.CameraFormats[0], appconfig.CameraFormats[1:]
				switch byDefault := decisions[defaultFormat]; {
				case !byDefault.Shared:
					props[camerashare.KeyUnavailable] = byDefault.Reason
				case unsupported:
					props[camerashare.KeyUnavailable] = errShareFreshStartUnsupported.Error()
				default:
					props[camerashare.KeyMode] = byDefault.Mode.String()
					for _, format := range otherFormats {
						if decision := decisions[format]; decision.Shared {
							props[camerashare.KeyModeOfFormat(format)] = decision.Mode.String()
						} else {
							props[camerashare.KeyUnavailableOfFormat(format)] = decision.Reason
						}
					}
				}
			}
		}
		if len(props) > 0 {
			report[dev.GetPath()] = props
		}
	}
	return report
}

// shareStatesOf returns the sorted IDs of the containers sharing the camera at
// path and, in the same order,
// "<id> [format <format>, stamp <stamp>] (<phase>[: <detail>][; <note>]...)"
// for each. A record whose camera was absent at its last resolve is not the
// camera at path (sharedModeOf).
func (s *VideoService) shareStatesOf(path string) (ids, states []string) {
	type snap struct {
		id                 string
		pump               *sharePump
		tookB, coverFailed string
		modeNote           string
		format, stamp      string
		asked, askedWhyNot string
	}
	var snaps []snap
	s.shares.mu.Lock()
	for id, c := range s.shares.containers {
		if c == nil {
			continue
		}
		for _, r := range c.shares {
			if r != nil && r.cameraPath == path && !r.cameraAbsent {
				snaps = append(snaps, snap{id, r.pump, r.tookRealCamera, r.coverFailed, r.modeNote, r.mode.Format, r.stamp, r.askedFormat, r.askedWhyNot})
				break
			}
		}
	}
	s.shares.mu.Unlock()
	slices.SortFunc(snaps, func(a, b snap) int { return strings.Compare(a.id, b.id) })
	for _, sn := range snaps {
		var text string
		switch {
		case sn.tookB != "":
			text = "not sharing: took the real camera after a replug"
		case sn.coverFailed != "":
			text = "not sharing: its start's cover failed: " + sn.coverFailed
		case sn.pump == nil:
			text = string(shareIdle)
		default:
			st := sn.pump.State()
			text = string(st.Phase)
			if st.Detail != "" {
				text += ": " + st.Detail
			}
		}
		if sn.modeNote != "" {
			text += "; " + sn.modeNote
		}
		if sn.asked != "" {
			text += "; " + describeServedInstead(sn.asked, sn.format, sn.askedWhyNot)
		}
		ids = append(ids, sn.id)
		states = append(states, fmt.Sprintf("%s [%s] (%s)", sn.id, describeShareSettings(sn.format, sn.stamp), text))
	}
	return ids, states
}
