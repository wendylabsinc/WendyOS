package fusion

import (
	"context"
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"testing"
	"time"
)

func TestVisualOnlyRetainsImageGeometryAndValidation(t *testing.T) {
	f := spatialFixture(t)
	var records []world.Component
	cfg := VisualConfig{Tenant: f.cfg.Tenant, Site: f.cfg.Site, Frame: f.cfg.Frame, Anchor: f.now.Add(-500 * time.Millisecond), Package: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Calibration: "c1", Simulation: world.SimulationContext{RunID: "run", ScenarioVersion: "v1", ClockDomain: "simulation:run"}, Sources: map[string]world.Source{}}
	for _, i := range []int{0, 1, 4} {
		c := f.records[i]
		s, _ := sensing.FromComponent(c)
		if i == 4 {
			s.SensorID = "lidar-site"
			s.ID = "run/lidar-site/1"
			c.Key.Source.Service = "lidar-site"
			c.Key.Entity = "run/lidar-site"
		}
		s.CalibrationRevision = "c1"
		c.Payload = marshalVisual(t, s)
		if cfg.Models == nil {
			cfg.Models = map[string]string{}
		}
		cfg.Models[s.SensorID] = s.SensorModel
		cfg.Sources[s.SensorID] = c.Key.Source
		records = append(records, c)
	}
	load := func(_ context.Context, e sensing.EvidenceRef) ([]byte, error) { return f.blobs[e.SHA256], nil }
	run := func() (VisualResult, error) {
		return EvaluateVisual(context.Background(), cfg, records, load, func() time.Time { return f.now })
	}
	out, e := run()
	if e != nil || !out.Spatial.Associated || len(out.Inputs) != 3 {
		t.Fatalf("%+v %v", out, e)
	}
	records[0].TTL = time.Millisecond
	if _, e = run(); e == nil {
		t.Fatal("expired accepted")
	}
	records[0].TTL = 2 * time.Second
	records[0].Frame.Revision = "wrong"
	if _, e = run(); e == nil {
		t.Fatal("wrong frame accepted")
	}
	records[0].Frame = cfg.Frame
	records[0] = records[1]
	if _, e = run(); e == nil {
		t.Fatal("duplicate source accepted")
	}
}

func marshalVisual(t *testing.T, v any) []byte {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
