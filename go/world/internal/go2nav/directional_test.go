package go2nav

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"testing"
)

func directionalGrid(t *testing.T) *Grid {
	t.Helper()
	s := scan()
	p := profile()
	p.MotionEnvelope = DirectionalV1
	g, e := Build(s, area(), p)
	if e != nil {
		t.Fatal(e)
	}
	return g
}

func TestDirectionalHistoricalInputs(t *testing.T) {
	for _, name := range []string{"d", "e"} {
		t.Run(name, func(t *testing.T) {
			var f struct {
				Scan    Scan
				Pose    Pose
				Motion  BodyMotion
				Target  Point
				Bounds  Bounds
				Profile Profile
				Limits  MotionLimits
			}
			b, e := os.ReadFile("testdata/corridor-" + name + ".json")
			if e != nil {
				t.Fatal(e)
			}
			if e = json.Unmarshal(b, &f); e != nil {
				t.Fatal(e)
			}
			original, _ := json.Marshal(f.Scan)
			for _, model := range []string{IsotropicV1, DirectionalV1} {
				f.Profile.MotionEnvelope = model // Explicit replay choice, never a missing-field default.
				g, e := Build(f.Scan, f.Bounds, f.Profile)
				if e != nil {
					t.Fatal(e)
				}
				cmd, e := g.CommandObserved(f.Pose, f.Target, f.Motion, f.Limits)
				if name == "e" && model == IsotropicV1 {
					if !errors.Is(e, ErrUnknown) {
						t.Fatalf("E historical refusal changed: %v", e)
					}
					continue
				}
				if e != nil {
					t.Fatalf("%s %s %v", name, model, e)
				}
				if name == "d" && cmd != (Command{}) {
					t.Fatal("D unsettled motion became positive")
				}
				if name == "e" && cmd != (Command{ForwardMPS: .15}) {
					t.Fatalf("E directional proposal %v", cmd)
				}
				if e = g.CheckContinuation(f.Pose, f.Motion, []Command{{}, {}}, cmd, f.Limits); e != nil {
					t.Fatalf("final joint continuation %v", e)
				}
				if name == "e" && g.cells[cell{-12, -7}] != unknown {
					t.Fatal("historical unknown cell relabeled")
				}
			}
			after, _ := json.Marshal(f.Scan)
			if string(after) != string(original) {
				t.Fatal("historical observation rewritten")
			}
		})
	}
}

func TestMotionEnvelopeRequiresExplicitChoice(t *testing.T) {
	for _, model := range []string{"", "directional", "physical", "DIRECTIONAL-V1"} {
		p := profile()
		p.MotionEnvelope = model
		if _, e := Build(scan(), area(), p); !errors.Is(e, ErrInvalid) {
			t.Fatalf("implicit model %q admitted", model)
		}
	}
}

func TestDirectionalInitialYawChangesCenterCone(t *testing.T) {
	g := directionalGrid(t)
	g.initialYawUncertainty = .25
	without := *g
	without.initialYawUncertainty = 0
	found := false
	for k, v := range g.cells {
		if v != free {
			continue
		}
		g.cells[k] = occupied
		before := without.continuationEnvelope(Pose{}, .1, .5, .05)
		after := g.continuationEnvelope(Pose{}, .1, .5, .05)
		g.cells[k] = v
		if before == nil && errors.Is(after, ErrBlocked) {
			found = true
			delete(g.cells, k)
			if !errors.Is(g.continuationEnvelope(Pose{}, .1, .5, .05), ErrUnknown) {
				t.Fatal("uncertain direction ignored missing cell")
			}
			g.cells[k] = v
			break
		}
	}
	if !found {
		t.Fatal("initial yaw did not expand future center travel")
	}
}

func TestDirectionalFallbackZeroTravelAndBounds(t *testing.T) {
	g := directionalGrid(t)
	for _, angle := range []float64{math.Pi/2 - g.initialYawUncertainty, math.Pi / 2, math.Pi, 4} {
		for k, v := range g.cells {
			if v != free || k.X%4 != 0 || k.Y%4 != 0 || math.Abs(float64(k.X)) > 16 || math.Abs(float64(k.Y)) > 16 {
				continue
			}
			g.cells[k] = occupied
			got := g.continuationEnvelope(Pose{}, .1, .4, angle)
			want := g.turnErrorPadding(Point{}, -math.Min(angle+g.initialYawUncertainty, math.Pi), math.Min(angle+g.initialYawUncertainty, math.Pi), .5)
			g.cells[k] = v
			if !errors.Is(got, want) {
				t.Fatalf("fallback differs at %v: %v %v", angle, got, want)
			}
		}
	}
	if !errors.Is(g.continuationEnvelope(Pose{X: 2}, .1, .8, .1), ErrBounds) {
		t.Fatal("directional swept endpoint left bounds")
	}
	for k, v := range g.cells {
		if v != free || k.X%4 != 0 || k.Y%4 != 0 || math.Abs(float64(k.X)) > 16 || math.Abs(float64(k.Y)) > 16 {
			continue
		}
		g.cells[k] = occupied
		got := g.continuationEnvelope(Pose{}, .2, 0, .3)
		want := g.turnErrorPadding(Point{}, -.3-g.initialYawUncertainty, .3+g.initialYawUncertainty, .2)
		g.cells[k] = v
		if !errors.Is(got, want) {
			t.Fatal("zero forward travel lost arbitrary drift envelope")
		}
	}
}

// Independently test rectangle/cell intersection with separating axes. Every
// touched full grid cell from adversarial paths must be covered, including
// corners and orientations between the production geometry's angular samples.
func touchesCell(q Point, yaw, hl, hw float64, k cell) bool {
	c := center(k)
	dx, dy := c.X-q.X, c.Y-q.Y
	sn, cs := math.Sincos(yaw)
	h := CellSize / 2
	return math.Abs(dx) < hl*math.Abs(cs)+hw*math.Abs(sn)+h && math.Abs(dy) < hl*math.Abs(sn)+hw*math.Abs(cs)+h && math.Abs(dx*cs+dy*sn) < hl+h*(math.Abs(cs)+math.Abs(sn)) && math.Abs(-dx*sn+dy*cs) < hw+h*(math.Abs(cs)+math.Abs(sn))
}
func TestDirectionalAdversarialPathCellContainment(t *testing.T) {
	g := directionalGrid(t)
	p := Pose{Yaw: .23}
	residual, forward, budget := .17, .55, .37
	theta := budget + g.initialYawUncertainty
	touched := map[cell]bool{}
	for pattern := 0; pattern < 4; pattern++ {
		centerPath := Point{}
		for step := 0; step <= 32; step++ {
			heading := []float64{-theta, theta, theta * math.Sin(float64(step)*1.7), theta * (1 - 2*float64(step%2))}[pattern]
			if step > 0 {
				ds := forward / 32
				centerPath.X += ds * math.Cos(p.Yaw+heading)
				centerPath.Y += ds * math.Sin(p.Yaw+heading)
			}
			for ri := 0; ri < 8; ri++ {
				rangle := float64(ri) * math.Pi / 4
				center := Point{centerPath.X + residual*math.Cos(rangle), centerPath.Y + residual*math.Sin(rangle)}
				for _, phi := range []float64{-theta, theta, theta*math.Sin(float64(step)*.71) + 1e-10} {
					for y := -15; y <= 20; y++ {
						for x := -15; x <= 20; x++ {
							k := cell{x, y}
							if touchesCell(center, p.Yaw+phi, g.halfLength, g.halfWidth, k) {
								touched[k] = true
							}
						}
					}
				}
			}
		}
	}
	for k := range touched {
		v := g.cells[k]
		g.cells[k] = occupied
		e := g.continuationEnvelope(p, residual, forward, budget)
		g.cells[k] = v
		if !errors.Is(e, ErrBlocked) {
			t.Fatalf("trajectory footprint cell %+v escaped envelope: %v", k, e)
		}
	}
	if len(touched) < 100 {
		t.Fatal("inadequate footprint coverage")
	}
	t.Logf("%d complete cells from piecewise heading paths contained", len(touched))
}

func TestDirectionalPriorPositiveWithZeroProposal(t *testing.T) {
	g := directionalGrid(t)
	l := limits()
	found := false
	for k, v := range g.cells {
		if v != free {
			continue
		}
		g.cells[k] = occupied
		noPrior := g.CheckContinuation(Pose{}, BodyMotion{}, nil, Command{}, l)
		prior := g.CheckContinuation(Pose{}, BodyMotion{}, []Command{{ForwardMPS: .3}}, Command{}, l)
		g.cells[k] = v
		if noPrior == nil && errors.Is(prior, ErrBlocked) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("zero proposal ignored retained forward target")
	}
	if !errors.Is(g.CheckContinuation(Pose{}, BodyMotion{}, []Command{{ForwardMPS: -.1}}, Command{}, l), ErrInvalid) {
		t.Fatal("backward target treated as nonnegative")
	}
}
