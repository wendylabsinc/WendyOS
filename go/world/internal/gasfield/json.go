package gasfield

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"io"
)

// Reject omitted/null fields and truncated coordinate arrays at the wire
// boundary. Zero is a valid coordinate and timestamp only when explicit.
func decodeRequired(data []byte, out any, required []string, pairs ...string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, name := range required {
		if raw, ok := fields[name]; !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("missing %s", name)
		}
	}
	for _, name := range pairs {
		var pair []*float64
		if err := json.Unmarshal(fields[name], &pair); err != nil || len(pair) != 2 || pair[0] == nil || pair[1] == nil {
			return fmt.Errorf("%s requires two coordinates", name)
		}
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if !errors.Is(d.Decode(new(any)), io.EOF) {
		return errors.New("one JSON value required")
	}
	return nil
}
func (c *Config) UnmarshalJSON(data []byte) error {
	var envelope struct {
		Simulation json.RawMessage `json:"simulation"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	if err := explicitSimulation(envelope.Simulation); err != nil {
		return err
	}
	type plain Config
	return decodeRequired(data, (*plain)(c), []string{"site", "frame", "simulation", "sensors", "min_xz", "max_xz", "step_m", "background_ppm"}, "min_xz", "max_xz")
}
func (s *Sensor) UnmarshalJSON(data []byte) error {
	type plain Sensor
	return decodeRequired(data, (*plain)(s), []string{"id", "source", "position_xz", "calibration", "sensor_type", "position_uncertainty_m"}, "position_xz")
}
func (a *Assumptions) UnmarshalJSON(data []byte) error {
	type plain Assumptions
	return decodeRequired(data, (*plain)(a), []string{"revision", "wind_radians", "downwind_m", "upwind_m", "crosswind_m"})
}
func (o *Observation) UnmarshalJSON(data []byte) error {
	var envelope struct {
		Provenance struct {
			Simulation json.RawMessage `json:"simulation"`
		} `json:"provenance"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	if err := explicitSimulation(envelope.Provenance.Simulation); err != nil {
		return err
	}
	type plain Observation
	return decodeRequired(data, (*plain)(o), []string{"id", "sensor", "source", "version", "site", "frame", "provenance", "calibration", "sensor_type", "units", "species", "captured_s", "valid_until_s", "value_ppm", "uncertainty_ppm"})
}
func explicitSimulation(data []byte) error {
	var sim world.SimulationContext
	return decodeRequired(data, &sim, []string{"run_id", "scenario_version", "seed", "clock_domain"})
}
func (r *Request) UnmarshalJSON(data []byte) error {
	type plain Request
	return decodeRequired(data, (*plain)(r), []string{"now_s", "assumptions", "observations"})
}
