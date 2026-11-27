package go2nav

import (
	"errors"
	"fmt"
	"math"
	"testing"
)

func TestROSLaserScanFloat32AngularMetadata(t *testing.T) {
	for _, count := range []int{180, 360, 720, 1440} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			s := scan()
			s.Ranges = make([]float64, count)
			for i := range s.Ranges {
				s.Ranges[i] = 6
			}
			s.AngleMin = float64(float32(-math.Pi))
			s.AngleIncrement = float64(float32(2 * math.Pi / float64(count)))
			originalMin, originalIncrement := s.AngleMin, s.AngleIncrement
			if count == 360 && (s.AngleMin != -3.1415927410125732 || s.AngleIncrement != 0.01745329238474369) {
				t.Fatal("fixture differs from ROS float32 values")
			}
			g, err := Build(s, area(), profile())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = g.Plan(Pose{}, Point{1, -.6}); err != nil {
				t.Fatal(err)
			}
			if s.AngleMin != originalMin || s.AngleIncrement != originalIncrement {
				t.Fatal("measured angles were rewritten")
			}
			s.AngleMin = -math.Pi - 2*angularQuantizationTolerance
			if _, err = Build(s, area(), profile()); !errors.Is(err, ErrInvalid) {
				t.Fatal("nonquantization angle error admitted")
			}
			s.AngleMin = originalMin
			s.AngleIncrement += 2 * angularQuantizationTolerance / float64(count)
			if _, err = Build(s, area(), profile()); !errors.Is(err, ErrInvalid) {
				t.Fatal("incomplete angular coverage admitted")
			}
		})
	}
}

func profile() Profile {
	return Profile{MotionEnvelope: IsotropicV1, FlatFloor: true, HalfLengthM: .425, HalfWidthM: .30, ClearanceM: .05, MaxPositionUncertaintyM: .3, MaxYawUncertaintyRad: .3}
}
func scan() Scan {
	r := make([]float64, 360)
	for i := range r {
		r[i] = 6
	}
	return Scan{Sequence: 1, CapturedUnixNS: 1000000000, WorldEpoch: 1, Frame: "odom", Ranges: r, AngleMin: -math.Pi, AngleIncrement: math.Pi / 180, RangeMin: .1, RangeMax: 12, Robot: Pose{}, SensorMount: Pose{X: .28}, MountKnown: true, UncertaintyKnown: true, PositionUncertaintyM: .02, YawUncertaintyRad: .01}
}
func area() Bounds { return Bounds{Point{-3, -3}, Point{3, 3}} }
func build(t *testing.T, s Scan) *Grid {
	t.Helper()
	g, e := Build(s, area(), profile())
	if e != nil {
		t.Fatal(e)
	}
	return g
}
func limits() MotionLimits {
	return MotionLimits{MaxForwardMPS: .3, MaxYawRateRPS: .35, HorizonS: 1, ExtraStopM: .15, ExtraStopRad: .15, WaypointToleranceM: .08}
}

func TestObservedOffAxisRouteAndCopiedScan(t *testing.T) {
	s := scan()
	g := build(t, s)
	path, err := g.Plan(Pose{}, Point{1, -.6})
	if err != nil {
		t.Fatal(err)
	}
	if len(path) != 2 || path[0] != (Point{}) || path[1] != (Point{1, -.6}) {
		t.Fatalf("unexpected route %v", path)
	}
	for i := range s.Ranges {
		s.Ranges[i] = math.Inf(1)
	}
	if !g.ClearSweep(Point{}, Point{1, 0}, 0) {
		t.Fatal("grid retained mutable input ranges")
	}
	if g.sequence != 1 || g.epoch != 1 || g.captured != 1000000000 {
		t.Fatal("capture identity was lost")
	}
}

func TestMissingAngularCoverageNeverClears(t *testing.T) {
	s := scan()
	for i := range s.Ranges {
		s.Ranges[i] = math.Inf(1)
	}
	g := build(t, s)
	if len(g.cells) != 0 {
		t.Fatal("missing returns invented free space")
	}
	if _, e := g.Plan(Pose{}, Point{1, 0}); !errors.Is(e, ErrUnknown) {
		t.Fatalf("unknown plan: %v", e)
	}
	s = scan()
	for i := 165; i <= 195; i++ {
		s.Ranges[i] = math.Inf(1)
	}
	g = build(t, s)
	if g.cells[at(Point{1, 0})] == free || g.ClearSweep(Point{}, Point{1, 0}, 0) {
		t.Fatal("unknown forward arc was crossed")
	}
	s = scan()
	s.Ranges[180] = math.Inf(1)
	g = build(t, s)
	if g.cells[at(Point{2, 0})] == free {
		t.Fatal("single missing bracketing ray was interpolated")
	}
}

func TestEndpointsOccludeAndSensorMountIsApplied(t *testing.T) {
	s := scan()
	s.Ranges[180] = 1
	g := build(t, s)
	endpoint := at(Point{1.28, 0})
	if g.cells[endpoint] != occupied {
		t.Fatal("actual endpoint not occupied")
	}
	if g.cells[at(Point{2, 0})] == free {
		t.Fatal("space behind endpoint was cleared")
	}
	if g.ClearSweep(Point{}, Point{1, 0}, 0) {
		t.Fatal("footprint crossed endpoint")
	}
	s = scan()
	s.SensorMount.Yaw = math.Pi / 2
	s.Robot = Pose{X: .4, Y: .2, Yaw: math.Pi / 2}
	s.Ranges[180] = 1
	g = build(t, s)
	if g.cells[at(Point{-.6, .48})] != occupied {
		t.Fatal("mount rotation or odometry transform missing")
	}
}

func TestInvalidScanGeometryAndMetadata(t *testing.T) {
	cases := map[string]func(*Scan){
		"no sequence": func(s *Scan) { s.Sequence = 0 }, "no capture": func(s *Scan) { s.CapturedUnixNS = 0 }, "no epoch": func(s *Scan) { s.WorldEpoch = 0 },
		"wrong frame": func(s *Scan) { s.Frame = "simulation_world" }, "missing mount": func(s *Scan) { s.MountKnown = false }, "missing uncertainty": func(s *Scan) { s.UncertaintyKnown = false },
		"nan range": func(s *Scan) { s.Ranges[1] = math.NaN() }, "negative range": func(s *Scan) { s.Ranges[1] = -.1 }, "negative infinity": func(s *Scan) { s.Ranges[1] = math.Inf(-1) },
		"too far": func(s *Scan) { s.Ranges[1] = 13 }, "below minimum": func(s *Scan) { s.Ranges[1] = .01 }, "partial scan": func(s *Scan) { s.Ranges = s.Ranges[:359] },
		"sparse scan": func(s *Scan) { s.Ranges = s.Ranges[:90]; s.AngleIncrement = math.Pi / 45 }, "negative increment": func(s *Scan) { s.AngleIncrement = -s.AngleIncrement },
		"nonfinite origin": func(s *Scan) { s.SensorMount.X = math.Inf(1) }, "negative uncertainty": func(s *Scan) { s.PositionUncertaintyM = -.1 },
		"excessive uncertainty": func(s *Scan) { s.YawUncertaintyRad = .4 }, "unknown yaw": func(s *Scan) { s.Robot.Yaw = math.NaN() },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s := scan()
			change(&s)
			if _, e := Build(s, area(), profile()); !errors.Is(e, ErrInvalid) {
				t.Fatalf("accepted invalid input: %v", e)
			}
		})
	}
}

func TestCommissionedBoundsAndProfileAreRequired(t *testing.T) {
	for _, b := range []Bounds{{Point{-5, -5}, Point{5, 5}}, {Point{}, Point{}}, {Point{math.NaN(), 0}, Point{1, 1}}} {
		if _, e := Build(scan(), b, profile()); e == nil {
			t.Fatal("bad area accepted")
		}
	}
	for _, mutate := range []func(*Profile){func(p *Profile) { p.FlatFloor = false }, func(p *Profile) { p.HalfLengthM = .1 }, func(p *Profile) { p.ClearanceM = 0 }, func(p *Profile) { p.MaxPositionUncertaintyM = 0 }} {
		p := profile()
		mutate(&p)
		if _, e := Build(scan(), area(), p); e == nil {
			t.Fatal("bad profile accepted")
		}
	}
	g := build(t, scan())
	if _, e := g.Plan(Pose{}, Point{4, 0}); !errors.Is(e, ErrBounds) {
		t.Fatalf("out-of-area goal %v", e)
	}
	if g.ClearSweep(Point{2.5, 0}, Point{2.9, 0}, 0) {
		t.Fatal("body crossed area while center stayed inside")
	}
}

// These geometry-only tests use explicit test grids. Production Build is the
// only observation-to-grid path and has no function to fill unknown floor.
func geometryGrid() *Grid {
	g := &Grid{area: area(), cells: map[cell]occupancy{}, halfLength: .425, halfWidth: .30}
	for y := -30; y < 30; y++ {
		for x := -30; x < 30; x++ {
			g.cells[cell{x, y}] = free
		}
	}
	return g
}

func TestSweptCornersAndRotationAreChecked(t *testing.T) {
	g := geometryGrid()
	g.cells[cell{4, 2}] = occupied
	if g.ClearSweep(Point{0, 0}, Point{.05, 0}, 0) {
		t.Fatal("intersecting cell corner ignored")
	}
	g = geometryGrid()
	g.cells[cell{3, 3}] = occupied
	if !g.ClearSweep(Point{}, Point{}, 0) || !g.ClearSweep(Point{}, Point{}, math.Pi/2) {
		t.Fatal("rotation fixture endpoints unexpectedly blocked")
	}
	if g.ClearTurn(Point{}, 0, math.Pi/2) {
		t.Fatal("intermediate rotated corner cut through obstacle")
	}
	g = geometryGrid()
	g.cells[cell{0, 3}] = occupied
	if !g.ClearSweep(Point{}, Point{}, 0) || g.ClearSweep(Point{}, Point{}, math.Pi/2) {
		t.Fatal("footprint orientation was ignored")
	}
}

func TestRouteFindsObservedDetourAndRejectsUnknownBarrier(t *testing.T) {
	g := geometryGrid()
	for y := -4; y <= 4; y++ {
		for x := 9; x <= 11; x++ {
			g.cells[cell{x, y}] = occupied
		}
	}
	path, e := g.Plan(Pose{}, Point{2, 0})
	if e != nil {
		t.Fatal(e)
	}
	if len(path) < 3 {
		t.Fatal("obstacle did not force a detour")
	}
	yaw := 0.
	for i := 1; i < len(path); i++ {
		if e := g.transition(path[i-1], yaw, path[i]); e != nil {
			t.Fatalf("unsafe returned edge: %v", e)
		}
		yaw = heading(path[i-1], path[i])
	}
	for y := -30; y < 30; y++ {
		for x := 9; x <= 11; x++ {
			delete(g.cells, cell{x, y})
		}
	}
	if _, e := g.Plan(Pose{}, Point{2, 0}); !errors.Is(e, ErrNoRoute) {
		t.Fatalf("unknown barrier route: %v", e)
	}
}

func TestCommandStoppingHorizonAndNoCombinedMotion(t *testing.T) {
	g := build(t, scan())
	l := limits()
	c, e := g.Command(Pose{}, Point{1, 1}, 0, 0, l)
	if e != nil || c.ForwardMPS != 0 || c.YawRateRPS != .35 {
		t.Fatalf("turn command %+v %v", c, e)
	}
	c, e = g.Command(Pose{}, Point{1, 0}, 0, 0, l)
	if e != nil || c.ForwardMPS != .3 || c.YawRateRPS != 0 {
		t.Fatalf("forward command %+v %v", c, e)
	}
	c, e = g.Command(Pose{}, Point{1, 0}, 0, .2, l)
	if e != nil || c != (Command{}) {
		t.Fatal("translation did not wait for yaw to settle")
	}
	if _, e = g.Command(Pose{}, Point{1, 0}, .2, .2, l); !errors.Is(e, ErrBlocked) {
		t.Fatal("compound drift was assumed straight")
	}
	g = geometryGrid()
	g.cells[at(Point{.75, 0})] = occupied
	if _, e = g.Command(Pose{}, Point{.1, 0}, 0, 0, l); !errors.Is(e, ErrBlocked) {
		t.Fatalf("command omitted beyond-target stopping area: %v", e)
	}
	if _, e = g.Command(Pose{}, Point{}, .3, 0, l); !errors.Is(e, ErrBlocked) {
		t.Fatalf("arrival ignored continuing measured motion: %v", e)
	}
}

func TestUncertaintyEnlargesFootprintAndMotionBounds(t *testing.T) {
	s := scan()
	g1 := build(t, s)
	s.PositionUncertaintyM = .2
	s.YawUncertaintyRad = .2
	g2 := build(t, s)
	if g2.halfLength <= g1.halfLength || g2.halfWidth <= g1.halfWidth {
		t.Fatal("uncertainty did not enlarge envelope")
	}
	for _, mutate := range []func(*MotionLimits){func(l *MotionLimits) { l.HorizonS = .1 }, func(l *MotionLimits) { l.MaxForwardMPS = .36 }, func(l *MotionLimits) { l.ExtraStopM = -1 }, func(l *MotionLimits) { l.MaxYawRateRPS = math.NaN() }} {
		l := limits()
		mutate(&l)
		if _, e := g1.Command(Pose{}, Point{1, 0}, 0, 0, l); !errors.Is(e, ErrInvalid) {
			t.Fatal("invalid motion limit accepted")
		}
	}
}

func TestAngularUncertaintyCannotCertifyAdjacentUnknownSpace(t *testing.T) {
	s := scan()
	// The missing ray is beside the target cell's nominal angular coverage.
	// An orientation error can put that ray through the cell, so the larger
	// uncertainty must remove its free classification.
	s.Ranges[186] = math.Inf(1)
	s.YawUncertaintyRad = 0
	g := build(t, s)
	c := at(Point{2, 0})
	if g.cells[c] != free {
		t.Fatal("nominal support fixture is not free")
	}
	s.YawUncertaintyRad = .15
	g = build(t, s)
	if g.cells[c] == free {
		t.Fatal("yaw uncertainty did not widen required ray support")
	}
}
