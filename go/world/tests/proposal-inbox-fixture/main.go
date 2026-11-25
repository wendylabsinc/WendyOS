// Test-only structural simulation context. No sensor capture, producer-signature
// verification, navigation clearance or executor behavior is claimed.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/proposals"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func nativePersonTrigger(cfg missioncontrol.Context, now, physical time.Time, target [3]float64) (*tasking.TriggerRef, error) {
	source := world.Source{Device: "gateway-a", App: "com.example.people", Service: "people", Stream: "tracks"}
	camera := source
	camera.Service, camera.Stream = "people-front", "people-front"
	ref := map[string]any{"key": world.Key{Tenant: cfg.Tenant, Site: cfg.Site, Entity: "sensor/people-front", Type: "wendy.sensor.capture.v1", Source: camera}, "version": world.Version{Epoch: 2, Sequence: 4}, "envelope_sha256": strings.Repeat("a", 64), "record_sha256": strings.Repeat("b", 64)}
	member := map[string]any{"proof": ref, "sensor_id": "people-front", "sample_id": cfg.RunID + "/people-front/1", "capture_sequence": 1, "member_index": 0, "sample_time": now, "valid_until": now.Add(2 * time.Minute), "physical_until": physical.Add(2 * time.Minute), "position_xz": [2]float64{target[0], target[2]}, "covariance": [4]float64{.1, 0, 0, .1}, "image": map[string]any{"sha256": strings.Repeat("c", 64), "content_type": "image/png", "size": 100}, "model_id": "test-model", "model_version": "1", "model_sha256": strings.Repeat("d", 64), "calibration_revision": "test-cal", "calibration_sha256": strings.Repeat("e", 64), "scene_boot_id": "test-scene", "captured_unix_ns": physical.UnixNano(), "simulation_anchor_unix_ns": now.UnixNano()}
	result := map[string]any{"schema": "wendy.person.track.v1", "algorithm": "camera-ankle-two-observation-tracking", "algorithm_version": "1", "track_id": "anonymous-1", "revision": 1, "run_id": cfg.RunID, "clock_domain": "simulation:" + cfg.RunID, "frame": cfg.Frame, "observed_at": now, "valid_until": now.Add(2 * time.Minute), "position_xz": [2]float64{target[0], target[2]}, "covariance": [4]float64{.1, 0, 0, .1}, "ambiguous": false, "motion_known": false, "current": []any{member}}
	body, _ := json.Marshal(result)
	payload, _ := json.Marshal(map[string]any{"version": 1, "result_schema": "wendy.person.track.v1", "result_json": body, "inputs": []any{ref}, "retain_until_unix_ms": physical.Add(2 * time.Minute).UnixMilli()})
	c := world.Component{Key: world.Key{Tenant: cfg.Tenant, Site: cfg.Site, Entity: cfg.RunID + "/track/anonymous-1", Type: missioncontrol.NativeInferenceType, Source: source}, Version: world.Version{Epoch: 3, Sequence: 9}, Frame: cfg.Frame, Timing: world.Timing{SampleTime: now, ClockDomain: "simulation:" + cfg.RunID, ErrorKnown: true}, TTL: 2 * time.Minute, Payload: payload, Provenance: world.Provenance{Origin: world.Inferred, AppVersion: "world-live-people-v1", ModelID: "camera-ankle-two-observation-tracking", ModelVersion: "1", Inputs: []world.Input{{ID: "sha256:" + strings.Repeat("a", 64), Origin: world.Simulated, Simulation: &world.SimulationContext{RunID: cfg.RunID, ScenarioVersion: "native-inbox-structural-fixture", Seed: 1, ClockDomain: "simulation:" + cfg.RunID}}}}}
	return tasking.TriggerFromComponent(c)
}

func main() {
	contextPath := flag.String("context", "", "existing signed simulation context")
	out := flag.String("out", "", "private artifact directory")
	id := flag.String("id", "planar-review", "proposal identity")
	flag.Parse()
	var signed missioncontrol.SignedContext
	raw, err := os.ReadFile(*contextPath)
	if err != nil {
		panic(err)
	}
	if err = json.Unmarshal(raw, &signed); err != nil {
		panic(err)
	}
	now := time.Now().UTC()
	cfg := signed.Context
	if cfg.RunID != "native-inbox" || cfg.Tenant != "lab" || cfg.Site != "synthetic-office" {
		panic("fixture scope required")
	}
	target := [3]float64{1, .2, .05}
	sample := time.Date(2026, 1, 1, 0, 0, 10, 0, time.UTC)
	trigger, err := nativePersonTrigger(cfg, sample, now, [3]float64{1, .2, 0})
	if err != nil {
		panic(err)
	}
	bytes, _ := json.Marshal(trigger)
	sum := sha256.Sum256(bytes)
	envelope := hex.EncodeToString(sum[:])
	destination := missioncontrol.InspectionDestination{Version: 1, Mode: "planar_nearby", FloorY: .2, MaxOffsetM: .1, PlanarConfigSHA256: strings.Repeat("1", 64)}
	if err = missioncontrol.ValidateNativeInspectionTarget(trigger.Component, target, &destination); err != nil {
		panic(err)
	}
	input := map[string]any{"version": 1, "trigger_json": bytes, "trigger_sha256": trigger.SHA256, "target": [3]float64{1, .2, 0}, "envelope_sha256": envelope, "result_schema": "wendy.person.track.v1", "sample_valid_until": sample.Add(2 * time.Minute), "physical_valid_until": now.Add(2 * time.Minute)}
	handoff := map[string]any{"version": 1, "proposal_id": *id, "mission_input": input, "original_envelope_sha256": envelope, "navigation_target": target, "inspection_destination": destination}
	parameters, _ := json.Marshal(missioncontrol.InspectParameters{Target: target, TriggerSHA256: trigger.SHA256, InspectionDestination: &destination})
	proposal := proposals.Request{Version: 1, ID: *id, Tenant: cfg.Tenant, Site: cfg.Site, RunID: cfg.RunID, Assignee: cfg.Assignee, Type: missioncontrol.InspectType, Parameters: parameters, Summary: "Inspect the nearby planar destination. Structural simulation fixture; no sensor truth claimed.", NotAfterUnixMS: now.Add(3 * time.Minute).UnixMilli()}
	for name, value := range map[string]any{"handoff.json": handoff, "proposal.json": proposal, "trigger.json": trigger, "scope.json": map[string]any{"scope": "Synthetic structural context through production typed trigger builder; no actual sensor or verified producer original", "created_at": now, "expires_at": now.Add(2 * time.Minute)}} {
		b, _ := json.MarshalIndent(value, "", "  ")
		if err = os.WriteFile(filepath.Join(*out, name), append(b, '\n'), 0600); err != nil {
			panic(err)
		}
	}
	fmt.Println("prepared structural fixture")
}
