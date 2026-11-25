// Package world provides validation and in-memory replica primitives for the
// private Wendy World prototype. It does not authenticate or sign records.
package world

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"
)

// GeographicAnchor identifies the geographic origin of a local site frame.
// AltitudeDatum must be explicit; this package does not transform coordinates.
type GeographicAnchor struct {
	LatitudeDegrees   float64 `json:"latitude_degrees"`
	LongitudeDegrees  float64 `json:"longitude_degrees"`
	AltitudeMetres    float64 `json:"altitude_metres"`
	AltitudeDatum     string  `json:"altitude_datum"`
	UncertaintyMetres float64 `json:"uncertainty_metres"`
}

func (a GeographicAnchor) Validate() error {
	if !finite(a.LatitudeDegrees, a.LongitudeDegrees, a.AltitudeMetres, a.UncertaintyMetres) || math.Abs(a.LatitudeDegrees) > 90 || math.Abs(a.LongitudeDegrees) > 180 || a.UncertaintyMetres < 0 || a.AltitudeDatum == "" {
		return errors.New("invalid geographic anchor or missing altitude datum")
	}
	return nil
}

type FrameRef struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
}

// AngularError bounds the relative rotation between a nominal and an actual
// basis. Unknown is distinct from a known zero angle. It is an assertion from
// the placement publisher, not a measurement or survey certification.
type AngularError struct {
	Known           bool     `json:"known"`
	MaxAngleRadians *float64 `json:"max_angle_radians,omitempty"`
}

func (e AngularError) Validate() error {
	if !e.Known {
		if e.MaxAngleRadians != nil {
			return errors.New("unknown angular error cannot carry a bound")
		}
		return nil
	}
	if e.MaxAngleRadians == nil || !finite(*e.MaxAngleRadians) || *e.MaxAngleRadians < 0 || *e.MaxAngleRadians > math.Pi {
		return errors.New("known angular error requires a finite bound from zero to pi radians")
	}
	return nil
}

func (f FrameRef) Validate() error {
	if f.ID == "" || f.Revision == "" {
		return errors.New("frame ID and revision are required")
	}
	return nil
}

// LocalFrame describes a placement within a parent frame. Translation uses
// metres, right-handed Y-up axes; RotationXYZW is a unit quaternion.
type LocalFrame struct {
	Ref               FrameRef      `json:"ref"`
	Parent            FrameRef      `json:"parent"`
	Translation       [3]float64    `json:"translation"`
	RotationXYZW      [4]float64    `json:"rotation_xyzw"`
	ValidFrom         time.Time     `json:"valid_from"`
	ValidUntil        time.Time     `json:"valid_until"`
	UncertaintyMetres float64       `json:"uncertainty_metres"`
	RotationError     *AngularError `json:"rotation_error,omitempty"`
}

func (f LocalFrame) Validate() error {
	if f.RotationError != nil {
		if err := f.RotationError.Validate(); err != nil {
			return err
		}
	}
	if err := f.Ref.Validate(); err != nil {
		return err
	}
	if err := f.Parent.Validate(); err != nil {
		return err
	}
	if f.Ref.ID == f.Parent.ID {
		return errors.New("frame cannot parent itself")
	}
	if !finite(f.Translation[:]...) || !finite(f.RotationXYZW[:]...) || !finite(f.UncertaintyMetres) || f.UncertaintyMetres < 0 {
		return errors.New("invalid frame transform")
	}
	var norm float64
	for _, v := range f.RotationXYZW {
		norm += v * v
	}
	if math.Abs(norm-1) > 1e-6 {
		return errors.New("rotation must be a unit quaternion")
	}
	if f.ValidFrom.IsZero() || !f.ValidUntil.After(f.ValidFrom) {
		return errors.New("frame needs a bounded validity interval")
	}
	return nil
}

type Source struct {
	Device  string `json:"device"`
	App     string `json:"app"`
	Service string `json:"service"`
	Stream  string `json:"stream"`
}

func (s Source) Validate() error {
	if s.Device == "" || s.App == "" || s.Service == "" || s.Stream == "" {
		return errors.New("complete device/app/service/stream source required")
	}
	return nil
}

type Origin string

const (
	Measured  Origin = "measured"
	Simulated Origin = "simulated"
	Replayed  Origin = "replayed"
	Inferred  Origin = "inferred"
	Authored  Origin = "authored"
)

type SimulationContext struct {
	RunID           string `json:"run_id"`
	ScenarioVersion string `json:"scenario_version"`
	Seed            uint64 `json:"seed"`
	ClockDomain     string `json:"clock_domain"`
}

// Input records flattened input ancestry. Inferred input references must first
// be resolved to their measured/simulated/replayed leaves by the caller.
type Input struct {
	ID                  string             `json:"id"`
	Origin              Origin             `json:"origin"`
	Simulation          *SimulationContext `json:"simulation,omitempty"`
	OriginalCaptureTime *time.Time         `json:"original_capture_time,omitempty"`
	OriginalClockDomain string             `json:"original_clock_domain,omitempty"`
}
type Provenance struct {
	Origin              Origin             `json:"origin"`
	AppVersion          string             `json:"app_version"`
	ModelID             string             `json:"model_id,omitempty"`
	ModelVersion        string             `json:"model_version,omitempty"`
	Simulation          *SimulationContext `json:"simulation,omitempty"`
	OriginalCaptureTime *time.Time         `json:"original_capture_time,omitempty"`
	OriginalClockDomain string             `json:"original_clock_domain,omitempty"`
	Inputs              []Input            `json:"inputs,omitempty"`
}

func validateOrigin(origin Origin, sim *SimulationContext, capture *time.Time, domain string) error {
	switch origin {
	case Measured:
		if sim != nil || capture != nil || domain != "" {
			return errors.New("measured input cannot carry replay/simulation metadata")
		}
	case Simulated:
		if sim == nil || sim.RunID == "" || sim.ScenarioVersion == "" || sim.ClockDomain == "" || capture != nil || domain != "" {
			return errors.New("simulation requires run, scenario and clock domain")
		}
	case Replayed:
		if capture == nil || capture.IsZero() || domain == "" {
			return errors.New("replay requires original capture time and clock domain")
		}
		if sim != nil && (sim.RunID == "" || sim.ScenarioVersion == "" || sim.ClockDomain == "") {
			return errors.New("invalid replayed simulation context")
		}
	default:
		return errors.New("input origin must be measured, simulated or replayed")
	}
	return nil
}
func (p Provenance) Validate() error {
	if p.AppVersion == "" {
		return errors.New("application version is required")
	}
	if p.Origin == Authored {
		// Human statements are not sensor measurements or model inferences.
		// This first authored format is restricted to simulation contexts.
		if err := validateOrigin(Simulated, p.Simulation, p.OriginalCaptureTime, p.OriginalClockDomain); err != nil {
			return err
		}
		if p.ModelID != "" || p.ModelVersion != "" || len(p.Inputs) != 0 {
			return errors.New("authored statements cannot carry model or input ancestry")
		}
	} else if p.Origin == Inferred {
		if p.ModelID == "" || p.ModelVersion == "" || len(p.Inputs) == 0 {
			return errors.New("inference requires model version and inputs")
		}
		if p.Simulation != nil || p.OriginalCaptureTime != nil || p.OriginalClockDomain != "" {
			return errors.New("inference carries origin metadata on its inputs")
		}
	} else {
		if err := validateOrigin(p.Origin, p.Simulation, p.OriginalCaptureTime, p.OriginalClockDomain); err != nil {
			return err
		}
		if len(p.Inputs) != 0 {
			return errors.New("only inferred claims carry input ancestry")
		}
	}
	seen := map[string]bool{}
	for _, in := range p.Inputs {
		if in.ID == "" || seen[in.ID] {
			return errors.New("input IDs must be nonempty and unique")
		}
		seen[in.ID] = true
		if err := validateOrigin(in.Origin, in.Simulation, in.OriginalCaptureTime, in.OriginalClockDomain); err != nil {
			return err
		}
	}
	return nil
}
func (p Provenance) Synthetic() bool {
	if p.Origin == Simulated || p.Origin == Replayed || p.Origin == Authored {
		return true
	}
	for _, in := range p.Inputs {
		if in.Origin == Simulated || in.Origin == Replayed {
			return true
		}
	}
	return false
}

type Timing struct {
	SampleTime  time.Time `json:"sample_time"`
	ClockDomain string    `json:"clock_domain"`
	// ErrorBound is meaningful only when ErrorKnown is true. A known zero bound
	// is distinct from an unknown bound; uncertainty is never silently zeroed.
	ErrorKnown bool          `json:"error_known"`
	ErrorBound time.Duration `json:"error_bound_ns"`
	AgeHint    time.Duration `json:"age_hint_ns"`
}

func (t Timing) Validate() error {
	if t.SampleTime.IsZero() || t.ClockDomain == "" || t.ErrorBound < 0 || t.AgeHint < 0 {
		return errors.New("invalid timing")
	}
	return nil
}

type ScalarSample struct {
	Value               float64 `json:"value"`
	Unit                string  `json:"unit"`
	SensorType          string  `json:"sensor_type"`
	CalibrationRevision string  `json:"calibration_revision"`
	Uncertainty         float64 `json:"uncertainty"`
}
type MediaSample struct {
	SHA256              string        `json:"sha256"`
	ContentType         string        `json:"content_type"`
	Duration            time.Duration `json:"duration_ns"`
	CalibrationRevision string        `json:"calibration_revision"`
}
type DerivedClaim struct {
	Label      string  `json:"label"`
	Confidence float64 `json:"confidence"`
}

// Observation is a typed, immutable measurement or derived claim. Exactly one
// body is required. Media is referenced by digest; bytes are stored elsewhere.
type Observation struct {
	ID         string        `json:"id"`
	Tenant     string        `json:"tenant"`
	Site       string        `json:"site"`
	Source     Source        `json:"source"`
	Frame      FrameRef      `json:"frame"`
	Timing     Timing        `json:"timing"`
	Provenance Provenance    `json:"provenance"`
	Scalar     *ScalarSample `json:"scalar,omitempty"`
	Image      *MediaSample  `json:"image,omitempty"`
	Audio      *MediaSample  `json:"audio,omitempty"`
	Claim      *DerivedClaim `json:"claim,omitempty"`
}

func (o Observation) Validate() error {
	if o.ID == "" || o.Tenant == "" || o.Site == "" {
		return errors.New("observation ID, tenant and site required")
	}
	if err := o.Source.Validate(); err != nil {
		return err
	}
	if err := o.Frame.Validate(); err != nil {
		return err
	}
	if err := o.Timing.Validate(); err != nil {
		return err
	}
	if err := o.Provenance.Validate(); err != nil {
		return err
	}
	if o.Provenance.Simulation != nil && o.Provenance.Origin == Simulated && o.Provenance.Simulation.ClockDomain != o.Timing.ClockDomain {
		return errors.New("simulation clock domain mismatch")
	}
	count := 0
	if s := o.Scalar; s != nil {
		count++
		if !finite(s.Value, s.Uncertainty) || s.Uncertainty < 0 || s.Unit == "" || s.SensorType == "" || s.CalibrationRevision == "" {
			return errors.New("invalid scalar measurement")
		}
	}
	for i, m := range []*MediaSample{o.Image, o.Audio} {
		if m == nil {
			continue
		}
		count++
		hash, err := hex.DecodeString(m.SHA256)
		if err != nil || len(hash) != 32 || m.Duration < 0 || m.CalibrationRevision == "" {
			return errors.New("invalid media sample")
		}
		prefix := "image/"
		if i == 1 {
			prefix = "audio/"
		}
		if len(m.ContentType) <= len(prefix) || m.ContentType[:len(prefix)] != prefix {
			return fmt.Errorf("media content type must start with %s", prefix)
		}
	}
	if c := o.Claim; c != nil {
		count++
		if c.Label == "" || !finite(c.Confidence) || c.Confidence < 0 || c.Confidence > 1 || o.Provenance.Origin != Inferred {
			return errors.New("invalid inferred claim")
		}
	}
	if count != 1 {
		return errors.New("exactly one observation body required")
	}
	return nil
}
func finite(values ...float64) bool {
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}
