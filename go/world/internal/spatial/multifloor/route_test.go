package multifloor

import (
	"errors"
	"math"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
)

// handRoom is a 10 x 6 m room with walls all round, 0.1 m cells. With a
// divider it has a wall along x = 5 with a doorway over z 2.5 to 3.5, or no
// doorway when the door is closed.
func handRoom(t *testing.T, divider, doorClosed bool) *grid.Grid {
	t.Helper()
	cfg := grid.DefaultConfig()
	cfg.CellSize = 0.1
	g, err := grid.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for iz := int32(-1); iz <= 60; iz++ {
		for ix := int32(-1); ix <= 100; ix++ {
			v := -1.0
			if ix < 0 || iz < 0 || ix == 100 || iz == 60 {
				v = 2
			}
			if divider && ix == 50 && (doorClosed || iz < 25 || iz >= 35) {
				v = 2
			}
			if err := g.Set(grid.Cell{IX: ix, IZ: iz}, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	return g
}

func handPortal(id string, kind spatial.PortalKind, a, b geom.Vec2) spatial.Portal {
	return spatial.Portal{
		ID: id, Kind: kind, State: spatial.PortalOpen, Confidence: 1,
		A:        spatial.PortalEnd{Floor: "g", PositionM: a.Lift(0).Array()},
		B:        spatial.PortalEnd{Floor: "u", PositionM: b.Lift(3.5).Array()},
		Location: spatial.Enclose(1.75, a, b),
	}
}

func TestPlanRoute(t *testing.T) {
	v := func(x, z float64) geom.Vec2 { return geom.Vec2{X: x, Z: z} }
	maps := map[spatial.FloorID]*grid.Grid{"g": handRoom(t, true, false), "u": handRoom(t, false, false)}
	lift := handPortal("lift", spatial.PortalLift, v(1, 5), v(1, 5))
	stairs := handPortal("stairs", spatial.PortalStairs, v(9, 5), v(9, 1))
	closedLift := lift
	closedLift.State = spatial.PortalClosed
	// A door inside one floor must be ignored: the map already has it.
	door := spatial.Portal{ID: "door", Kind: spatial.PortalDoor, State: spatial.PortalClosed, A: spatial.PortalEnd{Floor: "g", PositionM: [3]float64{5, 0, 3}}, B: spatial.PortalEnd{Floor: "g", PositionM: [3]float64{5, 0, 3}}}

	// Lengths by hand. Through the doorway at (5, 3): two 4.47 m diagonals,
	// which an eight-connected path walks as 4 + 2(√2 - 1) = 4.83 m each.
	// Stairs: hypot(4, 3.5) = 5.32 m. Lift: 3.5 m. Every place here sits on a
	// cell corner, 0.07 m from the cell centre the path runs through, so a leg
	// is up to 0.15 m longer than the distance between its ends.
	const doorway, leg = 2 * 4.47, 0.15
	flight := math.Hypot(4, 3.5)
	cases := []struct {
		name     string
		portals  []spatial.Portal
		cfg      RouteConfig
		from, to Place
		via      []string
		min, max float64 // LengthM
		cost     float64 // Cost - LengthM
	}{
		{"lift by default", []spatial.Portal{door, lift, stairs}, RouteConfig{}, Place{"g", v(1, 1)}, Place{"u", v(1, 1)}, []string{"lift"}, 11.5, 11.5 + 2*leg, 15},
		{"stairs when the lift is slow", []spatial.Portal{lift, stairs}, RouteConfig{LiftPenaltyM: 30}, Place{"g", v(1, 1)}, Place{"u", v(1, 1)}, []string{"stairs"}, doorway + flight + 8, doorway*1.09 + flight + 8 + 2*leg, flight},
		{"stairs when the lift is closed", []spatial.Portal{closedLift, stairs}, RouteConfig{}, Place{"g", v(1, 1)}, Place{"u", v(1, 1)}, []string{"stairs"}, doorway + flight + 8, doorway*1.09 + flight + 8 + 2*leg, flight},
		{"one floor", []spatial.Portal{lift, stairs}, RouteConfig{}, Place{"g", v(1, 1)}, Place{"g", v(9, 1)}, nil, doorway, doorway*1.09 + leg, 0},
		{"down", []spatial.Portal{lift, stairs}, RouteConfig{}, Place{"u", v(8, 1)}, Place{"g", v(8, 5)}, []string{"stairs"}, 1 + flight + 1, 1 + flight + 1 + 2*leg, flight},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := PlanRoute(maps, c.portals, c.from, c.to, c.cfg)
			if err != nil {
				t.Fatal(err)
			}
			var via []string
			sum := 0.0
			for i, leg := range r.Legs {
				if leg.Portal != "" {
					via = append(via, leg.Portal)
				}
				if (leg.Portal == "") != (i == len(r.Legs)-1) {
					t.Errorf("leg %d of %d has portal %q", i, len(r.Legs), leg.Portal)
				}
				walked := 0.0
				for k := 1; k < len(leg.Path); k++ {
					walked += leg.Path[k].Dist(leg.Path[k-1])
					if !maps[leg.Floor].LineOfSight(leg.Path[k-1], leg.Path[k]) || maps[leg.Floor].StateAt(leg.Path[k]) != grid.Free {
						t.Errorf("leg %d crosses a wall between %v and %v", i, leg.Path[k-1], leg.Path[k])
					}
				}
				if math.Abs(walked-leg.LengthM) > 1e-9 {
					t.Errorf("leg %d: path is %.3f m, LengthM %.3f", i, walked, leg.LengthM)
				}
				sum += leg.LengthM
			}
			if len(via) != len(c.via) || (len(via) > 0 && via[0] != c.via[0]) {
				t.Errorf("via %v, want %v", via, c.via)
			}
			if r.LengthM < c.min-1e-9 || r.LengthM > c.max {
				t.Errorf("length %.2f m, want %.2f to %.2f", r.LengthM, c.min, c.max)
			}
			if math.Abs(r.Cost-r.LengthM-c.cost) > 1e-9 || sum > r.LengthM+1e-9 {
				t.Errorf("cost %.2f for %.2f m (legs %.2f m), want %.2f over", r.Cost, r.LengthM, sum, c.cost)
			}
			if got := r.Legs[0].Path[0]; got != c.from.Position {
				t.Errorf("path starts at %v", got)
			}
			if last := r.Legs[len(r.Legs)-1]; last.Path[len(last.Path)-1] != c.to.Position || last.Floor != c.to.Floor {
				t.Errorf("path ends at %v on %q", last.Path[len(last.Path)-1], last.Floor)
			}
		})
	}
}

func TestPlanRouteDetoursOverAnotherFloor(t *testing.T) {
	v := func(x, z float64) geom.Vec2 { return geom.Vec2{X: x, Z: z} }
	// The door of the ground floor is shut: the far side is reached by going
	// up the lift, across the upper floor and down the stairs.
	maps := map[spatial.FloorID]*grid.Grid{"g": handRoom(t, true, true), "u": handRoom(t, false, false)}
	portals := []spatial.Portal{handPortal("lift", spatial.PortalLift, v(1, 5), v(1, 5)), handPortal("stairs", spatial.PortalStairs, v(9, 5), v(9, 1))}
	r, err := PlanRoute(maps, portals, Place{"g", v(1, 1)}, Place{"g", v(9, 1)}, RouteConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Legs) != 3 || r.Legs[0].Portal != "lift" || r.Legs[1].Portal != "stairs" || r.Legs[1].Floor != "u" {
		t.Fatalf("legs = %+v", r.Legs)
	}
	// 4 m to the lift, 3.5 m up, 8.94 m across (9.66 m on the grid), 5.32 m
	// down the stairs, 4 m back, and up to 0.15 m per leg to the cell centres.
	if r.LengthM < 25.7 || r.LengthM > 26.95 || math.Abs(r.Cost-r.LengthM-15-math.Hypot(4, 3.5)) > 1e-9 {
		t.Fatalf("length %.2f m, cost %.2f", r.LengthM, r.Cost)
	}
}

func TestPlanRouteRefuses(t *testing.T) {
	v := func(x, z float64) geom.Vec2 { return geom.Vec2{X: x, Z: z} }
	shut := map[spatial.FloorID]*grid.Grid{"g": handRoom(t, true, true), "u": handRoom(t, false, false)}
	lift := handPortal("lift", spatial.PortalLift, v(1, 5), v(1, 5))
	farStairs := handPortal("stairs", spatial.PortalStairs, v(30, 5), v(9, 1)) // foot outside the map
	cases := map[string]struct {
		portals  []spatial.Portal
		from, to Place
	}{
		"no portal":            {nil, Place{"g", v(1, 1)}, Place{"u", v(1, 1)}},
		"shut door, one lift":  {[]spatial.Portal{lift}, Place{"g", v(1, 1)}, Place{"g", v(9, 1)}},
		"portal end unmapped":  {[]spatial.Portal{farStairs}, Place{"g", v(1, 1)}, Place{"u", v(1, 1)}},
		"goal outside the map": {[]spatial.Portal{lift}, Place{"g", v(1, 1)}, Place{"u", v(20, 20)}},
		"start in the wall":    {[]spatial.Portal{lift}, Place{"g", v(-3, 3)}, Place{"u", v(1, 1)}},
		"unknown floor":        {[]spatial.Portal{lift}, Place{"g", v(1, 1)}, Place{"roof", v(1, 1)}},
	}
	for name, c := range cases {
		if _, err := PlanRoute(shut, c.portals, c.from, c.to, RouteConfig{}); !errors.Is(err, ErrNoRoute) {
			t.Errorf("%s: %v, want ErrNoRoute", name, err)
		}
	}
	if _, err := PlanRoute(shut, nil, Place{"g", v(1, 1)}, Place{"g", v(2, 1)}, RouteConfig{ClearanceM: 5}); err == nil || errors.Is(err, ErrNoRoute) {
		t.Errorf("clearance beyond the field distance: %v", err)
	}
}
