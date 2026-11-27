// Package tasking durably admits missions and evaluates trusted local telemetry.
// Authentication, independent actuator watchdogs and actual actuation are adapters.
package tasking

import (
	"context"
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
	"github.com/wendylabsinc/wendy/go/world/internal/supervision"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"time"
)

type Bounds struct {
	Min [3]float64 `json:"min"`
	Max [3]float64 `json:"max"`
}

const InspectType = "wendy.nav.inspect.v1"

type Spec struct {
	Patrol                *PatrolPolicy                         `json:"patrol,omitempty"`
	Go2                   *Go2Binding                           `json:"go2,omitempty"`
	Trigger               *TriggerRef                           `json:"trigger,omitempty"`
	InspectionDestination *missioncontrol.InspectionDestination `json:"inspection_destination,omitempty"`
	ControlContext        *missioncontrol.SignedContext         `json:"control_context,omitempty"`
	Proposal              *missioncontrol.ProposalRef           `json:"proposal,omitempty"`
	ID                    string                                `json:"id"`
	Type                  string                                `json:"type"`
	Site                  string                                `json:"site"`
	Assignee              string                                `json:"assignee"`
	Domain                mission.Domain                        `json:"domain"`
	Class                 mission.SafetyClass                   `json:"class"`
	Mode                  mission.Mode                          `json:"mode"`
	Frame                 world.FrameRef                        `json:"frame"`
	NotAfterUnixMS        int64                                 `json:"not_after_unix_ms"`
	Area                  Bounds                                `json:"area"`
	RequiredSensors       []string                              `json:"required_sensors"`
	MinimumEnergy         float64                               `json:"minimum_energy"`
	Fallback              mission.Action                        `json:"fallback"`
	Target                [3]float64                            `json:"target"`
	SpeedMPS              float64                               `json:"speed_mps"`
	ToleranceM            float64                               `json:"tolerance_m"`
	StopSpeedMPS          float64                               `json:"stop_speed_mps"`
}
type Config struct {
	ControlContext *missioncontrol.SignedContext
	Go2Binding     *Go2Binding // Exact commissioned simulation scope for the Go2 catalog.
	Metrics        *runtimeobs.Collector
	// Supervision is disabled unless the complete explicit scope and verifier
	// are configured. The verifier must recheck the exact ACK and current grant
	// without consuming a transport nonce. Operator.Proof is its canonical ACK.
	Tenant, Audience, GatewayPrincipal string
	ValidateSupervisor                 func(json.RawMessage) (supervision.Ack, supervision.Operator, error)
	// Context variants carry diagnostics only and must enforce the same policy.
	ValidateSupervisorContext func(context.Context, json.RawMessage) (supervision.Ack, supervision.Operator, error)
	// Batch results apply only to this validation stage. A callback error is a
	// global authority failure, not a selective operator denial. Single-proof
	// validation remains required for incoming ACK activation.
	ValidateSupervisorsContext            func(context.Context, []json.RawMessage) ([]supervision.Verification, error)
	MaxSupervisionLease                   time.Duration
	MaxSupervisors, MaxPendingSupervision int

	RunID                             string // Required for inspection; bound to the local simulation clock domain.
	Site, Assignee, ExecutorPrincipal string
	Domain                            mission.Domain
	Catalog                           map[string]mission.SafetyClass
	Now                               func() time.Time
	Monotonic                         func() time.Duration
	MaxLease, MaxTelemetryAge         time.Duration
	MaxTasks                          int
	// ValidateAuthority rechecks saved signed proof, including current revocation.
	// Required at an untrusted transport boundary; nil is for trusted local tests.
	ValidateAuthority        func(Record) error
	ValidateAuthorityContext func(context.Context, Record) error
	ValidateInspection       func(Inspection) error
}

// Authorization must only be constructed by a trusted verifier. It is never a
// network request type. Proof must bind the exact wire Spec and receiving node.
type Authorization struct {
	// ValidateAdmission is a trusted first-admission fence, never persisted or
	// reused for continuing authority. Proposal expiry belongs here only.
	ValidateAdmission       func() error
	Principal               string
	Operator, AllowAutonomy bool
	ValidUntil              time.Time
	Proof                   json.RawMessage
}
type Record struct {
	PatrolReturnResult       *PatrolReturnResult `json:"patrol_return_result,omitempty"`
	Result                   *Inspection         `json:"result,omitempty"`
	Spec                     Spec                `json:"spec"`
	Status                   mission.Status      `json:"status"`
	Proof                    json.RawMessage     `json:"proof"`
	AuthorizationUntilUnixMS int64               `json:"authorization_until_unix_ms"`
}
type SupervisionBudget struct {
	RemainingMS     int64  `json:"remaining_ms"`
	ExpiresAtUnixMS int64  `json:"expires_at_unix_ms"`
	ExecutorEpoch   uint64 `json:"executor_epoch"`
	GatewayBootID   string `json:"gateway_boot_id"`
}
type Decision struct {
	Action      mission.Action     `json:"action"`
	Record      Record             `json:"record"`
	Supervision *SupervisionBudget `json:"supervision,omitempty"`
}
type Telemetry struct {
	PatrolArrival      *PatrolArrival `json:"patrol_arrival,omitempty"`
	SampleSimulationNS int64          `json:"sample_simulation_ns,omitempty"`
	// LocalFault can only remove permission through the authenticated executor
	// evaluation path. It never supplies an observation timestamp or authority.
	LocalFault    bool            `json:"local_fault,omitempty"`
	BootID        string          `json:"boot_id,omitempty"`
	Session       string          `json:"session,omitempty"`
	Inspection    *Inspection     `json:"inspection,omitempty"`
	Frame         world.FrameRef  `json:"frame"`
	SampleUnixMS  int64           `json:"sample_unix_ms"`
	Position      [3]float64      `json:"position"`
	Velocity      [3]float64      `json:"velocity"`
	ObstacleClear bool            `json:"obstacle_clear"`
	Energy        float64         `json:"energy"`
	Sensors       map[string]bool `json:"sensors"`
	// Deprecated untrusted hints. The store ignores both lease fields.
	SupervisionUntilUnixMS int64 `json:"supervision_until_unix_ms"`
	CommandUntilUnixMS     int64 `json:"command_until_unix_ms"`
	ReturnSafe             bool  `json:"return_safe"`
}
type Audit struct {
	Sequence  uint64          `json:"sequence"`
	UnixMS    int64           `json:"unix_ms"`
	TaskID    string          `json:"task_id"`
	Principal string          `json:"principal"`
	Event     string          `json:"event"`
	Status    mission.Status  `json:"status"`
	Proof     json.RawMessage `json:"proof,omitempty"`
}
