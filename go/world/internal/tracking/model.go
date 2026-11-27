// Package tracking associates calibrated planar observations across sources and
// time. IDs are anonymous and local to one tracker instance, not person identities.
package tracking

import (
	"errors"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const Schema = "wendy.track.planar.v1"

// Detection is a projected measurement, never a scene object position. XZ uses
// the World right-handed Y-up site frame. Covariance is row-major in square metres.
// MemberIndex distinguishes multiple detections in the same original sample.
type Detection struct {
	Source              string                `json:"source"`
	SampleID            string                `json:"sample_id"`
	Version             world.Version         `json:"version"`
	MemberIndex         uint32                `json:"member_index"`
	SampleTime          time.Time             `json:"sample_time"`
	ValidUntil          time.Time             `json:"valid_until"`
	RunID               string                `json:"run_id"`
	ClockDomain         string                `json:"clock_domain"`
	Frame               world.FrameRef        `json:"frame"`
	PositionXZ          [2]float64            `json:"position_xz"`
	Covariance          [4]float64            `json:"covariance"`
	ModelID             string                `json:"model_id"`
	ModelVersion        string                `json:"model_version"`
	ModelSHA256         string                `json:"model_sha256"`
	CalibrationRevision string                `json:"calibration_revision,omitempty"`
	Evidence            []sensing.EvidenceRef `json:"evidence,omitempty"`
}

type Config struct {
	TrackerID, RunID, ClockDomain string
	Frame                         world.FrameRef
	MaxSkew                       time.Duration // Default 350 ms.
	MaxMiss                       time.Duration // Default 2 s; no coast beyond input expiry.
	MaxFusionDistanceM            float64       // Default .65 m, a hard cross-source gate.
	MaxSpeedMPS                   float64       // Default 3 m/s.
	GateSquared                   float64       // Default 9, squared Mahalanobis gate.
	ProcessNoiseMPS               float64       // Default .5 m/s position uncertainty growth.
	MaxDetections                 int           // Default 128 per Step, at most 512.
	MaxTracks                     int           // Default 128, at most 1024.
	MaxSources                    int           // Default 16, at most 64.
}

// Track is an estimate at Time. LastObserved and ValidUntil are not refreshed by
// coasting or duplicate input. Members are the original measurements supporting
// the latest observation update; Revision links successive estimates locally.
// ObservedPositionXZ/ObservedCovariance are the measurement-updated estimate at
// LastObserved. PositionXZ/Covariance additionally extrapolate that estimate to Time.
// Neither estimate is a new sensor observation; Members retain the original inputs.
type Track struct {
	ObservedPositionXZ [2]float64     `json:"observed_position_xz"`
	ObservedCovariance [4]float64     `json:"observed_covariance"`
	Schema             string         `json:"schema"`
	ID                 string         `json:"id"`
	Revision           uint64         `json:"revision"`
	RunID              string         `json:"run_id"`
	ClockDomain        string         `json:"clock_domain"`
	Frame              world.FrameRef `json:"frame"`
	State              string         `json:"state"` // observed or coasting
	Time               time.Time      `json:"time"`
	FirstSeen          time.Time      `json:"first_seen"`
	LastObserved       time.Time      `json:"last_observed"`
	ValidUntil         time.Time      `json:"valid_until"`
	PositionXZ         [2]float64     `json:"position_xz"`
	VelocityXZ         [2]float64     `json:"velocity_xz"`
	Covariance         [4]float64     `json:"covariance"`
	Ambiguous          bool           `json:"ambiguous"`
	Members            []Detection    `json:"members"`
}

func finite(v ...float64) bool {
	for _, x := range v {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return false
		}
	}
	return true
}

func (c *Config) defaults() error {
	if c.TrackerID == "" || len(c.TrackerID) > 80 || c.RunID == "" || len(c.RunID) > 160 || c.ClockDomain == "" || len(c.ClockDomain) > 256 || c.Frame.Validate() != nil {
		return errors.New("tracking requires an explicit tracker, run, clock and frame")
	}
	if c.MaxSkew == 0 {
		c.MaxSkew = 350 * time.Millisecond
	}
	if c.MaxMiss == 0 {
		c.MaxMiss = 2 * time.Second
	}
	if c.MaxFusionDistanceM == 0 {
		c.MaxFusionDistanceM = .65
	}
	if c.MaxSpeedMPS == 0 {
		c.MaxSpeedMPS = 3
	}
	if c.GateSquared == 0 {
		c.GateSquared = 9
	}
	if c.ProcessNoiseMPS == 0 {
		c.ProcessNoiseMPS = .5
	}
	if c.MaxDetections == 0 {
		c.MaxDetections = 128
	}
	if c.MaxTracks == 0 {
		c.MaxTracks = 128
	}
	if c.MaxSources == 0 {
		c.MaxSources = 16
	}
	if c.MaxSkew < 0 || c.MaxSkew > time.Second || c.MaxMiss <= 0 || c.MaxMiss > 10*time.Second ||
		!finite(c.MaxFusionDistanceM, c.MaxSpeedMPS, c.GateSquared, c.ProcessNoiseMPS) || c.MaxFusionDistanceM <= 0 || c.MaxFusionDistanceM > 10 || c.MaxSpeedMPS <= 0 || c.MaxSpeedMPS > 50 || c.GateSquared <= 0 || c.GateSquared > 100 || c.ProcessNoiseMPS <= 0 || c.ProcessNoiseMPS > 50 ||
		c.MaxDetections < 1 || c.MaxDetections > 512 || c.MaxTracks < 1 || c.MaxTracks > 1024 || c.MaxSources < 1 || c.MaxSources > 64 {
		return errors.New("tracking configuration outside supported bounds")
	}
	return nil
}

func (d Detection) validate(c Config) error {
	if d.Source == "" || len(d.Source) > 256 || d.SampleID == "" || len(d.SampleID) > 512 || d.Version.Epoch == 0 || d.Version.Sequence == 0 ||
		d.RunID != c.RunID || d.ClockDomain != c.ClockDomain || d.Frame != c.Frame || d.SampleTime.IsZero() || !d.ValidUntil.After(d.SampleTime) ||
		d.ModelID == "" || len(d.ModelID) > 256 || d.ModelVersion == "" || len(d.ModelVersion) > 256 || !sensing.DigestValid(d.ModelSHA256) || !finite(d.PositionXZ[:]...) || !positive(d.Covariance) || len(d.Evidence) > 8 {
		return errors.New("invalid projected detection or mismatched tracking scope")
	}
	if len(d.CalibrationRevision) > 256 {
		return errors.New("calibration revision too long")
	}
	for _, e := range d.Evidence {
		if err := e.Validate(); err != nil {
			return err
		}
	}
	return nil
}
