package node

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// Structural original context; the test signs the proposal and operator task
// through the existing HTTP verifier. It does not assert origin verification.
func destinationHazardTrigger(t *testing.T, s tasking.Spec) *tasking.TriggerRef {
	t.Helper()
	c := nativePersonTrigger(t, s).Component
	now := c.Timing.SampleTime
	run := s.ControlContext.Context.RunID
	refs, inputs := []any{}, []any{}
	ancestry := []world.Input{}
	for i, sensor := range []string{"audio-1", "camera-a", "camera-b", "gas-1"} {
		source := c.Key.Source
		source.Service, source.Stream = sensor, sensor
		version := world.Version{Epoch: 1, Sequence: uint64(i + 1)}
		proof := strings.Repeat(string("bcde"[i]), 64)
		key := world.Key{Tenant: c.Key.Tenant, Site: c.Key.Site, Entity: "sensor/" + sensor, Type: "wendy.sensor.capture.v1", Source: source}
		refs = append(refs, map[string]any{"key": key, "version": version, "envelope_sha256": proof, "record_sha256": strings.Repeat("f", 64)})
		inputs = append(inputs, map[string]any{"sample_id": run + "/" + sensor + "/1", "sensor_id": sensor, "kind": strings.Split(sensor, "-")[0], "source": source, "version": version, "sample_time": now, "capture_start": now, "valid_until": now.Add(time.Second), "capture": map[string]any{"proof_sha256": proof, "scene_boot_id": "fixture", "simulation_anchor": now}})
		ancestry = append(ancestry, world.Input{ID: "sha256:" + proof, Origin: world.Simulated, Simulation: &world.SimulationContext{RunID: run, ScenarioVersion: "test-only", Seed: 1, ClockDomain: "simulation:" + run}})
	}
	observed := s.Target
	observed[1] = .65
	body, _ := json.Marshal(map[string]any{"label": "possible-gas-hazard", "confidence": .8, "confidence_meaning": "rule support", "position_m": observed, "uncertainty_m": .5, "location_basis": "structural fixture", "gas_value_ppm": 90, "gas_uncertainty_ppm": 5, "gas_species": "simulated", "cameras": []any{}, "audio": map[string]any{}, "inputs": inputs, "evidence": []any{}, "valid_until": now.Add(time.Second), "input_digest": strings.Repeat("1", 64), "physical_valid_until": now.Add(4 * time.Second)})
	c.Payload, _ = json.Marshal(map[string]any{"version": 1, "result_schema": "wendy.hazard.v1", "result_json": body, "inputs": refs, "retain_until_unix_ms": now.Add(time.Minute).UnixMilli()})
	c.Key.Entity, c.Key.Source.Service, c.Key.Source.Stream = run+"/possible-gas-hazard", "fusion", "hazards"
	c.Provenance = world.Provenance{Origin: world.Inferred, AppVersion: "world-fusion-v2", ModelID: "synthetic-gas-warning-rule", ModelVersion: "2", Inputs: ancestry}
	trigger, err := tasking.TriggerFromComponent(c)
	if err != nil {
		t.Fatal(err)
	}
	return trigger
}

func TestInspectionDestinationSignedAdmissionPreservesOriginalAndReviewedPoint(t *testing.T) {
	for kind, original := range map[string]func(*testing.T, tasking.Spec) *tasking.TriggerRef{"person": nativePersonTrigger, "hazard": destinationHazardTrigger} {
		t.Run(kind, func(t *testing.T) {
			r, op, s := nativeReviewFixtureArea(t, missioncontrol.Bounds{Min: [3]float64{-8, .1, -8}, Max: [3]float64{8, 1, 8}})
			s.Target[0] = 3.5 // Both schemas exercise a real target outside the old X[-2,2] branch.
			s.Trigger = original(t, s)
			originalBytes, _ := json.Marshal(s.Trigger)
			observed, err := missioncontrol.NativeTriggerTarget(s.Trigger.Component, .24)
			if err != nil {
				t.Fatal(err)
			}
			s.Target = [3]float64{observed[0] - .5, .24, observed[2] + .5}
			s.InspectionDestination = &missioncontrol.InspectionDestination{Version: 1, Mode: "planar_nearby", FloorY: .24, MaxOffsetM: 2, PlanarConfigSHA256: strings.Repeat("a", 64)}
			params, _ := json.Marshal(missioncontrol.InspectParameters{Target: s.Target, TriggerSHA256: s.Trigger.SHA256, InspectionDestination: s.InspectionDestination})
			s.Proposal = nativeProposal(t, r, s, string(params))
			body := instructionBody(t, r.f, op, "mission", s)
			code, reply := call(t, r.f, op, "POST", "/v1/tasks", "mission", body)
			var accepted tasking.Record
			if code != 200 || json.Unmarshal(reply, &accepted) != nil || accepted.Spec.Target != s.Target || !reflect.DeepEqual(accepted.Spec.InspectionDestination, s.InspectionDestination) {
				t.Fatal("reviewed navigation point was not admitted", code, string(reply))
			}
			retained, _ := json.Marshal(accepted.Spec.Trigger)
			if !bytes.Equal(originalBytes, retained) || kind == "hazard" && observed[1] != .65 {
				t.Fatal("original observation was projected or replaced")
			}
			// The durable store owns the descriptor and exact accepted retry.
			s.InspectionDestination.MaxOffsetM = 5
			r.now.Store(time.Now().Add(20 * time.Minute).UnixMilli())
			code, reply = call(t, r.f, op, "POST", "/v1/tasks", "mission", body)
			var again tasking.Record
			if code != 200 || json.Unmarshal(reply, &again) != nil || !bytes.Equal(accepted.Proof, again.Proof) || tasking.SpecDigest(accepted.Spec) != tasking.SpecDigest(again.Spec) {
				t.Fatal("exact retry changed destination or original", code, string(reply))
			}
		})
	}
}

func TestInspectionDestinationProposalAndTaskCannotSubstituteReviewedFields(t *testing.T) {
	for _, mode := range []string{"task-omits", "proposal-omits", "map", "radius", "floor", "target", "trigger", "context", "configured-context", "legacy-trigger", "out-of-radius", "out-of-area"} {
		t.Run(mode, func(t *testing.T) {
			r, op, s := nativeReviewFixture(t)
			s.Trigger = destinationHazardTrigger(t, s)
			s.Target = [3]float64{.5, .24, .5}
			s.InspectionDestination = &missioncontrol.InspectionDestination{Version: 1, Mode: "planar_nearby", FloorY: .24, MaxOffsetM: 2, PlanarConfigSHA256: strings.Repeat("a", 64)}
			d := *s.InspectionDestination
			p := missioncontrol.InspectParameters{Target: s.Target, TriggerSHA256: s.Trigger.SHA256, InspectionDestination: &d}
			switch mode {
			case "task-omits":
				s.InspectionDestination = nil
			case "proposal-omits":
				p.InspectionDestination = nil
			case "map":
				s.InspectionDestination.PlanarConfigSHA256 = strings.Repeat("b", 64)
			case "radius":
				s.InspectionDestination.MaxOffsetM = 3
			case "floor":
				s.InspectionDestination.FloorY = .25
			case "target":
				s.Target[0] = 0
			case "trigger":
				s.Trigger = nil
			case "context":
				s.ControlContext = nil
			case "configured-context":
				// Even a root-signed different area cannot replace the node's
				// commissioned policy just because the new point fits inside it.
				context := s.ControlContext.Context
				context.Area.Max[0] = 4
				changed, err := authority.SignMissionControlContext(context, r.f.root)
				if err != nil {
					t.Fatal(err)
				}
				s.ControlContext = &changed
				s.Area.Max[0] = 4
				s.Target = [3]float64{3, .24, 0}
				p.Target = s.Target
			case "legacy-trigger":
				s.Trigger.Component.Key.Type = "wendy.hazard.v1"
			case "out-of-radius":
				s.Target[0] = -1.5
				p.Target = s.Target
			case "out-of-area":
				s.Target[2] = 1.5
				p.Target = s.Target
			}
			raw, _ := json.Marshal(p)
			s.Proposal = nativeProposal(t, r, s, string(raw))
			code, reply := call(t, r.f, op, "POST", "/v1/tasks", "mission", instructionBody(t, r.f, op, "mission", s))
			if code == 200 {
				t.Fatal("changed destination admitted", mode, string(reply))
			}
		})
	}
}
