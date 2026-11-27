package missioncontrol

import (
	"encoding/json"
	"errors"
	"math"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// InspectionDestination separates a reviewed navigation point from its original
// observation. Target remains on the task; the original TriggerRef is unchanged.
// The map binding is checked by the planar executor, not a route-clearance claim.
type InspectionDestination struct {
	Version            int     `json:"version"`
	Mode               string  `json:"mode"`
	FloorY             float64 `json:"floor_y"`
	MaxOffsetM         float64 `json:"max_offset_m"`
	PlanarConfigSHA256 string  `json:"planar_config_sha256"`
}

func (d InspectionDestination) Validate() error {
	if d.Version != 1 || d.Mode != "planar_nearby" || !finite(d.FloorY) || !finite(d.MaxOffsetM) || d.MaxOffsetM <= 0 || d.MaxOffsetM > 5 || !hashPattern.MatchString(d.PlanarConfigSHA256) {
		return errors.New("invalid planar inspection destination")
	}
	return nil
}

func (d *InspectionDestination) UnmarshalJSON(raw []byte) error {
	if _, err := triggerFields(raw, "version mode floor_y max_offset_m planar_config_sha256", ""); err != nil {
		return err
	}
	type wire InspectionDestination
	var out wire
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	value := InspectionDestination(out)
	if err := value.Validate(); err != nil {
		return err
	}
	*d = value
	return nil
}

// EqualInspectionDestination compares the complete optional proposal/task
// descriptor. Omission and an explicit descriptor are different instructions.
func EqualInspectionDestination(a, b *InspectionDestination) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// ValidateNativeInspectionTarget preserves exact legacy targets when d is nil.
// With an explicit descriptor, only the horizontal displacement changes: a
// native hazard retains its original 3D position and a person retains its XZ.
// The caller separately checks the commissioned run/frame and signed task area.
func ValidateNativeInspectionTarget(c world.Component, target [3]float64, d *InspectionDestination) error {
	floor := target[1]
	if d != nil {
		if err := d.Validate(); err != nil {
			return err
		}
		floor = d.FloorY
	}
	observed, err := NativeTriggerTarget(c, floor)
	if err != nil {
		return err
	}
	if d == nil {
		if observed != target {
			return errors.New("mission target differs from native observation")
		}
		return nil
	}
	for _, v := range target {
		if !finite(v) {
			return errors.New("invalid planar inspection point")
		}
	}
	if target[1] != d.FloorY || math.Hypot(target[0]-observed[0], target[2]-observed[2]) > d.MaxOffsetM {
		return errors.New("inspection point exceeds reviewed floor or offset")
	}
	return nil
}
