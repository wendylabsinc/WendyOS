package mcp

import (
	"context"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"google.golang.org/grpc/status"
)

// watchState is a watch's state as clients see it (design §6.3).
type watchState string

const (
	watchPreparing watchState = "PREPARING"
	watchReady     watchState = "READY"
	watchError     watchState = "ERROR" // not terminal: the device keeps retrying
	watchEnded     watchState = "ENDED"
)

// watchSpec is what a watch asked for. Name is the device-side identity the
// manager assigns (a campaign name for the campaign backend).
type watchSpec struct {
	Name          string
	CameraID      string
	CameraName    string
	Classes       []string
	MinConfidence float64
	Label         string
}

// watchClass is one detected class and its score.
type watchClass struct {
	Label string  `json:"label"`
	Score float64 `json:"score"`
}

// watchUpdate is one thing a backend reports, in order: an event, a status
// change, or a gap (a stretch of detections that may have been missed).
type watchUpdate struct {
	Event  *watchEventUpdate
	Status *watchStatusUpdate
	Gap    string
}

type watchEventUpdate struct {
	Kind       string // "entered" or "left"
	Classes    []watchClass
	OccurredAt time.Time
}

type watchStatusUpdate struct {
	State  watchState
	Reason string
}

// watchBackend runs watches on a device. Backends only translate; the manager
// assigns sequence numbers and owns buffering and notifications (design §6.2).
type watchBackend interface {
	// Start creates the watch on the device and returns once it exists there.
	// Readiness and events arrive on the handle. A failure the device did not
	// confirm is a watchUnconfirmedError.
	Start(ctx context.Context, conn *grpcclient.AgentConnection, spec watchSpec) (watchHandle, error)
}

// watchUnconfirmedError is a Start failure after which the device may still
// create the watch: the deploy ended without the device's answer. The watch's
// lease removes it then. It reads as the error it wraps.
type watchUnconfirmedError struct{ err error }

func (e watchUnconfirmedError) Error() string { return e.err.Error() }
func (e watchUnconfirmedError) Unwrap() error { return e.err }

// GRPCStatus is the wrapped error's own status, so tools report the device's
// code and message as they would without the wrapper.
func (e watchUnconfirmedError) GRPCStatus() *status.Status { return status.Convert(e.err) }

// watchHandle is one running watch. Updates is closed after Stop, or after the
// backend reports the watch ENDED by itself.
type watchHandle interface {
	Updates() <-chan watchUpdate
	Stop(ctx context.Context) error
}
