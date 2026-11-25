package tasking

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"math"
	"time"
)

// TriggerRef preserves the operator-selected observation even after a latest-value
// register changes. The operator signs this context; it is not an origin proof.
type TriggerRef struct {
	SHA256    string          `json:"sha256"`
	Component world.Component `json:"component"`
}

func TriggerFromComponent(c world.Component) (*TriggerRef, error) {
	c.Timing.AgeHint = 0
	b, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	t := &TriggerRef{hex.EncodeToString(sum[:]), c}
	if err = t.Validate(c.Key.Site, c.Frame); err != nil {
		return nil, err
	}
	return t, nil
}
func (t TriggerRef) Validate(site string, frame world.FrameRef) error {
	c := t.Component
	if c.Key.Type != "wendy.hazard.v1" && !missioncontrol.IsTypedInspectionTrigger(c.Key.Type) || c.Key.Site != site || c.Key.Tenant == "" || c.Key.Entity == "" || c.Frame != frame || c.Tombstone || c.Version.Epoch == 0 || c.Version.Sequence == 0 || c.Timing.AgeHint != 0 || c.TTL <= 0 || !json.Valid(c.Payload) || !sensing.DigestValid(t.SHA256) {
		return errors.New("invalid trigger context")
	}
	if err := c.Key.Source.Validate(); err != nil {
		return err
	}
	if err := c.Timing.Validate(); err != nil {
		return err
	}
	if err := c.Provenance.Validate(); err != nil {
		return err
	}
	if c.Provenance.Origin != world.Inferred || !c.Provenance.Synthetic() {
		return errors.New("inspection trigger must retain inferred synthetic ancestry")
	}
	if missioncontrol.IsTypedInspectionTrigger(c.Key.Type) {
		if _, err := missioncontrol.NativeTriggerTarget(c, 0); err != nil {
			return err
		}
	}
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != t.SHA256 {
		return errors.New("trigger digest mismatch")
	}
	return nil
}

type Inspection struct {
	TaskID                   string           `json:"task_id"`
	Trigger                  *TriggerRef      `json:"trigger,omitempty"`
	RunID                    string           `json:"run_id"`
	BootID                   string           `json:"boot_id"`
	Session                  string           `json:"session"`
	Sequence                 uint64           `json:"sequence"`
	SettledSinceSimulationNS int64            `json:"settled_since_simulation_ns"`
	SettledSinceUnixMS       int64            `json:"settled_since_unix_ms"`
	CapturedUnixMS           int64            `json:"captured_unix_ms"`
	SimulationTimeNS         int64            `json:"simulation_time_ns"`
	Frame                    world.FrameRef   `json:"frame"`
	Position                 [3]float64       `json:"position_m"`
	Velocity                 [3]float64       `json:"velocity_mps"`
	Sensors                  map[string]bool  `json:"sensors"`
	ObstacleClear            bool             `json:"obstacle_clear"`
	Energy                   float64          `json:"energy"`
	Samples                  []sensing.Sample `json:"samples"`
}

func (i *Inspection) UnmarshalJSON(raw []byte) error {
	type plain Inspection
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, key := range []string{"task_id", "run_id", "boot_id", "session", "sequence", "captured_unix_ms", "settled_since_unix_ms", "settled_since_simulation_ns", "simulation_time_ns", "frame", "position_m", "velocity_mps", "sensors", "obstacle_clear", "energy", "samples"} {
		b, ok := fields[key]
		if !ok || bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
			return fmt.Errorf("missing inspection field %s", key)
		}
	}
	for _, key := range []string{"position_m", "velocity_mps"} {
		var x []*float64
		if err := json.Unmarshal(fields[key], &x); err != nil || len(x) != 3 {
			return errors.New("inspection vector must have three numbers")
		}
		for _, v := range x {
			if v == nil {
				return errors.New("null inspection coordinate")
			}
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	var out plain
	if err := d.Decode(&out); err != nil {
		return err
	}
	var samples []json.RawMessage
	if err := json.Unmarshal(fields["samples"], &samples); err != nil {
		return err
	}
	for j, raw := range samples {
		sample, err := sensing.Decode(raw)
		if err != nil {
			return err
		}
		out.Samples[j] = sample
	}
	*i = Inspection(out)
	return nil
}

func validateInspection(spec Spec, t Telemetry, wall int64, maxAge time.Duration) error {
	if maxAge > 500*time.Millisecond {
		maxAge = 500 * time.Millisecond
	}
	i := t.Inspection
	if i == nil || i.TaskID != spec.ID || i.RunID == "" || i.BootID == "" || i.Session == "" || i.BootID != t.BootID || i.Session != t.Session || i.Sequence == 0 || i.SettledSinceUnixMS <= 0 || i.SettledSinceUnixMS > i.CapturedUnixMS || i.SettledSinceSimulationNS < 0 || i.SettledSinceSimulationNS > i.SimulationTimeNS || i.SimulationTimeNS < 0 || i.CapturedUnixMS <= 0 || i.CapturedUnixMS > wall || wall-i.CapturedUnixMS > maxAge.Milliseconds() || i.CapturedUnixMS > t.SampleUnixMS || i.Frame != spec.Frame || !i.ObstacleClear || !finite(i.Energy) || i.Energy < spec.MinimumEnergy || i.Energy > 1 {
		return errors.New("invalid or stale inspection capture")
	}
	a, _ := json.Marshal(spec.Trigger)
	b, _ := json.Marshal(i.Trigger)
	if string(a) != string(b) {
		return errors.New("inspection changed signed trigger")
	}
	tele := Telemetry{Position: i.Position, Velocity: i.Velocity}
	if !inside(spec.Area, i.Position) || !reached(spec, tele) {
		return errors.New("inspection was not captured at settled target")
	}
	for j, v := range i.Position {
		if !finite(v) || !finite(i.Velocity[j]) || math.Abs(v-t.Position[j]) > .002 {
			return errors.New("inspection differs from current measured pose")
		}
	}
	for _, name := range spec.RequiredSensors {
		if !i.Sensors[name] {
			return errors.New("inspection required sensor unavailable")
		}
	}
	if len(i.Samples) != 4 {
		return errors.New("inspection needs both cameras, gas and audio")
	}
	expected := map[string]string{"camera-a": "camera", "camera-b": "camera", "gas-1": "gas", "audio-1": "audio"}
	packageID, scenario := "", ""
	var seed uint64
	for _, sample := range i.Samples {
		if err := sample.Validate(); err != nil {
			return err
		}
		kind, ok := expected[sample.SensorID]
		if !ok || kind != sample.Kind || sample.RunID != i.RunID || sample.Sequence != i.Sequence || sample.SampleNS != i.SimulationTimeNS || sample.SiteID != spec.Site || sample.Frame != spec.Frame {
			return errors.New("inspection samples do not belong to captured scene")
		}
		delete(expected, sample.SensorID)
		if packageID == "" {
			packageID, scenario, seed = sample.PackageSHA256, sample.ScenarioVersion, sample.Seed
		} else if packageID != sample.PackageSHA256 || scenario != sample.ScenarioVersion || seed != sample.Seed {
			return errors.New("inspection mixed scene lineage")
		}
		age := time.Duration(wall-i.CapturedUnixMS) * time.Millisecond
		if sample.Kind == "gas" || sample.Kind == "audio" {
			for j, v := range sample.PositionM {
				if math.Abs(v-i.Position[j]) > .002 {
					return errors.New("inspection local sensor position differs from robot")
				}
			}
		}
		if sample.Camera != nil && age >= time.Duration(sample.Camera.CalibrationValidForNS) {
			return errors.New("inspection camera calibration expired")
		}
		if sample.Audio != nil && (sample.SampleNS-sample.Audio.DurationNS < i.SettledSinceSimulationNS || time.Duration(sample.Audio.DurationNS)+age > maxAge || time.Duration(i.CapturedUnixMS-i.SettledSinceUnixMS)*time.Millisecond < time.Duration(sample.Audio.DurationNS)) {
			return errors.New("inspection audio interval too old")
		}
	}
	return nil
}
