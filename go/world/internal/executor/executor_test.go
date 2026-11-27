package executor

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func sample() State {
	return State{Session: "session", BootID: "boot", Position: [3]float64{-1, .2, 0}, SampleUnixMS: time.Now().UnixMilli(), Frame: world.FrameRef{ID: "office-floor", Revision: "office-frame-v1"}, Sensors: map[string]bool{"gas": true, "local-obstacles": true}, ObstacleClear: true, Energy: 1, Synthetic: true}
}
func record() tasking.Record {
	return tasking.Record{Spec: tasking.Spec{ID: "one", Type: "wendy.nav.go_to.v1", Class: mission.Move, Mode: mission.Autonomous, Domain: mission.Simulation, Frame: sample().Frame, Area: tasking.Bounds{Min: [3]float64{-2, 0, -1}, Max: [3]float64{2, 1, 1}}, Target: [3]float64{1, .2, 0}, SpeedMPS: .4, ToleranceM: .03}, Status: mission.Status{Phase: mission.Running, Version: 2}}
}

type fakeAdapter struct {
	state           State
	commands, stops int
	velocity        float64
	err             error
}

func (a *fakeAdapter) Session(context.Context) (State, error)     { return a.state, a.err }
func (a *fakeAdapter) State(context.Context) (State, error)       { return a.state, a.err }
func (a *fakeAdapter) Stop(context.Context, string, uint64) error { a.stops++; return nil }
func (a *fakeAdapter) Command(_ context.Context, _ string, _ uint64, v float64, _ time.Duration) error {
	a.commands++
	a.velocity = v
	return nil
}

type fakeGateway struct {
	records      []tasking.Record
	decision     tasking.Decision
	evaluations  int
	err          error
	perTask      map[string]tasking.Decision
	evaluateHook func()
}

func (g *fakeGateway) Begin(context.Context) (uint64, error)           { return 1, nil }
func (g *fakeGateway) Tasks(context.Context) ([]tasking.Record, error) { return g.records, g.err }
func (g *fakeGateway) Evaluate(_ context.Context, id string, _ uint64, _ tasking.Telemetry) (tasking.Decision, error) {
	g.evaluations++
	if g.evaluateHook != nil {
		g.evaluateHook()
	}
	if g.perTask != nil {
		return g.perTask[id], g.err
	}
	return g.decision, g.err
}
func controller(t *testing.T) (*Controller, *fakeAdapter, *fakeGateway) {
	t.Helper()
	a := &fakeAdapter{state: sample()}
	r := record()
	g := &fakeGateway{records: []tasking.Record{r}, decision: tasking.Decision{Action: mission.Continue, Record: r}}
	c := &Controller{Adapter: a, Gateway: g}
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c, a, g
}
func TestControllerCommandsOnlyCurrentRunningDecision(t *testing.T) {
	c, a, g := controller(t)
	if _, err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.commands != 1 || a.velocity <= 0 {
		t.Fatal("no commanded motion")
	}
	g.decision.Action = mission.Hold
	if _, err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.commands != 1 || a.stops != 1 {
		t.Fatal("hold must brake")
	}
	g.decision.Action = mission.Continue
	g.decision.Record.Spec.ID = "other"
	if _, err := c.Tick(context.Background()); err == nil {
		t.Fatal("accepted mismatched decision")
	}
	if a.commands != 1 {
		t.Fatal("mismatched task renewed control")
	}
}
func TestStaleSessionAndMalformedStateCannotReachEvaluator(t *testing.T) {
	for _, mutate := range []func(*State){func(s *State) { s.Session = "old" }, func(s *State) { s.BootID = "restarted" }, func(s *State) { s.Position[0] = math.NaN() }, func(s *State) { delete(s.Sensors, "gas") }} {
		c, a, g := controller(t)
		mutate(&a.state)
		if _, err := c.Tick(context.Background()); err == nil {
			t.Fatal("invalid state accepted")
		}
		if a.commands != 0 || g.evaluations != 0 {
			t.Fatal("invalid state crossed authority boundary")
		}
	}
}
func TestNoTaskAndGatewayFailureNeverRenewControl(t *testing.T) {
	c, a, g := controller(t)
	g.records = nil
	if _, err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.stops != 1 || a.commands != 0 {
		t.Fatal("idle must brake")
	}
	g.err = errors.New("offline")
	if _, err := c.Tick(context.Background()); err == nil {
		t.Fatal("missing gateway failure")
	}
	if a.commands != 0 {
		t.Fatal("failed decision renewed lease")
	}
}
func TestBoundaryReservesFullLeaseAndServoLag(t *testing.T) {
	s := sample()
	r := record()
	s.Position[0] = 1.95
	v, brake, err := boundedVelocity(s, r.Spec, .4, time.Now())
	if err != nil || brake || v <= 0 || v > .049/1.32+1e-12 {
		t.Fatalf("unsafe boundary command %v %v %v", v, brake, err)
	}
	s.Velocity[0] = .4
	_, brake, err = boundedVelocity(s, r.Spec, .1, time.Now())
	if err != nil || !brake {
		t.Fatal("outward momentum must brake")
	}
	s.Velocity[0] = 0
	s.CommandedVelocity = .4
	_, brake, err = boundedVelocity(s, r.Spec, .1, time.Now())
	if err != nil || !brake {
		t.Fatal("previous target may accelerate before replacement arrives")
	}
	s.CommandedVelocity = 0
	s.Position[0] = -1.95
	s.Velocity[0] = -.4
	_, brake, err = boundedVelocity(s, r.Spec, -.1, time.Now())
	if err != nil || !brake {
		t.Fatal("negative outward momentum must brake")
	}
	s.Velocity[0] = 0
	s.SampleUnixMS = time.Now().Add(-time.Second).UnixMilli()
	if _, _, err = boundedVelocity(s, r.Spec, .1, time.Now()); err == nil {
		t.Fatal("stale sample accepted")
	}
}
func TestControllerBrakesBeforeRenewingNearBoundary(t *testing.T) {
	c, a, _ := controller(t)
	a.state.Position[0] = 1.99
	a.state.Velocity[0] = .4
	if _, err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.commands != 0 || a.stops != 1 {
		t.Fatal("boundary did not brake")
	}
}
func TestDecodeStateRejectsMissingAndZeroFilledTelemetry(t *testing.T) {
	b, _ := json.Marshal(sample())
	var original map[string]json.RawMessage
	_ = json.Unmarshal(b, &original)
	if _, err := decodeState(b); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"position_m", "velocity_mps", "sample_unix_ms", "energy", "sensors", "frame", "obstacle_clear"} {
		fields := map[string]json.RawMessage{}
		for k, v := range original {
			fields[k] = v
		}
		delete(fields, key)
		bad, _ := json.Marshal(fields)
		if _, err := decodeState(bad); err == nil {
			t.Fatalf("missing %s accepted", key)
		}
	}
	for key, values := range map[string][]string{"position_m": {"[]", "[0]", "[0,0,0,0]", "[null,0,0]", "null"}, "velocity_mps": {"[0,0]", "[0,0,null]"}, "sensors": {"{}", "{\"gas\":null,\"local-obstacles\":true}"}, "energy": {"null", "1e1000"}} {
		for _, value := range values {
			fields := map[string]json.RawMessage{}
			for k, v := range original {
				fields[k] = v
			}
			fields[key] = json.RawMessage(value)
			bad, _ := json.Marshal(fields)
			if _, err := decodeState(bad); err == nil {
				t.Fatalf("accepted %s=%s", key, value)
			}
		}
	}
	duplicate := append([]byte(`{"energy":0,`), b[1:]...)
	if _, err := decodeState(duplicate); err == nil {
		t.Fatal("duplicate state field accepted")
	}
}
