// Package models runs catalog models on this device for clients such as
// wendy chat. For each instance it picks the variant that fits the hardware,
// fetches and verifies the model file, runs an agent-owned host container, and
// streams the host's detections to watchers. An instance lives while a watch
// holds it, plus a grace period for reconnects
// (specs/2026-09-25-model-watch-design.md).
package models

import (
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// AppIDPrefix is the data-socket and cgroup identity prefix of model hosts.
// The agent refuses user apps whose appId starts with it.
const AppIDPrefix = appconfig.ReservedModelAppIDPrefix

// State is an instance's lifecycle state.
type State int

const (
	StatePreparing State = iota + 1
	StateStarting
	StateReady
	StateRestarting
	StateFailed
	StateStopped
)

func (s State) String() string {
	switch s {
	case StatePreparing:
		return "preparing"
	case StateStarting:
		return "starting"
	case StateReady:
		return "ready"
	case StateRestarting:
		return "restarting"
	case StateFailed:
		return "failed"
	case StateStopped:
		return "stopped"
	}
	return "unspecified"
}

// Stats are a host's latest self-reported numbers.
type Stats struct {
	ProcessedFPS  float32
	LatencyP50Ms  float32
	FramesSkipped uint64
}

// InstanceInfo is a snapshot of one model instance.
type InstanceInfo struct {
	ID             string
	ModelID        string
	VariantID      string
	Engine         string
	CameraSourceID string
	State          State
	StateDetail    string
	Watchers       int
	WatchLabels    []string
	StartedAt      time.Time
	Stats          Stats
	FileSHA256     string
}

// Errors callers map to gRPC codes.
var (
	ErrUnknownModel        = errors.New("unknown model")
	ErrUnknownCamera       = errors.New("unknown camera")
	ErrUnknownInstance     = errors.New("unknown model instance")
	ErrUnknownWatch        = errors.New("unknown watch")
	ErrNoVariant           = errors.New("no variant of this model runs on this device")
	ErrCameraNotStreamable = errors.New("camera cannot stream to a model")
	ErrCapacity            = errors.New("this device is already running its maximum number of models")
	ErrInvalidFilter       = errors.New("invalid watch filter")
)
