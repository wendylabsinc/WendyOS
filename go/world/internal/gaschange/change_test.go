package gaschange

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/gasfield"
	"github.com/wendylabsinc/wendy/go/world/internal/gasreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	mathrand "math/rand"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixture(t *testing.T) Config {
	t.Helper()
	p := gasreplica.Config{Tenant: "lab", Anchor: time.Unix(1795600000, 0).UTC(), Field: gasfield.Config{Site: "lab", Frame: world.FrameRef{ID: "floor", Revision: "1"}, Simulation: world.SimulationContext{RunID: "change", ScenarioVersion: "v1", ClockDomain: "simulation:change", Seed: 31}, Min: [2]float64{-2, -2}, Max: [2]float64{2, 2}, Step: .1, Background: 2}, Assumptions: gasfield.Assumptions{Revision: "wind", WindRadians: .25, Downwind: 3, Upwind: 1, Crosswind: 1.3}, Gateways: map[string]world.Source{}}
	c := Config{Public: p, MinSpan: .6, MaxGap: 1.5, Rate: 1}
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
func originals(t *testing.T, c Config, at float64, seq uint64, values ...float64) []world.Component {
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
func rising(t *testing.T, c Config) (*Session, Result) {
	s, _ := New(c)
	var r Result
	for i := 0; i < 4; i++ {
		var e error
		r, e = s.Step(originals(t, c, float64(i)*.4, uint64(i+1)), c.Public.Instant(float64(i)*.4+.01))
		if e != nil {
			t.Fatal(e)
		}
	}
	if r.Window.State != "rising" || len(r.History) != 1 {
		t.Fatalf("expected rising episode: %+v", r)
	}
	return s, r
}
func TestOriginalHistoryDuplicatesGapsExpiryAndRestart(t *testing.T) {
	c := fixture(t)
	s, r := rising(t, c)
	digest := r.Window.InputDigest
	deadline := r.Window.ValidUntil
	last := []world.Component{}
	for _, x := range r.Inputs {
		if x.Version.Sequence == 4 {
			last = append(last, x)
		}
	}
	for _, at := range []float64{1.3, 1.5, 2.0} {
		next, e := s.Step(last, c.Public.Instant(at))
		if e != nil || next.Window.InputDigest != digest || next.Window.ValidUntil != deadline || len(next.History) != 1 {
			t.Fatalf("duplicate advanced history: %+v %v", next, e)
		}
	}
	old, e := s.Step(originals(t, c, .8, 3), c.Public.Instant(2.1))
	if e != nil || old.Window.InputDigest != digest || len(old.Rejected) != 4 {
		t.Fatalf("reordered originals: %+v %v", old, e)
	}
	expired, e := s.Step(nil, c.Public.Instant(2.71))
	if e != nil || expired.Window.State != "unknown" || expired.History[len(expired.History)-1].State != "interrupted" {
		t.Fatalf("silence resolved episode: %+v %v", expired, e)
	}
	restart, _ := New(c)
	warm, e := restart.Step(originals(t, c, 3, 5), c.Public.Instant(3.01))
	if e != nil || warm.Window.State != "warming" || len(warm.History) != 0 {
		t.Fatalf("restart reconstructed dwell %+v %v", warm, e)
	}
	warm, e = s.Step(originals(t, c, 3, 5), c.Public.Instant(3.01))
	if e != nil || warm.Window.State != "warming" {
		t.Fatalf("gap did not warm %+v %v", warm, e)
	}
	if _, e = s.Step(nil, c.Public.Instant(3)); e == nil {
		t.Fatal("clock reversal accepted")
	}
}
func TestBiasSpikeOpposingAndDecline(t *testing.T) {
	c := fixture(t)
	for name, fn := range map[string]func(int, int) float64{"bias": func(i, j int) float64 {
		if j == 0 {
			return 70
		}
		return 20
	}, "spike": func(i, j int) float64 {
		if i == 1 {
			return 50
		}
		return 20
	}, "opposing": func(i, j int) float64 {
		if j == 0 {
			return 30 - float64(i)*4
		}
		return 20 + float64(i)*4
	}, "falling": func(i, j int) float64 { return 50 - float64(i)*4 }} {
		t.Run(name, func(t *testing.T) {
			s, _ := New(c)
			var r Result
			for i := 0; i < 3; i++ {
				values := []float64{}
				for j := 0; j < 4; j++ {
					values = append(values, fn(i, j))
				}
				r, _ = s.Step(originals(t, c, float64(i)*.4, uint64(i+1), values...), c.Public.Instant(float64(i)*.4+.01))
			}
			want := map[string]string{"bias": "steady", "spike": "mixed", "opposing": "mixed", "falling": "falling"}[name]
			if r.Window.State != want {
				t.Fatalf("got %s want %s", r.Window.State, want)
			}
		})
	}
}
func TestClaimReproductionScopeOwnershipAndSignedSize(t *testing.T) {
	c := fixture(t)
	_, r := rising(t, c)
	claim, e := MakeClaim(c, "east", world.Version{Epoch: 1, Sequence: 1}, r)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = ValidateClaim(c, claim, c.Public.Instant(1.3)); e != nil {
		t.Fatal(e)
	}
	r.Inputs[0].Provenance.Simulation.RunID = "mutated"
	if _, e = ValidateClaim(c, claim, c.Public.Instant(1.3)); e != nil {
		t.Fatal("output aliases original")
	}
	if _, e = ValidateClaim(c, claim, c.Public.Instant(r.Window.ValidUntil)); e == nil {
		t.Fatal("expired claim accepted")
	}
	for name, mutate := range map[string]func(*world.Component){"frame": func(x *world.Component) { x.Frame.Revision = "wrong" }, "ttl": func(x *world.Component) { x.TTL += time.Second }, "run": func(x *world.Component) { x.Provenance.Inputs[0].Simulation.RunID = "other" }, "alias": func(x *world.Component) { x.Provenance.Inputs[1] = x.Provenance.Inputs[0] }, "model": func(x *world.Component) { x.Provenance.ModelID = "wrong" }, "publisher": func(x *world.Component) { x.Key.Source = c.Public.Field.Sensors[0].Source }, "duplicate": func(x *world.Component) {
		var p Claim
		_ = json.Unmarshal(x.Payload, &p)
		p.Inputs[1] = p.Inputs[0]
		x.Payload, _ = json.Marshal(p)
	}} {
		t.Run(name, func(t *testing.T) {
			bad := clone(claim)
			mutate(&bad)
			if _, e := ValidateClaim(c, bad, c.Public.Instant(1.3)); e == nil {
				t.Fatal("tamper accepted")
			}
		})
	}
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	_, root, _ := ed25519.GenerateKey(rand.Reader)
	at := c.Public.Instant(1.3)
	g, e := authority.SignGrant(authority.Grant{Version: authority.Version, ID: "test", Tenant: "lab", Site: "lab", Domain: "simulation", Principal: "east", Kind: "app", Source: c.Public.Gateways["east"], PublicKey: pub, Scopes: []string{"publish"}, PublishAudience: "node-1", NotBeforeUnixMS: at.Add(-time.Minute).UnixMilli(), NotAfterUnixMS: at.Add(time.Minute).UnixMilli()}, root)
	if e != nil {
		t.Fatal(e)
	}
	record, e := authority.SignRecord(g, key, claim, at)
	if e != nil {
		t.Fatal(e)
	}
	if e = gasreplica.CheckSignedSize(record); e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(record)
	t.Logf("12 originals complete signed envelope: %d / %d bytes", len(raw), authority.MaxEnvelopeBytes)
	record.Signature = []byte(strings.Repeat("x", authority.MaxEnvelopeBytes))
	if gasreplica.CheckSignedSize(record) == nil {
		t.Fatal("oversize envelope allowed")
	}
}
func TestSourceScopeAndConcurrentReaders(t *testing.T) {
	c := fixture(t)
	s, _ := New(c)
	good := originals(t, c, 1, 1)
	for name, mutate := range map[string]func(*world.Component){"calibration": func(x *world.Component) {
		var o gasfield.Observation
		_ = json.Unmarshal(x.Payload, &o)
		o.Calibration = "other"
		x.Payload, _ = json.Marshal(o)
	}, "run": func(x *world.Component) { x.Provenance.Simulation.RunID = "other" }, "clock": func(x *world.Component) { x.Timing.ClockDomain = "other" }} {
		t.Run(name, func(t *testing.T) {
			bad := clone(good)
			mutate(&bad[0])
			r, e := s.Step(bad, c.Public.Instant(1.01))
			if e != nil || r.Window.State != "unknown" {
				t.Fatalf("bad scope accepted %+v %v", r, e)
			}
		})
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = s.Step(good, c.Public.Instant(1.01)) }()
	}
	wg.Wait()
}

func TestSupersedingInvalidOriginalInterruptsWarmHistory(t *testing.T) {
	c := fixture(t)
	for _, kind := range []string{"calibration", "run", "conflict"} {
		t.Run(kind, func(t *testing.T) {
			s, prior := rising(t, c)
			at, seq := 1.4, uint64(5)
			if kind == "conflict" {
				at, seq = 1.2, 4
			}
			bad := originals(t, c, at, seq)
			var o gasfield.Observation
			_ = json.Unmarshal(bad[0].Payload, &o)
			if kind == "run" {
				o.Provenance.Simulation.RunID = "other"
			} else {
				o.Calibration = "changed"
			}
			bad[0].Payload, _ = json.Marshal(o)
			r, e := s.Step(bad, c.Public.Instant(1.41))
			if e != nil || r.Window.State != "unknown" {
				t.Fatalf("superseded invalid history still usable: %+v %v", r, e)
			}
			event := r.History[len(r.History)-1]
			if event.State != "interrupted" || event.Witness != prior.Window.InputDigest || event.OriginalUntil != prior.Window.ValidUntil {
				t.Fatalf("interrupted event lost original witness: %+v prior=%+v", event, prior.Window)
			}
			// Later valid but older source bytes cannot resurrect the invalidated history.
			r, e = s.Step(originals(t, c, 1.2, 4), c.Public.Instant(1.45))
			if e != nil || r.Window.State != "unknown" {
				t.Fatalf("old repair restored invalidated history: %+v %v", r, e)
			}
		})
	}
	s, prior := rising(t, c)
	foreign := originals(t, c, 1.4, 5)[0]
	foreign.Key.Entity = "unrelated"
	r, e := s.Step([]world.Component{foreign}, c.Public.Instant(1.41))
	if e != nil || r.Window.InputDigest != prior.Window.InputDigest {
		t.Fatalf("foreign key erased history %+v %v", r, e)
	}
}

func TestDelayedDeliveryNeedsNewOriginalAndValidCaptureGap(t *testing.T) {
	cfg := fixture(t)
	s, _ := New(cfg)
	var prior Result
	for i := 0; i < 3; i++ {
		prior, _ = s.Step(originals(t, cfg, float64(i)*.4, uint64(i+1)), cfg.Public.Instant(float64(i)*.4+.01))
	}
	if prior.Window.State != "rising" {
		t.Fatal(prior.Window.State)
	}
	stale, e := s.Step(nil, cfg.Public.Instant(2.31))
	if e != nil || stale.Window.State != "unknown" {
		t.Fatalf("late delivery must become unknown %+v %v", stale, e)
	}
	duplicate, e := s.Step(originals(t, cfg, .8, 3), cfg.Public.Instant(2.32))
	if e != nil || duplicate.Window.State != "unknown" {
		t.Fatal("duplicate restored stale window")
	}
	restored, e := s.Step(originals(t, cfg, 2.2, 4), cfg.Public.Instant(2.4))
	if e != nil || restored.Window.State != "rising" {
		t.Fatalf("delayed real capture with valid 1.4s interval lost: %+v %v", restored, e)
	}
	if len(restored.History) != 3 || restored.History[1].State != "interrupted" || restored.History[2].ID == prior.History[0].ID {
		t.Fatalf("delivery gap hid interruption: %+v", restored.History)
	}
	gap, e := s.Step(originals(t, cfg, 3.8, 5), cfg.Public.Instant(3.81))
	if e != nil || gap.Window.State != "warming" {
		t.Fatalf("actual 1.6s capture gap bridged: %+v %v", gap, e)
	}
	expired, e := s.Step(originals(t, cfg, 3.8, 5), cfg.Public.Instant(7.81))
	if e != nil || expired.Window.State != "unknown" || len(expired.Inputs) != 0 {
		t.Fatalf("expired originals retained as usable: %+v %v", expired, e)
	}
}

func TestClaimCannotManufactureChronologyFromOriginalVersions(t *testing.T) {
	cfg := fixture(t)
	for _, sequences := range [][]uint64{{1, 1, 1}, {3, 2, 1}} {
		inputs := []world.Component{}
		for i, seq := range sequences {
			inputs = append(inputs, originals(t, cfg, float64(i)*.4, seq)...)
		}
		if _, e := Evaluate(cfg, inputs, cfg.Public.Instant(.9)); e == nil {
			t.Fatalf("invalid original version chronology accepted %v", sequences)
		}
	}
}

func TestHeldOutNoiseAndCadenceSeeds(t *testing.T) {
	cfg := fixture(t)
	for _, seed := range []int64{7, 19, 43, 61, 89, 103, 127, 151, 179} {
		for _, slope := range []float64{-10, 0, 10} {
			session, _ := New(cfg)
			rng := mathrand.New(mathrand.NewSource(seed))
			at := 0.0
			var r Result
			for i := 0; i < 4; i++ {
				at += .35 + rng.Float64()*.2
				values := []float64{}
				for j := 0; j < 4; j++ {
					values = append(values, 50+slope*at+(rng.Float64()-.5)*.1)
				}
				var e error
				r, e = session.Step(originals(t, cfg, at, uint64(i+1), values...), cfg.Public.Instant(at+.03))
				if e != nil {
					t.Fatal(e)
				}
			}
			want := "steady"
			if slope < 0 {
				want = "falling"
			}
			if slope > 0 {
				want = "rising"
			}
			if r.Window.State != want {
				t.Fatalf("seed %d slope %g got %s", seed, slope, r.Window.State)
			}
		}
	}
}
