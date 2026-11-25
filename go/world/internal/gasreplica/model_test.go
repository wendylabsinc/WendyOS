package gasreplica_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/gasfield"
	"github.com/wendylabsinc/wendy/go/world/internal/gasreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/gasview"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func fixture(t *testing.T) (gasreplica.Config, []world.Component) {
	t.Helper()
	cfg := gasreplica.Config{Tenant: "lab", Anchor: time.Unix(1795600000, 0).UTC(), Field: gasfield.Config{Site: "lab", Frame: world.FrameRef{ID: "floor", Revision: "1"}, Simulation: world.SimulationContext{RunID: "test", ScenarioVersion: "v1", ClockDomain: "simulation:test", Seed: 31}, Min: [2]float64{-2, -2}, Max: [2]float64{2, 2}, Step: .1, Background: 2}, Assumptions: gasfield.Assumptions{Revision: "wind-1", WindRadians: .25, Downwind: 3, Upwind: 1, Crosswind: 1.3}, Gateways: map[string]world.Source{}}
	for _, name := range []string{"east", "west"} {
		cfg.Gateways[name] = world.Source{Device: name, App: "gas", Service: "estimate", Stream: "source"}
	}
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("sensor-%d", i)
		cfg.Field.Sensors = append(cfg.Field.Sensors, gasfield.Sensor{ID: id, Source: world.Source{Device: id, App: "gas", Service: "sensor", Stream: "ppm"}, Position: [2]float64{-2 + float64(i%4)*4/3, -1.6 + float64(i/4)*3.2}, Calibration: "c1", SensorType: "scalar-v1"})
	}
	return cfg, originals(t, cfg, [2]float64{-.8, -.4}, 2, 1)
}
func originals(t *testing.T, cfg gasreplica.Config, pos [2]float64, at float64, seq uint64) []world.Component {
	t.Helper()
	out := []world.Component{}
	for _, s := range cfg.Field.Sensors {
		sim := cfg.Field.Simulation
		o := gasfield.Observation{ID: s.ID + "/sample", Sensor: s.ID, Source: s.Source, Version: world.Version{Epoch: 1, Sequence: seq}, Site: cfg.Field.Site, Frame: cfg.Field.Frame, Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "generator-v1", Simulation: &sim}, Calibration: s.Calibration, SensorType: s.SensorType, Units: "ppm", Species: "synthetic-tracer", Captured: at, ValidUntil: at + 1.5, Value: 2 + 120*gasfield.Kernel(s.Position, pos, cfg.Assumptions), Uncertainty: 1}
		c, e := gasreplica.ObservationComponent(cfg, o)
		if e != nil {
			t.Fatal(e)
		}
		out = append(out, c)
	}
	return out
}
func claim(t *testing.T, cfg gasreplica.Config, inputs []world.Component, name string, at float64, version world.Version) world.Component {
	t.Helper()
	r, used, held, e := gasreplica.Evaluate(cfg, inputs, cfg.Instant(at))
	if e != nil || len(held) != 0 || r.Estimate == nil {
		t.Fatalf("evaluate %s %v %v", r.State, held, e)
	}
	c, e := gasreplica.MakeClaim(cfg, name, version, r, used)
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func TestRoundTripExpiryAndIndependentClaims(t *testing.T) {
	cfg, inputs := fixture(t)
	east := claim(t, cfg, inputs, "east", 2.1, world.Version{Epoch: 1, Sequence: 1})
	west := claim(t, cfg, inputs, "west", 2.1, world.Version{Epoch: 1, Sequence: 1})
	for _, c := range []world.Component{east, west} {
		v, e := gasreplica.ValidateClaim(cfg, c, cfg.Instant(2.2))
		if e != nil || v.Result.Coverage != 8 || v.Result.Estimate.ValidUntil != 3.5 {
			t.Fatalf("round trip %+v %v", v, e)
		}
		if _, e = gasreplica.ValidateClaim(cfg, c, cfg.Instant(3.5)); e == nil {
			t.Fatal("expired claim accepted")
		}
	}
	if gasreplica.ComponentHash(east) == gasreplica.ComponentHash(west) {
		t.Fatal("independent sources coalesced")
	}
	selector, _ := gasview.New(cfg)
	s, e := selector.Step([]world.Component{east, west}, cfg.Instant(2.2))
	if e != nil || s.Gateway != "east" || s.Result.Coverage != 8 {
		t.Fatalf("selection %+v %v", s, e)
	}
	// Only the surviving gateway's original readings advance. Expired east cannot
	// be revived by retransmitting its original signed component.
	fresh := originals(t, cfg, [2]float64{-.8, -.4}, 3, 2)
	west = claim(t, cfg, fresh, "west", 3.1, world.Version{Epoch: 1, Sequence: 2})
	s, e = selector.Step([]world.Component{east, west}, cfg.Instant(3.6))
	if e != nil || s.Gateway != "west" || s.Hash != gasreplica.ComponentHash(west) {
		t.Fatalf("fallback %+v %v", s, e)
	}
	s, e = selector.Step([]world.Component{east, west}, cfg.Instant(4.5))
	if e != nil || s.State != "unavailable" || s.Result.Estimate != nil {
		t.Fatalf("duplicates revived %+v %v", s, e)
	}
}
func TestClaimScopeSupportAndModelTampering(t *testing.T) {
	cfg, inputs := fixture(t)
	base := claim(t, cfg, inputs, "east", 2.1, world.Version{Epoch: 1, Sequence: 1})
	cases := map[string]func(*world.Component){
		"frame": func(c *world.Component) { c.Frame.Revision = "other" }, "publisher": func(c *world.Component) { c.Key.Source = cfg.Field.Sensors[0].Source }, "header-ttl": func(c *world.Component) { c.TTL += time.Second },
		"model": func(c *world.Component) {
			var p gasreplica.Claim
			_ = json.Unmarshal(c.Payload, &p)
			p.Contract = strings.Repeat("0", 64)
			c.Payload, _ = json.Marshal(p)
		},
		"repeated-support": func(c *world.Component) {
			var p gasreplica.Claim
			_ = json.Unmarshal(c.Payload, &p)
			p.Inputs[1] = p.Inputs[0]
			c.Payload, _ = json.Marshal(p)
		},
		"calibration": func(c *world.Component) {
			var p gasreplica.Claim
			_ = json.Unmarshal(c.Payload, &p)
			var o gasfield.Observation
			_ = json.Unmarshal(p.Inputs[0].Payload, &o)
			o.Calibration = "other"
			p.Inputs[0].Payload, _ = json.Marshal(o)
			c.Payload, _ = json.Marshal(p)
		},
		"run": func(c *world.Component) {
			var p gasreplica.Claim
			_ = json.Unmarshal(c.Payload, &p)
			p.Inputs[0].Provenance.Simulation.RunID = "other"
			c.Payload, _ = json.Marshal(p)
		},
		"mask": func(c *world.Component) {
			var p gasreplica.Claim
			_ = json.Unmarshal(c.Payload, &p)
			p.Region.Mask[0] ^= 1
			c.Payload, _ = json.Marshal(p)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			var c world.Component
			raw, _ := json.Marshal(base)
			_ = json.Unmarshal(raw, &c)
			mutate(&c)
			if _, e := gasreplica.ValidateClaim(cfg, c, cfg.Instant(2.2)); e == nil {
				t.Fatal("tampered claim accepted")
			}
		})
	}
}
func TestConflictAndRestartHighwater(t *testing.T) {
	cfg, inputs := fixture(t)
	east := claim(t, cfg, inputs, "east", 2.1, world.Version{Epoch: 1, Sequence: 2})
	west := claim(t, cfg, originals(t, cfg, [2]float64{.8, .4}, 2, 1), "west", 2.1, world.Version{Epoch: 1, Sequence: 1})
	s, _ := gasview.New(cfg)
	out, e := s.Step([]world.Component{east, west}, cfg.Instant(2.2))
	if e != nil || out.State != "disputed" || out.Result.Estimate != nil {
		t.Fatalf("conflict %+v %v", out, e)
	}
	east.Version = world.Version{Epoch: 2, Sequence: 1}
	out, e = s.Step([]world.Component{east}, cfg.Instant(2.3))
	if e != nil || out.Gateway != "east" {
		t.Fatalf("restart %+v %v", out, e)
	}
	east.Version = world.Version{Epoch: 1, Sequence: 500}
	out, e = s.Step([]world.Component{east}, cfg.Instant(2.4))
	if e != nil || out.State != "unavailable" {
		t.Fatal("reordered epoch accepted")
	}
}
func TestTransitAgeAndConcurrentReader(t *testing.T) {
	cfg, inputs := fixture(t)
	c := claim(t, cfg, inputs, "east", 2.1, world.Version{Epoch: 1, Sequence: 1})
	c.Timing.AgeHint = time.Second
	s, _ := gasview.New(cfg)
	out, e := s.Step([]world.Component{c}, cfg.Instant(2.2))
	if e != nil || out.Candidates[0].Remaining > .500001 {
		t.Fatalf("age %+v %v", out, e)
	}
	c.Timing.AgeHint = 0
	out, e = s.Step([]world.Component{c}, cfg.Instant(2.8))
	if e != nil || out.State != "unavailable" {
		t.Fatal("duplicate transit age renewed deadline")
	}
	s, _ = gasview.New(cfg)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = s.Step([]world.Component{c}, cfg.Instant(2.2)) }()
	}
	wg.Wait()
}
func TestActualSignedEnvelopeFits(t *testing.T) {
	cfg, inputs := fixture(t)
	cfg.Field.Max = [2]float64{3.875, 3.875}
	cfg.Field.Min = [2]float64{-4, -4}
	cfg.Field.Step = .125 // exactly 4096 grid cells
	for i := 0; i < 8; i++ {
		sensor := cfg.Field.Sensors[i]
		sensor.ID = fmt.Sprintf("extra-%d", i)
		sensor.Source.Device = sensor.ID
		cfg.Field.Sensors = append(cfg.Field.Sensors, sensor)
	}
	inputs = originals(t, cfg, [2]float64{-.8, -.4}, 2, 1)
	c := claim(t, cfg, inputs, "east", 2.1, world.Version{Epoch: 1, Sequence: 1})
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	_, root, _ := ed25519.GenerateKey(rand.Reader)
	now := cfg.Instant(2)
	g, e := authority.SignGrant(authority.Grant{Version: 1, PublishAudience: "node-1", ID: "grant", Tenant: cfg.Tenant, Site: cfg.Field.Site, Domain: "simulation", Principal: "gateway", Kind: "app", Source: cfg.Gateways["east"], PublicKey: pub, Scopes: []string{"publish"}, NotBeforeUnixMS: now.Add(-time.Minute).UnixMilli(), NotAfterUnixMS: now.Add(time.Hour).UnixMilli()}, root)
	if e != nil {
		t.Fatal(e)
	}
	record, e := authority.SignRecord(g, key, c, now)
	if e != nil {
		t.Fatal(e)
	}
	if e = gasreplica.CheckSignedSize(record); e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(record)
	t.Logf("complete envelope %d bytes against %d-byte admission bound", len(raw), authority.MaxEnvelopeBytes)
	record.Component.Payload = make([]byte, authority.MaxEnvelopeBytes)
	if e = gasreplica.CheckSignedSize(record); e == nil {
		t.Fatal("oversized complete envelope allowed")
	}
}

func TestAmbiguousGeometryIsNotPromotedByPublication(t *testing.T) {
	cfg, inputs := fixture(t)
	r, used, _, e := gasreplica.Evaluate(cfg, inputs[:4], cfg.Instant(2.1))
	if e != nil {
		t.Fatal(e)
	}
	if r.State == "localized" {
		t.Fatal("collinear support promoted")
	}
	if r.Estimate != nil {
		c, e := gasreplica.MakeClaim(cfg, "east", world.Version{Epoch: 1, Sequence: 1}, r, used)
		if e != nil {
			t.Fatal(e)
		}
		v, e := gasreplica.ValidateClaim(cfg, c, cfg.Instant(2.2))
		if e != nil || v.Claim.State != "ambiguous" {
			t.Fatalf("ambiguous claim changed: %s %v", v.Claim.State, e)
		}
	}
}
