package collabdemo

import (
	"encoding/json"
	"math"
	"math/rand"
	"os"
	"reflect"
	"testing"
)

func routeLength(path []Vec) float64 {
	var result float64
	for i := 1; i < len(path); i++ {
		result += distance(path[i-1], path[i])
	}
	return result
}

// closedCellHit is an independent slab-intersection oracle, not a grid walk.
func closedCellHit(a, b Vec, low, high Vec) bool {
	entry, exit := 0., 1.
	for axis := 0; axis < 2; axis++ {
		delta := b[axis] - a[axis]
		if delta == 0 {
			if a[axis] < low[axis] || a[axis] > high[axis] {
				return false
			}
			continue
		}
		x, y := (low[axis]-a[axis])/delta, (high[axis]-a[axis])/delta
		if x > y {
			x, y = y, x
		}
		entry, exit = math.Max(entry, x), math.Min(exit, y)
		if entry > exit {
			return false
		}
	}
	return true
}

func assertStaticRoute(t *testing.T, c *Coordinator, path []Vec) {
	t.Helper()
	if len(path) == 0 {
		t.Fatal("no route")
	}
	g := c.scene.Grid
	for i := 1; i < len(path); i++ {
		for cell := range c.blocked {
			low := Vec{g.Origin[0] + float64(cell[0])*g.Resolution, g.Origin[1] + float64(cell[1])*g.Resolution}
			high := Vec{low[0] + g.Resolution, low[1] + g.Resolution}
			if closedCellHit(path[i-1], path[i], low, high) {
				t.Fatalf("segment %v -> %v touches blocked cell %v", path[i-1], path[i], cell)
			}
		}
	}
}

func geometryCoordinator(t *testing.T, blocked ...Cell) *Coordinator {
	t.Helper()
	return coordinator(t, Scene{Grid: Grid{Resolution: 1, Width: 10, Height: 10, Blocked: blocked},
		Robots: []Robot{{ID: "robot", Start: Vec{.5, .5}}}, Jobs: []Job{{ID: "job", Position: Vec{9.5, 9.5}}}})
}

func TestSupercoverTouchesAllClosedCells(t *testing.T) {
	g := Grid{Resolution: 1, Width: 8, Height: 8}
	segments := [][2]Vec{
		{{.5, .5}, {7.5, 7.5}}, {{.5, 7.5}, {7.5, .5}},
		{{1, .5}, {1, 7.5}}, {{.5, 1}, {7.5, 1}},
		{{1, 1}, {6, 6}}, {{1, 6}, {6, 1}}, {{2, 3}, {2, 3}},
		{{1, 1}, {1.01, 1.01}}, {{1, 1}, {.99, .99}},
	}
	random := rand.New(rand.NewSource(47))
	for i := 0; i < 500; i++ {
		segments = append(segments, [2]Vec{{.01 + random.Float64()*7.98, .01 + random.Float64()*7.98},
			{.01 + random.Float64()*7.98, .01 + random.Float64()*7.98}})
	}
	for _, segment := range segments {
		for _, reverse := range []bool{false, true} {
			a, b := segment[0], segment[1]
			if reverse {
				a, b = b, a
			}
			touched := map[Cell]bool{}
			if !g.clearSegment(a, b, func(cell Cell) bool { touched[cell] = true; return g.inside(cell) }) {
				t.Fatalf("in-grid unobstructed segment refused %v %v", a, b)
			}
			for x := 0; x < g.Width; x++ {
				for y := 0; y < g.Height; y++ {
					cell := Cell{x, y}
					if closedCellHit(a, b, Vec{float64(x), float64(y)}, Vec{float64(x + 1), float64(y + 1)}) && !touched[cell] {
						t.Fatalf("missed touched cell %v for %v -> %v", cell, a, b)
					}
				}
			}
		}
	}
}

func TestSupercoverRefusesCornerBoundaryAndUnknownSpace(t *testing.T) {
	g := Grid{Resolution: 1, Width: 5, Height: 5}
	for _, tc := range []struct {
		name    string
		a, b    Vec
		blocked Cell
	}{
		{"corner east", Vec{.5, .5}, Vec{1.5, 1.5}, Cell{1, 0}},
		{"corner north", Vec{.5, .5}, Vec{1.5, 1.5}, Cell{0, 1}},
		{"vertical edge", Vec{1, .5}, Vec{1, 4.5}, Cell{0, 2}},
		{"horizontal edge", Vec{.5, 1}, Vec{4.5, 1}, Cell{2, 0}},
		{"endpoint corner", Vec{1, 1}, Vec{3.5, 2.5}, Cell{0, 0}},
		{"interior obstacle", Vec{.5, .5}, Vec{4.5, 4.5}, Cell{2, 2}},
		{"outside", Vec{-.01, 2.5}, Vec{4.5, 2.5}, Cell{-10, -10}},
		{"unknown boundary", Vec{0, .5}, Vec{0, 4.5}, Cell{-10, -10}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			allowed := func(cell Cell) bool { return g.inside(cell) && cell != tc.blocked }
			if g.clearSegment(tc.a, tc.b, allowed) || g.clearSegment(tc.b, tc.a, allowed) {
				t.Fatal("unsafe segment accepted")
			}
		})
	}
}

func TestDiagonalCannotCutBlockedCorner(t *testing.T) {
	c := geometryCoordinator(t, Cell{1, 0})
	path := c.plan(Vec{.5, .5}, Vec{1.5, 1.5}, nil)
	assertStaticRoute(t, c, path)
	if len(path) < 3 || math.Abs(routeLength(path)-2) > 1e-9 {
		t.Fatalf("should go around blocked corner, got %v", path)
	}
	c = geometryCoordinator(t, Cell{1, 0}, Cell{0, 1})
	if path := c.plan(Vec{.5, .5}, Vec{1.5, 1.5}, nil); path != nil {
		t.Fatalf("escaped isolated start diagonally: %v", path)
	}
}

func TestOpenSpacePullsToExactEndpoints(t *testing.T) {
	c := geometryCoordinator(t)
	start, goal := Vec{.7, .6}, Vec{8.8, 7.9}
	want := []Vec{start, goal}
	for i := 0; i < 20; i++ {
		path := c.plan(start, goal, nil)
		if !reflect.DeepEqual(path, want) {
			t.Fatalf("open route not deterministic direct segment: %v", path)
		}
	}
	if path := c.plan(start, start, nil); !reflect.DeepEqual(path, []Vec{start}) {
		t.Fatalf("same-position route changed endpoint: %v", path)
	}
}

func TestPullingKeepsBlockedShortcutOutOfRoute(t *testing.T) {
	c := geometryCoordinator(t, Cell{4, 2}, Cell{4, 3}, Cell{4, 4}, Cell{4, 5}, Cell{4, 6})
	start, goal := Vec{2.5, 4.5}, Vec{7.5, 4.5}
	path := c.plan(start, goal, nil)
	assertStaticRoute(t, c, path)
	if len(path) < 3 || len(path) > 6 || path[0] != start || path[len(path)-1] != goal {
		t.Fatalf("unexpected obstacle route %v", path)
	}
	if !reflect.DeepEqual(path, c.plan(start, goal, nil)) {
		t.Fatal("route changed without input change")
	}
}

func TestSegmentReservationsCoverInteriorsAndNearParallelPaths(t *testing.T) {
	for _, tc := range []struct {
		name string
		path []Vec
	}{
		{"interior crossing", []Vec{{2, -2}, {2, 2}}},
		{"parallel interior", []Vec{{1, .5}, {3, .5}}},
		{"point in interior", []Vec{{2, .5}}},
		{"collinear overlap", []Vec{{1, 0}, {3, 0}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !segmentNearPath(Vec{0, 0}, Vec{4, 0}, tc.path, Separation) {
				t.Fatal("continuous reservation missed")
			}
		})
	}
	if segmentNearPath(Vec{0, 0}, Vec{4, 0}, []Vec{{1, .7}, {3, .7}}, Separation) {
		t.Fatal("continuous reservation margin changed")
	}
	c := geometryCoordinator(t)
	reservation := []Vec{{5, 2}, {5, 6}}
	path := c.plan(Vec{1.5, 3.5}, Vec{8.5, 3.5}, [][]Vec{reservation})
	assertStaticRoute(t, c, path)
	if len(path) < 3 {
		t.Fatal("shortcut crossed reservation")
	}
	for i := 1; i < len(path); i++ {
		if segmentsDistance(path[i-1], path[i], reservation[0], reservation[1]) < Separation {
			t.Fatalf("route segment violates reservation: %v", path)
		}
	}
}

func TestCoordinatorSparseOutputHasContinuousPeerClearance(t *testing.T) {
	c := coordinator(t, fixture())
	in := initial(c.scene)
	out := c.Tick(in)
	paths := make([][]Vec, len(out.Robots))
	for i, robot := range out.Robots {
		if robot.JobID == "" {
			t.Fatal("expected three actual coordinator assignments")
		}
		paths[i] = append([]Vec{in.Robots[i].Position}, robot.Path...)
	}
	// Independent Lipschitz bound: every point on a sampled segment is at
	// most half one sample spacing from a tested point. Requiring that much
	// extra clearance proves the entire segment stays separated, without
	// calling the new segment/segment intersection implementation.
	for i, path := range paths {
		for j, other := range paths {
			if i == j {
				continue
			}
			for segment := 1; segment < len(path); segment++ {
				a, b := path[segment-1], path[segment]
				length := distance(a, b)
				steps := max(1, int(math.Ceil(length/.01)))
				for sample := 0; sample <= steps; sample++ {
					fraction := float64(sample) / float64(steps)
					point := Vec{a[0] + fraction*(b[0]-a[0]), a[1] + fraction*(b[1]-a[1])}
					for otherSegment := 1; otherSegment < len(other); otherSegment++ {
						if segmentDistance(point, other[otherSegment-1], other[otherSegment])-length/(2*float64(steps)) < Separation {
							t.Fatalf("continuous clearance not established for output paths %d/%d", i, j)
						}
					}
				}
			}
		}
	}
}

func TestPlannerRefusesInvalidAnchorsAndReservationData(t *testing.T) {
	c := geometryCoordinator(t, Cell{2, 2})
	for _, pair := range [][2]Vec{
		{{-.1, .5}, {3.5, 3.5}}, {{.5, .5}, {10, 4}},
		{{.5, .5}, {2, 2}}, {{math.NaN(), .5}, {4, 4}},
	} {
		if path := c.plan(pair[0], pair[1], nil); path != nil {
			t.Fatalf("invalid endpoint accepted: %v", path)
		}
	}
	if path := c.plan(Vec{.5, .5}, Vec{4, 4}, [][]Vec{{{math.Inf(1), 2}}}); path != nil {
		t.Fatal("invalid reservation ignored")
	}
}

func TestStringPullChecksAreLinearAndCannotSkipFailedNextSegment(t *testing.T) {
	var path []Vec
	for i := 0; i < 1000; i++ {
		path = append(path, Vec{float64(i), 0})
	}
	calls := 0
	result := pullPath(path, func(a, b Vec) bool { calls++; return distance(a, b) <= 3 })
	if len(result) == 0 || calls > 2*len(path) || result[0] != path[0] || result[len(result)-1] != path[len(path)-1] {
		t.Fatalf("unbounded or incomplete pull: calls=%d points=%d", calls, len(result))
	}
	if result := pullPath(path, func(a, b Vec) bool { return b[0] < 5 }); result != nil {
		t.Fatal("unverified segment returned")
	}
}

func TestActualOfficeSmoothRoutesRemainClear(t *testing.T) {
	raw, err := os.ReadFile("../../examples/collaborative-office/scene.json")
	if err != nil {
		t.Fatal(err)
	}
	var scene Scene
	if err := json.Unmarshal(raw, &scene); err != nil {
		t.Fatal(err)
	}
	c := coordinator(t, scene)
	for _, robot := range scene.Robots {
		for _, job := range scene.Jobs {
			path := c.plan(robot.Start, job.Position, nil)
			assertStaticRoute(t, c, path)
			if path[0] != robot.Start || path[len(path)-1] != job.Position {
				t.Fatal("office endpoints changed")
			}
			t.Logf("%s -> %s: waypoints=%d length_m=%.6f", robot.ID, job.ID, len(path), routeLength(path))
		}
	}
}
