package missioncontrol

import (
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"math"
	"time"
)

// IncidentInspectionType is a retained review context, not a hazard or a
// clearance instruction. Its source attests the geometry-to-support binding.
const IncidentInspectionType = "wendy.incident.inspection.v1"

type IncidentSupport struct {
	Kind       string        `json:"kind"`
	SHA256     string        `json:"sha256"`
	Source     world.Source  `json:"source"`
	Version    world.Version `json:"version"`
	Captured   time.Time     `json:"captured_at"`
	ValidUntil time.Time     `json:"valid_until"`
}
type IncidentSelection struct {
	Schema         string            `json:"schema"`
	IncidentSHA256 string            `json:"incident_sha256"`
	Contract       string            `json:"contract_sha256"`
	InputDigest    string            `json:"input_digest"`
	State          string            `json:"state"`
	Position       [3]float64        `json:"position_m"`
	Uncertainty    float64           `json:"uncertainty_m"`
	Supports       []IncidentSupport `json:"supports"`
	ValidUntil     time.Time         `json:"valid_until"`
}

func IsTypedInspectionTrigger(kind string) bool {
	return kind == NativeInferenceType || kind == IncidentInspectionType || kind == RisingInspectionType
}

// IncidentTriggerTarget validates immutable historical review context. It does
// not claim current freshness, verify sensor truth, or authorize movement.
func IncidentTriggerTarget(c world.Component) ([3]float64, error) {
	var zero [3]float64
	bad := errors.New("invalid incident inspection context")
	if c.Key.Type != IncidentInspectionType || c.Key.Tenant == "" || c.Key.Site == "" || c.Key.Source.Validate() != nil || c.Version.Epoch == 0 || c.Version.Sequence == 0 || c.Frame.Validate() != nil || c.Tombstone || c.Timing.Validate() != nil || !c.Timing.ErrorKnown || c.Timing.ErrorBound != 0 || c.Timing.AgeHint != 0 || c.TTL <= 0 || c.TTL > 10*time.Second || c.Provenance.Validate() != nil || c.Provenance.Origin != world.Inferred || c.Provenance.AppVersion != "world-incident-inspection-v1" || c.Provenance.ModelID != "incident-inspection-selection" || c.Provenance.ModelVersion != "1" {
		return zero, bad
	}
	var p IncidentSelection
	if triggerJSON(c.Payload, &p, 12<<10) != nil || p.Schema != IncidentInspectionType || p.State != "consistent" || !hashPattern.MatchString(p.IncidentSHA256) || !hashPattern.MatchString(p.Contract) || !hashPattern.MatchString(p.InputDigest) || len(p.Supports) != 2 || p.Uncertainty <= 0 || p.Uncertainty > 10 || math.IsNaN(p.Uncertainty) || math.IsInf(p.Uncertainty, 0) {
		return zero, bad
	}
	fields, err := triggerFields(c.Payload, "schema incident_sha256 contract_sha256 input_digest state position_m uncertainty_m supports valid_until", "")
	if err != nil {
		return zero, bad
	}
	pos, err := triggerVector(fields["position_m"], 3)
	if err != nil {
		return zero, bad
	}
	for _, v := range pos {
		if math.Abs(v) > 10000 {
			return zero, bad
		}
	}
	seen := map[string]bool{}
	oldest := p.Supports[0].Captured
	latest := oldest
	deadline := p.Supports[0].ValidUntil
	for _, s := range p.Supports {
		if (s.Kind != "gas" && s.Kind != "visual") || seen[s.Kind] || !hashPattern.MatchString(s.SHA256) || s.Source.Validate() != nil || s.Version.Epoch == 0 || s.Version.Sequence == 0 || s.Captured.IsZero() || !s.ValidUntil.After(s.Captured) || s.ValidUntil.Sub(s.Captured) > 10*time.Second {
			return zero, bad
		}
		seen[s.Kind] = true
		if s.Captured.Before(oldest) {
			oldest = s.Captured
		}
		if s.Captured.After(latest) {
			latest = s.Captured
		}
		if s.ValidUntil.Before(deadline) {
			deadline = s.ValidUntil
		}
	}
	if p.Supports[0].SHA256 == p.Supports[1].SHA256 || p.Supports[0].Source == p.Supports[1].Source || latest.Sub(oldest) > 350*time.Millisecond || !p.ValidUntil.Equal(deadline) || !c.Timing.SampleTime.Equal(oldest) || !oldest.Add(c.TTL).Equal(deadline) {
		return zero, bad
	}
	if len(c.Provenance.Inputs) < 4 || len(c.Provenance.Inputs) > 16 {
		return zero, bad
	}
	ids := map[string]bool{}
	run := ""
	for _, input := range c.Provenance.Inputs {
		s := input.Simulation
		if input.Origin != world.Simulated || s == nil || s.RunID == "" || c.Timing.ClockDomain != "simulation:"+s.RunID || s.ClockDomain != c.Timing.ClockDomain || ids[input.ID] {
			return zero, bad
		}
		ids[input.ID] = true
		if run != "" && run != s.RunID {
			return zero, bad
		}
		run = s.RunID
	}
	if c.Key.Entity != run+"/incident-inspection" {
		return zero, bad
	}
	return p.Position, nil
}

// DecodeIncidentSelection exposes the validated compact context for review.
func DecodeIncidentSelection(c world.Component) (IncidentSelection, error) {
	var p IncidentSelection
	_, err := IncidentTriggerTarget(c)
	if err == nil {
		err = json.Unmarshal(c.Payload, &p)
	}
	return p, err
}
