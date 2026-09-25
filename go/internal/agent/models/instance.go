package models

import (
	"context"
	"sort"
	"sync"
	"time"
)

// instance is one model instance. Fields above mu are fixed at creation; the
// rest are guarded by mu.
type instance struct {
	id      string
	model   Model
	variant Variant
	camera  string
	started time.Time
	runDir  string

	ctx      context.Context // cancelled when the instance must stop
	cancel   context.CancelFunc
	wake     chan struct{} // pokes the run loop; capacity 1
	done     chan struct{} // closed once the instance is fully removed
	admitted chan struct{} // closed once Start has settled the camera, granted or refused

	mu          sync.Mutex
	state       State
	detail      string
	stats       Stats
	node        string // the camera's two-plane node
	modelFile   string // the verified model file on the device
	stopReason  string
	hostRunning bool      // a host from the current start speaks for the instance
	hostFailure string    // reason from a host-reported failure
	lastStatus  time.Time // latest model.status from the live host

	watches  map[string]*Watch
	grace    Timer  // pending lease expiry; nil while watched
	graceGen uint64 // invalidates a grace callback that fires after being replaced
	ring     *ring  // the latest detections, numbered
}

func (inst *instance) poke() {
	select {
	case inst.wake <- struct{}{}:
	default:
	}
}

// infoLocked snapshots the instance. Caller holds mu.
func (inst *instance) infoLocked() InstanceInfo {
	labels := make([]string, 0, len(inst.watches))
	for _, w := range inst.watches {
		if w.Label != "" {
			labels = append(labels, w.Label)
		}
	}
	sort.Strings(labels)
	return InstanceInfo{
		ID: inst.id, ModelID: inst.model.ID, VariantID: inst.variant.ID, Engine: inst.variant.Engine,
		CameraSourceID: inst.camera, State: inst.state, StateDetail: inst.detail,
		Watchers: len(inst.watches), WatchLabels: labels,
		StartedAt: inst.started, Stats: inst.stats, FileSHA256: inst.variant.File.SHA256,
	}
}

func (inst *instance) info() InstanceInfo {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.infoLocked()
}

func (inst *instance) failure() string {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.hostFailure
}

// setStateLocked records a state change, tells every watch, and reports
// whether anything changed. Caller holds mu.
func (inst *instance) setStateLocked(s State, detail string) bool {
	if inst.state == s && inst.detail == detail {
		return false
	}
	inst.state, inst.detail = s, detail
	inst.broadcastLocked()
	return true
}

// broadcastLocked sends the current snapshot to every watch. Caller holds mu.
func (inst *instance) broadcastLocked() {
	info := inst.infoLocked()
	for _, w := range inst.watches {
		snapshot := info
		w.offer(WatchMessage{Status: &snapshot})
	}
}

// requestStopLocked asks the run loop to stop the instance; the first reason
// is the one reported. Caller holds mu.
func (inst *instance) requestStopLocked(reason string) {
	if inst.stopReason == "" {
		inst.stopReason = reason
	}
	inst.cancel()
}
