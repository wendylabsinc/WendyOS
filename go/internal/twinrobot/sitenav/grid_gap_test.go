package sitenav

import (
	"math"
	"testing"
)

func TestMissingFloorDiagnosticUsesExactSweepWithoutChangingEvidence(t *testing.T) {
	g := Grid{Floor: map[Cell]bool{}, Obstacles: map[Cell]bool{}}
	for x := -20; x < 20; x++ {
		for y := -20; y < 20; y++ {
			g.Floor[Cell{x, y}] = true
		}
	}
	if _, found := g.NearestMissingFloorInSweep(Point{}, Point{X: .4}, 0, .1); found {
		t.Fatal("invented floor gap")
	}
	gapCell := Cell{0, 2}
	delete(g.Floor, gapCell)
	gap, found := g.NearestMissingFloorInSweep(Point{}, Point{X: .4}, 0, .1)
	if !found || gap != center(gapCell) || g.Floor[gapCell] {
		t.Fatalf("incorrect nearest gap %+v %v", gap, found)
	}
	if g.SweepWithMargin(Point{}, Point{X: .4}, 0, .1) {
		t.Fatal("diagnostic changed clearance")
	}
	// The same cell lies outside a narrow unswept footprint.
	if _, found := g.NearestMissingFloorInSweep(Point{}, Point{X: .4}, 0, 0); found {
		t.Fatal("reported missing floor outside the tested sweep")
	}
	for _, margin := range []float64{-1, 2, math.NaN()} {
		if _, found := g.NearestMissingFloorInSweep(Point{}, Point{}, 0, margin); found {
			t.Fatal("invalid query produced evidence")
		}
	}
}
