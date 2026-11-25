package livecapture

import (
	"encoding/json"
	"errors"
	"math"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
)

const PlanarStateSchema = "wendy.sim.actuator.planar.v1"

// PlanarConfig is explicit selection of the shared-office inspection endpoint.
// It changes only actuator kinematics, not capture lineage or freshness checks.
type PlanarConfig struct {
	RobotID string
	FloorY  float64
}

func (p PlanarConfig) validate() error {
	if p.RobotID == "" || len(p.RobotID) > 128 || !finite(p.FloorY) {
		return errors.New("invalid planar capture configuration")
	}
	return nil
}
func (p PlanarConfig) check(s State) error {
	if p.validate() != nil || s.RobotID != p.RobotID || math.Abs(s.Position[1]-p.FloorY) > 1e-6 || math.Abs(s.Velocity[1]) > 1e-6 || math.Hypot(s.Velocity[0], s.Velocity[2]) > .501 || s.CommandedVelocityVector[1] != 0 {
		return errors.New("capture differs from planar robot")
	}
	for _, value := range s.CommandedVelocityVector {
		if !finite(value) {
			return errors.New("nonfinite planar command")
		}
	}
	return nil
}

// The planar wire uses a vector. The scalar magnitude is only the internal
// shared validation projection; marshaling retains the original vector form.
func (s *State) UnmarshalJSON(raw []byte) error {
	type plain State
	var value plain
	wire := struct {
		*plain
		Commanded json.RawMessage `json:"commanded_velocity_mps"`
	}{plain: &value}
	if err := authority.DecodeStrictLimit(raw, &wire, MaxResponseBytes); err != nil {
		return err
	}
	if value.SchemaVersion == PlanarStateSchema {
		var parts []json.RawMessage
		if json.Unmarshal(wire.Commanded, &parts) != nil || len(parts) != 3 {
			return errors.New("planar capture command requires three values")
		}
		for i, rawValue := range parts {
			var v *float64
			if json.Unmarshal(rawValue, &v) != nil || v == nil || !finite(*v) {
				return errors.New("invalid planar capture command")
			}
			value.CommandedVelocityVector[i] = *v
		}
		value.CommandedVelocity = math.Hypot(value.CommandedVelocityVector[0], value.CommandedVelocityVector[2])
	} else if len(wire.Commanded) != 0 {
		if err := json.Unmarshal(wire.Commanded, &value.CommandedVelocity); err != nil {
			return err
		}
	}
	*s = State(value)
	return nil
}

func (s State) MarshalJSON() ([]byte, error) {
	type plain State
	if s.SchemaVersion != PlanarStateSchema {
		return json.Marshal(plain(s))
	}
	return json.Marshal(struct {
		plain
		Commanded [3]float64 `json:"commanded_velocity_mps"`
	}{plain: plain(s), Commanded: s.CommandedVelocityVector})
}
