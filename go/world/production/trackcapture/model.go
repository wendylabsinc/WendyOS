// Package trackcapture adapts freshly verified original camera captures to
// anonymous tracking and the existing generic derived-result publication API.
package trackcapture

import (
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/pose"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/tracking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

const (
	ResultSchema     = "wendy.person.track.v1"
	AlgorithmID      = "camera-ankle-two-observation-tracking"
	AlgorithmVersion = "1"
	AppVersion       = "world-live-people-v1"
	DefaultLifetime  = 1250 * time.Millisecond
	MaxMembers       = 8
)

type Config struct {
	Tenant, Site                       string
	Tracking                           tracking.Config
	Projection                         pose.Config
	Sources                            map[string]world.Source
	ModelID, ModelVersion, ModelSHA256 string
}

// Verified must come from the caller's fresh signature/current-source verifier.
// The adapter checks binding to Envelope but does not establish authority itself.
// SupportOnly inputs are previous originals freshly reverified for motion.
type Input struct {
	Envelope    []byte
	Verified    serviceorigin.VerifiedRecord
	Image       []byte
	Poses       pose.ImageResult
	SupportOnly bool
}

type Camera struct {
	SensorID  string           `json:"sensor_id"`
	SampleID  string           `json:"sample_id"`
	Detected  int              `json:"detected"`
	Projected int              `json:"projected"`
	Rejected  []pose.Rejection `json:"rejected"`
}

// Member exposes geometric measurements and exact lineage, never pose pixels,
// bounding boxes, encoded media, appearance descriptors or arbitrary extensions.
type Member struct {
	Proof                  proofref.Reference   `json:"proof"`
	SensorID               string               `json:"sensor_id"`
	SampleID               string               `json:"sample_id"`
	CaptureSequence        uint64               `json:"capture_sequence"`
	MemberIndex            uint32               `json:"member_index"`
	SampleTime             time.Time            `json:"sample_time"`
	ValidUntil             time.Time            `json:"valid_until"`
	PhysicalUntil          time.Time            `json:"physical_until"`
	PositionXZ             [2]float64           `json:"position_xz"`
	Covariance             [4]float64           `json:"covariance"`
	Image                  sensing.EvidenceRef  `json:"image"`
	OriginalImage          *sensing.EvidenceRef `json:"original_image,omitempty"`
	ModelID                string               `json:"model_id"`
	ModelVersion           string               `json:"model_version"`
	ModelSHA256            string               `json:"model_sha256"`
	CalibrationRevision    string               `json:"calibration_revision"`
	CalibrationSHA256      string               `json:"calibration_sha256"`
	SceneBootID            string               `json:"scene_boot_id"`
	CapturedUnixNS         int64                `json:"captured_unix_ns"`
	SimulationAnchorUnixNS int64                `json:"simulation_anchor_unix_ns"`
}

type Payload struct {
	Schema             string         `json:"schema"`
	Algorithm          string         `json:"algorithm"`
	AlgorithmVersion   string         `json:"algorithm_version"`
	TrackID            string         `json:"track_id"`
	Revision           uint64         `json:"revision"`
	RunID              string         `json:"run_id"`
	ClockDomain        string         `json:"clock_domain"`
	Frame              world.FrameRef `json:"frame"`
	ObservedAt         time.Time      `json:"observed_at"`
	ValidUntil         time.Time      `json:"valid_until"`
	PositionXZ         [2]float64     `json:"position_xz"`
	Covariance         [4]float64     `json:"covariance"`
	Ambiguous          bool           `json:"ambiguous"`
	MotionKnown        bool           `json:"motion_known"`
	VelocityXZ         *[2]float64    `json:"velocity_xz,omitempty"`
	VelocityCovariance *[4]float64    `json:"velocity_covariance,omitempty"`
	PreviousObservedAt *time.Time     `json:"previous_observed_at,omitempty"`
	Current            []Member       `json:"current"`
	Previous           []Member       `json:"previous,omitempty"`
}

type candidate struct {
	payload Payload
	inputs  []resultrecord.ValidatedInput
}

type Snapshot struct {
	Tracks     []tracking.Track `json:"tracks"`
	Cameras    []Camera         `json:"cameras"`
	candidates map[string]candidate
	now        time.Time
	physical   time.Time
}
