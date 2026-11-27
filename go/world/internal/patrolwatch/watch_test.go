package patrolwatch_test

import (
	"encoding/json"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/gasfield"
	"github.com/wendylabsinc/wendy/go/world/internal/gasreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/patrol"
	. "github.com/wendylabsinc/wendy/go/world/internal/patrolwatch"
	"github.com/wendylabsinc/wendy/go/world/internal/patrolwork"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"testing"
	"time"
)

func fixture(t *testing.T) (Policy, func(float64, [2]float64, string, uint64) world.Component) {
	t.Helper()
	cfg := gasreplica.Config{Tenant: "simulation", Anchor: time.Unix(1795600000, 0).UTC(), Field: gasfield.Config{Site: "studio-01", Frame: world.FrameRef{ID: "floor", Revision: "1"}, Simulation: world.SimulationContext{RunID: "test", ScenarioVersion: "v1", ClockDomain: "simulation:test", Seed: 31}, Min: [2]float64{-2, -2}, Max: [2]float64{2, 2}, Step: .1, Background: 2}, Assumptions: gasfield.Assumptions{Revision: "wind-1", WindRadians: .25, Downwind: 3, Upwind: 1, Crosswind: 1.3}, Gateways: map[string]world.Source{}}
	for _, name := range []string{"east", "west"} {
		cfg.Gateways[name] = world.Source{Device: name, App: "gas", Service: "estimate", Stream: "source"}
	}
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("sensor-%d", i)
		cfg.Field.Sensors = append(cfg.Field.Sensors, gasfield.Sensor{ID: id, Source: world.Source{Device: id, App: "gas", Service: "sensor", Stream: "ppm"}, Position: [2]float64{-2 + float64(i%4)*4/3, -1.6 + float64(i/4)*3.2}, Calibration: "c1", SensorType: "scalar-v1"})
	}
	p := Policy{Method: Method, Contract: cfg, ContractSHA256: cfg.ContractDigest(), ReaderPrincipal: "reader", ReaderAudience: "node-3", MinXZ: []float64{.4, -.9}, MaxXZ: []float64{1.5, .9}, MaxAgeMS: 4000}
	makeClaim := func(at float64, pos [2]float64, name string, seq uint64) world.Component {
		inputs := []world.Component{}
		for _, s := range cfg.Field.Sensors {
			sim := cfg.Field.Simulation
			o := gasfield.Observation{ID: s.ID + "/sample", Sensor: s.ID, Source: s.Source, Version: world.Version{Epoch: 1, Sequence: seq}, Site: cfg.Field.Site, Frame: cfg.Field.Frame, Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "fixture", Simulation: &sim}, Calibration: s.Calibration, SensorType: s.SensorType, Units: "ppm", Species: "synthetic-tracer", Captured: at, ValidUntil: at + 1.5, Value: 2 + 120*gasfield.Kernel(s.Position, pos, cfg.Assumptions), Uncertainty: 1}
			c, e := gasreplica.ObservationComponent(cfg, o)
			if e != nil {
				t.Fatal(e)
			}
			inputs = append(inputs, c)
		}
		result, used, _, e := gasreplica.Evaluate(cfg, inputs, cfg.Instant(at))
		if e != nil {
			t.Fatal(e)
		}
		c, e := gasreplica.MakeClaim(cfg, name, world.Version{Epoch: 1, Sequence: seq}, result, used)
		if e != nil {
			t.Fatal(e)
		}
		return c
	}
	return p, makeClaim
}
func TestSameMillisecondObservationThenTick(t *testing.T) {
	p, _ := fixture(t)
	l, _ := New(p, "test", p.Contract.Field.Frame)
	at := p.Contract.Instant(2)
	l.Observe(nil, "connected", at.Add(time.Hour), at.Add(900*time.Microsecond))
	l.Check(time.UnixMilli(at.UnixMilli()))
	if l.Permanent {
		t.Fatal("same physical millisecond became a permanent rollback")
	}
}
func TestOriginalExpiryOverlapRecoveryAndAuthority(t *testing.T) {
	p, claim := fixture(t)
	l, e := New(p, "test", p.Contract.Field.Frame)
	if e != nil {
		t.Fatal(e)
	}
	now := p.Contract.Instant(2)
	until := now.Add(time.Hour)
	outside := claim(2, [2]float64{-.8, -.4}, "east", 1)
	l.Observe([]world.Component{outside}, "connected", until, now)
	if !l.Ready || !l.Held {
		t.Fatal(l.Reason)
	}
	if e = l.Resume(now); e != nil {
		t.Fatal(e)
	}
	l.Observe(nil, "unavailable", until, now.Add(time.Second))
	if !l.Ready || l.Held {
		t.Fatal("partition erased unexpired original")
	}
	l.Check(now.Add(1500 * time.Millisecond))
	if l.Ready || !l.Held {
		t.Fatal("expired cache permitted motion")
	}
	l.Observe([]world.Component{outside}, "connected", until, now.Add(1600*time.Millisecond))
	if l.Ready {
		t.Fatal("duplicate renewed evidence")
	}
	l.Observe([]world.Component{claim(4, [2]float64{-.8, -.4}, "east", 2)}, "connected", until, p.Contract.Instant(4))
	if !l.Ready || !l.Held {
		t.Fatal("recovery did not remain held")
	}
	_ = l.Resume(p.Contract.Instant(4))
	l.Observe([]world.Component{claim(5, [2]float64{.8, .2}, "east", 3)}, "connected", until, p.Contract.Instant(5))
	if l.Ready || !l.Held {
		t.Fatal("overlap accepted")
	}
	l.Observe([]world.Component{claim(6, [2]float64{-.8, -.4}, "east", 4), claim(6, [2]float64{.8, .2}, "west", 1)}, "connected", until, p.Contract.Instant(6))
	if l.Ready || l.Selection.State != "disputed" {
		t.Fatal("disagreement accepted", l.Selection.State)
	}
	l.Observe([]world.Component{claim(7, [2]float64{-.8, -.4}, "east", 5)}, "connected", until, p.Contract.Instant(7))
	l.Observe(nil, "refused", until, p.Contract.Instant(7.1))
	if l.Ready {
		t.Fatal("authorization refusal reused cache")
	}
	l.Observe([]world.Component{claim(8, [2]float64{-.8, -.4}, "east", 6)}, "connected", p.Contract.Instant(8), p.Contract.Instant(8))
	if l.Ready {
		t.Fatal("expired reader accepted")
	}
}
func TestPolicyScopeAndStrictCorners(t *testing.T) {
	p, _ := fixture(t)
	if _, e := New(p, "different", p.Contract.Field.Frame); e == nil {
		t.Fatal("wrong run")
	}
	raw, _ := json.Marshal(p)
	for _, malformed := range [][]byte{
		append(append([]byte(nil), raw...), []byte(` {}`)...),
		append([]byte(`{"method":"duplicate",`), raw[1:]...),
		append([]byte(`{"unknown":true,`), raw[1:]...),
	} {
		var decoded Policy
		if json.Unmarshal(malformed, &decoded) == nil {
			t.Fatal("non-canonical policy accepted")
		}
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	for _, key := range []string{"min_xz", "max_xz"} {
		old := body[key]
		body[key] = []any{nil, 1}
		b, _ := json.Marshal(body)
		var bad Policy
		if json.Unmarshal(b, &bad) == nil {
			t.Fatal("null corner accepted")
		}
		body[key] = old
	}
	p.ContractSHA256 = "wrong"
	if p.Validate() == nil {
		t.Fatal("changed catalog accepted")
	}
}

func TestCapturedTickCannotUndoNewerObservationButClockRollbackHolds(t *testing.T) {
	p, _ := fixture(t)
	l, _ := New(p, "test", p.Contract.Field.Frame)
	at := p.Contract.Instant(2)
	l.Observe(nil, "connected", at.Add(time.Hour), at)
	l.CheckTick(at.Add(-200 * time.Millisecond).UnixMilli())
	if l.Permanent {
		t.Fatal("old captured engine tick classified as a clock reading")
	}
	l.Observe(nil, "connected", at.Add(time.Hour), at.Add(-time.Millisecond))
	if !l.Permanent || l.Ready {
		t.Fatal("genuine observation clock rollback was not latched")
	}
}

type fakeNav struct{ goals, cancels int }

func (f *fakeNav) Plan(patrol.Point) (patrol.Route, error) {
	return patrol.Route{Reachable: true, Length: 1}, nil
}
func (f *fakeNav) Goal(string, patrol.Point) error  { f.goals++; return nil }
func (f *fakeNav) Cancel() error                    { f.cancels++; return nil }
func (f *fakeNav) Capture() (patrol.Capture, error) { return patrol.Capture{}, nil }
func TestEngineHoldReturnResumeStopAndOtherGates(t *testing.T) {
	p, claim := fixture(t)
	now := p.Contract.Instant(2)
	f := &fakeNav{}
	e, err := patrol.New("mission", now.Add(time.Minute).UnixMilli(), []patrol.Room{{ID: "one", Name: "Room", Polygon: [][2]float64{{0, 0}, {2, 0}, {2, 2}}, Candidates: []patrol.Point{{X: 1, Y: 1}}}}, f)
	if err != nil {
		t.Fatal(err)
	}
	if e.SetReturn(patrol.Point{}, 0) != nil || e.SetWorldWatch(p, "test", p.Contract.Field.Frame) != nil {
		t.Fatal("setup")
	}
	feed := func(at float64, pos [2]float64, seq uint64) {
		e.ObserveWorld([]world.Component{claim(at, pos, "east", seq)}, "connected", now.Add(time.Hour), p.Contract.Instant(at))
	}
	feed(2, [2]float64{-.8, -.4}, 1)
	if e.State.State != "world_held" || f.goals != 0 {
		t.Fatal("initial hold missing")
	}
	if err = e.Control("resume", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err = e.Control("recall", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	nav := patrol.Navigation{Ready: true, Stamp: 2, SensorAge: .1}
	_ = e.Tick(now.UnixMilli(), nav)
	if f.goals != 1 || e.State.State != "returning" {
		t.Fatal("return not dispatched", e.State.State)
	}
	feed(2.2, [2]float64{.8, .2}, 2)
	if e.State.State != "world_held" || f.cancels == 0 {
		t.Fatal("return not held")
	}
	feed(2.4, [2]float64{-.8, -.4}, 3)
	_ = e.Tick(p.Contract.Instant(2.4).UnixMilli(), nav)
	if f.goals != 1 || !e.State.WorldWatch.Held {
		t.Fatal("automatically resumed")
	}
	e.State.Work = &patrolwork.Ledger{Valid: false, Permanent: true}
	if e.Control("resume", p.Contract.Instant(2.4).UnixMilli()) == nil {
		t.Fatal("World recovery erased exhausted work")
	}
	e.State.Work = nil
	if e.Control("resume", p.Contract.Instant(2.4).UnixMilli()) != nil {
		t.Fatal("resume failed")
	}
	nav.Ready = false
	_ = e.Tick(p.Contract.Instant(2.4).UnixMilli(), nav)
	if e.State.State != "return_held" || f.goals != 1 {
		t.Fatal("World evidence supplied localization")
	}
	_ = e.Control("stop", p.Contract.Instant(2.4).UnixMilli())
	feed(2.6, [2]float64{-.8, -.4}, 4)
	if e.State.State != "stopped" || e.Control("resume", p.Contract.Instant(2.6).UnixMilli()) == nil {
		t.Fatal("World update erased Stop")
	}
}

func TestReplacingSnapshotDoesNotRetainPreviousRecordProvenance(t *testing.T) {
	p, claim := fixture(t)
	c := claim(2, [2]float64{-.8, -.4}, "east", 1)
	var payload gasreplica.Claim
	if err := json.Unmarshal(c.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	l, _ := New(p, "test", p.Contract.Field.Frame)
	now := p.Contract.Instant(2)
	// Query ordering changes as expiring raw observations enter/leave a replica.
	// A derived claim must not inherit a raw record's omitted Simulation pointer.
	l.Observe([]world.Component{payload.Inputs[0]}, "connected", now.Add(time.Hour), now)
	l.Observe([]world.Component{c}, "connected", now.Add(time.Hour), now.Add(time.Millisecond))
	if !l.Ready {
		t.Fatalf("valid replacement rejected after prior raw observation: %+v", l.Selection.Candidates)
	}
}
