package main

import (
	"bytes"
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/patrol"
	"github.com/wendylabsinc/wendy/go/world/internal/patrolenvironment"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"os"
	"path/filepath"
	"testing"
)

func TestEnvironmentSelectionIsSignedAndCannotBeRemoved(t *testing.T) {
	rooms := []patrol.Room{{ID: "office"}}
	path := filepath.Join(t.TempDir(), "schedule.json")
	p := patrolenvironment.Policy{SensorID: "gas-1", Calibration: "go2-virtual-mounts-v1", Species: "synthetic-test-gas", Unit: "ppm", HoldPPM: 80, ClearPPM: 65, ClearForMS: 2000}
	raw, _ := json.Marshal(scheduleDocument{RevisitSeconds: map[string]int{"office": 300}, Environment: &p})
	_ = os.WriteFile(path, raw, 0600)
	cfg, err := readPatrolConfig(path, rooms)
	if err != nil {
		t.Fatal(err)
	}
	approval, spec, err := signFixture("environment", world.FrameRef{ID: "office", Revision: "v1"}, rooms, 600, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Patrol.Environment == nil || *spec.Patrol.Environment != p || !bytes.Contains(approval.Instruction.Payload, []byte(`"environment"`)) {
		t.Fatal("environment selection not retained")
	}
	store, _, _, _, err := admitApproval(t.TempDir(), "environment", approval, approval.RootPublicKey, spec)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	spec.Patrol.Environment = nil
	altered, _ := json.Marshal(spec)
	approval.Instruction.Payload = altered
	if store, _, _, _, err = admitApproval(t.TempDir(), "environment", approval, approval.RootPublicKey, spec); err == nil {
		store.Close()
		t.Fatal("removed signed environmental policy accepted")
	}
	_, legacy, err := signFixture("legacy", world.FrameRef{ID: "office", Revision: "v1"}, rooms, 600)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(legacy)
	if bytes.Contains(raw, []byte(`"environment"`)) {
		t.Fatal("absent policy changed legacy canonical payload")
	}
}
