package missioncontrol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const NativeInferenceType = "wendy.inference.result.v1"

type triggerReference struct {
	Key            world.Key     `json:"key"`
	Version        world.Version `json:"version"`
	EnvelopeSHA256 string        `json:"envelope_sha256"`
	RecordSHA256   string        `json:"record_sha256"`
}

type triggerResult struct {
	Version           int                `json:"version"`
	ResultSchema      string             `json:"result_schema"`
	ResultJSON        []byte             `json:"result_json"`
	Inputs            []triggerReference `json:"inputs"`
	RetainUntilUnixMS int64              `json:"retain_until_unix_ms"`
}

// rejectTriggerDuplicates applies to the whole embedded context, including
// opaque lineage fields which this mission adapter does not interpret.
func rejectTriggerDuplicates(d *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("trigger JSON nesting exceeds limit")
	}
	t, err := d.Token()
	if err != nil {
		return err
	}
	switch t {
	case json.Delim('{'):
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			name, ok := key.(string)
			if err != nil || !ok || seen[name] {
				return errors.New("duplicate trigger field")
			}
			seen[name] = true
			if err = rejectTriggerDuplicates(d, depth+1); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("invalid trigger object")
		}
	case json.Delim('['):
		for d.More() {
			if err := rejectTriggerDuplicates(d, depth+1); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("invalid trigger array")
		}
	}
	return nil
}

func triggerJSON(raw []byte, out any, limit int) error {
	if len(raw) == 0 || len(raw) > limit {
		return errors.New("trigger JSON size exceeds limit")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	if err := rejectTriggerDuplicates(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing trigger JSON")
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(out)
}

func triggerFields(raw []byte, required, optional string) (map[string]json.RawMessage, error) {
	f, err := objectFields(raw)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	for _, key := range strings.Fields(required) {
		if len(f[key]) == 0 || bytes.Equal(bytes.TrimSpace(f[key]), []byte("null")) {
			return nil, errors.New("missing native trigger field")
		}
		allowed[key] = true
	}
	for _, key := range strings.Fields(optional) {
		allowed[key] = true
	}
	for key := range f {
		if !allowed[key] {
			return nil, errors.New("unsupported native trigger field")
		}
	}
	return f, nil
}

func triggerTime(raw []byte) (time.Time, error) {
	var value time.Time
	if json.Unmarshal(raw, &value) != nil || value.IsZero() || value.UnixMilli() <= 0 || value.Year() > 9999 {
		return value, errors.New("invalid native trigger time")
	}
	return value, nil
}

func triggerVector(raw []byte, size int) ([]float64, error) {
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil || len(parts) != size {
		return nil, errors.New("invalid native trigger vector")
	}
	values := make([]float64, size)
	for i, part := range parts {
		if bytes.Equal(bytes.TrimSpace(part), []byte("null")) || json.Unmarshal(part, &values[i]) != nil || !finite(values[i]) {
			return nil, errors.New("invalid native trigger coordinate")
		}
	}
	return values, nil
}

func hazardReferences(raw json.RawMessage, refs []triggerReference) error {
	var inputs []json.RawMessage
	if json.Unmarshal(raw, &inputs) != nil || len(inputs) != len(refs) || len(inputs) < 4 {
		return errors.New("invalid hazard input context")
	}
	for i, input := range inputs {
		f, err := triggerFields(input, "sample_id sensor_id kind source version sample_time capture_start valid_until capture", "sample lidar_summary evidence")
		if err != nil {
			return err
		}
		var source world.Source
		var version world.Version
		var sensor string
		capture, err := triggerFields(f["capture"], "proof_sha256 scene_boot_id simulation_anchor", "original_evidence")
		var proof string
		if err != nil || triggerJSON(f["source"], &source, 4096) != nil || triggerJSON(f["version"], &version, 4096) != nil || json.Unmarshal(f["sensor_id"], &sensor) != nil || json.Unmarshal(capture["proof_sha256"], &proof) != nil || source != refs[i].Key.Source || version != refs[i].Version || proof != refs[i].EnvelopeSHA256 || sensor != source.Stream || refs[i].Key.Entity != "sensor/"+sensor {
			return errors.New("hazard context differs from original references")
		}
	}
	return nil
}

func personReferences(body map[string]json.RawMessage, refs []triggerReference) error {
	var motion, ambiguous bool
	if json.Unmarshal(body["motion_known"], &motion) != nil || json.Unmarshal(body["ambiguous"], &ambiguous) != nil {
		return errors.New("invalid person state")
	}
	if _, err := triggerVector(body["covariance"], 4); err != nil {
		return err
	}
	seen := map[string]triggerReference{}
	ordered := []triggerReference{}
	for _, key := range []string{"current", "previous"} {
		var members []json.RawMessage
		if len(body[key]) != 0 && json.Unmarshal(body[key], &members) != nil || len(members) > 8 || key == "current" && len(members) == 0 || key == "previous" && motion != (len(members) > 0) {
			return errors.New("invalid person observation support")
		}
		for _, member := range members {
			f, err := triggerFields(member, "proof sensor_id sample_id capture_sequence member_index sample_time valid_until physical_until position_xz covariance image model_id model_version model_sha256 calibration_revision calibration_sha256 scene_boot_id captured_unix_ns simulation_anchor_unix_ns", "original_image")
			if err != nil {
				return err
			}
			var ref triggerReference
			var sensor string
			if triggerJSON(f["proof"], &ref, 4096) != nil || json.Unmarshal(f["sensor_id"], &sensor) != nil || sensor != ref.Key.Source.Stream || ref.Key.Entity != "sensor/"+sensor {
				return errors.New("person original reference differs")
			}
			if old, ok := seen[ref.EnvelopeSHA256]; ok {
				if old != ref {
					return errors.New("person proof identity changes")
				}
			} else {
				seen[ref.EnvelopeSHA256] = ref
				ordered = append(ordered, ref)
			}
		}
	}
	if len(ordered) != len(refs) {
		return errors.New("person input set differs")
	}
	for i := range refs {
		if ordered[i] != refs[i] {
			return errors.New("person input order or identity differs")
		}
	}
	for key, size := range map[string]int{"velocity_xz": 2, "velocity_covariance": 4} {
		if !motion && len(body[key]) != 0 {
			return errors.New("unknown motion has velocity")
		}
		if motion {
			if _, err := triggerVector(body[key], size); err != nil {
				return err
			}
		}
	}
	if motion {
		previous, err := triggerTime(body["previous_observed_at"])
		observed, e := triggerTime(body["observed_at"])
		if err != nil || e != nil || !previous.Before(observed) {
			return errors.New("invalid motion observation interval")
		}
	} else if len(body["previous_observed_at"]) != 0 {
		return errors.New("unknown motion has previous time")
	}
	return nil
}

// NativeTriggerTarget validates the task-relevant shape of the two native result
// schemas and returns a fixed observed location. It preserves the component;
// origin verification and full inference/input validation belong to the native
// adapter. It does not re-run inference, read clocks or authorize a mission.
// For a planar person observation, floorY is supplied by commissioning/review.
func NativeTriggerTarget(c world.Component, floorY float64) ([3]float64, error) {
	var target [3]float64
	if c.Key.Type == RisingInspectionType {
		return RisingTriggerTarget(c)
	}
	if c.Key.Type == IncidentInspectionType {
		return IncidentTriggerTarget(c)
	}
	if c.Key.Type != NativeInferenceType || c.Key.Tenant == "" || c.Key.Site == "" || c.Frame.Validate() != nil || c.Tombstone || !finite(floorY) || c.Timing.Validate() != nil || !c.Timing.ErrorKnown || c.TTL <= c.Timing.ErrorBound || c.Provenance.Validate() != nil || c.Provenance.Origin != world.Inferred {
		return target, errors.New("invalid native trigger component")
	}
	var result triggerResult
	if triggerJSON(c.Payload, &result, 32<<10) != nil || result.Version != 1 || len(result.Inputs) == 0 || len(result.Inputs) > 16 || result.RetainUntilUnixMS <= 0 {
		return target, errors.New("invalid native trigger envelope")
	}
	if _, err := triggerFields(c.Payload, "version result_schema result_json inputs retain_until_unix_ms", ""); err != nil {
		return target, err
	}
	if len(result.Inputs) != len(c.Provenance.Inputs) {
		return target, errors.New("native trigger ancestry differs")
	}
	seen := map[string]bool{}
	for i, ref := range result.Inputs {
		leaf := c.Provenance.Inputs[i]
		if ref.Key.Tenant != c.Key.Tenant || ref.Key.Site != c.Key.Site || ref.Key.Type != "wendy.sensor.capture.v1" || ref.Key.Entity == "" || ref.Key.Source.Validate() != nil || ref.Version.Epoch == 0 || ref.Version.Sequence == 0 || !hashPattern.MatchString(ref.EnvelopeSHA256) || !hashPattern.MatchString(ref.RecordSHA256) || seen[ref.EnvelopeSHA256] || leaf.ID != "sha256:"+ref.EnvelopeSHA256 || leaf.Origin != world.Simulated || leaf.Simulation == nil || leaf.Simulation.ClockDomain != c.Timing.ClockDomain || c.Timing.ClockDomain != "simulation:"+leaf.Simulation.RunID {
			return target, errors.New("invalid native trigger reference")
		}
		seen[ref.EnvelopeSHA256] = true
	}
	var body map[string]json.RawMessage
	if triggerJSON(result.ResultJSON, &body, 24<<10) != nil {
		return target, errors.New("invalid native result body")
	}
	var err error
	switch result.ResultSchema {
	case "wendy.hazard.v1":
		body, err = triggerFields(result.ResultJSON, "label confidence confidence_meaning position_m uncertainty_m location_basis gas_value_ppm gas_uncertainty_ppm gas_species cameras audio inputs evidence valid_until input_digest physical_valid_until", "spatial")
		if err != nil || c.Provenance.AppVersion != "world-fusion-v2" || c.Provenance.ModelID != "synthetic-gas-warning-rule" || c.Provenance.ModelVersion != "2" {
			return target, errors.New("unsupported native hazard context")
		}
		var label, inputDigest string
		if json.Unmarshal(body["label"], &label) != nil || label != "possible-gas-hazard" || json.Unmarshal(body["input_digest"], &inputDigest) != nil || !hashPattern.MatchString(inputDigest) {
			return target, errors.New("invalid native hazard identity")
		}
		if _, err = triggerTime(body["physical_valid_until"]); err != nil {
			return target, err
		}
		if err = hazardReferences(body["inputs"], result.Inputs); err != nil {
			return target, err
		}
		position, err := triggerVector(body["position_m"], 3)
		if err != nil {
			return target, err
		}
		copy(target[:], position)
	case "wendy.person.track.v1":
		body, err = triggerFields(result.ResultJSON, "schema algorithm algorithm_version track_id revision run_id clock_domain frame observed_at valid_until position_xz covariance ambiguous motion_known current", "velocity_xz velocity_covariance previous_observed_at previous")
		if err != nil || c.Provenance.AppVersion != "world-live-people-v1" || c.Provenance.ModelID != "camera-ankle-two-observation-tracking" || c.Provenance.ModelVersion != "1" {
			return target, errors.New("unsupported native person context")
		}
		for key, want := range map[string]string{"schema": result.ResultSchema, "algorithm": c.Provenance.ModelID, "algorithm_version": c.Provenance.ModelVersion, "clock_domain": c.Timing.ClockDomain, "run_id": c.Provenance.Inputs[0].Simulation.RunID} {
			var got string
			if json.Unmarshal(body[key], &got) != nil || got != want {
				return target, errors.New("native person context differs")
			}
		}
		var frame world.FrameRef
		var id string
		var revision uint64
		if triggerJSON(body["frame"], &frame, 4096) != nil || frame != c.Frame || json.Unmarshal(body["track_id"], &id) != nil || id == "" || c.Key.Entity != c.Provenance.Inputs[0].Simulation.RunID+"/track/"+id || json.Unmarshal(body["revision"], &revision) != nil || revision == 0 {
			return target, errors.New("invalid native person identity")
		}
		if _, err = triggerTime(body["observed_at"]); err != nil {
			return target, err
		}
		if err = personReferences(body, result.Inputs); err != nil {
			return target, err
		}
		position, err := triggerVector(body["position_xz"], 2)
		if err != nil {
			return target, err
		}
		target = [3]float64{position[0], floorY, position[1]}
	default:
		return target, errors.New("unsupported native inference trigger schema")
	}
	until, err := triggerTime(body["valid_until"])
	if err != nil || !until.After(c.Timing.SampleTime) || c.Timing.SampleTime.Add(c.TTL).After(until) {
		return target, errors.New("native trigger deadline differs")
	}
	return target, nil
}

// InspectParameters keeps proposals small: the full original TriggerRef lives
// in the operator-signed task, bound here by its canonical component digest.
type InspectParameters struct {
	Target                [3]float64             `json:"target"`
	TriggerSHA256         string                 `json:"trigger_sha256,omitempty"`
	InspectionDestination *InspectionDestination `json:"inspection_destination,omitempty"`
}

func DecodeInspectParameters(raw []byte) (InspectParameters, error) {
	var out InspectParameters
	f, err := triggerFields(raw, "target", "trigger_sha256 inspection_destination")
	if err != nil {
		return out, err
	}
	out.Target, err = DecodeTarget(append(append([]byte(`{"target":`), f["target"]...), '}'))
	if err != nil {
		return out, err
	}
	if value, ok := f["trigger_sha256"]; ok {
		if json.Unmarshal(value, &out.TriggerSHA256) != nil || !hashPattern.MatchString(out.TriggerSHA256) {
			return out, errors.New("invalid proposal trigger digest")
		}
	}
	if value, ok := f["inspection_destination"]; ok {
		var destination InspectionDestination
		if out.TriggerSHA256 == "" || json.Unmarshal(value, &destination) != nil {
			return out, errors.New("invalid proposal inspection destination")
		}
		out.InspectionDestination = &destination
	}
	return out, nil
}
