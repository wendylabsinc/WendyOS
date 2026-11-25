package go2nav

import (
	"errors"
	"math"
	"testing"
)

func gaitGrid(t *testing.T) (*Grid, MotionLimits) {
	t.Helper()
	p := profile()
	p.MotionEnvelope = IsotropicGaitV1
	g, err := Build(scan(), Bounds{Min: Point{-3, -3}, Max: Point{3, 3}}, p)
	if err != nil {
		t.Fatal(err)
	}
	l := limits()
	l.MaxMeasuredLateralMPS = .15
	return g, l
}

func TestGaitProposalKeepsJointMotionInsteadOfLegacySettling(t *testing.T) {
	g, l := gaitGrid(t)
	m := BodyMotion{ForwardMPS: .2, LateralMPS: .12, YawRateRPS: .1}
	for _, tc := range []struct {
		target Point
		want   Command
	}{{Point{1, 0}, Command{ForwardMPS: l.MaxForwardMPS}}, {Point{1, 1}, Command{YawRateRPS: l.MaxYawRateRPS}}, {Point{}, Command{}}} {
		got, err := g.CommandObserved(Pose{}, tc.target, m, l)
		if err != nil || got != tc.want {
			t.Fatalf("target %+v: %+v %v", tc.target, got, err)
		}
	}
	for _, model := range []string{IsotropicV1, DirectionalV1} {
		g.motionEnvelope = model
		if _, err := g.CommandObserved(Pose{}, Point{1, 0}, m, l); !errors.Is(err, ErrInvalid) {
			t.Fatalf("legacy lateral cap changed: %s %v", model, err)
		}
		m.LateralMPS = .04
		got, err := g.CommandObserved(Pose{}, Point{1, 0}, m, l)
		if err != nil || got != (Command{}) {
			t.Fatalf("legacy settling changed: %s %+v %v", model, got, err)
		}
		m.LateralMPS = .12
	}
}

func TestGaitMeasuredBoundsAreExplicit(t *testing.T) {
	g, l := gaitGrid(t)
	for _, value := range []float64{0, -.01, .201, math.NaN(), math.Inf(1)} {
		l.MaxMeasuredLateralMPS = value
		if _, err := g.CommandObserved(Pose{}, Point{1, 0}, BodyMotion{}, l); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid commissioning %g: %v", value, err)
		}
		if err := g.CheckContinuation(Pose{}, BodyMotion{}, nil, Command{}, l); !errors.Is(err, ErrInvalid) {
			t.Fatalf("zero bypassed commissioning: %g %v", value, err)
		}
	}
	l.MaxMeasuredLateralMPS = .15
	for _, m := range []BodyMotion{{LateralMPS: math.Nextafter(.15, 1)}, {LateralMPS: math.NaN()}, {ForwardMPS: .501}, {YawRateRPS: .801}} {
		if _, err := g.CommandObserved(Pose{}, Point{1, 0}, m, l); !errors.Is(err, ErrInvalid) {
			t.Fatalf("out-of-profile %+v: %v", m, err)
		}
	}
	for _, vy := range []float64{-.15, .15} {
		if _, err := g.CommandObserved(Pose{}, Point{1, 0}, BodyMotion{LateralMPS: vy}, l); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGaitLateralEnvelopeDoesNotClearUnknownOrObstacle(t *testing.T) {
	g, l := gaitGrid(t)
	m := BodyMotion{LateralMPS: .15}
	found := false
	for c, v := range g.cells {
		if v != free {
			continue
		}
		g.cells[c] = occupied
		stationary := g.CheckContinuation(Pose{}, BodyMotion{}, nil, Command{}, l)
		_, drift := g.CommandObserved(Pose{}, Point{}, m, l)
		g.cells[c] = v
		if stationary == nil && errors.Is(drift, ErrBlocked) {
			delete(g.cells, c)
			if _, err := g.CommandObserved(Pose{}, Point{}, m, l); !errors.Is(err, ErrUnknown) {
				t.Fatalf("unknown lateral residual admitted: %v", err)
			}
			g.cells[c] = v
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no lateral-only envelope cell exercised")
	}
}

func TestGaitPriorTargetStillCountsForZeroProposal(t *testing.T) {
	g, l := gaitGrid(t)
	m := BodyMotion{ForwardMPS: .1, LateralMPS: .1, YawRateRPS: .1}
	prior := []Command{{ForwardMPS: .3}, {YawRateRPS: .3}}
	found := false
	for c, v := range g.cells {
		if v != free {
			continue
		}
		g.cells[c] = occupied
		without := g.CheckContinuation(Pose{}, m, nil, Command{}, l)
		with := g.CheckContinuation(Pose{}, m, prior, Command{}, l)
		g.cells[c] = v
		if without == nil && errors.Is(with, ErrBlocked) {
			delete(g.cells, c)
			if err := g.CheckContinuation(Pose{}, m, prior, Command{}, l); !errors.Is(err, ErrUnknown) {
				t.Fatal(err)
			}
			g.cells[c] = v
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no prior-only joint envelope cell exercised")
	}
}

func TestGaitEnvelopeMatchesExistingIsotropicGeometry(t *testing.T) {
	g, _ := gaitGrid(t)
	p := Pose{Yaw: .3}
	residual, forward, angle := .4, .3, .6
	// Proposal/settling changes do not remove a single geometric cell. Check all
	// observed cells with an occupied witness against the legacy isotropic model.
	for c, v := range g.cells {
		if v != free {
			continue
		}
		g.cells[c] = occupied
		g.motionEnvelope = IsotropicV1
		old := g.continuationEnvelope(p, residual, forward, angle)
		g.motionEnvelope = IsotropicGaitV1
		next := g.continuationEnvelope(p, residual, forward, angle)
		g.cells[c] = v
		if old != next {
			t.Fatalf("cell %+v changed geometry: %v / %v", c, old, next)
		}
	}
}

func TestGaitBoundsAndCommandLimitsRemain(t *testing.T) {
	g, l := gaitGrid(t)
	for _, tc := range []struct {
		pose   Pose
		target Point
	}{{Pose{X: 2.7}, Point{2.8, 0}}, {Pose{}, Point{4, 0}}} {
		if _, err := g.CommandObserved(tc.pose, tc.target, BodyMotion{LateralMPS: .1}, l); !errors.Is(err, ErrBounds) {
			t.Fatalf("bounds bypass: %v", err)
		}
	}
	if err := g.CheckContinuation(Pose{}, BodyMotion{}, nil, Command{ForwardMPS: .2, YawRateRPS: .1}, l); !errors.Is(err, ErrInvalid) {
		t.Fatal("combined axis command admitted")
	}
	l.MaxForwardMPS = .351
	if _, err := g.CommandObserved(Pose{}, Point{1, 0}, BodyMotion{}, l); !errors.Is(err, ErrInvalid) {
		t.Fatal("speed cap widened")
	}
}
