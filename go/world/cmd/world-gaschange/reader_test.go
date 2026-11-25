package main

import (
	"encoding/json"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/gaschange"
	"github.com/wendylabsinc/wendy/go/world/internal/gasfield"
	"github.com/wendylabsinc/wendy/go/world/internal/gasreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func readerFixture(t *testing.T) gaschange.Config {
	t.Helper()
	p := gasreplica.Config{Tenant: "lab", Anchor: time.Unix(1795600000, 0).UTC(), Field: gasfield.Config{Site: "lab", Frame: world.FrameRef{ID: "floor", Revision: "1"}, Simulation: world.SimulationContext{RunID: "change", ScenarioVersion: "v1", ClockDomain: "simulation:change", Seed: 31}, Min: [2]float64{-2, -2}, Max: [2]float64{2, 2}, Step: .1, Background: 2}, Assumptions: gasfield.Assumptions{Revision: "wind", WindRadians: .25, Downwind: 3, Upwind: 1, Crosswind: 1.3}, Gateways: map[string]world.Source{}}
	c := gaschange.Config{Public: p, MinSpan: .6, MaxGap: 1.5, Rate: 1}
	for _, n := range []string{"east", "west"} {
		c.Public.Gateways[n] = world.Source{Device: n, App: "gas", Service: "change", Stream: "state"}
	}
	for i := 0; i < 4; i++ {
		id := fmt.Sprint("s", i)
		c.Sensors = append(c.Sensors, id)
		c.Public.Field.Sensors = append(c.Public.Field.Sensors, gasfield.Sensor{ID: id, Source: world.Source{Device: id, App: "gas", Service: "sensor", Stream: "ppm"}, Position: [2]float64{float64(i%2)*2 - 1, float64(i/2)*2 - 1}, Calibration: "c1", SensorType: "scalar-v1"})
	}
	if e := c.Validate(); e != nil {
		t.Fatal(e)
	}
	return c
}
func originals(t *testing.T, c gaschange.Config, at float64, seq uint64, values ...float64) []world.Component {
	t.Helper()
	out := []world.Component{}
	for i, s := range c.Public.Field.Sensors {
		v := 20 + at*10
		if len(values) > 0 {
			v = values[i]
		}
		sim := c.Public.Field.Simulation
		o := gasfield.Observation{ID: fmt.Sprintf("%s/%d", s.ID, seq), Sensor: s.ID, Source: s.Source, Version: world.Version{Epoch: 1, Sequence: seq}, Site: c.Public.Field.Site, Frame: c.Public.Field.Frame, Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "fixture", Simulation: &sim}, Calibration: s.Calibration, SensorType: s.SensorType, Units: "ppm", Species: "synthetic-tracer", Captured: at, ValidUntil: at + 4, Value: v, Uncertainty: .1}
		x, e := gasreplica.ObservationComponent(c.Public, o)
		if e != nil {
			t.Fatal(e)
		}
		out = append(out, x)
	}
	return out
}

func TestIndependentReaderExpiryAndHistoricalWitness(t *testing.T) {
	cfg := readerFixture(t)
	session, _ := gaschange.New(cfg)
	var r gaschange.Result
	for i := 0; i < 3; i++ {
		r, _ = session.Step(originals(t, cfg, float64(i)*.4, uint64(i+1)), cfg.Public.Instant(float64(i)*.4+.01))
	}
	claim, e := gaschange.MakeClaim(cfg, "east", world.Version{Epoch: 1, Sequence: 1}, r)
	if e != nil {
		t.Fatal(e)
	}
	reader := newChangeReader(cfg)
	first, e := reader.step([]world.Component{claim}, cfg.Public.Instant(.9))
	if e != nil || first.State != "rising" {
		t.Fatalf("%+v %v", first, e)
	}
	event := first.Candidates[0].History[0]
	duplicate, e := reader.step([]world.Component{claim}, cfg.Public.Instant(1.5))
	if e != nil || len(duplicate.Candidates[0].History) != 1 || duplicate.Candidates[0].Remaining >= first.Candidates[0].Remaining {
		t.Fatal("duplicate extended history/deadline")
	}
	expired, e := reader.step([]world.Component{claim}, cfg.Public.Instant(2.31))
	if e != nil || expired.State != "unknown" {
		t.Fatalf("expired current state %+v %v", expired, e)
	}
	last := expired.Candidates[0].History[1]
	if last.State != "interrupted" || last.Witness != event.Witness || last.OriginalUntil != event.OriginalUntil {
		t.Fatalf("historical witness lost %+v %+v", last, event)
	}
	tombReader := newChangeReader(cfg)
	_, _ = tombReader.step([]world.Component{claim}, cfg.Public.Instant(.9))
	tomb := claim
	tomb.Tombstone = true
	tomb.Payload = nil
	tomb.Version.Sequence++
	_, _ = tombReader.step([]world.Component{tomb}, cfg.Public.Instant(1))
	old, _ := tombReader.step([]world.Component{claim}, cfg.Public.Instant(1.1))
	if old.State != "unknown" {
		t.Fatal("reordered claim resurrected withdrawn window")
	}
	empty := newChangeReader(cfg)
	restart, _ := empty.step([]world.Component{claim}, cfg.Public.Instant(2.31))
	if restart.State != "unknown" || len(restart.Candidates[0].History) != 0 {
		t.Fatal("expired repair reconstructed episode")
	}
}
func TestPrivateSetupUsesDeclaredThreeNodeChain(t *testing.T) {
	cfg := readerFixture(t)
	cfg.Public.Anchor = time.Now()
	out := filepath.Join(t.TempDir(), "private")
	if e := setup(out, cfg.Public, 29580); e != nil {
		t.Fatal(e)
	}
	for i := 1; i <= 3; i++ {
		var node struct {
			Peers []struct {
				Audience string `json:"audience"`
			} `json:"peers"`
		}
		raw, e := os.ReadFile(filepath.Join(out, fmt.Sprintf("node-%d.json", i)))
		if e != nil {
			t.Fatal(e)
		}
		if e = json.Unmarshal(raw, &node); e != nil {
			t.Fatal(e)
		}
		want := 0
		if i > 1 {
			want = 1
		}
		if len(node.Peers) != want {
			t.Fatal("unexpected cyclic repair topology")
		}
		if i > 1 && node.Peers[0].Audience != fmt.Sprintf("node-%d", i-1) {
			t.Fatal("wrong preceding replica")
		}
	}
	var sensors runtimeConfig
	raw, _ := os.ReadFile(filepath.Join(out, "sensors.json"))
	_ = json.Unmarshal(raw, &sensors)
	for _, c := range sensors.Sensors {
		if c.Audience != "node-1" || c.Grant.Grant.PublishAudience != "node-1" {
			t.Fatal("original publisher owner mismatch")
		}
	}
	if e := setup(out, cfg.Public, 29580); e == nil {
		t.Fatal("private runtime directory overwritten")
	}
}

func TestValidSameVersionConflictLatchesUntilNewerClaim(t *testing.T) {
	cfg := readerFixture(t)
	session, _ := gaschange.New(cfg)
	var r gaschange.Result
	for i := 0; i < 3; i++ {
		r, _ = session.Step(originals(t, cfg, float64(i)*.4, uint64(i+1)), cfg.Public.Instant(float64(i)*.4+.01))
	}
	original, e := gaschange.MakeClaim(cfg, "east", world.Version{Epoch: 1, Sequence: 1}, r)
	if e != nil {
		t.Fatal(e)
	}
	reader := newChangeReader(cfg)
	first, _ := reader.step([]world.Component{original}, cfg.Public.Instant(.9))
	if first.State != "rising" {
		t.Fatal(first)
	}
	r, e = session.Step(originals(t, cfg, 1.2, 4), cfg.Public.Instant(1.21))
	if e != nil {
		t.Fatal(e)
	}
	conflict, e := gaschange.MakeClaim(cfg, "east", original.Version, r)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = gaschange.ValidateClaim(cfg, conflict, cfg.Public.Instant(1.3)); e != nil {
		t.Fatal("test conflict must be individually valid", e)
	}
	blocked, _ := reader.step([]world.Component{conflict}, cfg.Public.Instant(1.3))
	if blocked.State != "unknown" {
		t.Fatal("same-version conflict remained current")
	}
	replay, _ := reader.step([]world.Component{original}, cfg.Public.Instant(1.4))
	if replay.State != "unknown" {
		t.Fatal("original replay restored a conflicted version")
	}
	newer, e := gaschange.MakeClaim(cfg, "east", world.Version{Epoch: 1, Sequence: 2}, r)
	if e != nil {
		t.Fatal(e)
	}
	recovered, _ := reader.step([]world.Component{newer}, cfg.Public.Instant(1.5))
	if recovered.State != "rising" {
		t.Fatal("genuinely newer claim could not recover")
	}
}
