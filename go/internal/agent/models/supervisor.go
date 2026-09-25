package models

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"go.uber.org/zap"
)

// Limits and lifetimes from the design (§5, §7).
const (
	defaultMaxRunning = 2
	cameraTimeout     = 10 * time.Second
	removeTimeout     = 30 * time.Second
	leaseGrace        = 60 * time.Second
)

// Config holds a Supervisor's dependencies.
type Config struct {
	Catalog    Catalog
	Device     DeviceProfile
	Runtime    Runtime
	Cameras    Cameras
	Files      Files
	Root       string // state directory, e.g. /var/lib/wendy/models
	Logger     *zap.Logger
	Clock      Clock // nil: the real clock
	MaxRunning int   // 0: two
}

// Supervisor owns every model instance on the device.
type Supervisor struct {
	cfg   Config
	clock Clock
	log   *zap.Logger

	mu        sync.Mutex
	instances map[string]*instance
	byKey     map[string]*instance // model id + NUL + camera source id
}

// NewSupervisor returns a Supervisor with no instances.
func NewSupervisor(cfg Config) *Supervisor {
	if cfg.Clock == nil {
		cfg.Clock = realClock{}
	}
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}
	if cfg.MaxRunning == 0 {
		cfg.MaxRunning = defaultMaxRunning
	}
	return &Supervisor{cfg: cfg, clock: cfg.Clock, log: cfg.Logger,
		instances: map[string]*instance{}, byKey: map[string]*instance{}}
}

// CatalogEntry is one catalog model as it applies to this device.
type CatalogEntry struct {
	Model             Model
	Variant           *Variant // nil when no variant fits the device
	UnavailableReason string
	ImageCached       bool
	DownloadBytes     int64 // model file bytes a first start downloads; 0 when cached
	NeedsEngineBuild  bool
}

// CatalogView is the catalog, the cameras, and the free slots on this device.
type CatalogView struct {
	Version    string
	Models     []CatalogEntry
	Cameras    []Camera
	MaxRunning int
	Running    int
}

// Catalog describes what this device can run right now.
func (s *Supervisor) Catalog(ctx context.Context) CatalogView {
	view := CatalogView{Version: s.cfg.Catalog.Version, Cameras: s.cfg.Cameras.List(ctx), MaxRunning: s.cfg.MaxRunning}
	for _, m := range s.cfg.Catalog.Models {
		entry := CatalogEntry{Model: m}
		v, reason, ok := SelectVariant(m, s.cfg.Device)
		if !ok {
			entry.UnavailableReason = reason
			view.Models = append(view.Models, entry)
			continue
		}
		entry.Variant = &v
		entry.ImageCached = s.cfg.Runtime.HasImage(ctx, v.HostImage)
		if !s.cfg.Files.Has(v.File.SHA256) {
			entry.DownloadBytes = v.File.Bytes
		}
		entry.NeedsEngineBuild = s.needsEngineBuild(v)
		view.Models = append(view.Models, entry)
	}
	s.mu.Lock()
	view.Running = len(s.instances)
	s.mu.Unlock()
	return view
}

func (s *Supervisor) needsEngineBuild(v Variant) bool {
	return v.Engine == EngineTensorRT && !engineCached(s.enginesDir(), v.File.SHA256, s.cfg.Device.GPUArch)
}

func (s *Supervisor) enginesDir() string { return filepath.Join(s.cfg.Root, "engines") }

// Start runs modelID on the camera, or returns the instance already doing so
// (reused). The instance prepares in the background; watch it for progress.
func (s *Supervisor) Start(ctx context.Context, modelID, cameraSourceID string) (InstanceInfo, bool, error) {
	model, ok := s.cfg.Catalog.Model(modelID)
	if !ok {
		return InstanceInfo{}, false, fmt.Errorf("%w %q", ErrUnknownModel, modelID)
	}
	variant, reason, ok := SelectVariant(model, s.cfg.Device)
	if !ok {
		return InstanceInfo{}, false, fmt.Errorf("%w: %s", ErrNoVariant, reason)
	}
	if !s.hasCamera(ctx, cameraSourceID) {
		return InstanceInfo{}, false, fmt.Errorf("%w %q", ErrUnknownCamera, cameraSourceID)
	}

	key := modelID + "\x00" + cameraSourceID
	var inst *instance
	for {
		s.mu.Lock()
		if existing := s.byKey[key]; existing != nil {
			s.mu.Unlock()
			info, reused, err := s.awaitExisting(ctx, existing)
			if err != nil {
				return InstanceInfo{}, false, err
			}
			if reused {
				return info, true, nil
			}
			continue // existing has fully gone; look again and admit afresh if the key is free
		}
		if len(s.instances) >= s.cfg.MaxRunning {
			running := s.idsLocked()
			s.mu.Unlock()
			return InstanceInfo{}, false, fmt.Errorf("%w (%d): %s", ErrCapacity, s.cfg.MaxRunning, strings.Join(running, ", "))
		}
		inst = s.newInstance(model, variant, cameraSourceID)
		s.instances[inst.id] = inst
		s.byKey[key] = inst
		s.mu.Unlock()
		break
	}

	// A camera that cannot stream is the caller's error, not a failed
	// instance, so it is checked before the start counts.
	acquireCtx, cancel := context.WithTimeout(ctx, cameraTimeout)
	node, err := s.cfg.Cameras.Acquire(acquireCtx, inst.id, cameraSourceID)
	cancel()
	if err != nil {
		s.forget(inst)
		inst.cancel()
		close(inst.done)
		close(inst.admitted)
		if !errors.Is(err, ErrCameraNotStreamable) {
			err = fmt.Errorf("%w: %v", ErrCameraNotStreamable, err)
		}
		return InstanceInfo{}, false, err
	}

	inst.mu.Lock()
	inst.node = node
	s.startGraceLocked(inst) // no watch yet: stop unless one attaches
	info := inst.infoLocked()
	inst.mu.Unlock()
	close(inst.admitted)
	go s.run(inst)
	return info, false, nil
}

// awaitExisting waits out another Start already admitting existing for the
// same key. When existing turns out live, it is the instance to reuse. When
// it was refused or is stopping, this waits for it to fully go so the caller
// can look again and admit a fresh instance in its place.
func (s *Supervisor) awaitExisting(ctx context.Context, existing *instance) (InstanceInfo, bool, error) {
	select {
	case <-existing.admitted:
	case <-ctx.Done():
		return InstanceInfo{}, false, ctx.Err()
	}
	if existing.ctx.Err() == nil {
		return existing.info(), true, nil
	}
	select {
	case <-existing.done:
	case <-ctx.Done():
		return InstanceInfo{}, false, ctx.Err()
	}
	return InstanceInfo{}, false, nil
}

func (s *Supervisor) hasCamera(ctx context.Context, sourceID string) bool {
	for _, c := range s.cfg.Cameras.List(ctx) {
		if c.SourceID == sourceID {
			return true
		}
	}
	return false
}

func (s *Supervisor) idsLocked() []string {
	ids := make([]string, 0, len(s.instances))
	for id := range s.instances {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (s *Supervisor) newInstance(m Model, v Variant, camera string) *instance {
	id := newID("m-")
	ctx, cancel := context.WithCancel(context.Background())
	return &instance{
		id: id, model: m, variant: v, camera: camera, started: s.clock.Now(),
		runDir: filepath.Join(s.cfg.Root, "run", id),
		ctx:    ctx, cancel: cancel, wake: make(chan struct{}, 1), done: make(chan struct{}),
		admitted: make(chan struct{}),
		watches:  map[string]*Watch{},
		state:    StatePreparing, detail: "starting",
	}
}

// newID returns prefix followed by eight random hex digits.
func newID(prefix string) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// forget removes inst from the supervisor's maps, if it is still there.
func (s *Supervisor) forget(inst *instance) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.instances[inst.id] == inst {
		delete(s.instances, inst.id)
	}
	if key := inst.model.ID + "\x00" + inst.camera; s.byKey[key] == inst {
		delete(s.byKey, key)
	}
}

func (s *Supervisor) lookup(id string) (*instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if inst := s.instances[id]; inst != nil {
		return inst, nil
	}
	return nil, fmt.Errorf("%w %q", ErrUnknownInstance, id)
}

// List returns every instance, oldest first.
func (s *Supervisor) List() []InstanceInfo {
	s.mu.Lock()
	insts := make([]*instance, 0, len(s.instances))
	for _, inst := range s.instances {
		insts = append(insts, inst)
	}
	s.mu.Unlock()
	out := make([]InstanceInfo, 0, len(insts))
	for _, inst := range insts {
		out = append(out, inst.info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

// Stop with an empty watchID stops the instance outright. With a watchID it
// ends that watch, and stops the instance if it was the last. When the
// instance stops, Stop waits for its removal and returns the final state.
func (s *Supervisor) Stop(ctx context.Context, instanceID, watchID string) (InstanceInfo, error) {
	if watchID != "" {
		inst, stopping, err := s.detach(instanceID, watchID, true)
		if err != nil {
			return InstanceInfo{}, err
		}
		if !stopping {
			return inst.info(), nil
		}
		return s.awaitRemoval(ctx, inst)
	}
	inst, err := s.lookup(instanceID)
	if err != nil {
		return InstanceInfo{}, err
	}
	inst.mu.Lock()
	inst.requestStopLocked("stopped")
	inst.mu.Unlock()
	return s.awaitRemoval(ctx, inst)
}

func (s *Supervisor) awaitRemoval(ctx context.Context, inst *instance) (InstanceInfo, error) {
	select {
	case <-inst.done:
		return inst.info(), nil
	case <-ctx.Done():
		return inst.info(), ctx.Err()
	}
}

// Shutdown stops every instance and waits for their removal, or for ctx.
func (s *Supervisor) Shutdown(ctx context.Context) {
	s.mu.Lock()
	insts := make([]*instance, 0, len(s.instances))
	for _, inst := range s.instances {
		insts = append(insts, inst)
	}
	s.mu.Unlock()
	for _, inst := range insts {
		inst.mu.Lock()
		inst.requestStopLocked("the agent is shutting down")
		inst.mu.Unlock()
	}
	for _, inst := range insts {
		select {
		case <-inst.done:
		case <-ctx.Done():
			return
		}
	}
}

// PublishApplicationRecord receives every record the app data socket
// accepts. Records from model hosts update their instance; everything else is
// ignored. It never blocks, so it is safe on the socket's read path.
func (s *Supervisor) PublishApplicationRecord(appID string, rec data.ApplicationRecord) {
	id, ok := strings.CutPrefix(appID, AppIDPrefix)
	if !ok || rec.Type != "event" {
		return
	}
	s.mu.Lock()
	inst := s.instances[id]
	s.mu.Unlock()
	if inst == nil {
		return
	}
	if rec.Name == RecordStatus {
		st, err := parseStatus(rec)
		if err != nil {
			s.log.Debug("ignoring a malformed model status", zap.String("instance", id), zap.Error(err))
			return
		}
		s.handleStatus(inst, st)
	}
}

// Watch attaches a watch to a live instance. It returns the watch, the
// instance snapshot, and the sequence of the instance's newest event, which a
// client passes back as AfterSequence when it re-attaches.
func (s *Supervisor) Watch(req WatchRequest) (*Watch, InstanceInfo, uint64, error) {
	inst, err := s.lookup(req.InstanceID)
	if err != nil {
		return nil, InstanceInfo{}, 0, err
	}
	select {
	case <-inst.admitted:
	default:
		// Start has not settled the camera yet, so the instance may never run.
		return nil, InstanceInfo{}, 0, fmt.Errorf("%w %q", ErrUnknownInstance, req.InstanceID)
	}
	if err := inst.model.checkFilter(req.Filter); err != nil {
		return nil, InstanceInfo{}, 0, err
	}
	ch := make(chan WatchMessage, watchBuffer)
	w := &Watch{ID: newID("w-"), InstanceID: inst.id, Label: req.Label, C: ch, c: ch, filter: req.Filter}

	inst.mu.Lock()
	defer inst.mu.Unlock()
	if inst.ctx.Err() != nil {
		return nil, InstanceInfo{}, 0, fmt.Errorf("%w %q", ErrUnknownInstance, req.InstanceID)
	}
	inst.watches[w.ID] = w
	s.cancelGraceLocked(inst)
	return w, inst.infoLocked(), 0, nil
}

// Detach ends a watch whose client went away. The instance stays for
// leaseGrace so the client can re-attach. Unknown ids are ignored.
func (s *Supervisor) Detach(instanceID, watchID string) {
	_, _, _ = s.detach(instanceID, watchID, false)
}

// detach ends one watch. When it was the last, an explicit detach stops the
// instance at once and a lost client starts the grace period. It reports
// whether the instance is now stopping.
func (s *Supervisor) detach(instanceID, watchID string, explicit bool) (*instance, bool, error) {
	inst, err := s.lookup(instanceID)
	if err != nil {
		return nil, false, err
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()
	w := inst.watches[watchID]
	if w == nil {
		return inst, false, fmt.Errorf("%w %q", ErrUnknownWatch, watchID)
	}
	delete(inst.watches, watchID)
	w.end(nil)
	if len(inst.watches) > 0 {
		return inst, false, nil
	}
	if explicit {
		inst.requestStopLocked("stopped by its last watcher")
		return inst, true, nil
	}
	s.startGraceLocked(inst)
	return inst, false, nil
}

// startGraceLocked stops the instance unless a watch attaches within
// leaseGrace. Caller holds inst.mu.
func (s *Supervisor) startGraceLocked(inst *instance) {
	s.cancelGraceLocked(inst)
	gen := inst.graceGen
	inst.grace = s.clock.AfterFunc(leaseGrace, func() {
		inst.mu.Lock()
		defer inst.mu.Unlock()
		if inst.graceGen == gen && len(inst.watches) == 0 {
			inst.requestStopLocked("no watchers for 60 s")
		}
	})
}

// cancelGraceLocked drops a pending lease expiry. Caller holds inst.mu.
func (s *Supervisor) cancelGraceLocked(inst *instance) {
	inst.graceGen++
	if inst.grace != nil {
		inst.grace.Stop()
		inst.grace = nil
	}
}
