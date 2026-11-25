package collabdemo

import (
	"errors"
	"math"
	"sort"
)

// ReachableGoal is a proposed fixed destination, not an execution permission.
// Coordinates use the same MuJoCo XY plane as Grid and PlanRoute.
type ReachableGoal struct {
	Goal    Vec
	Path    []Vec
	OffsetM float64
	LengthM float64
}

func inspectionRouteLength(path []Vec) float64 {
	var result float64
	for i := 1; i < len(path); i++ {
		result += distance(path[i-1], path[i])
	}
	return result
}

// NearestReachableGoal tries the exact observed horizontal point first. If it
// has no clear route, it chooses a reachable cell center within maxOffsetM,
// ordered by observation distance, route length, then cell X/Y. PlanRoute keeps
// its exact-goal semantics; every candidate uses its normal segment/reservation
// checks. This is not a closest-point search over continuous free space.
func NearestReachableGoal(grid Grid, start, observed Vec, maxOffsetM float64, reservations [][]Vec) (ReachableGoal, error) {
	var zero ReachableGoal
	if !validVec(observed) || !finite(maxOffsetM) || maxOffsetM <= 0 || maxOffsetM > 5 {
		return zero, errors.New("bounded observed point and inspection offset required")
	}
	// Validate the existing grid and actual start, without snapping the robot to
	// a different free cell or ignoring an occupied/reserved start.
	if _, err := PlanRoute(grid, start, start, reservations); err != nil {
		return zero, err
	}
	if path, err := PlanRoute(grid, start, observed, reservations); err == nil {
		return ReachableGoal{Goal: observed, Path: path, LengthM: inspectionRouteLength(path)}, nil
	}
	blocked := make(map[Cell]bool, len(grid.Blocked))
	for _, cell := range grid.Blocked {
		blocked[cell] = true
	}
	type option struct {
		cell Cell
		d2   float64
	}
	options := []option{}
	for x := 0; x < grid.Width; x++ {
		for y := 0; y < grid.Height; y++ {
			cell := Cell{x, y}
			point := grid.center(cell)
			dx, dy := point[0]-observed[0], point[1]-observed[1]
			d2 := dx*dx + dy*dy
			if !blocked[cell] && d2 <= maxOffsetM*maxOffsetM {
				options = append(options, option{cell, d2})
			}
		}
	}
	sort.Slice(options, func(i, j int) bool {
		if options[i].d2 != options[j].d2 {
			return options[i].d2 < options[j].d2
		}
		if options[i].cell[0] != options[j].cell[0] {
			return options[i].cell[0] < options[j].cell[0]
		}
		return options[i].cell[1] < options[j].cell[1]
	})
	best, bestD2 := zero, math.Inf(1)
	for _, candidate := range options {
		if candidate.d2 > bestD2 {
			break
		}
		goal := grid.center(candidate.cell)
		path, err := PlanRoute(grid, start, goal, reservations)
		if err != nil {
			continue
		}
		length := inspectionRouteLength(path)
		if best.Path == nil || length < best.LengthM {
			best = ReachableGoal{Goal: goal, Path: path, OffsetM: math.Sqrt(candidate.d2), LengthM: length}
			bestD2 = candidate.d2
		}
	}
	if best.Path == nil {
		return zero, errors.New("no reachable inspection point within offset")
	}
	return best, nil
}
