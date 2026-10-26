package sitenav

import (
	"math"
	"testing"
	"time"
)

func narrowAisle() Grid {
	g := floorGrid()
	for x := -20; x < 60; x++ {
		g.Obstacles[Cell{x, 3}] = true
		g.Obstacles[Cell{x, -4}] = true
	}
	return g
}

func TestFootprintFitsNarrowAisleOnlyLengthways(t *testing.T) {
	// 60 cm of clear floor fits the 36 cm body envelope lengthways.
	// The previous 40 cm center-radius check rejected these side walls.
	g := narrowAisle()
	for _, start := range []Point{{0, 0}, {.023, 0}} {
		goal := Point{start.X + 2, 0}
		path, err := g.PlanPose(start, 0, goal)
		if err != nil || len(path) != 2 {
			t.Fatalf("straight narrow route: %v %v", path, err)
		}
		if !g.ClearPose(start, 0) || g.ClearPose(start, math.Pi/2) {
			t.Fatal("footprint ignored heading")
		}
		if g.Turn(start, 0, math.Pi/2) {
			t.Fatal("turned through aisle walls")
		}
		if _, err := g.PlanPose(start, math.Pi/2, goal); err == nil {
			t.Fatal("accepted sideways start")
		}
	}
}

func TestFootprintWithoutExtraMargin(t *testing.T) {
	for _, test := range []struct {
		name     string
		obstacle Cell
		clear    bool
	}{
		{"body", Cell{0, 1}, false},
		{"two cm beside body", Cell{0, 2}, true},
		{"more than ten cm beside body", Cell{0, 3}, true},
		{"two cm in front", Cell{4, 0}, true},
		{"more than ten cm in front", Cell{5, 0}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			g := floorGrid()
			g.Obstacles[test.obstacle] = true
			if g.ClearPose(Point{}, 0) != test.clear {
				t.Fatal("incorrect clearance")
			}
		})
	}
	// Sub-cell positions must not inherit the center cell's clearance.
	g := floorGrid()
	g.Obstacles[Cell{0, 2}] = true
	if g.ClearPose(Point{0, .029}, 0) {
		t.Fatal("missed cell-edge overlap")
	}
}

func TestFootprintSweepsIntermediateTranslationAndRotation(t *testing.T) {
	g := floorGrid()
	g.Obstacles[Cell{10, 0}] = true
	if !g.ClearPose(Point{}, 0) || !g.ClearPose(Point{2, 0}, 0) || g.Sweep(Point{}, Point{2, 0}, 0) {
		t.Fatal("did not check between translation endpoints")
	}
	g = floorGrid()
	g.Obstacles[Cell{3, 3}] = true
	if !g.ClearPose(Point{}, 0) || !g.ClearPose(Point{}, math.Pi/2) {
		t.Fatal("fixture endpoints blocked")
	}
	if g.Turn(Point{}, 0, math.Pi/2) {
		t.Fatal("corner swept through obstacle between clear headings")
	}
	// A diagonal sweep must include cells touched only by the rotated corners.
	g = floorGrid()
	g.Obstacles[Cell{3, 0}] = true
	if g.Sweep(Point{}, Point{1, 1}, math.Pi/4) {
		t.Fatal("missed diagonal corner")
	}
}

func TestFootprintKeepsUnknownAndMeasuredDropsBlocked(t *testing.T) {
	for _, drop := range []bool{false, true} {
		g := narrowAisle()
		if drop {
			g.Obstacles[Cell{10, 0}] = true
		} else {
			delete(g.Floor, Cell{10, 0})
		}
		g = g.WithDeparture(&Departure{Position: Point{}, Yaw: 0})
		if g.Transition(Point{}, 0, Point{2, 0}) {
			t.Fatal("crossed drop or unknown floor")
		}
		if _, err := g.PlanPose(Point{}, 0, Point{2, 0}); err == nil {
			t.Fatal("planned through hazard in aisle")
		}
	}
}

func TestFootprintDetourPreservesTurnClearance(t *testing.T) {
	g := floorGrid()
	for y := -5; y <= 15; y++ {
		g.Obstacles[Cell{20, y}] = true
	}
	for _, start := range []Point{{0, 0}, {.027, .019}} {
		path, err := g.PlanPose(start, .17, Point{4, 0})
		if err != nil {
			t.Fatal(err)
		}
		yaw := .17
		for i := 1; i < len(path); i++ {
			if !g.Transition(path[i-1], yaw, path[i]) {
				t.Fatalf("unsafe reconstructed turn/segment %v", path)
			}
			yaw = Heading(path[i-1], path[i])
		}
	}
}

func TestFootprintVelocitySeparatesTurnAndTranslation(t *testing.T) {
	for _, yaw := range []float64{0, .01, .1, math.Pi / 2} {
		forward, turn := Velocity(Point{}, yaw, Point{1, 0})
		if forward > 0 && turn != 0 {
			t.Fatal("simultaneous unplanned translation and rotation")
		}
		if forward > WalkingSpeed {
			t.Fatal("walking speed exceeded")
		}
	}
	far, _ := Velocity(Point{}, 0, Point{1, 0})
	near, _ := Velocity(Point{}, 0, Point{.1, 0})
	if near != WalkingSpeed || far != WalkingSpeed {
		t.Fatal("approach entered the robot walking deadband")
	}
}

func TestUnobservedFloorPolicyPreservesObstaclesAndPlanningBounds(t *testing.T) {
	g := Grid{Floor: map[Cell]bool{}, Obstacles: map[Cell]bool{{0, 0}: true}}
	allowed := g.WithUnobservedFloor(Point{})
	if allowed.ClearPose(Point{}, 0) {
		t.Fatal("erased occupied cell or drop")
	}
	if g.Floor[Cell{10, 0}] || len(g.ObservedClearFloor()) != 0 {
		t.Fatal("modified measured evidence")
	}
	delete(g.Obstacles, Cell{0, 0})
	if _, err := allowed.PlanPose(Point{}, 0, Point{2, 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := allowed.PlanPose(Point{}, 0, Point{MaxDistance + 1, 0}); err == nil {
		t.Fatal("extended motion bounds")
	}
	if allowed.ClearPose(Point{20, 0}, 0) {
		t.Fatal("allowance is unbounded")
	}
}

func TestRouteCanDepartAlongActualHeadingBeforeTurning(t *testing.T) {
	start, yaw := Point{.023, .019}, 2.0
	g := Grid{Floor: map[Cell]bool{}, Obstacles: map[Cell]bool{}}
	exit := Point{start.X + .8*math.Cos(yaw), start.Y + .8*math.Sin(yaw)}
	g.sweptCells(start, exit, yaw, 0, func(c Cell) bool { g.Floor[c] = true; return true })
	for x := -30; x <= 30; x++ {
		for y := -30; y <= 30; y++ {
			c := Cell{x, y}
			p := center(c)
			forward := (p.X-start.X)*math.Cos(yaw) + (p.Y-start.Y)*math.Sin(yaw)
			if forward > .2 {
				g.Floor[c] = true
			}
		}
	}
	goal := Point{start.X + 1.5*math.Cos(yaw) + math.Cos(yaw+math.Pi/2), start.Y + 1.5*math.Sin(yaw) + math.Sin(yaw+math.Pi/2)}
	if g.Transition(start, yaw, goal) {
		t.Fatal("fixture must need a straight departure")
	}
	if _, err := g.planPose(start, yaw, goal, time.Now().Add(time.Second)); err == nil {
		t.Fatal("fixture must block the original eight-heading departure")
	}
	path, err := g.PlanPose(start, yaw, goal)
	if err != nil {
		t.Fatal(err)
	}
	if len(path) < 3 || math.Abs(math.Remainder(Heading(path[0], path[1])-yaw, 2*math.Pi)) > 1e-8 {
		t.Fatal("did not start along actual heading", path)
	}
	heading := yaw
	for i := 1; i < len(path); i++ {
		if !g.Transition(path[i-1], heading, path[i]) {
			t.Fatal("unchecked route edge", path)
		}
		heading = Heading(path[i-1], path[i])
	}
	// A new obstacle across the straight escape cannot be bypassed by the prefix.
	g.Obstacles[cell(start)] = true
	if _, err := g.PlanPose(start, yaw, goal); err == nil {
		t.Fatal("departure bypassed occupied start")
	}
}

func TestRouteTurnsStayAboveSDKMinimumUntilAligned(t *testing.T) {
	for _, yaw := range []float64{0, math.Pi - .02, -math.Pi + .02} {
		for _, angle := range []float64{-.6, -.1, -.041, .041, .1, .6} {
			target := Point{math.Cos(yaw + angle), math.Sin(yaw + angle)}
			forward, turn := Velocity(Point{}, yaw, target)
			if forward != 0 || turn != math.Copysign(TurningSpeed, angle) {
				t.Fatalf("heading error %v produced %v %v", angle, forward, turn)
			}
		}
		for _, angle := range []float64{-.039, 0, .039} {
			forward, turn := Velocity(Point{}, yaw, Point{math.Cos(yaw + angle), math.Sin(yaw + angle)})
			if forward != WalkingSpeed || turn != 0 {
				t.Fatal("rotation did not stop once aligned")
			}
		}
		if forward, turn := Velocity(Point{}, yaw, Point{}); forward != 0 || turn != 0 {
			t.Fatal("arrival issued a rotation")
		}
	}
}
