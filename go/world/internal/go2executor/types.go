// Package go2executor connects signed autonomous simulation tasks to a private
// planar Go2 bridge. It never treats an observational import report as authority.
package go2executor

import (
	"context"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/go2nav"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

// Adapter must enforce the fixed command deadline and one-use capture binding
// before publishing. The stock ROS receiver alone does not provide this API.
// Stop latches the session; it reports attempted zero publication, not a physical
// stopping guarantee. Session is called once, never as automatic error recovery.
type Adapter interface {
	Session(context.Context, tasking.Go2Binding) (State, error)
	Begin(context.Context, BeginRequest) error
	State(context.Context, string) (State, error)
	Command(context.Context, CommandRequest) error
	Stop(context.Context, StopRequest) error
}

type BeginRequest struct {
	Session       string `json:"session"`
	ExecutorEpoch uint64 `json:"executor_epoch"`
	TaskID        string `json:"task_id"`
	SpecDigest    string `json:"spec_digest"`
	BindingSHA256 string `json:"binding_sha256"`
}
type CommandRequest struct {
	Session             string  `json:"session"`
	Sequence            uint64  `json:"sequence"`
	ExecutorEpoch       uint64  `json:"executor_epoch"`
	TaskID              string  `json:"task_id"`
	SpecDigest          string  `json:"spec_digest"`
	CaptureSequence     uint64  `json:"capture_sequence"`
	BodyVX              float64 `json:"body_vx"`
	BodyVY              float64 `json:"body_vy"`
	YawRate             float64 `json:"yaw_rate"`
	NotAfterUnixMS      int64   `json:"not_after_unix_ms"`
	DecisionRemainingMS int64   `json:"decision_remaining_ms"`
}
type StopRequest struct {
	Session  string `json:"session"`
	Sequence uint64 `json:"sequence"`
}

// State is an adapter observation. Publisher identities and runtime assertions
// are lifecycle fences, not cryptographic origin or physical authority.
type State struct {
	Cloud              *CloudState        `json:"cloud,omitempty"`
	OdometryEpochAgeS  float64            `json:"odometry_epoch_age_s"`
	RollRad            float64            `json:"roll_rad"`
	PitchRad           float64            `json:"pitch_rad"`
	BridgeBootID       string             `json:"bridge_boot_id"`
	Session            string             `json:"session"`
	Sequence           uint64             `json:"sequence"`
	ExecutorEpoch      uint64             `json:"executor_epoch"`
	Binding            tasking.Go2Binding `json:"binding"`
	CaptureSequence    uint64             `json:"capture_sequence"`
	SampleUnixMS       int64              `json:"sample_unix_ms"`
	OdomPublisher      string             `json:"odom_publisher"`
	ScanPublisher      string             `json:"scan_publisher"`
	CommandPublisher   string             `json:"command_publisher"`
	Owner              string             `json:"owner"`
	AutoControl        bool               `json:"auto_control"`
	Synthetic          bool               `json:"synthetic"`
	Ready              bool               `json:"ready"`
	Fault              string             `json:"fault"`
	Pose               go2nav.Pose        `json:"-"`
	BodyVelocity       [3]float64         `json:"body_velocity"`
	PreviousCommand    [3]float64         `json:"previous_command"`
	Scan               go2nav.Scan        `json:"-"`
	OdomCapturedUnixNS int64              `json:"odom_captured_unix_ns"`
	ScanCapturedUnixNS int64              `json:"scan_captured_unix_ns"`
	PoseCovariance     [36]float64        `json:"pose_covariance"`
	TwistCovariance    [36]float64        `json:"twist_covariance"`
}

// CloudState preserves one source-associated tuple for a contained simulation
// experiment. Hashes and DDS identifiers are trace and lifecycle metadata, not
// authentication. The scan is supplied by State, never duplicated on the wire.
type CloudState struct {
	Model               string    `json:"model"`
	SourceDigest        string    `json:"source_digest"`
	Frame               string    `json:"frame"`
	PublisherGID        string    `json:"publisher_gid"`
	SerializedSHA256    string    `json:"serialized_sha256"`
	CapturedUnixNS      int64     `json:"captured_unix_ns"`
	Ranges              []float64 `json:"ranges"`
	Sequence            uint64    `json:"sequence"`
	ObservedEpoch       uint64    `json:"observed_epoch"`
	ScanSHA256          string    `json:"scan_sha256"`
	OdomSHA256          string    `json:"odom_sha256"`
	EpochSHA256         string    `json:"epoch_sha256"`
	MountSHA256         string    `json:"mount_sha256"`
	CloudMountSHA256    string    `json:"cloud_mount_sha256"`
	EpochPublisher      string    `json:"epoch_publisher"`
	MountPublisher      string    `json:"mount_publisher"`
	CloudMountPublisher string    `json:"cloud_mount_publisher"`
}

func (c *CloudState) observation(scan go2nav.Scan) go2nav.CloudObservation {
	return go2nav.CloudObservation{Model: c.Model, SourceDigest: c.SourceDigest,
		Frame: c.Frame, PublisherGID: c.PublisherGID, SerializedSHA256: c.SerializedSHA256,
		CapturedUnixNS: c.CapturedUnixNS, Ranges: c.Ranges, Scan: scan}
}

func copyCloud(c *CloudState) *CloudState {
	if c == nil {
		return nil
	}
	out := *c
	out.Ranges = append([]float64(nil), c.Ranges...)
	return &out
}

type Config struct {
	TaskID                     string
	Site                       string
	Binding                    tasking.Go2Binding
	AlignmentJSON, ProfileJSON []byte
	Now                        func() time.Time
	Monotonic                  func() time.Duration
}
