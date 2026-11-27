package fusiondemo

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
)

func TestQuorumParticipantPersistsAlternativeAndExpires(t *testing.T) {
	cfg := config("atlas")
	cfg.CameraQuorum = []string{"camera-a", "camera-b", "camera-c"}
	cfg.Sensors = append(cfg.Sensors, Sensor{"camera-c", "camera", "hub"})
	dir := filepath.Join(t.TempDir(), "participant")
	e, err := New(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	base, media := samples(t, "hub", 1, 250*time.Millisecond)
	var c sensing.Sample
	if err = json.Unmarshal(base[1], &c); err != nil {
		t.Fatal(err)
	}
	c.SensorID = "camera-c"
	c.ID = "office/camera-c/1"
	raw, _ := json.Marshal(c)
	// Keep A and its alternative C; B is catalogued but unavailable.
	hub, err := New(Config{Version: cfg.Version, ParticipantID: "hub", RunID: cfg.RunID, Frame: cfg.Frame, Sensors: cfg.Sensors, CameraQuorum: cfg.CameraQuorum}, filepath.Join(t.TempDir(), "hub"))
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	h := tick(t, hub, Tick{NowNS: 250000000, LocalSamples: append([]json.RawMessage{base[0], raw}, base[2:]...), Media: media})
	scan, _ := samples(t, "atlas", 1, 250*time.Millisecond)
	out := tick(t, e, Tick{NowNS: 250000000, LocalSamples: scan, Received: h.ReplicaRecords, Media: h.Media})
	if out.Result.State != fusion.HazardDetected || out.Result.Quorum.Selected[1] != "camera-c" {
		t.Fatalf("fallback: %+v", out.Result)
	}
	digest := out.Result.InputDigest
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = New(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	reopened := tick(t, e, Tick{NowNS: 250000000})
	if reopened.Result.InputDigest != digest || reopened.Result.State != fusion.HazardDetected {
		t.Fatal("reopen changed original winning evidence")
	}
	expired := tick(t, e, Tick{NowNS: int64(2250 * time.Millisecond)})
	if expired.Result.Hazard != nil || expired.Result.State != fusion.InsufficientEvidence {
		t.Fatal("restart refreshed expired quorum")
	}
}
