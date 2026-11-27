package patrol

import (
	"github.com/wendylabsinc/wendy/go/world/internal/patrolenvironment"
	"github.com/wendylabsinc/wendy/go/world/internal/patrolwork"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"strings"
	"testing"
)

func envSample(seq uint64, value float64) patrolenvironment.Sample {
	return patrolenvironment.Sample{Schema: patrolenvironment.Schema, Method: patrolenvironment.Method, RunID: "run", BootID: "boot", ModelSHA256: strings.Repeat("a", 64), Frame: world.FrameRef{ID: "office-floor", Revision: "v1"}, SensorID: "gas-1", Calibration: "go2-virtual-mounts-v1", Species: "synthetic-test-gas", Unit: "ppm", Provenance: "synthetic", Sequence: seq, SimulationNS: int64(seq) * 100000000, CapturedUnixMS: 1000 + int64(seq)*100, ValuePPM: value, UncertaintyPPM: 1.5, Valid: true}
}
func envRig(t *testing.T) (*Engine, *returnAdapter) {
	e, f := returnRig(t)
	s := envSample(1, 10)
	p := patrolenvironment.Policy{SensorID: s.SensorID, Calibration: s.Calibration, Species: s.Species, Unit: s.Unit, HoldPPM: 80, ClearPPM: 65, ClearForMS: 1000}
	if err := e.SetEnvironment(p, s.RunID, s.ModelSHA256, s.BootID, s.Frame); err != nil {
		t.Fatal(err)
	}
	return e, f
}
func envFeed(e *Engine, from, to uint64, value float64) {
	for i := from; i <= to; i++ {
		s := envSample(i, value)
		e.ObserveEnvironment([]patrolenvironment.Sample{s}, s.CapturedUnixMS)
	}
}
func TestEnvironmentHoldsWalkingAndReturnUntilExplicitClearResume(t *testing.T) {
	e, f := envRig(t)
	envFeed(e, 1, 11, 10)
	_ = e.Tick(2100, rn(f, 2.1))
	if e.State.State != "environment_held" || f.goal != "" {
		t.Fatal(e.State)
	}
	if err := e.Control("resume", 2100); err != nil {
		t.Fatal(err)
	}
	selectRoom(t, e, &f.fake, 2100)
	if e.State.State != "navigating" {
		t.Fatal(e.State)
	}
	before := f.canceled
	envFeed(e, 12, 12, 90)
	if e.State.State != "environment_held" || f.canceled <= before || e.Control("recall", 2200) == nil {
		t.Fatal(e.State)
	}
	envFeed(e, 13, 23, 10)
	_ = e.Tick(3300, rn(f, 3.3))
	if e.State.State != "environment_held" {
		t.Fatal("auto resumed")
	}
	if e.Control("resume", 3300) != nil || e.Control("recall", 3300) != nil {
		t.Fatal(e.State)
	}
	_ = e.Tick(3300, rn(f, 3.3))
	if !e.State.Return.Requested || e.State.State != "returning" {
		t.Fatal(e.State)
	}
	e.returnSettledMS = 3000
	e.returnFirstStamp = 3
	e.returnLastStamp = 3.3
	envFeed(e, 24, 24, 90)
	if e.returnSettledMS != 0 || e.returnFirstStamp != 0 || e.EnvironmentMotionReady() {
		t.Fatal("held return retained settling evidence")
	}
	envFeed(e, 25, 35, 10)
	if e.Control("resume", 4500) != nil || e.State.State != "return_planning" {
		t.Fatal(e.State)
	}
	_ = e.Control("stop", 4501)
	envFeed(e, 36, 46, 10)
	if e.State.State != "stopped" || e.Control("resume", 5600) == nil {
		t.Fatal("Stop cleared")
	}
}
func TestEnvironmentDoesNotClearWorkOrDeadline(t *testing.T) {
	e, f := envRig(t)
	s := workSample(1000, 50)
	if err := e.SetWork(patrolwork.Policy{LimitJ: 100}, s, 1000, s.RunID, s.ModelSHA256); err != nil {
		t.Fatal(err)
	}
	envFeed(e, 1, 11, 10)
	s = workSample(2100, 151)
	e.ObserveWork(&s, 2100)
	if e.Control("resume", 2100) == nil {
		t.Fatal("environment cleared exhausted work")
	}
	_ = e.Tick(100000, rn(f, 100))
	if e.State.State != "expired" {
		t.Fatal(e.State)
	}
}

func TestEnvironmentalRecoveryCannotSupplyLocalizationOrObservation(t *testing.T) {
	e, f := envRig(t)
	envFeed(e, 1, 11, 10)
	if err := e.Control("resume", 2100); err != nil {
		t.Fatal(err)
	}
	// Environmental clearance is never localization or collision-clear evidence.
	n := rn(f, 2.1)
	n.Ready = false
	if err := e.Tick(2100, n); err != nil {
		t.Fatal(err)
	}
	if f.goal != "" || f.captures != 0 || e.State.State != "waiting" {
		t.Fatal(e.State)
	}
	selectRoom(t, e, &f.fake, 2100)
	e.State.State = "observing"
	e.settledAt = 1000
	envFeed(e, 12, 12, 90)
	if err := e.Tick(2200, rn(f, 2.2)); err != nil {
		t.Fatal(err)
	}
	if e.State.State != "environment_held" || f.captures != 0 || e.settledAt != 0 {
		t.Fatal("environment hold fabricated an observation", e.State)
	}
}
