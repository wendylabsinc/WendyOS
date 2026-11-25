package incidentchange

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/internal/gaschange"
	"github.com/wendylabsinc/wendy/go/world/internal/gasfield"
	"github.com/wendylabsinc/wendy/go/world/internal/gasreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/incident"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"image"
	"image/color"
	"image/png"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixture(t *testing.T) (incident.Config, world.Component, world.Component) {
	t.Helper()
	g := gasreplica.Config{Tenant: "lab", Anchor: time.Unix(1795600000, 0).UTC(), Field: gasfield.Config{Site: "lab", Frame: world.FrameRef{ID: "floor", Revision: "1"}, Simulation: world.SimulationContext{RunID: "run", ScenarioVersion: "v1", ClockDomain: "simulation:run", Seed: 31}, Min: [2]float64{-2, 2}, Max: [2]float64{2, 6}, Step: .1, Background: 2}, Assumptions: gasfield.Assumptions{Revision: "wind1", WindRadians: .25, Downwind: 3, Upwind: 1, Crosswind: 1.3}, Gateways: map[string]world.Source{}}
	for _, n := range []string{"east", "west"} {
		g.Gateways[n] = source("gas-"+n, "estimator")
	}
	for i := 0; i < 8; i++ {
		id := fmt.Sprint("gas-", i)
		g.Field.Sensors = append(g.Field.Sensors, gasfield.Sensor{ID: id, Source: source(id, "sensor"), Position: [2]float64{-2 + float64(i%4)*4/3, 2.4 + float64(i/4)*3.2}, Calibration: "c1", SensorType: "scalar-v1"})
	}
	cfg := incident.Config{Gas: g, Visual: fusion.VisualConfig{Tenant: g.Tenant, Site: g.Field.Site, Frame: g.Field.Frame, Anchor: g.Anchor, Simulation: g.Field.Simulation, Package: strings.Repeat("a", 64), Calibration: "c1", Sources: map[string]world.Source{}}, VisualPublisher: source("visual", "fixture"), Readers: map[string]world.Source{"east": source("reader-east", "association"), "west": source("reader-west", "association")}}
	for _, id := range []string{"camera-a", "camera-b", "lidar-site"} {
		if cfg.Visual.Models == nil {
			cfg.Visual.Models = map[string]string{}
		}
		cfg.Visual.Models[id] = "fixture"
		cfg.Visual.Sources[id] = source("camera", id)
	}
	var inputs []world.Component
	for _, s := range g.Field.Sensors {
		sim := g.Field.Simulation
		o := gasfield.Observation{ID: s.ID + "/1", Sensor: s.ID, Source: s.Source, Version: world.Version{Epoch: 1, Sequence: 1}, Site: g.Field.Site, Frame: g.Field.Frame, Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "fixture", Simulation: &sim}, Calibration: s.Calibration, SensorType: s.SensorType, Units: "ppm", Species: "synthetic-tracer", Captured: 2, ValidUntil: 3.5, Value: 2 + 120*gasfield.Kernel(s.Position, [2]float64{0, 4}, g.Assumptions), Uncertainty: 1}
		c, e := gasreplica.ObservationComponent(g, o)
		if e != nil {
			t.Fatal(e)
		}
		inputs = append(inputs, c)
	}
	r, used, _, e := gasreplica.Evaluate(g, inputs, g.Instant(2.1))
	if e != nil || r.State != "localized" {
		t.Fatalf("%+v %v", r, e)
	}
	gas, e := gasreplica.MakeClaim(g, "east", world.Version{Epoch: 1, Sequence: 1}, r, used)
	if e != nil {
		t.Fatal(e)
	}
	return cfg, gas, visualFixture(t, cfg, 0, 2, 1)
}
func source(device, service string) world.Source {
	return world.Source{Device: device, App: "fixture", Service: service, Stream: "state"}
}
func visualFixture(t *testing.T, cfg incident.Config, shift, at float64, seq uint64) world.Component {
	t.Helper()
	images := map[string][]byte{}
	records := []world.Component{}
	for i, id := range []string{"camera-a", "camera-b", "lidar-site"} {
		s := sensing.Sample{SchemaVersion: sensing.Schema, ID: fmt.Sprintf("run/%s/%d", id, seq), SiteID: cfg.Visual.Site, Frame: cfg.Visual.Frame, SensorID: id, Sequence: seq, SampleNS: int64(at * 1e9), RunID: "run", ScenarioVersion: "v1", Seed: 31, PackageSHA256: cfg.Visual.Package, SensorModel: "fixture", PositionM: []float64{shift, 0, 0}, UncertaintyM: .02, CalibrationRevision: "c1"}
		if i < 2 {
			s.Kind = "camera"
			x := shift - 1
			pixel := 42
			if i == 1 {
				x = shift + 1
				pixel = 22
			}
			s.PositionM = []float64{x, 0, 0}
			s.Camera = &sensing.Camera{Width: 64, Height: 48, FX: 40, FY: 40, CX: 32, CY: 24, CameraToSite: []float64{1, 0, 0, x, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}, CalibrationValidForNS: int64(1500 * time.Millisecond)}
			im := image.NewRGBA(image.Rect(0, 0, 64, 48))
			for y := 0; y < 48; y++ {
				for x := 0; x < 64; x++ {
					im.Set(x, y, color.RGBA{G: 180, A: 255})
				}
			}
			for y := 22; y < 26; y++ {
				for x := pixel - 2; x < pixel+2; x++ {
					im.Set(x, y, color.RGBA{R: 255, A: 255})
				}
			}
			var b bytes.Buffer
			png.Encode(&b, im)
			h := sha256.Sum256(b.Bytes())
			digest := hex.EncodeToString(h[:])
			s.Evidence = &sensing.EvidenceRef{SHA256: digest, Size: int64(b.Len()), ContentType: "image/png"}
			images[digest] = b.Bytes()
		} else {
			s.Kind = "lidar"
			s.Lidar = &sensing.Lidar{PointsM: [][3]float64{{shift, 0, 4}, {shift + .04, 0, 4}, {shift + 3, 0, 4}}, MaxRangeM: 6}
		}
		converter, e := sensing.New(sensing.Config{Tenant: cfg.Gas.Tenant, Site: cfg.Visual.Site, RunID: "run", Frame: cfg.Visual.Frame, Source: cfg.Visual.Sources[id], Epoch: 1, Anchor: cfg.Visual.Anchor, TTL: 1500 * time.Millisecond})
		if e != nil {
			t.Fatal(e)
		}
		raw, _ := json.Marshal(s)
		c, e := converter.Convert(raw)
		if e != nil {
			t.Fatal(e)
		}
		records = append(records, c)
	}
	c, e := incident.MakeVisual(cfg, world.Version{Epoch: 1, Sequence: seq}, records, images, cfg.Gas.Instant(at+.1))
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func joinedFixture(t *testing.T, state string) (Config, []world.Component) {
	t.Helper()
	sp, gas, visual := fixture(t)
	tc := gaschange.Config{Public: sp.Gas, MinSpan: .6, MaxGap: 1.5, Rate: .1}
	for _, sensor := range sp.Gas.Field.Sensors[:4] {
		tc.Sensors = append(tc.Sensors, sensor.ID)
	}
	var p gasreplica.Claim
	json.Unmarshal(gas.Payload, &p)
	session, _ := gaschange.New(tc)
	var result gaschange.Result
	for i, at := range []float64{1.2, 1.6, 2.0} {
		samples := []world.Component{}
		for j, c := range p.Inputs {
			if j >= 4 {
				continue
			}
			var o gasfield.Observation
			json.Unmarshal(c.Payload, &o)
			o.ID = fmt.Sprintf("%s/%d", o.Sensor, i)
			o.Version.Sequence = uint64(i + 1)
			o.Captured = at
			o.ValidUntil = at + 1.5
			o.Uncertainty = .1
			if state == "rising" {
				o.Value *= .6 + .2*float64(i)
			}
			c, e := gasreplica.ObservationComponent(sp.Gas, o)
			if e != nil {
				t.Fatal(e)
			}
			samples = append(samples, c)
			if i == 2 {
				p.Inputs[j] = c
			}
		}
		var e error
		result, e = session.Step(samples, sp.Gas.Instant(at+.01))
		if e != nil {
			t.Fatal(e)
		}
	}
	if result.Window.State != state {
		t.Fatalf("temporal fixture: %+v", result)
	}
	region, used, _, e := gasreplica.Evaluate(sp.Gas, p.Inputs, sp.Gas.Instant(2.02))
	if e != nil || region.State != "localized" {
		t.Fatalf("gas fixture: %+v %v", region, e)
	}
	gas, e = gasreplica.MakeClaim(sp.Gas, "east", world.Version{Epoch: 1, Sequence: 2}, region, used)
	if e != nil {
		t.Fatal(e)
	}
	claim, e := gaschange.MakeClaim(tc, "east", world.Version{Epoch: 1, Sequence: 2}, result)
	if e != nil {
		t.Fatal(e)
	}
	return Config{sp, tc}, []world.Component{gas, visual, claim}
}
func TestExactWitnessJoinAndIndependentExpiry(t *testing.T) {
	for _, state := range []string{"rising", "steady"} {
		cfg, records := joinedFixture(t, state)
		s, _ := New(cfg)
		r, e := s.Step(records, cfg.Spatial.Gas.Instant(2.15))
		want := "non_rising"
		if state == "rising" {
			want = "supported_rise"
		}
		if e != nil || r.State != want || len(r.Witnesses) != 4 {
			t.Fatalf("%+v %v", r, e)
		}
		c, e := Component(cfg, "east", world.Version{Epoch: 1, Sequence: 1}, r)
		if e != nil {
			t.Fatal(e)
		}
		if c.Timing.SampleTime.Add(c.TTL).After(cfg.Spatial.Gas.Instant(2.7)) {
			t.Fatal("joined lifetime extended")
		}
		later, e := s.Step(records, cfg.Spatial.Gas.Instant(2.8))
		if e != nil || later.State != "unavailable" || later.Spatial.State != "consistent" {
			t.Fatalf("temporal expiry erased independent spatial layer: %+v %v", later, e)
		}
		if _, e = Component(cfg, "east", world.Version{Epoch: 1, Sequence: 2}, later); e == nil {
			t.Fatal("published stale context")
		}
	}
}
func TestRejectTimestampOnlyJoinAndScope(t *testing.T) {
	cfg, records := joinedFixture(t, "rising")
	for _, name := range []string{"hash", "frame", "run", "calibration"} {
		t.Run(name, func(t *testing.T) {
			rs := own(records)
			var p gaschange.Claim
			json.Unmarshal(rs[2].Payload, &p)
			last := len(p.Inputs) - 1
			if name == "hash" {
				var o gasfield.Observation
				json.Unmarshal(p.Inputs[last].Payload, &o)
				o.ID += "-different-original"
				p.Inputs[last], _ = gasreplica.ObservationComponent(cfg.Spatial.Gas, o)
				w, e := gaschange.Evaluate(cfg.Temporal, p.Inputs, cfg.Spatial.Gas.Instant(p.Window.Evaluated))
				if e != nil {
					t.Fatal(e)
				}
				rs[2], e = gaschange.MakeClaim(cfg.Temporal, "east", rs[2].Version, gaschange.Result{Window: w, Inputs: p.Inputs})
				if e != nil {
					t.Fatal(e)
				}
			} else {
				if name == "frame" {
					rs[2].Frame.Revision = "wrong"
				}
				if name == "run" {
					p.Inputs[last].Provenance.Simulation.RunID = "wrong"
				}
				if name == "calibration" {
					var o gasfield.Observation
					json.Unmarshal(p.Inputs[last].Payload, &o)
					o.Calibration = "wrong"
					p.Inputs[last].Payload, _ = json.Marshal(o)
				}
				if name != "frame" {
					rs[2].Payload, _ = json.Marshal(p)
				}
			}
			s, _ := New(cfg)
			r, e := s.Step(rs, cfg.Spatial.Gas.Instant(2.15))
			if e != nil || r.State == "supported_rise" || r.Spatial.State != "consistent" {
				t.Fatalf("%+v %v", r, e)
			}
		})
	}
}
func TestVersionConflictReorderAndOwnership(t *testing.T) {
	cfg, records := joinedFixture(t, "rising")
	s, _ := New(cfg)
	now := cfg.Spatial.Gas.Instant(2.15)
	r, _ := s.Step(records, now)
	records[2].Provenance.Inputs[0].Simulation.RunID = "mutated"
	c, e := Component(cfg, "east", world.Version{Epoch: 2, Sequence: 1}, r)
	if e != nil {
		t.Fatal(e)
	}
	for _, leaf := range c.Provenance.Inputs {
		if leaf.Simulation.RunID != "run" {
			t.Fatal("input alias")
		}
	}
	c.Provenance.Inputs[0].Simulation.RunID = "output mutation"
	c, e = Component(cfg, "east", world.Version{Epoch: 2, Sequence: 2}, r)
	if e != nil || c.Provenance.Inputs[0].Simulation.RunID != "run" {
		t.Fatal("output alias")
	}
	_, records = joinedFixture(t, "rising")
	valid := own(records)
	bad := own(records)
	bad[2].Provenance.AppVersion = "conflicting-app"
	r, _ = s.Step(bad, now.Add(time.Millisecond))
	if r.State == "supported_rise" {
		t.Fatal("same-version conflict accepted")
	}
	r, _ = s.Step(valid, now.Add(2*time.Millisecond))
	if r.State == "supported_rise" {
		t.Fatal("conflicted original restored")
	}
	var claim gaschange.Claim
	json.Unmarshal(valid[2].Payload, &claim)
	valid[2], e = gaschange.MakeClaim(cfg.Temporal, "east", world.Version{Epoch: 2, Sequence: 1}, gaschange.Result{Window: claim.Window, Inputs: claim.Inputs})
	if e != nil {
		t.Fatal(e)
	}
	r, e = s.Step(valid, now.Add(3*time.Millisecond))
	if e != nil || r.State != "supported_rise" {
		t.Fatalf("new version failed %+v %v", r, e)
	}
	r.State = "non_rising"
	if _, e = Component(cfg, "east", world.Version{Epoch: 3, Sequence: 1}, r); e == nil {
		t.Fatal("mutated result published")
	}
}
func TestCompleteSignedEnvelopeBound(t *testing.T) {
	cfg, rs := joinedFixture(t, "rising")
	s, _ := New(cfg)
	r, _ := s.Step(rs, cfg.Spatial.Gas.Instant(2.15))
	c, e := Component(cfg, "east", world.Version{Epoch: 1, Sequence: 1}, r)
	if e != nil {
		t.Fatal(e)
	}
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	_, root, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now()
	g, e := authority.SignGrant(authority.Grant{Version: authority.Version, ID: "test", Tenant: cfg.Spatial.Gas.Tenant, Site: cfg.Spatial.Gas.Field.Site, Domain: "simulation", Principal: "reader", Kind: "app", Source: c.Key.Source, PublicKey: pub, Scopes: []string{"publish"}, PublishAudience: "node-2", NotBeforeUnixMS: now.Add(-time.Second).UnixMilli(), NotAfterUnixMS: now.Add(time.Minute).UnixMilli()}, root)
	if e != nil {
		t.Fatal(e)
	}
	signed, e := authority.SignRecord(g, key, c, now)
	if e != nil {
		t.Fatal(e)
	}
	if e = gasreplica.CheckSignedSize(signed); e != nil {
		t.Fatal(e)
	}
}
func TestValidSameVersionConflictCannotRecoverByReplay(t *testing.T) {
	cfg, rs := joinedFixture(t, "rising")
	s, _ := New(cfg)
	now := cfg.Spatial.Gas.Instant(2.15)
	r, _ := s.Step(rs, now)
	if r.State != "supported_rise" {
		t.Fatal(r.State)
	}
	bad := own(rs)
	var p gaschange.Claim
	json.Unmarshal(bad[2].Payload, &p)
	p.Window.ValidUntil -= .05
	var e error
	bad[2], e = gaschange.MakeClaim(cfg.Temporal, "east", bad[2].Version, gaschange.Result{Window: p.Window, Inputs: p.Inputs})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = gaschange.ValidateClaim(cfg.Temporal, bad[2], now); e != nil {
		t.Fatal(e)
	}
	r, _ = s.Step(bad, now.Add(time.Millisecond))
	if r.State == "supported_rise" {
		t.Fatal("valid conflicting version accepted")
	}
	r, _ = s.Step(rs, now.Add(2*time.Millisecond))
	if r.State == "supported_rise" {
		t.Fatal("replay recovered conflicted version")
	}
}
func TestDuplicateDeadlineForeignResultAndConcurrentReads(t *testing.T) {
	cfg, rs := joinedFixture(t, "rising")
	s, _ := New(cfg)
	rs[2].Timing.AgeHint = 1200 * time.Millisecond
	now := cfg.Spatial.Gas.Instant(2.15)
	r, e := s.Step(rs, now)
	if e != nil || r.State != "supported_rise" {
		t.Fatal(r.State, e)
	}
	rs[2].Timing.AgeHint = 0
	r, e = s.Step(rs, cfg.Spatial.Gas.Instant(2.55))
	if e != nil || r.State != "unavailable" {
		t.Fatal("duplicate age stripping renewed support", r.State, e)
	}
	s2, _ := New(cfg)
	r, _ = s2.Step(rs, now)
	foreign := own(cfg)
	foreign.Temporal.Rate += .1
	if _, e = Component(foreign, "east", world.Version{Epoch: 1, Sequence: 1}, r); e == nil {
		t.Fatal("foreign contract accepted")
	}
	raw, _ := json.Marshal(r)
	var reconstructed Result
	json.Unmarshal(raw, &reconstructed)
	if _, e = Component(cfg, "east", world.Version{Epoch: 1, Sequence: 1}, reconstructed); e == nil {
		t.Fatal("unsealed result published")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := s2.Step(rs, now)
			if e != nil || r.State != "supported_rise" {
				t.Error(r.State, e)
			}
		}()
	}
	wg.Wait()
}
