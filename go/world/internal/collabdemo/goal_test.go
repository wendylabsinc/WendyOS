package collabdemo

import (
	"math"
	"reflect"
	"testing"
)

func TestInspectionGoalPreservesClearExactPoint(t *testing.T) {
	g := Grid{Resolution: 1, Width: 8, Height: 8, Blocked: []Cell{{3, 3}}}
	start, observed := Vec{.5, .5}, Vec{6.2, 6.3}
	want, err := PlanRoute(g, start, observed, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := NearestReachableGoal(g, start, observed, 2, nil)
	if err != nil || got.Goal != observed || got.OffsetM != 0 || !reflect.DeepEqual(got.Path, want) || got.LengthM <= distance(start, observed) {
		t.Fatal(got, err)
	}
}

func TestInspectionGoalUsesNearestReachableCenterAndRouteTieBreak(t *testing.T) {
	g := Grid{Resolution: 1, Width: 7, Height: 7, Blocked: []Cell{{3, 3}}}
	start, observed := Vec{.5, 3.5}, Vec{3.5, 3.5}
	if _, err := PlanRoute(g, start, observed, nil); err == nil {
		t.Fatal("exact blocked goal accepted")
	}
	for i := 0; i < 3; i++ {
		got, err := NearestReachableGoal(g, start, observed, 2, nil)
		if err != nil || got.Goal != (Vec{2.5, 3.5}) || got.OffsetM != 1 || got.LengthM != 2 {
			t.Fatal(got, err)
		}
	}
	// A whole-path reservation removes the nearest approach on the left.
	reserved := [][]Vec{{{2.5, 3.5}}}
	got, err := NearestReachableGoal(g, Vec{.5, .5}, observed, 2, reserved)
	if err != nil || got.Goal != (Vec{3.5, 2.5}) {
		t.Fatal(got, err)
	}
	if _, err := PlanRoute(g, Vec{.5, .5}, got.Goal, reserved); err != nil {
		t.Fatal(err)
	}
	if _, err := PlanRoute(g, start, observed, nil); err == nil {
		t.Fatal("nearest helper changed exact planner")
	}
}

func TestInspectionGoalStableCellOrderAndNoFartherFallback(t *testing.T) {
	g := Grid{Resolution: 1, Width: 7, Height: 7, Blocked: []Cell{{3, 3}}}
	got, err := NearestReachableGoal(g, Vec{1.5, 1.5}, Vec{3.5, 3.5}, 1, nil)
	if err != nil || got.Goal != (Vec{2.5, 3.5}) {
		t.Fatal(got, err)
	}
	for y := 0; y < 7; y++ {
		g.Blocked = append(g.Blocked, Cell{2, y})
	}
	if _, err := NearestReachableGoal(g, Vec{.5, .5}, Vec{4.5, 3.5}, 2, nil); err == nil {
		t.Fatal("crossed wall or expanded offset")
	}
	for _, offset := range []float64{0, -1, 5.1, math.NaN()} {
		if _, err := NearestReachableGoal(g, Vec{.5, .5}, Vec{4.5, 3.5}, offset, nil); err == nil {
			t.Fatal("invalid offset", offset)
		}
	}
	if _, err := NearestReachableGoal(g, Vec{-1, .5}, Vec{1.5, 1.5}, 2, nil); err == nil {
		t.Fatal("unknown start snapped into grid")
	}
}
