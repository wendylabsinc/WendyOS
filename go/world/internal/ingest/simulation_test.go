package ingest

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func fixture(t *testing.T) (*Converter, SimulationSample) {
	t.Helper()
	c, err := New(Config{Tenant: "tenant", Site: "office", Frame: world.FrameRef{ID: "floor", Revision: "r1"}, Source: world.Source{Device: "node", App: "bridge", Service: "publisher", Stream: "sim"}, Epoch: 2, RunID: "run", Anchor: time.Unix(100, 0), TTL: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	s := SimulationSample{Schema: "wendy.sim.observation.v0", ID: "run/1", Site: "office", FrameRevision: "r1", Kind: "pose", Sequence: 1, Source: world.Source{Device: "sim", App: "sim", Service: "mujoco", Stream: "pose"}}
	s.Timing.ClockDomain = "simulation:run"
	s.Timing.SampleNS = 100000000
	s.Provenance.Mode = "simulated"
	s.Provenance.RunID = "run"
	s.Provenance.PackageRevision = "p1"
	s.Provenance.PackageSHA256 = strings.Repeat("a", 64)
	s.Provenance.MuJoCoVersion = "3.3.7"
	s.Provenance.SensorModel = "body-position"
	s.Value.Position = []float64{1, 2, 3}
	return c, s
}
func encode(t *testing.T, s SimulationSample) []byte {
	t.Helper()
	b, e := json.Marshal(s)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestConvertPreservesLineageAndUsesAuthenticatedPublisher(t *testing.T) {
	c, s := fixture(t)
	v, e := c.Convert(encode(t, s))
	if e != nil {
		t.Fatal(e)
	}
	if v.Key.Source != c.config.Source || v.Version.Epoch != 2 || v.Provenance.Origin != world.Simulated || !v.Timing.SampleTime.Equal(time.Unix(100, 100000000)) {
		t.Fatal(v)
	}
	var got SimulationSample
	if e = json.Unmarshal(v.Payload, &got); e != nil || got.Source != s.Source || got.Provenance.PackageSHA256 != s.Provenance.PackageSHA256 {
		t.Fatal(got, e)
	}
	if _, e = c.Convert(encode(t, s)); e == nil {
		t.Fatal("duplicate sequence accepted")
	}
	s.Sequence = 2
	s.ID = "run/2"
	s.Kind = "gas"
	v1, n := 10.0, 0.1
	s.Value.Concentration = &v1
	s.Value.Noise = &n
	s.Value.Species = "test"
	s.Value.Unit = "ppm"
	if _, e = c.Convert(encode(t, s)); e != nil {
		t.Fatal(e)
	}
}
func TestConvertRejectsIncompatibleAndMalformedInput(t *testing.T) {
	for _, mutate := range []func(*SimulationSample){func(s *SimulationSample) { s.Provenance.Mode = "measured" }, func(s *SimulationSample) { s.Timing.ClockDomain = "physical" }, func(s *SimulationSample) { s.Site = "elsewhere" }, func(s *SimulationSample) { s.FrameRevision = "r2" }, func(s *SimulationSample) { s.Provenance.PackageSHA256 = "bad" }, func(s *SimulationSample) { s.Timing.SampleNS = -1 }, func(s *SimulationSample) { s.Value.Position = []float64{1} }, func(s *SimulationSample) { s.Kind = "gas" }, func(s *SimulationSample) { s.Sequence = 0 }} {
		c, s := fixture(t)
		mutate(&s)
		if _, e := c.Convert(encode(t, s)); e == nil {
			t.Fatalf("accepted invalid sample %+v", s)
		}
	}
	c, s := fixture(t)
	b := encode(t, s)
	if _, e := c.Convert(append(b, []byte(" {}")...)); e == nil {
		t.Fatal("multiple JSON values accepted")
	}
	if _, e := c.Convert([]byte(strings.Repeat(" ", 65537))); e == nil {
		t.Fatal("oversized sample accepted")
	}
	if _, e := c.Convert(b); e != nil {
		t.Fatal("failed input advanced converter", e)
	}
}
