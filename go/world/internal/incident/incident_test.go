package incident

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/internal/gasfield"
	"github.com/wendylabsinc/wendy/go/world/internal/gasreplica"
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

func fixture(t *testing.T) (Config, world.Component, world.Component) {
	t.Helper()
	g := gasreplica.Config{Tenant: "lab", Anchor: time.Unix(1795600000, 0).UTC(), Field: gasfield.Config{Site: "lab", Frame: world.FrameRef{ID: "floor", Revision: "1"}, Simulation: world.SimulationContext{RunID: "run", ScenarioVersion: "v1", ClockDomain: "simulation:run", Seed: 31}, Min: [2]float64{-2, 2}, Max: [2]float64{2, 6}, Step: .1, Background: 2}, Assumptions: gasfield.Assumptions{Revision: "wind1", WindRadians: .25, Downwind: 3, Upwind: 1, Crosswind: 1.3}, Gateways: map[string]world.Source{}}
	for _, n := range []string{"east", "west"} {
		g.Gateways[n] = source("gas-"+n, "estimator")
	}
	for i := 0; i < 8; i++ {
		id := fmt.Sprint("gas-", i)
		g.Field.Sensors = append(g.Field.Sensors, gasfield.Sensor{ID: id, Source: source(id, "sensor"), Position: [2]float64{-2 + float64(i%4)*4/3, 2.4 + float64(i/4)*3.2}, Calibration: "c1", SensorType: "scalar-v1"})
	}
	cfg := Config{Gas: g, Visual: fusion.VisualConfig{Tenant: g.Tenant, Site: g.Field.Site, Frame: g.Field.Frame, Anchor: g.Anchor, Simulation: g.Field.Simulation, Package: strings.Repeat("a", 64), Calibration: "c1", Sources: map[string]world.Source{}}, VisualPublisher: source("visual", "fixture"), Readers: map[string]world.Source{"east": source("reader-east", "association"), "west": source("reader-west", "association")}}
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
func visualFixture(t *testing.T, cfg Config, shift, at float64, seq uint64) world.Component {
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
	c, e := MakeVisual(cfg, world.Version{Epoch: 1, Sequence: seq}, records, images, cfg.Gas.Instant(at+.1))
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func TestFiveStatesAndSeparateExpiry(t *testing.T) {
	cfg, g, v := fixture(t)
	for _, tt := range []struct {
		name, want string
		records    []world.Component
	}{{"matching", "consistent", []world.Component{g, v}}, {"gas", "gas_only", []world.Component{g}}, {"visual", "visual_only", []world.Component{v}}, {"absent", "unavailable", nil}, {"displaced", "conflicting", []world.Component{g, visualFixture(t, cfg, 1.4, 2, 2)}}} {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := New(cfg)
			r, e := s.Step(tt.records, cfg.Gas.Instant(2.2))
			if e != nil || r.State != tt.want {
				t.Fatalf("%s %s %v", r.State, r.Reason, e)
			}
			if len(r.Supports) > 0 {
				c, e := Component(cfg, "east", world.Version{Epoch: 1, Sequence: 1}, r)
				if e != nil || c.Provenance.Validate() != nil {
					t.Fatal(e, c.Provenance.Validate())
				}
				if c.Timing.SampleTime.Add(c.TTL).After(cfg.Gas.Instant(3.5)) {
					t.Fatal("extended expiry")
				}
			}
		})
	}
	s, _ := New(cfg)
	a, e := s.Step([]world.Component{g, v}, cfg.Gas.Instant(2.2))
	if e != nil {
		t.Fatal(e)
	}
	b, _ := s.Step([]world.Component{v, g}, cfg.Gas.Instant(2.3))
	if a.InputDigest != b.InputDigest || !a.ValidUntil.Equal(b.ValidUntil) {
		t.Fatal("duplicate renewed identity or deadline")
	}
	b, _ = s.Step([]world.Component{g, v}, cfg.Gas.Instant(3.5))
	if b.State != "unavailable" || len(b.Supports) != 0 {
		t.Fatal("expired originals returned", b.State)
	}
}
func TestTemporalWindowVersionAndTamper(t *testing.T) {
	cfg, g, v := fixture(t)
	late := visualFixture(t, cfg, 0, 2.5, 2)
	s, _ := New(cfg)
	r, e := s.Step([]world.Component{g, late}, cfg.Gas.Instant(2.7))
	if e != nil || r.State != "unavailable" || !strings.Contains(r.Reason, "350 ms") {
		t.Fatal(r.State, r.Reason, e)
	}
	r, _ = s.Step([]world.Component{v}, cfg.Gas.Instant(2.8))
	if r.Visual.State != "unavailable" {
		t.Fatal("reordered accepted")
	}
	for _, name := range []string{"frame", "run", "calibration", "image", "ancestry", "ttl"} {
		t.Run(name, func(t *testing.T) {
			c := v
			var p VisualClaim
			json.Unmarshal(c.Payload, &p)
			switch name {
			case "frame":
				c.Frame.Revision = "wrong"
			case "run":
				p.Contract = strings.Repeat("0", 64)
			case "calibration":
				var sample sensing.Sample
				json.Unmarshal(p.Result.Inputs[0].Payload, &sample)
				sample.CalibrationRevision = "wrong"
				p.Result.Inputs[0].Payload, _ = json.Marshal(sample)
			case "image":
				for k := range p.Images {
					p.Images[k] = []byte("broken")
					break
				}
			case "ancestry":
				c.Provenance.Inputs = c.Provenance.Inputs[:1]
			case "ttl":
				c.TTL += time.Second
			}
			c.Payload, _ = json.Marshal(p)
			if _, e := ValidateVisual(cfg, c, cfg.Gas.Instant(2.2)); e == nil {
				t.Fatal("tamper accepted")
			}
		})
	}
}
func TestConcurrentReaderAndIndependentIdentity(t *testing.T) {
	cfg, g, v := fixture(t)
	a, _ := New(cfg)
	b, _ := New(cfg)
	r1, _ := a.Step([]world.Component{g, v}, cfg.Gas.Instant(2.2))
	r2, _ := b.Step([]world.Component{v, g}, cfg.Gas.Instant(2.2))
	if r1.InputDigest != r2.InputDigest {
		t.Fatal("ordering changed result")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := a.Step([]world.Component{g, v}, cfg.Gas.Instant(2.2)); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
}

func TestAssociationCannotBeMutatedAfterEvaluation(t *testing.T) {
	cfg, g, v := fixture(t)
	s, _ := New(cfg)
	r, e := s.Step([]world.Component{g, v}, cfg.Gas.Instant(2.2))
	if e != nil {
		t.Fatal(e)
	}
	r.ValidUntil = r.ValidUntil.Add(time.Hour)
	if _, e = Component(cfg, "east", world.Version{Epoch: 1, Sequence: 1}, r); e == nil {
		t.Fatal("mutated lifetime accepted")
	}
}

func TestOriginalAndPublishedAncestryAreOwned(t *testing.T) {
	cfg, g, v := fixture(t)
	s, _ := New(cfg)
	r, e := s.Step([]world.Component{g, v}, cfg.Gas.Instant(2.2))
	if e != nil {
		t.Fatal(e)
	}
	original := cfg.Visual.Simulation
	g.Provenance.Inputs[0].Simulation.RunID = "changed-input"
	v.Provenance.Inputs[0].Simulation.Seed++
	first, e := Component(cfg, "east", world.Version{Epoch: 1, Sequence: 1}, r)
	if e != nil {
		t.Fatal(e)
	}
	for _, leaf := range first.Provenance.Inputs {
		if leaf.Simulation == nil || *leaf.Simulation != original {
			t.Fatal("input mutation escaped ownership", leaf)
		}
	}
	first.Provenance.Inputs[0].Simulation.RunID = "changed-output"
	second, e := Component(cfg, "east", world.Version{Epoch: 1, Sequence: 2}, r)
	if e != nil {
		t.Fatal(e)
	}
	for _, leaf := range second.Provenance.Inputs {
		if leaf.Simulation == nil || *leaf.Simulation != original {
			t.Fatal("returned output aliases next publication", leaf)
		}
	}
}
