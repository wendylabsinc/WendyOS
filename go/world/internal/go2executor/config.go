package go2executor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/go2nav"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type Alignment struct {
	Schema               string         `json:"schema"`
	SourceFrame          string         `json:"source_frame"`
	TargetFrame          world.FrameRef `json:"target_frame"`
	TranslationXYM       [2]float64     `json:"translation_xy_m"`
	YawRad               float64        `json:"yaw_rad"`
	ReferencePoint       string         `json:"reference_point"`
	Synthetic            bool           `json:"synthetic"`
	PositionUncertaintyM float64        `json:"position_uncertainty_m"`
	YawUncertaintyRad    float64        `json:"yaw_uncertainty_rad"`
}

const SupportedOdometrySourceDigest = "sha256:2869d018ae55e4c83318329bb20cc670e16f6f62838989bb5192829002af8d2b"

// CloudExperiment is an explicit, digest-bound simulator selection. It does
// not qualify finite lidar rays as physical clearance evidence.
type CloudExperiment struct {
	Model               string `json:"model"`
	Scope               string `json:"scope"`
	CollectorDurationMS int64  `json:"collector_duration_ms"`
}

type Profile struct {
	CloudExperiment               *CloudExperiment          `json:"cloud_experiment,omitempty"`
	MaxMeasuredLateralMPS         *float64                  `json:"max_measured_lateral_mps,omitempty"`
	TiltedScan                    *go2nav.TiltedScanProfile `json:"tilted_scan,omitempty"`
	MotionEnvelope                string                    `json:"motion_envelope"`
	OdometryMaxEpochAgeS          float64                   `json:"odometry_max_epoch_age_s"`
	OdometryIntegrationAllowanceM float64                   `json:"odometry_integration_allowance_m"`
	ClockErrorMS                  int64                     `json:"clock_error_ms"`
	MaxAbsRollRad                 float64                   `json:"max_abs_roll_rad"`
	MaxAbsPitchRad                float64                   `json:"max_abs_pitch_rad"`
	VerticalObstacles             bool                      `json:"vertical_obstacles"`
	Schema                        string                    `json:"schema"`
	FlatFloor                     bool                      `json:"flat_floor"`
	EnergyObserved                bool                      `json:"energy_observed"`
	HalfLengthM                   float64                   `json:"half_length_m"`
	HalfWidthM                    float64                   `json:"half_width_m"`
	ClearanceM                    float64                   `json:"clearance_m"`
	MaxPositionUncertaintyM       float64                   `json:"max_position_uncertainty_m"`
	MaxYawUncertaintyRad          float64                   `json:"max_yaw_uncertainty_rad"`
	MaxForwardMPS                 float64                   `json:"max_forward_mps"`
	MaxYawRateRPS                 float64                   `json:"max_yaw_rate_rps"`
	ObservationMaxAgeMS           int64                     `json:"observation_max_age_ms"`
	SourceSkewMaxMS               int64                     `json:"source_skew_max_ms"`
	DecisionMaxMS                 int64                     `json:"decision_max_ms"`
	ExpiryLeadMS                  int64                     `json:"expiry_lead_ms"`
	HorizonS                      float64                   `json:"horizon_s"`
	ExtraStopM                    float64                   `json:"extra_stop_m"`
	ExtraStopRad                  float64                   `json:"extra_stop_rad"`
}

func SHA256(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }
func finite(v float64) bool     { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func same(a, b any) bool {
	x, e := json.Marshal(a)
	y, f := json.Marshal(b)
	return e == nil && f == nil && bytes.Equal(x, y)
}

// decodeConfig requires every typed field, including false and zero values.
// Hashes bind original file bytes; neither decoder rewrites signed configuration.
func decodeConfig(raw []byte, out any) error {
	if len(raw) == 0 || len(raw) > 16<<10 {
		return errors.New("configuration exceeds bound")
	}
	if err := authority.DecodeStrict(raw, out); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := authority.DecodeStrict(raw, &fields); err != nil {
		return err
	}
	canonical, _ := json.Marshal(out)
	var expected map[string]json.RawMessage
	_ = json.Unmarshal(canonical, &expected)
	if len(fields) != len(expected) {
		return errors.New("configuration fields missing")
	}
	for k := range expected {
		if len(fields[k]) == 0 || bytes.Equal(bytes.TrimSpace(fields[k]), []byte("null")) {
			return errors.New("configuration field absent or null")
		}
	}
	return nil
}
func DecodeConfiguration(cfg Config) (Alignment, Profile, error) {
	var a Alignment
	var p Profile
	if SHA256(cfg.AlignmentJSON) != cfg.Binding.AlignmentSHA256 || SHA256(cfg.ProfileJSON) != cfg.Binding.ProfileSHA256 {
		return a, p, errors.New("commissioned configuration digest differs")
	}
	if err := decodeConfig(cfg.AlignmentJSON, &a); err != nil {
		return a, p, err
	}
	if err := decodeConfig(cfg.ProfileJSON, &p); err != nil {
		return a, p, err
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(cfg.AlignmentJSON, &fields)
	var translation []json.RawMessage
	if json.Unmarshal(fields["translation_xy_m"], &translation) != nil || len(translation) != 2 {
		return a, p, errors.New("alignment translation must have exactly two coordinates")
	}
	for _, v := range translation {
		var n *float64
		if json.Unmarshal(v, &n) != nil || n == nil {
			return a, p, errors.New("alignment coordinate invalid")
		}
	}
	if a.Schema != "wendy.world.go2-alignment.v1" || a.SourceFrame != "odom" || a.TargetFrame != cfg.Binding.Frame || a.TargetFrame.Validate() != nil || a.ReferencePoint != "base_footprint" || !a.Synthetic || a.TranslationXYM != [2]float64{} || a.YawRad != 0 {
		return a, p, errors.New("only explicit synthetic identity odometry alignment is supported")
	}
	if cfg.Binding.SourceDigest != SupportedOdometrySourceDigest || !finite(p.OdometryMaxEpochAgeS) || p.OdometryMaxEpochAgeS <= 0 || p.OdometryMaxEpochAgeS > 120 || p.OdometryIntegrationAllowanceM != .05 {
		return a, p, errors.New("unsupported odometry source, epoch age or integration allowance")
	}
	if p.MotionEnvelope != go2nav.IsotropicV1 && p.MotionEnvelope != go2nav.DirectionalV1 && p.MotionEnvelope != go2nav.IsotropicGaitV1 {
		return a, p, errors.New("explicit supported motion envelope required")
	}
	if p.MotionEnvelope == go2nav.IsotropicGaitV1 {
		if p.Schema != "wendy.world.go2-profile.v2" || p.MaxMeasuredLateralMPS == nil || !finite(*p.MaxMeasuredLateralMPS) || *p.MaxMeasuredLateralMPS <= 0 || *p.MaxMeasuredLateralMPS > .2 {
			return a, p, errors.New("gait profile requires an explicit v2 measured lateral bound")
		}
	} else if p.MaxMeasuredLateralMPS != nil && (!finite(*p.MaxMeasuredLateralMPS) || *p.MaxMeasuredLateralMPS != 0) {
		return a, p, errors.New("legacy profile cannot override its measured lateral bound")
	}
	if !p.FlatFloor || p.EnergyObserved || p.ClockErrorMS != 50 || !p.VerticalObstacles {
		return a, p, errors.New("explicit synthetic flat-floor profile with unobserved energy required")
	}
	switch p.Schema {
	case "wendy.world.go2-profile.v1":
		if p.TiltedScan != nil || p.MaxAbsRollRad != .05 || p.MaxAbsPitchRad != .05 {
			return a, p, errors.New("legacy profile cannot select tilted scan")
		}
	case "wendy.world.go2-profile.v2":
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(cfg.ProfileJSON, &raw)
		if p.TiltedScan == nil || decodeConfig(raw["tilted_scan"], p.TiltedScan) != nil || p.TiltedScan.Validate() != nil || !finite(p.MaxAbsRollRad) || !finite(p.MaxAbsPitchRad) || p.MaxAbsRollRad <= 0 || p.MaxAbsRollRad > .25 || p.MaxAbsPitchRad <= 0 || p.MaxAbsPitchRad > .25 {
			return a, p, errors.New("explicit bounded tilted scan profile required")
		}
	default:
		return a, p, errors.New("unsupported simulation operating profile version")
	}
	if p.CloudExperiment != nil {
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(cfg.ProfileJSON, &raw)
		x := p.CloudExperiment
		if decodeConfig(raw["cloud_experiment"], x) != nil || x.Model != go2nav.CloudModelV1 ||
			x.Scope != "contained-simulation" || x.CollectorDurationMS < 100 || x.CollectorDurationMS > 60000 ||
			p.Schema != "wendy.world.go2-profile.v2" || p.TiltedScan == nil || p.MotionEnvelope != go2nav.IsotropicGaitV1 {
			return a, p, errors.New("explicit bounded contained-simulation cloud profile required")
		}
	}
	for _, v := range []float64{a.PositionUncertaintyM, a.YawUncertaintyRad, p.HalfLengthM, p.HalfWidthM, p.ClearanceM, p.MaxPositionUncertaintyM, p.MaxYawUncertaintyRad, p.MaxForwardMPS, p.MaxYawRateRPS, p.HorizonS, p.ExtraStopM, p.ExtraStopRad} {
		if !finite(v) {
			return a, p, errors.New("nonfinite simulation operating profile")
		}
	}
	if a.PositionUncertaintyM < 0 || a.YawUncertaintyRad < 0 || a.PositionUncertaintyM > p.MaxPositionUncertaintyM || a.YawUncertaintyRad > p.MaxYawUncertaintyRad || p.MaxPositionUncertaintyM <= 0 || p.MaxPositionUncertaintyM > .5 || p.MaxYawUncertaintyRad <= 0 || p.MaxYawUncertaintyRad > .5 || p.HalfLengthM < .425 || p.HalfLengthM > .6 || p.HalfWidthM < .30 || p.HalfWidthM > .4 || p.ClearanceM < .02 || p.ClearanceM > .3 || p.MaxForwardMPS <= 0 || p.MaxForwardMPS > .35 || p.MaxYawRateRPS <= 0 || p.MaxYawRateRPS > .5 {
		return a, p, errors.New("unsupported footprint, speed or uncertainty bound")
	}
	if p.ObservationMaxAgeMS <= 0 || p.ObservationMaxAgeMS > 250 || p.SourceSkewMaxMS < 0 || p.SourceSkewMaxMS > 50 || p.DecisionMaxMS <= 0 || p.DecisionMaxMS > 500 || p.ExpiryLeadMS < 450 || p.ExpiryLeadMS > 1000 || p.HorizonS < 1 || p.HorizonS > 2 || p.ExtraStopM < .05 || p.ExtraStopM > .5 || p.ExtraStopRad < .05 || p.ExtraStopRad > .5 {
		return a, p, errors.New("unsupported freshness or conditional stopping envelope")
	}
	return a, p, nil
}
func (p Profile) navigation() go2nav.Profile {
	return go2nav.Profile{TiltedScan: p.TiltedScan, MaxAbsRollRad: p.MaxAbsRollRad, MaxAbsPitchRad: p.MaxAbsPitchRad, MotionEnvelope: p.MotionEnvelope, FlatFloor: p.FlatFloor, HalfLengthM: p.HalfLengthM, HalfWidthM: p.HalfWidthM, ClearanceM: p.ClearanceM, MaxPositionUncertaintyM: p.MaxPositionUncertaintyM, MaxYawUncertaintyRad: p.MaxYawUncertaintyRad}
}

func (p Profile) measuredLateralLimit() float64 {
	if p.MotionEnvelope == go2nav.IsotropicGaitV1 && p.MaxMeasuredLateralMPS != nil {
		return *p.MaxMeasuredLateralMPS
	}
	return .05
}
