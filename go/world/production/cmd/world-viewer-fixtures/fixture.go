package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/fusioncapture"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trackcapture"
)

const tenant = "11111111-1111-4111-8111-111111111111"
const app = "com.wendy.world.viewer-fixture"
const run = "viewer-native-v1"
const scenario = "synthetic-viewer-contract-v1"
const domain = "simulation:" + run

var anchor = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
var physical = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
var frame = world.FrameRef{ID: "office", Revision: "viewer-fixture-v1"}

type fixture struct {
	Version     int             `json:"version"`
	Description string          `json:"description"`
	Scope       map[string]any  `json:"scope"`
	Clocks      map[string]any  `json:"clocks"`
	Snapshot    client.Snapshot `json:"snapshot"`
	Cases       []viewerCase    `json:"cases"`
	Rejections  []rejection     `json:"rejection_cases"`
}
type viewerCase struct {
	Name             string         `json:"name"`
	ComponentIndex   int            `json:"component_index"`
	ResultJSONSHA256 string         `json:"result_json_sha256"`
	Expected         map[string]any `json:"expected"`
}
type rejection struct {
	Name      string          `json:"name"`
	Component world.Component `json:"component"`
	Reason    string          `json:"reason"`
}

func hash(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

// jsonBytes only encodes finite constants and closed Go fixture types. A failure
// here is a programming error in the generator, never untrusted input.
func jsonBytes(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic("non-JSON fixture constant: " + err.Error())
	}
	return b
}
func device(b bool) string {
	id := "22222222-2222-4222-8222-222222222222"
	if b {
		id = "33333333-3333-4333-8333-333333333333"
	}
	return "spiffe://wendy.sh/tenant/" + tenant + "/device/" + id
}
func source(sensor string) world.Source {
	return world.Source{Device: device(sensor == "camera-b" || sensor == "audio-1" || sensor == "people-oblique"), App: app, Service: sensor, Stream: sensor}
}
func project(p serviceorigin.PublishRequest, source world.Source, seq uint64) world.Component {
	return world.Component{Key: world.Key{Tenant: tenant, Site: "studio-01", Entity: p.Entity, Type: p.ComponentType, Source: source}, Version: world.Version{Epoch: 7, Sequence: seq}, Frame: p.Frame, Timing: p.Timing, TTL: p.TTL, Provenance: p.Provenance, Payload: append(json.RawMessage(nil), p.Payload...), Tombstone: p.Tombstone}
}
func request(c world.Component) serviceorigin.PublishRequest {
	return serviceorigin.PublishRequest{Entity: c.Key.Entity, ComponentType: c.Key.Type, Stream: c.Key.Source.Stream, Frame: c.Frame, Timing: c.Timing, TTL: c.TTL, Provenance: c.Provenance, Payload: append([]byte(nil), c.Payload...), Tombstone: c.Tombstone}
}

// These are synthetic identifiers, not hashes of retained signed envelopes.
// The fixture exercises exact reference correspondence without test private keys.
func reference(c world.Component) proofref.Reference {
	identity := string(jsonBytes(struct {
		Key     world.Key
		Version world.Version
	}{c.Key, c.Version}))
	return proofref.Reference{Key: c.Key, Version: c.Version, EnvelopeSHA256: hash([]byte("synthetic-envelope:" + identity)), RecordSHA256: hash([]byte("synthetic-record:" + identity))}
}
func leaf(ref proofref.Reference) world.Input {
	return world.Input{ID: ref.ID(), Origin: world.Simulated, Simulation: &world.SimulationContext{RunID: run, ScenarioVersion: scenario, Seed: 7, ClockDomain: domain}}
}

func capture(sensor, kind string, at time.Duration, seq uint64) (sensing.Sample, sensorrecord.Capture, resultrecord.ValidatedInput, error) {
	s := sensing.Sample{SchemaVersion: sensing.Schema, ID: fmt.Sprintf("%s/%s/%d", run, sensor, seq), SiteID: "studio-01", Frame: frame, SensorID: sensor, Kind: kind, Sequence: seq, SampleNS: int64(at), RunID: run, ScenarioVersion: scenario, Seed: 7, PackageSHA256: hash([]byte("synthetic office package")), SensorModel: "synthetic-" + kind + "-fixture-v1", PositionM: []float64{1, .24, 2}, UncertaintyM: .05, CalibrationRevision: "viewer-calibration-v1"}
	switch kind {
	case "camera":
		x := -1.
		if sensor == "camera-b" || sensor == "people-oblique" {
			x = 1
		}
		s.PositionM = []float64{x, 1, 0}
		s.Camera = &sensing.Camera{Width: 320, Height: 240, FX: 240, FY: 240, CX: 160, CY: 120, CameraToSite: []float64{1, 0, 0, x, 0, 1, 0, 1, 0, 0, 1, 0, 0, 0, 0, 1}, CalibrationValidForNS: int64(time.Minute)}
		s.Evidence = &sensing.EvidenceRef{SHA256: hash([]byte("synthetic PNG reference:" + sensor)), ContentType: "image/png", Size: 1024}
	case "gas":
		s.Gas = &sensing.Gas{ValuePPM: 80, UncertaintyPPM: 1.5, Species: "synthetic-test-gas"}
	case "audio":
		s.Audio = &sensing.Audio{SampleRateHz: 8000, Channels: 1, DurationNS: int64(250 * time.Millisecond)}
		s.Evidence = &sensing.EvidenceRef{SHA256: hash([]byte("synthetic WAV reference")), ContentType: "audio/wav", Size: 4044}
	case "lidar":
		s.Lidar = &sensing.Lidar{PointsM: [][3]float64{{1, .7, 2}, {1.02, .7, 2}}, MaxRangeM: 6}
	}
	c := sensorrecord.Capture{Version: sensorrecord.Version, SceneBootID: "synthetic-scene-boot", CapturedUnixNS: physical.Add(at - 2*time.Second - 100*time.Millisecond).UnixNano(), SimulationAnchorUnixNS: anchor.UnixNano(), SampleJSON: jsonBytes(s)}
	p, err := sensorrecord.Publication(c, 3*time.Second)
	if err != nil {
		return s, c, resultrecord.ValidatedInput{}, err
	}
	component := project(p, source(sensor), seq)
	sampleUntil, physicalUntil, err := sensorrecord.LiveDeadlines(component)
	return s, c, resultrecord.ValidatedInput{Reference: reference(component), Component: component, SampleValidUntil: sampleUntil, PhysicalValidUntil: physicalUntil}, err
}

func person(motion bool) (world.Component, error) {
	p := trackcapture.Payload{Schema: trackcapture.ResultSchema, Algorithm: trackcapture.AlgorithmID, AlgorithmVersion: trackcapture.AlgorithmVersion, TrackID: "anonymous-1", Revision: 2, RunID: run, ClockDomain: domain, Frame: frame, ObservedAt: anchor.Add(2 * time.Second), PositionXZ: [2]float64{1, 2}, Covariance: [4]float64{.04, 0, 0, .09}, MotionKnown: motion, Ambiguous: !motion}
	if !motion {
		p.TrackID = "anonymous-2"
		p.Revision = 1
		p.PositionXZ = [2]float64{2, 3}
	}
	var inputs []resultrecord.ValidatedInput
	for group, at := range []time.Duration{2 * time.Second, 1750 * time.Millisecond} {
		if group == 1 && !motion {
			break
		}
		for _, sensor := range []string{"people-front", "people-oblique"} {
			seq := uint64(11 - group)
			s, c, in, err := capture(sensor, "camera", at, seq)
			if err != nil {
				return world.Component{}, err
			}
			until := anchor.Add(at + trackcapture.DefaultLifetime)
			in.SampleValidUntil = until
			cal, err := sensorrecord.CalibrationDigest(s)
			if err != nil {
				return world.Component{}, err
			}
			position := p.PositionXZ
			if group == 1 {
				position[0] -= .1
			}
			m := trackcapture.Member{Proof: in.Reference, SensorID: sensor, SampleID: s.ID, CaptureSequence: seq, MemberIndex: 0, SampleTime: anchor.Add(at), ValidUntil: until, PhysicalUntil: in.PhysicalValidUntil, PositionXZ: position, Covariance: p.Covariance, Image: *s.Evidence, ModelID: "synthetic-pose-fixture", ModelVersion: "1", ModelSHA256: hash([]byte("synthetic pose model")), CalibrationRevision: s.CalibrationRevision, CalibrationSHA256: cal, SceneBootID: c.SceneBootID, CapturedUnixNS: c.CapturedUnixNS, SimulationAnchorUnixNS: c.SimulationAnchorUnixNS}
			if group == 0 {
				p.Current = append(p.Current, m)
			} else {
				p.Previous = append(p.Previous, m)
			}
			inputs = append(inputs, in)
		}
	}
	p.ValidUntil = anchor.Add(2*time.Second + trackcapture.DefaultLifetime)
	if motion {
		previous := anchor.Add(1750 * time.Millisecond)
		p.PreviousObservedAt = &previous
		p.ValidUntil = previous.Add(trackcapture.DefaultLifetime)
		v, covariance := [2]float64{.4, 0}, [4]float64{1.28, 0, 0, 2.88}
		p.VelocityXZ, p.VelocityCovariance = &v, &covariance
	}
	r := resultrecord.Result{Version: resultrecord.Version, ResultSchema: trackcapture.ResultSchema, ResultJSON: jsonBytes(p), RetainUntilUnixMS: physical.Add(7 * 24 * time.Hour).UnixMilli()}
	q := serviceorigin.PublishRequest{Entity: run + "/track/" + p.TrackID, ComponentType: resultrecord.ComponentType, Stream: "tracks", Frame: frame, Timing: world.Timing{SampleTime: p.ObservedAt, ClockDomain: domain, ErrorKnown: true}, Provenance: world.Provenance{Origin: world.Inferred, AppVersion: trackcapture.AppVersion, ModelID: trackcapture.AlgorithmID, ModelVersion: trackcapture.AlgorithmVersion}}
	for _, in := range inputs {
		r.Inputs = append(r.Inputs, in.Reference)
		q.Provenance.Inputs = append(q.Provenance.Inputs, leaf(in.Reference))
		if in.Component.Timing.SampleTime.Before(q.Timing.SampleTime) {
			q.Timing.SampleTime = in.Component.Timing.SampleTime
		}
	}
	q.TTL = p.ValidUntil.Sub(q.Timing.SampleTime)
	var err error
	q.Payload, err = resultrecord.Encode(r)
	if err != nil {
		return world.Component{}, err
	}
	if _, err = resultrecord.ValidatePublication(q, inputs); err != nil {
		return world.Component{}, err
	}
	if _, err = trackcapture.ValidatePublication(q); err != nil {
		return world.Component{}, err
	}
	return project(q, world.Source{Device: device(false), App: app, Service: "people", Stream: "tracks"}, p.Revision), nil
}

func hazard() (world.Component, error) {
	h := &fusion.Hazard{Label: "possible-gas-hazard", Confidence: .8, ConfidenceMeaning: "fixed synthetic rule score, not a calibrated probability", PositionM: [3]float64{1, .7, 2}, UncertaintyM: .2, LocationBasis: "two calibrated camera observations associated with LiDAR surface returns", GasValuePPM: 80, GasUncertaintyPPM: 1.5, GasSpecies: "synthetic-test-gas", Audio: fusion.AudioFeatures{RMS: .2, ToneFraction: .99}}
	var inputs []resultrecord.ValidatedInput
	for _, pair := range [][2]string{{"audio-1", "audio"}, {"camera-a", "camera"}, {"camera-b", "camera"}, {"gas-1", "gas"}, {"lidar-atlas", "lidar"}} {
		s, c, in, err := capture(pair[0], pair[1], 2*time.Second, 11)
		if err != nil {
			return world.Component{}, err
		}
		start := in.Component.Timing.SampleTime
		if s.Audio != nil {
			start = start.Add(-time.Duration(s.Audio.DurationNS))
		}
		h.Inputs = append(h.Inputs, fusion.InputRef{Sample: s, SampleID: s.ID, SensorID: s.SensorID, Kind: s.Kind, Source: in.Component.Key.Source, Version: in.Component.Version, SampleTime: in.Component.Timing.SampleTime, CaptureStart: start, ValidUntil: in.SampleValidUntil, Evidence: s.Evidence, Capture: &fusion.CaptureOrigin{ProofSHA256: in.Reference.EnvelopeSHA256, SceneBootID: c.SceneBootID, SimulationAnchor: anchor}})
		inputs = append(inputs, in)
		if h.ValidUntil.IsZero() || in.SampleValidUntil.Before(h.ValidUntil) {
			h.ValidUntil = in.SampleValidUntil
		}
		if s.Evidence != nil {
			h.Evidence = append(h.Evidence, *s.Evidence)
		}
		if s.Camera != nil {
			x := s.PositionM[0]
			h.Cameras = append(h.Cameras, fusion.CameraFeature{SensorID: s.SensorID, RedFraction: .02, Observation: &fusion.CameraObservation{Pixels: 100, CentroidPX: [2]float64{160, 120}, BoundsPX: [4]int{155, 115, 165, 125}, RayOriginM: [3]float64{x, 1, 0}, RayDirection: [3]float64{0, 0, 1}, ReprojectionErrorPX: .5}})
		}
	}
	h.InputDigest = hash(jsonBytes(h.Inputs))
	h.Spatial = &fusion.SpatialFeatures{Associated: true, Reason: "two camera rays and observed LiDAR surfaces agree", PositionM: h.PositionM, TriangulatedM: h.PositionM, UncertaintyM: h.UncertaintyM, RaySeparationM: .01, RayAngleDeg: 25, ReprojectionErrorPX: .5, Cameras: h.Cameras, LidarPointsConsidered: 2, LidarSupportCount: 2, LidarSupportPoints: [][3]float64{{1, .7, 2}, {1.02, .7, 2}}}
	q, err := fusioncapture.BuildPublication(fusion.Result{State: fusion.HazardDetected, InputDigest: h.InputDigest, Hazard: h}, inputs, "hazard", physical.Add(7*24*time.Hour))
	if err != nil {
		return world.Component{}, err
	}
	if _, err = fusioncapture.ValidateReplicaPublication(q); err != nil {
		return world.Component{}, err
	}
	return project(q, world.Source{Device: device(false), App: app, Service: "fusion", Stream: "hazard"}, 11), nil
}

func validate(c world.Component) error {
	r, err := resultrecord.Decode(c.Payload)
	if err != nil {
		return err
	}
	if c.Key.Tenant != tenant || c.Key.Site != "studio-01" || c.Key.Source.Validate() != nil || c.Version.Epoch == 0 || c.Version.Sequence == 0 {
		return fmt.Errorf("invalid fixture component scope")
	}
	for _, ref := range r.Inputs {
		if ref.Key.Tenant != c.Key.Tenant || ref.Key.Site != c.Key.Site {
			return fmt.Errorf("input scope mismatch")
		}
	}
	switch r.ResultSchema {
	case trackcapture.ResultSchema:
		_, err = trackcapture.ValidatePublication(request(c))
	case fusion.HazardType:
		_, err = fusioncapture.ValidateReplicaPublication(request(c))
	default:
		return fmt.Errorf("unsupported typed result schema")
	}
	return err
}

func expected(name string, index int, c world.Component) viewerCase {
	r, _ := resultrecord.Decode(c.Payload)
	e := map[string]any{"run_id": run, "frame": c.Frame, "source": c.Key.Source, "version": c.Version, "model_id": c.Provenance.ModelID, "model_version": c.Provenance.ModelVersion, "sample_time": c.Timing.SampleTime, "clock_domain": c.Timing.ClockDomain, "sample_valid_until": c.Timing.SampleTime.Add(c.TTL - max(c.Timing.ErrorBound, c.Timing.AgeHint)), "input_refs": r.Inputs, "retention_is_live_validity": false, "inputs_independently_verified": false, "evidence_available": false}
	if r.ResultSchema == trackcapture.ResultSchema {
		p, _ := trackcapture.Decode(r.ResultJSON)
		e["kind"], e["position_xz"], e["covariance"], e["motion_known"], e["ambiguous"], e["physical_valid_until"] = "person", p.PositionXZ, p.Covariance, p.MotionKnown, p.Ambiguous, p.PhysicalValidUntil()
		e["current_members"], e["previous_members"], e["track_id"] = len(p.Current), len(p.Previous), p.TrackID
		e["observed_at"] = p.ObservedAt
		if p.MotionKnown {
			e["velocity_xz"], e["velocity_covariance"] = p.VelocityXZ, p.VelocityCovariance
		}
	} else {
		h, _ := fusioncapture.ValidateReplicaPublication(request(c))
		e["kind"], e["position_m"], e["uncertainty_m"], e["confidence"], e["confidence_meaning"], e["evidence"], e["physical_valid_until"] = "hazard", h.Hazard.PositionM, h.Hazard.UncertaintyM, h.Hazard.Confidence, h.Hazard.ConfidenceMeaning, h.Hazard.Evidence, h.PhysicalValidUntil
		e["gas_value_ppm"], e["gas_uncertainty_ppm"], e["lidar_returns"] = h.Hazard.GasValuePPM, h.Hazard.GasUncertaintyPPM, 2
	}
	return viewerCase{Name: name, ComponentIndex: index, ResultJSONSHA256: hash(r.ResultJSON), Expected: e}
}

func generate() (fixture, error) {
	f := fixture{Version: 1, Description: "Synthetic Go wire contract, not recorded inference, signed proof verification or available media. Proof and media hashes are deterministic fixture identifiers. Sample and physical clocks are distinct; UTC-only viewers must not call these simulation records current.", Scope: map[string]any{"tenant": tenant, "site": "studio-01", "run_id": run, "frame": frame}, Clocks: map[string]any{"sample_now": anchor.Add(2100 * time.Millisecond), "clock_domain": domain, "physical_now": physical}, Snapshot: client.Snapshot{Cursor: "synthetic-viewer-fixture-v1"}}
	known, err := person(true)
	if err != nil {
		return f, err
	}
	unknown, err := person(false)
	if err != nil {
		return f, err
	}
	h, err := hazard()
	if err != nil {
		return f, err
	}
	f.Snapshot.Components = []world.Component{known, unknown, h}
	for i, name := range []string{"person-motion", "person-motion-unknown", "hazard-spatial"} {
		c := f.Snapshot.Components[i]
		if err := validate(c); err != nil {
			return f, err
		}
		f.Cases = append(f.Cases, expected(name, i, c))
	}
	for _, name := range []string{"invalid-base64", "result-json-object", "unknown-schema", "mismatched-frame", "changed-proof-version", "unknown-motion-with-velocity", "missing-hazard-physical-cap"} {
		c := known
		if name == "missing-hazard-physical-cap" {
			c = h
		}
		var outer map[string]json.RawMessage
		_ = json.Unmarshal(c.Payload, &outer)
		r, _ := resultrecord.Decode(c.Payload)
		var body map[string]json.RawMessage
		_ = json.Unmarshal(r.ResultJSON, &body)
		switch name {
		case "invalid-base64":
			outer["result_json"] = json.RawMessage(`"%%%"`)
		case "result-json-object":
			outer["result_json"] = r.ResultJSON
		case "unknown-schema":
			outer["result_schema"] = jsonBytes("wendy.unknown.v1")
		case "mismatched-frame":
			c.Frame.Revision = "different"
		case "changed-proof-version":
			r.Inputs[0].Version.Sequence++
			outer["inputs"] = jsonBytes(r.Inputs)
		case "unknown-motion-with-velocity":
			body["motion_known"] = json.RawMessage("false")
			outer["result_json"] = jsonBytes(jsonBytes(body))
		case "missing-hazard-physical-cap":
			delete(body, "physical_valid_until")
			outer["result_json"] = jsonBytes(jsonBytes(body))
		}
		c.Payload = jsonBytes(outer)
		if err := validate(c); err == nil {
			return f, fmt.Errorf("negative fixture accepted: %s", name)
		}
		f.Rejections = append(f.Rejections, rejection{Name: name, Component: c, Reason: name})
	}
	return f, nil
}
