package node

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// This is unsigned structural inference context. Tests below exercise real
// operator/proposal signatures; production/missiontrigger separately tests
// actual native result signatures and the full typed producer validation.
func nativePersonTrigger(t *testing.T, s tasking.Spec) *tasking.TriggerRef {
	t.Helper()
	cfg := s.ControlContext.Context
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	source := world.Source{Device: "gateway-a", App: "com.example.people", Service: "people", Stream: "tracks"}
	camera := source
	camera.Service, camera.Stream = "people-front", "people-front"
	ref := map[string]any{"key": world.Key{Tenant: cfg.Tenant, Site: cfg.Site, Entity: "sensor/people-front", Type: "wendy.sensor.capture.v1", Source: camera}, "version": world.Version{Epoch: 2, Sequence: 4}, "envelope_sha256": strings.Repeat("a", 64), "record_sha256": strings.Repeat("b", 64)}
	member := map[string]any{"proof": ref, "sensor_id": "people-front", "sample_id": cfg.RunID + "/people-front/1", "capture_sequence": 1, "member_index": 0, "sample_time": now, "valid_until": now.Add(time.Second), "physical_until": now.Add(4 * time.Second), "position_xz": [2]float64{s.Target[0], s.Target[2]}, "covariance": [4]float64{.1, 0, 0, .1}, "image": map[string]any{"sha256": strings.Repeat("c", 64), "content_type": "image/png", "size": 100}, "model_id": "test-model", "model_version": "1", "model_sha256": strings.Repeat("d", 64), "calibration_revision": "test-cal", "calibration_sha256": strings.Repeat("e", 64), "scene_boot_id": "test-scene", "captured_unix_ns": now.UnixNano(), "simulation_anchor_unix_ns": now.UnixNano()}
	result := map[string]any{"schema": "wendy.person.track.v1", "algorithm": "camera-ankle-two-observation-tracking", "algorithm_version": "1", "track_id": "anonymous-1", "revision": 1, "run_id": cfg.RunID, "clock_domain": "simulation:" + cfg.RunID, "frame": s.Frame, "observed_at": now, "valid_until": now.Add(time.Second), "position_xz": [2]float64{s.Target[0], s.Target[2]}, "covariance": [4]float64{.1, 0, 0, .1}, "ambiguous": false, "motion_known": false, "current": []any{member}}
	body, _ := json.Marshal(result)
	payload, _ := json.Marshal(map[string]any{"version": 1, "result_schema": "wendy.person.track.v1", "result_json": body, "inputs": []any{ref}, "retain_until_unix_ms": now.Add(time.Minute).UnixMilli()})
	c := world.Component{Key: world.Key{Tenant: cfg.Tenant, Site: cfg.Site, Entity: cfg.RunID + "/track/anonymous-1", Type: missioncontrol.NativeInferenceType, Source: source}, Version: world.Version{Epoch: 3, Sequence: 9}, Frame: s.Frame, Timing: world.Timing{SampleTime: now, ClockDomain: "simulation:" + cfg.RunID, ErrorKnown: true}, TTL: time.Second, Payload: payload, Provenance: world.Provenance{Origin: world.Inferred, AppVersion: "world-live-people-v1", ModelID: "camera-ankle-two-observation-tracking", ModelVersion: "1", Inputs: []world.Input{{ID: "sha256:" + strings.Repeat("a", 64), Origin: world.Simulated, Simulation: &world.SimulationContext{RunID: cfg.RunID, ScenarioVersion: "test-only", Seed: 1, ClockDomain: "simulation:" + cfg.RunID}}}}}
	trigger, err := tasking.TriggerFromComponent(c)
	if err != nil {
		t.Fatal(err)
	}
	return trigger
}

func TestNativeInferenceProposalRetainsSignedOriginalAndFixedTarget(t *testing.T) {
	r, op, s := nativeReviewFixture(t)
	s.Trigger = nativePersonTrigger(t, s)
	parameters, _ := json.Marshal(missioncontrol.InspectParameters{Target: s.Target, TriggerSHA256: s.Trigger.SHA256})
	s.Proposal = nativeProposal(t, r, s, string(parameters))
	body := instructionBody(t, r.f, op, "mission", s)
	code, reply := call(t, r.f, op, "POST", "/v1/tasks", "mission", body)
	if code != 200 {
		t.Fatal(code, string(reply))
	}
	var accepted tasking.Record
	if json.Unmarshal(reply, &accepted) != nil || !reflect.DeepEqual(accepted.Spec.Trigger, s.Trigger) || accepted.Spec.Target != s.Target || accepted.Spec.Trigger.Component.Key.Type != missioncontrol.NativeInferenceType {
		t.Fatal("signed native context changed")
	}
	r.now.Store(time.Now().Add(20 * time.Minute).UnixMilli())
	code, reply = call(t, r.f, op, "POST", "/v1/tasks", "mission", body)
	var again tasking.Record
	_ = json.Unmarshal(reply, &again)
	if code != 200 || !bytes.Equal(accepted.Proof, again.Proof) || tasking.SpecDigest(accepted.Spec) != tasking.SpecDigest(again.Spec) {
		t.Fatal("retained approval changed with proposal expiry", code)
	}
	stop := instructionBody(t, r.f, op, "stop", StopRequest{TaskID: s.ID})
	if code, reply = call(t, r.f, op, "POST", "/v1/tasks/stop", "stop", stop); code != 200 {
		t.Fatal("native trigger prevented stop", code, string(reply))
	}
}

func TestNativeInferenceProposalCannotSubstituteTriggerOrTarget(t *testing.T) {
	for _, mode := range []string{"proposal-hash", "proposal-no-hash", "task-no-trigger", "target", "component", "tenant", "run"} {
		t.Run(mode, func(t *testing.T) {
			r, op, s := nativeReviewFixture(t)
			s.Trigger = nativePersonTrigger(t, s)
			parameters := missioncontrol.InspectParameters{Target: s.Target, TriggerSHA256: s.Trigger.SHA256}
			switch mode {
			case "proposal-hash":
				parameters.TriggerSHA256 = strings.Repeat("f", 64)
			case "proposal-no-hash":
				parameters.TriggerSHA256 = ""
			case "task-no-trigger":
				s.Trigger = nil
			case "target":
				s.Target[0] = 0
				parameters.Target = s.Target
			case "component":
				s.Trigger.Component.Version.Sequence++
			case "tenant":
				s.Trigger.Component.Key.Tenant = "other"
			case "run":
				s.Trigger.Component.Timing.ClockDomain = "simulation:other"
			}
			raw, _ := json.Marshal(parameters)
			s.Proposal = nativeProposal(t, r, s, string(raw))
			code, reply := call(t, r.f, op, "POST", "/v1/tasks", "mission", instructionBody(t, r.f, op, "mission", s))
			if code == 200 {
				t.Fatal("changed native proposal admitted", string(reply))
			}
		})
	}
}
