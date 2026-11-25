package go2nav

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"testing"
)

func TestHistoricalDMeasuredMotion(t *testing.T) {
	var f struct {
		Scan    Scan
		Pose    Pose
		Motion  BodyMotion
		Target  Point
		Bounds  Bounds
		Profile Profile
		Limits  MotionLimits
	}
	raw, e := os.ReadFile("testdata/corridor-d.json")
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(raw, &f); e != nil {
		t.Fatal(e)
	}
	f.Profile.MotionEnvelope = IsotropicV1 // Explicit historical replay; never an admission default.
	g, e := Build(f.Scan, f.Bounds, f.Profile)
	if e != nil {
		t.Fatal(e)
	}
	if e = g.sweepError(point(f.Pose), point(f.Pose), f.Pose.Yaw, 0); e != nil {
		t.Fatalf("D current footprint fails before mixed guard: %v", e)
	}
	if _, e = g.Command(f.Pose, f.Target, f.Motion.ForwardMPS, f.Motion.YawRateRPS, f.Limits); !errors.Is(e, ErrBlocked) {
		t.Fatalf("old exact mixed-motion branch %v", e)
	}
	c, e := g.CommandObserved(f.Pose, f.Target, f.Motion, f.Limits)
	if e != nil || c != (Command{}) {
		t.Fatalf("historical D envelope: %+v %v", c, e)
	}
	t.Logf("historical D footprint clear; old mixed guard blocks; conservative joint envelope permits zero only. radius=%g angle=%g", math.Hypot(f.Motion.ForwardMPS, f.Motion.LateralMPS)*f.Limits.HorizonS+f.Limits.ExtraStopM, math.Abs(f.Motion.YawRateRPS)*f.Limits.HorizonS+f.Limits.ExtraStopRad)
}

func TestObservedJointEnvelope(t *testing.T) {
	g := build(t, scan())
	l := limits()
	motion := BodyMotion{.2, .04, .2}
	if c, e := g.CommandObserved(Pose{}, Point{1, 1}, motion, l); e != nil || c != (Command{}) {
		t.Fatalf("mixed motion must settle: %+v %v", c, e)
	}
	// Find an actually observed cell hit only by the combined envelope. Neither
	// the old separate straight nor pivot sweep covers this drifted corner.
	found := false
	for k, v := range g.cells {
		if v != free {
			continue
		}
		g.cells[k] = occupied
		oldStraight := g.sweepError(Point{}, Point{.35, 0}, 0, 0)
		oldTurn := g.turnError(Point{}, 0, .35)
		_, joint := g.CommandObserved(Pose{}, Point{1, 1}, motion, l)
		g.cells[k] = v
		if oldStraight == nil && oldTurn == nil && errors.Is(joint, ErrBlocked) {
			found = true
			delete(g.cells, k)
			if _, e := g.CommandObserved(Pose{}, Point{1, 1}, motion, l); !errors.Is(e, ErrUnknown) {
				t.Fatalf("joint unknown admitted: %v", e)
			}
			g.cells[k] = v
			break
		}
	}
	if !found {
		t.Fatal("no combined-only collision was tested")
	}
	for _, m := range []BodyMotion{{LateralMPS: math.NaN()}, {LateralMPS: .051}, {ForwardMPS: .501}, {YawRateRPS: .801}} {
		if _, e := g.CommandObserved(Pose{}, Point{1, 0}, m, l); !errors.Is(e, ErrInvalid) {
			t.Fatalf("invalid motion %+v: %v", m, e)
		}
	}
	if _, e := g.CommandObserved(Pose{X: 2.4}, Point{2.5, 0}, motion, l); !errors.Is(e, ErrBounds) {
		t.Fatalf("joint bounds: %v", e)
	}
}

func TestObservedLateralDriftAndContinuousRotationPadding(t *testing.T) {
	g := build(t, scan())
	l := limits()
	// Nonzero lateral travel increases the envelope, even with zero forward/yaw.
	found := false
	for k, v := range g.cells {
		if v != free {
			continue
		}
		g.cells[k] = occupied
		_, stationary := g.CommandObserved(Pose{}, Point{}, BodyMotion{}, l)
		_, drift := g.CommandObserved(Pose{}, Point{}, BodyMotion{LateralMPS: .05}, l)
		g.cells[k] = v
		if stationary == nil && errors.Is(drift, ErrBlocked) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("lateral drift was ignored")
	}
	// Sub-sample rotation must include expanded corner displacement. Use a
	// deliberately tiny interval and place a cell edge between endpoint and
	// intermediate support; compare padded samples with a dense exact oracle.
	// Exhaustive cells around an expanded footprint: every cell touched by any
	// of 201 exact intermediate rectangles must be refused by the bounded check.
	from, to, pad := .01, .043, .35
	for k, v := range g.cells {
		if v != free {
			continue
		}
		g.cells[k] = occupied
		touched := false
		for i := 0; i <= 200; i++ {
			if errors.Is(g.sweepError(Point{}, Point{}, from+(to-from)*float64(i)/200, pad), ErrBlocked) {
				touched = true
				break
			}
		}
		if touched && !errors.Is(g.turnErrorPadding(Point{}, from, to, pad), ErrBlocked) {
			t.Fatalf("continuous expanded corner missed cell %+v", k)
		}
		g.cells[k] = v
	}
}

func TestResidualAndProposedMotionUseJointBudget(t *testing.T) {
	g := build(t, scan())
	l := limits()
	m := BodyMotion{ForwardMPS: .02, LateralMPS: .02, YawRateRPS: .02}
	found := false
	for k, v := range g.cells {
		if v != free {
			continue
		}
		g.cells[k] = occupied
		radius := math.Hypot(m.ForwardMPS, m.LateralMPS)*l.HorizonS + l.ExtraStopM
		angle := math.Abs(m.YawRateRPS)*l.HorizonS + l.ExtraStopRad
		measured := g.turnErrorPadding(Point{}, -angle, angle, radius)
		_, proposed := g.Command(Pose{}, Point{1, 0}, m.ForwardMPS, m.YawRateRPS, l)
		_, joint := g.CommandObserved(Pose{}, Point{1, 0}, m, l)
		g.cells[k] = v
		if measured == nil && proposed == nil && errors.Is(joint, ErrBlocked) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no residual+proposed joint-only collision exercised")
	}
}

func TestPriorTargetRemainsCoupledWithZeroProposal(t *testing.T) {
	g := build(t, scan())
	l := limits()
	measured := BodyMotion{ForwardMPS: .04, LateralMPS: .02, YawRateRPS: .1}
	prior := []Command{{YawRateRPS: .3}}
	found := false
	for k, v := range g.cells {
		if v != free {
			continue
		}
		g.cells[k] = occupied
		_, observed := g.CommandObserved(Pose{}, Point{1, 1}, measured, l)
		_, axis := g.Command(Pose{}, Point{1, 1}, 0, .3, l)
		combined := g.CheckContinuation(Pose{}, measured, prior, Command{}, l)
		g.cells[k] = v
		if observed == nil && axis == nil && errors.Is(combined, ErrBlocked) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("zero proposal dropped prior-positive coupled motion")
	}
	for _, p := range [][]Command{{{}, {}, {}}, {{ForwardMPS: .1, YawRateRPS: .1}}, {{ForwardMPS: -.1}}, {{YawRateRPS: math.NaN()}}, {{ForwardMPS: .36}}} {
		if e := g.CheckContinuation(Pose{}, measured, p, Command{}, l); !errors.Is(e, ErrInvalid) {
			t.Fatalf("invalid prior accepted %v", e)
		}
	}
	// Maximal legal angular budgets exceed one turn's diameter. The full
	// orientation envelope remains bounded and must not alias back to zero turn.
	if e := g.CheckContinuation(Pose{}, BodyMotion{YawRateRPS: .8}, []Command{{YawRateRPS: .5}}, Command{YawRateRPS: .5}, MotionLimits{MaxForwardMPS: .35, MaxYawRateRPS: .5, HorizonS: 2, ExtraStopM: .5, ExtraStopRad: .5, WaypointToleranceM: .1}); e != nil {
		t.Fatalf("bounded full orientation in open grid: %v", e)
	}
}
