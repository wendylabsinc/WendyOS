package gasfield

import (
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"math"
	"math/rand"
	"strings"
	"sync"
	"testing"
)

func fixture() (Config, Assumptions) {
	c := Config{Site: "laboratory", Frame: world.FrameRef{ID: "floor", Revision: "1"}, Simulation: world.SimulationContext{RunID: "test", ScenarioVersion: "v1", ClockDomain: "simulation:test", Seed: 17}, Min: [2]float64{-2, -2}, Max: [2]float64{2, 2}, Step: .1, Background: 2}
	for z := 0; z < 2; z++ {
		for x := 0; x < 4; x++ {
			id := string(rune('a' + z*4 + x))
			c.Sensors = append(c.Sensors, Sensor{ID: id, Source: world.Source{Device: "gateway-" + id, App: "gas", Service: id, Stream: "ppm"}, Position: [2]float64{-2 + float64(x)*4/3, -1.6 + float64(z)*3.2}, Calibration: "c1", SensorType: "synthetic-scalar-v1"})
		}
	}
	return c, Assumptions{Revision: "wind-1", WindRadians: .25, Downwind: 3, Upwind: 1, Crosswind: 1.3}
}
func observations(c Config, a Assumptions, position [2]float64, seed int64, now float64, seq uint64) []Observation {
	rng := rand.New(rand.NewSource(seed))
	out := []Observation{}
	for _, sensor := range c.Sensors {
		sim := c.Simulation
		out = append(out, Observation{ID: sensor.ID + "/sample", Sensor: sensor.ID, Source: sensor.Source, Version: world.Version{Epoch: 1, Sequence: seq}, Site: c.Site, Frame: c.Frame, Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "generator-v1", Simulation: &sim}, Calibration: sensor.Calibration, SensorType: sensor.SensorType, Units: "ppm", Species: "synthetic-tracer", Captured: now, ValidUntil: now + 1.5, Value: c.Background + 120*Kernel(sensor.Position, position, a) + rng.NormFloat64()*.25, Uncertainty: 1})
	}
	return out
}
func TestHeldOutSourceLocations(t *testing.T) {
	c, a := fixture()
	for _, seed := range []int64{31, 811, 4099, 12289} {
		for _, p := range [][2]float64{{-.9, -.5}, {.35, .65}, {-.15, .2}} {
			s := new(Session)
			r, err := s.Step(Request{&c, 2, a, observations(c, a, p, seed, 2, 1)})
			if err != nil {
				t.Fatal(err)
			}
			errorM := math.Hypot(r.Estimate.Position[0]-p[0], r.Estimate.Position[1]-p[1])
			if r.State != "localized" || errorM > .2 {
				t.Fatalf("seed %d source %v: %s error %.3f", seed, p, r.State, errorM)
			}
			if r.Estimate.Score > 3 || len(r.Provenance.Inputs) != 8 || r.Estimate.ValidUntil != 3.5 {
				t.Fatalf("bad evidence %+v", r)
			}
		}
	}
}
func TestMovementAndDeclaredWind(t *testing.T) {
	c, a := fixture()
	s := new(Session)
	for n, p := range [][2]float64{{-.9, -.5}, {0, .3}, {.7, -.3}} {
		a.WindRadians = float64(n) * .65
		a.Revision = string(rune('a' + n))
		r, err := s.Step(Request{&c, float64(n + 1), a, observations(c, a, p, int64(n+100), float64(n+1), uint64(n+1))})
		if err != nil {
			t.Fatal(err)
		}
		if r.State != "localized" || math.Hypot(r.Estimate.Position[0]-p[0], r.Estimate.Position[1]-p[1]) > .2 {
			t.Fatalf("movement %d: %+v", n, r)
		}
	}
}
func TestMismatchBiasGeometryAndMissing(t *testing.T) {
	c, a := fixture()
	for _, name := range []string{"wind-mismatch", "bias", "line", "dropout", "background"} {
		t.Run(name, func(t *testing.T) {
			o := observations(c, a, [2]float64{-.7, .3}, 123, 2, 1)
			switch name {
			case "wind-mismatch":
				a.WindRadians = -1.5
			case "bias":
				o[2].Value += 35
			case "line":
				o = o[:4]
			case "dropout":
				o = o[:3]
			case "background":
				for i := range o {
					o[i].Value = 2
				}
			}
			r, err := new(Session).Step(Request{&c, 2, a, o})
			if err != nil {
				t.Fatal(err)
			}
			if name == "line" {
				if r.State != "ambiguous" && r.State != "unavailable" {
					t.Fatalf("line should be uncertain: %s", r.State)
				}
			} else if r.State != "unavailable" {
				t.Fatalf("%s: %s (%s)", name, r.State, r.Reason)
			}
		})
	}
}
func TestExpiryDuplicatesReorderAndTransaction(t *testing.T) {
	c, a := fixture()
	s := new(Session)
	o := observations(c, a, [2]float64{-.7, .3}, 12, 2, 1)
	r, err := s.Step(Request{&c, 2, a, o})
	if err != nil {
		t.Fatal(err)
	}
	deadline := r.Estimate.ValidUntil
	r, err = s.Step(Request{nil, 3, a, o})
	if err != nil || r.Estimate.ValidUntil != deadline {
		t.Fatalf("duplicate renew: %v", err)
	}
	bad := copyJSON(o)
	bad[7].Calibration = "new"
	bad[0].Version.Sequence = 2
	bad[0].Captured = 3
	bad[0].ValidUntil = 4.5
	if _, err = s.Step(Request{nil, 3, a, bad}); err == nil {
		t.Fatal("partial invalid batch admitted")
	}
	r, err = s.Step(Request{nil, deadline, a, []Observation{}})
	if err != nil || r.State != "unavailable" || r.Coverage != 0 {
		t.Fatalf("expiry: %s %v", r.State, err)
	}
	newer := observations(c, a, [2]float64{-.7, .3}, 13, 4, 2)
	r, err = s.Step(Request{nil, 4, a, newer})
	if err != nil || r.State != "localized" {
		t.Fatal(err)
	}
	r, err = s.Step(Request{nil, 4.1, a, o})
	if err != nil || r.Estimate.ValidUntil != 5.5 {
		t.Fatal("reorder replaced current")
	}
	conflict := copyJSON(newer)
	conflict[0].Value++
	if _, err = s.Step(Request{nil, 4.2, a, conflict}); err == nil {
		t.Fatal("source-version conflict accepted")
	}
	if _, err = s.Step(Request{nil, 3, a, nil}); err == nil {
		t.Fatal("clock rewind")
	}
}
func TestCaptureAlignmentAndOwnedCopies(t *testing.T) {
	c, a := fixture()
	s := new(Session)
	o := observations(c, a, [2]float64{0, 0}, 13, 2, 1)
	r, err := s.Step(Request{&c, 2, a, o})
	if err != nil {
		t.Fatal(err)
	}
	o[0].Provenance.Simulation.RunID = "mutated"
	r.Readings[0].Observation.Value = 9999
	r.Provenance.Inputs[0].Simulation.RunID = "mutated"
	next := observations(c, a, [2]float64{0, 0}, 14, 2.5, 2)
	r, err = s.Step(Request{nil, 2.5, a, next[:3]})
	if err != nil {
		t.Fatal(err)
	}
	if r.Coverage != 3 || r.State != "unavailable" {
		t.Fatal("misaligned samples used")
	}
	if r.Readings[3].Observation.Provenance.Simulation.RunID != "test" {
		t.Fatal("aliased original")
	}
}
func TestScopeAndBounds(t *testing.T) {
	for _, name := range []string{"run", "scenario", "seed", "clock", "frame", "measured", "inferred", "calibration", "source", "future", "lifetime", "units", "nan", "too-many"} {
		t.Run(name, func(t *testing.T) {
			c, a := fixture()
			o := observations(c, a, [2]float64{0, 0}, 10, 2, 1)
			switch name {
			case "run":
				o[0].Provenance.Simulation.RunID = "another"
			case "scenario":
				o[0].Provenance.Simulation.ScenarioVersion = "v2"
			case "seed":
				o[0].Provenance.Simulation.Seed++
			case "clock":
				o[0].Provenance.Simulation.ClockDomain = "physical"
			case "frame":
				o[0].Frame.Revision = "2"
			case "measured":
				o[0].Provenance.Origin = world.Measured
				o[0].Provenance.Simulation = nil
			case "inferred":
				o[0].Provenance.Origin = world.Inferred
			case "calibration":
				o[0].Calibration = "c2"
			case "source":
				o[0].Source.Device = "spoof"
			case "future":
				o[0].Captured = 2.01
			case "lifetime":
				o[0].ValidUntil = 100
			case "units":
				o[0].Units = "percent"
			case "nan":
				o[0].Value = math.NaN()
			case "too-many":
				for len(o) < 33 {
					o = append(o, o[0])
				}
			}
			if _, err := new(Session).Step(Request{&c, 2, a, o}); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
	c, a := fixture()
	c.Step = .001
	if _, err := new(Session).Step(Request{&c, 2, a, nil}); err == nil {
		t.Fatal("unbounded grid")
	}
}
func TestConcurrentViews(t *testing.T) {
	c, a := fixture()
	s := new(Session)
	if _, err := s.Step(Request{&c, 2, a, observations(c, a, [2]float64{0, 0}, 2, 2, 1)}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 3; n++ {
				if _, err := s.Step(Request{nil, 2, a, nil}); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
}
func TestExplicitJSONFields(t *testing.T) {
	c, a := fixture()
	req := Request{&c, 2, a, observations(c, a, [2]float64{0, 0}, 2, 2, 1)}
	raw, _ := json.Marshal(req)
	for _, change := range []func(string) string{
		func(s string) string { return strings.Replace(s, `"captured_s":2,`, "", 1) },
		func(s string) string { return strings.Replace(s, `"wind_radians":0.25,`, "", 1) },
		func(s string) string { return strings.Replace(s, `"min_xz":[-2,-2]`, `"min_xz":[-2]`, 1) },
		func(s string) string { return strings.Replace(s, `"background_ppm":2`, `"background_ppm":null`, 1) },
	} {
		var decoded Request
		if err := json.Unmarshal([]byte(change(string(raw))), &decoded); err == nil {
			t.Fatal("missing/null input silently becomes zero")
		}
	}
}

func TestPlacementBoundsAndRestart(t *testing.T) {
	c, a := fixture()
	c.Sensors[0].PositionUncertainty = .1
	if _, err := new(Session).Step(Request{&c, 2, a, nil}); err == nil {
		t.Fatal("unsupported uncertain placement admitted")
	}
	c, a = fixture()
	o := observations(c, a, [2]float64{0, 0}, 12, 2, 1)
	r, err := new(Session).Step(Request{&c, 4, a, o})
	if err != nil || r.State != "unavailable" || r.Coverage != 0 {
		t.Fatal("restart renewed old input")
	}
	s := new(Session)
	if _, err := s.Step(Request{&c, 2, a, o}); err != nil {
		t.Fatal(err)
	}
	next := observations(c, a, [2]float64{0, 0}, 13, 3, 1)
	for i := range next {
		next[i].Version.Epoch = 2
	}
	r, err = s.Step(Request{nil, 3, a, next})
	if err != nil || r.State != "localized" {
		t.Fatal("valid new publisher epoch refused")
	}
	r, err = s.Step(Request{nil, 3.1, a, o})
	if err != nil || r.Estimate.ValidUntil != 4.5 {
		t.Fatal("old epoch replaced new sample")
	}
	if r.Estimate.Radius < c.Step/math.Sqrt2 {
		t.Fatal("grid discretization omitted from region radius")
	}
}

func TestNonAlignedSearchEdgeIsAmbiguous(t *testing.T) {
	c, a := fixture()
	c.Max[0] = 2.09
	o := observations(c, a, [2]float64{2, 0}, 2, 2, 1)
	for i := range o {
		o[i].Value = c.Background + 120*Kernel(c.Sensors[i].Position, [2]float64{2, 0}, a)
		o[i].Uncertainty = .1
	}
	r, err := new(Session).Step(Request{&c, 2, a, o})
	if err != nil {
		t.Fatal(err)
	}
	if r.State != "ambiguous" || r.Estimate == nil || r.Estimate.Position[0] != 2 {
		t.Fatalf("last sampled grid column must be an ambiguous edge: %+v", r)
	}
}
