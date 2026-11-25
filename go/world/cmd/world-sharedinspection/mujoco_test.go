package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/livecapture"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func TestLocalMuJoCoReachableDestinationAndFourOriginals(t *testing.T) {
	python := os.Getenv("WENDY_INSPECTION_MUJOCO_PYTHON")
	if python == "" {
		t.Skip("explicit existing MuJoCo 4 Python required for local physics check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	root, _ := filepath.Abs("../..")
	process := exec.CommandContext(ctx, python, filepath.Join(root, "cmd/world-sharedinspection/mujoco_route_check.py"))
	process.Env = append(os.Environ(), "PYTHONPATH="+root, "PYTHONDONTWRITEBYTECODE=1")
	input, err := process.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := process.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	process.Stderr = &stderr
	if err = process.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = input.Close()
		if process.ProcessState == nil {
			_ = process.Process.Kill()
			_ = process.Wait()
		}
	}()
	lines := bufio.NewScanner(output)
	lines.Buffer(make([]byte, 4096), 2<<20)
	if !lines.Scan() {
		t.Fatal("missing actual source identity", lines.Err(), stderr.String())
	}
	var identity struct {
		URL           string         `json:"url"`
		RunID         string         `json:"run_id"`
		Frame         world.FrameRef `json:"frame"`
		PackageSHA256 string         `json:"package_sha256"`
		ModelSHA256   string         `json:"model_sha256"`
	}
	if err := json.Unmarshal(lines.Bytes(), &identity); err != nil {
		t.Fatal(err)
	}
	in, office, _ := inputFixture(t)
	in.RunID, in.Frame, in.PackageSHA256, in.ModelSHA256, in.SceneURL = identity.RunID, identity.Frame, identity.PackageSHA256, identity.ModelSHA256, identity.URL
	now := time.Now()
	files, err := commission(in, office, now)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	for name, value := range files {
		if err := writeNew(filepath.Join(directory, name), value); err != nil {
			t.Fatal(err)
		}
	}
	tokenPath := filepath.Join(directory, "scene-read-token")
	if err := os.WriteFile(tokenPath, bytes.Repeat([]byte("r"), 32), 0600); err != nil {
		t.Fatal(err)
	}
	observation := hazardInput(t, in, now)
	// Structural elevated observation inside an inflated furniture cell. This
	// is synthetic selection context, not a claim of camera-derived inference.
	var trigger tasking.TriggerRef
	_ = json.Unmarshal(observation.TriggerJSON, &trigger)
	var generic map[string]json.RawMessage
	_ = json.Unmarshal(trigger.Component.Payload, &generic)
	var bodyRaw []byte
	_ = json.Unmarshal(generic["result_json"], &bodyRaw)
	var body map[string]json.RawMessage
	_ = json.Unmarshal(bodyRaw, &body)
	observation.Target = [3]float64{-3.6, .65, 2.4}
	body["position_m"], _ = json.Marshal(observation.Target)
	bodyRaw, _ = json.Marshal(body)
	generic["result_json"], _ = json.Marshal(bodyRaw)
	trigger.Component.Payload, _ = json.Marshal(generic)
	bound, err := tasking.TriggerFromComponent(trigger.Component)
	if err != nil {
		t.Fatal(err)
	}
	observation.TriggerJSON, _ = json.Marshal(bound)
	observation.TriggerSHA256 = bound.SHA256
	plan, err := prepareNavigation(ctx, files["proposer.json"].(client.Config), observation, directory, tokenPath)
	if err != nil {
		t.Fatal(err, stderr.String())
	}
	if plan.Review.OffsetM <= 0 || plan.Review.OffsetM > 2 || plan.Target[1] != .24 || observation.Target[1] != .65 {
		t.Fatal("missing explicit nearby destination", plan)
	}
	if err := json.NewEncoder(input).Encode(map[string]any{"route_world": plan.Review.Route, "target": plan.Target}); err != nil {
		t.Fatal(err)
	}
	if !lines.Scan() {
		t.Fatal("missing settled originals", lines.Err(), stderr.String())
	}
	raw := append([]byte(nil), lines.Bytes()...)
	if err := process.Wait(); err != nil {
		t.Fatal(err, stderr.String())
	}
	snapshot, err := livecapture.Decode(raw, livecapture.Config{RunID: in.RunID, Site: in.Site, Frame: in.Frame, MaxAge: 5 * time.Second,
		Planar: &livecapture.PlanarConfig{RobotID: "atlas", FloorY: .24}})
	if err != nil {
		t.Fatal(err)
	}
	position := snapshot.State.Position
	if math.Hypot(position[0]-plan.Target[0], position[2]-plan.Target[2]) > .05 || len(snapshot.Observations) != 4 || len(snapshot.Media) != 3 {
		t.Fatal("destination/capture incomplete")
	}
	ids := []string{}
	for _, sample := range snapshot.Observations {
		ids = append(ids, sample.ID)
		if sample.Evidence != nil {
			if _, err := snapshot.Evidence(*sample.Evidence); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Log only small correspondence metadata; image/audio bytes stay in memory.
	report, _ := json.Marshal(map[string]any{"scope": "owned accelerated MuJoCo 4 route follower and Go capture decoder; no deployed/signed mission",
		"observed_position": observation.Target, "destination": plan.Target, "actual_position": position, "route_length_m": plan.LengthM,
		"offset_m": plan.Review.OffsetM, "nominal_travel_ms": plan.NominalTravelMS, "minimum_duration_ms": plan.MinimumDurationMS,
		"model_sha256": in.ModelSHA256, "package_sha256": in.PackageSHA256, "frame": in.Frame, "original_sample_ids": ids,
		"original_sample_ns": snapshot.SimulationTimeNS, "four_originals_decoded": true, "media_hashes_verified": true})
	t.Log(string(report))
}
