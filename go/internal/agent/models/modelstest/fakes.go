// Package modelstest provides fakes for testing code built on package models.
package modelstest

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/models"
)

// Clock is a manual clock. AfterFunc callbacks run, in deadline order, when
// Advance moves time past them.
type Clock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*timer
}

type timer struct {
	clock *Clock
	at    time.Time
	f     func()
	done  bool // fired or stopped
}

func (t *timer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.done {
		return false
	}
	t.done = true
	return true
}

// NewClock starts at a fixed instant.
func NewClock() *Clock { return &Clock{now: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)} }

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *Clock) AfterFunc(d time.Duration, f func()) models.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &timer{clock: c, at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return t
}

// Advance moves time forward by d and runs every callback that falls due, on
// the calling goroutine and without holding the clock's lock.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []*timer
	kept := c.timers[:0]
	for _, t := range c.timers {
		switch {
		case t.done:
		case !t.at.After(c.now):
			t.done = true
			due = append(due, t)
		default:
			kept = append(kept, t)
		}
	}
	c.timers = kept
	c.mu.Unlock()
	sort.SliceStable(due, func(i, j int) bool { return due[i].at.Before(due[j].at) })
	for _, t := range due {
		t.f()
	}
}

// Runtime is an in-memory models.Runtime.
type Runtime struct {
	// BlockEnsure, when set before the supervisor uses the runtime, makes
	// EnsureImage wait for it to close or for the context to end.
	BlockEnsure chan struct{}
	// BlockRemove, when set before use, makes RemoveHost wait for it to
	// close or for the context to end before it removes the host.
	BlockRemove chan struct{}

	mu        sync.Mutex
	images    map[string]bool
	hosts     map[string]chan models.HostExit
	starts    map[string]int
	specs     []models.HostSpec
	removed   []string
	leftovers []string
	removing  map[string]bool
}

func NewRuntime() *Runtime {
	return &Runtime{images: map[string]bool{}, hosts: map[string]chan models.HostExit{}, starts: map[string]int{}, removing: map[string]bool{}}
}

func (r *Runtime) HasImage(_ context.Context, ref string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.images[ref]
}

func (r *Runtime) EnsureImage(ctx context.Context, ref string) error {
	if r.BlockEnsure != nil {
		select {
		case <-r.BlockEnsure:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.images[ref] = true
	return nil
}

func (r *Runtime) StartHost(_ context.Context, spec models.HostSpec) (<-chan models.HostExit, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, running := r.hosts[spec.InstanceID]; running {
		return nil, fmt.Errorf("host %s already runs", spec.InstanceID)
	}
	ch := make(chan models.HostExit, 1)
	r.hosts[spec.InstanceID] = ch
	r.starts[spec.InstanceID]++
	r.specs = append(r.specs, spec)
	return ch, nil
}

// RemoveHost ends a running host the way killing its task does.
func (r *Runtime) RemoveHost(ctx context.Context, id string) error {
	r.mu.Lock()
	r.removing[id] = true
	r.mu.Unlock()
	if r.BlockRemove != nil {
		select {
		case <-r.BlockRemove:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if ch, ok := r.hosts[id]; ok {
		ch <- models.HostExit{Code: 137}
		delete(r.hosts, id)
	}
	r.leftovers = slices.DeleteFunc(r.leftovers, func(s string) bool { return s == id })
	r.removed = append(r.removed, id)
	return nil
}

// Removing reports whether RemoveHost has been entered for id, so a test can
// tell a removal is underway even while it is blocked in BlockRemove.
func (r *Runtime) Removing(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.removing[id]
}

func (r *Runtime) ListHosts(context.Context) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := slices.Clone(r.leftovers)
	for id := range r.hosts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// Exit ends a running host's process with code, as a crash would.
func (r *Runtime) Exit(id string, code uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ch, ok := r.hosts[id]; ok {
		ch <- models.HostExit{Code: code}
		delete(r.hosts, id)
	}
}

// AddLeftover records a host container a previous agent process left behind.
func (r *Runtime) AddLeftover(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leftovers = append(r.leftovers, id)
}

func (r *Runtime) Starts(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.starts[id]
}

func (r *Runtime) Running(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.hosts[id]
	return ok
}

func (r *Runtime) LastSpec() models.HostSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.specs) == 0 {
		return models.HostSpec{}
	}
	return r.specs[len(r.specs)-1]
}

func (r *Runtime) Removed() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.removed)
}

// Cameras is a fixed camera list whose nodes can be pinned.
type Cameras struct {
	// BlockAcquire, when set before use, makes Acquire wait for it to close
	// or for the context to end before it decides.
	BlockAcquire chan struct{}

	mu     sync.Mutex
	cams   []models.Camera
	refuse map[string]bool
	owners map[string]string // owner -> source
}

func NewCameras(cams ...models.Camera) *Cameras {
	return &Cameras{cams: cams, refuse: map[string]bool{}, owners: map[string]string{}}
}

// Refuse makes Acquire fail for sourceID, like a stream that cannot carry
// frame identity.
func (c *Cameras) Refuse(sourceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refuse[sourceID] = true
}

func (c *Cameras) List(context.Context) []models.Camera {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.cams)
}

func (c *Cameras) Acquire(ctx context.Context, owner, sourceID string) (string, error) {
	if c.BlockAcquire != nil {
		select {
		case <-c.BlockAcquire:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refuse[sourceID] {
		return "", fmt.Errorf("%w: %s carries no frame identity", models.ErrCameraNotStreamable, sourceID)
	}
	c.owners[owner] = sourceID
	return "/dev/video255", nil
}

func (c *Cameras) Release(_ context.Context, owner string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.owners, owner)
}

// Owners lists the owners holding a camera pin.
func (c *Cameras) Owners() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for o := range c.owners {
		out = append(out, o)
	}
	sort.Strings(out)
	return out
}

// Files is an in-memory models.Files.
type Files struct {
	mu   sync.Mutex
	have map[string]bool
}

func NewFiles() *Files { return &Files{have: map[string]bool{}} }

func (f *Files) Has(sha string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.have[sha]
}

func (f *Files) Fetch(_ context.Context, file models.File) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.have[file.SHA256] = true
	return "/var/lib/wendy/models/files/sha256/" + file.SHA256, nil
}

// Status builds the model.status record a healthy host heartbeats.
func Status(state string) data.ApplicationRecord {
	return data.ApplicationRecord{Version: 1, Type: "event", Name: models.RecordStatus,
		Attributes: map[string]any{"state": state, "processed_fps": 9.5, "latency_p50_ms": 31.0}}
}

// Failed builds the final model.status record of a host that gives up.
func Failed(reason string) data.ApplicationRecord {
	rec := Status(models.HostFailed)
	rec.Attributes["reason"] = reason
	return rec
}

// Entered builds a model.entered record for a detection.
func Entered(class string, confidence float64, track int) data.ApplicationRecord {
	return detection(models.RecordEntered, class, confidence, track)
}

// Left builds a model.left record for a detection.
func Left(class string, confidence float64, track int) data.ApplicationRecord {
	return detection(models.RecordLeft, class, confidence, track)
}

func detection(name, class string, confidence float64, track int) data.ApplicationRecord {
	return data.ApplicationRecord{Version: 1, Type: "event", Name: name, Model: "test",
		Attributes: map[string]any{"class": class, "confidence": confidence, "track_id": float64(track),
			"box": map[string]any{"x": 0.1, "y": 0.1, "width": 0.2, "height": 0.4}},
		Inputs: []data.SampleRef{{SourceID: "v4l2:/dev/video0", SampleID: uint64(track)}}}
}

// Catalog is a one-model catalog whose only variant runs anywhere on engine.
func Catalog(engine string) models.Catalog {
	return models.Catalog{Version: "test", Models: []models.Model{{
		ID: "coco-detector", Description: "test detector", Kind: models.KindDetector,
		Labels: []string{"person", "car", "dog"},
		Variants: []models.Variant{{ID: "test-" + engine, Engine: engine,
			HostImage: "ghcr.io/wendylabsinc/wendy-model-host-test@sha256:" + strings.Repeat("a", 64),
			File:      models.File{URL: "https://example.invalid/model", SHA256: strings.Repeat("1", 64), Bytes: 10},
			InputSize: 320}},
	}}}
}

// Eventually polls cond for up to two seconds.
func Eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// AdvanceUntil advances clock in steps until cond holds, pausing between
// steps so the supervisor's goroutines can arm their timers.
func AdvanceUntil(t testing.TB, clock *Clock, step time.Duration, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		clock.Advance(step)
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out advancing the clock until %s", what)
}
