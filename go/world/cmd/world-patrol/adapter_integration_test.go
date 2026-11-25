package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/fusiondemo"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// Opt in with PATROL_PHYSICS_URL=http://127.0.0.1:18081. This samples an
// already-running isolated simulator; it never sends motion or reset commands.
func TestPhysicsCaptureIntegration(t *testing.T) {
	endpoint := os.Getenv("PATROL_PHYSICS_URL")
	if endpoint == "" {
		t.Skip("set PATROL_PHYSICS_URL to an isolated Go2 physics fixture")
	}
	a := &adapter{physics: endpoint, dir: t.TempDir(), client: &http.Client{Timeout: 15 * time.Second}}
	var state struct {
		RunID      string         `json:"run_id"`
		Frame      world.FrameRef `json:"frame"`
		Simulation bool           `json:"simulation"`
	}
	if err := a.request(endpoint, "/state", nil, &state); err != nil {
		t.Fatal(err)
	}
	if !state.Simulation || state.RunID == "" || state.Frame.Validate() != nil {
		t.Fatal("physics must identify a simulation run and valid frame")
	}
	a.runID, a.frame = state.RunID, state.Frame
	for _, name := range []string{"captures", "evidence"} {
		if err := os.Mkdir(filepath.Join(a.dir, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	a.fuser, err = fusiondemo.New(fusiondemo.Config{Version: 1, ParticipantID: "hub", RunID: a.runID, Frame: a.frame, Sensors: []fusiondemo.Sensor{
		{SensorID: "camera-a", Kind: "camera", Owner: "hub"},
		{SensorID: "camera-b", Kind: "camera", Owner: "hub"},
		{SensorID: "gas-1", Kind: "gas", Owner: "hub"},
		{SensorID: "audio-1", Kind: "audio", Owner: "hub"},
		{SensorID: "lidar-go2", Kind: "lidar", Owner: "go2"},
	}}, filepath.Join(a.dir, "fusion"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.fuser.Close()
	capture, err := a.Capture()
	if err != nil {
		t.Fatal(err)
	}
	if capture.TimeS <= 0 || len(capture.Evidence) != 4 || a.latest == nil || len(a.latest.Observations) != 5 {
		t.Fatalf("incomplete capture: time=%v evidence=%d fusion_present=%v", capture.TimeS, len(capture.Evidence), a.latest != nil)
	}
	seen := make(map[string]bool)
	for _, sample := range a.latest.Observations {
		if sample.Frame != a.frame || sample.RunID != a.runID {
			t.Fatal("fusion lost capture scope")
		}
		seen[sample.SensorID] = true
	}
	for _, id := range []string{"camera-a", "camera-b", "lidar-go2", "gas-1", "audio-1"} {
		if !seen[id] {
			t.Fatal("fusion omitted", id)
		}
	}
	files, err := filepath.Glob(filepath.Join(a.dir, "captures", "capture-*.json"))
	if err != nil || len(files) != 1 {
		t.Fatal("complete capture was not persisted", files, err)
	}
	media, err := os.ReadDir(filepath.Join(a.dir, "evidence"))
	if err != nil || len(media) != 4 {
		t.Fatal("camera/audio originals and sensor manifest were not persisted", media, err)
	}
	refs := make(map[string]bool)
	for _, ref := range capture.Evidence {
		refs[ref] = true
	}
	for _, entry := range media {
		data, err := os.ReadFile(filepath.Join(a.dir, "evidence", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		digest := hex.EncodeToString(sum[:])
		if !refs[digest] || strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())) != digest {
			t.Fatal("persisted evidence differs from returned digest", entry.Name())
		}
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Samples []json.RawMessage `json:"samples"`
	}
	if err := json.Unmarshal(raw, &saved); err != nil || len(saved.Samples) != 5 {
		t.Fatal("persisted capture omitted a modality", err)
	}
	for _, rawSample := range saved.Samples {
		if _, err := sensing.Decode(rawSample); err != nil {
			t.Fatal("persisted sample violates existing sensing contract", err)
		}
	}

	// A fresh HTTP receipt cannot make previously consumed sensor samples fresh.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var replay map[string]json.RawMessage
		_ = json.Unmarshal(raw, &replay)
		replay["captured_unix_ms"], _ = json.Marshal(time.Now().UnixMilli())
		_ = json.NewEncoder(w).Encode(replay)
	}))
	defer server.Close()
	a.physics = server.URL
	if _, err := a.Capture(); err == nil {
		t.Fatal("replayed sensor capture accepted with a new HTTP receipt")
	}
	t.Logf("run %s: five modalities passed scope, freshness, fusion, persistence and replay checks", state.RunID)
}
