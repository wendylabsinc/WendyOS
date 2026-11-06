package sitenav

import (
	"context"
	"errors"
	"math"
	"testing"
)

func TestMeasuredClearFloorDisambiguatesSymmetricWallReturns(t *testing.T) {
	room := testRoom(t, true)
	position, rotation := Point{2.6, 3.2}, .7
	s, c := math.Sincos(-rotation)
	local := func(p Point) Point { x, y := p.X-position.X, p.Y-position.Y; return Point{c*x - s*y, s*x + c*y} }
	var scan, free []Point
	for i, p := range room.walls {
		if i%3 == 0 && (p.X == 0 || p.X == 6 || p.Y == 0 || p.Y == 8) {
			scan = append(scan, local(p))
		}
	}
	if results, err := MatchRoom(context.Background(), room, scan, Point{}); !errors.Is(err, ErrAmbiguousRoom) {
		t.Fatalf("outer walls should have competing headings: %+v, %v", results, err)
	}
	// The scan cannot see the internal partitions. This measured clear patch is
	// compatible with the true pose but crosses them at the 180-degree alternative.
	for x := 1.3; x < 3; x += .05 {
		for y := 1.2; y < 4.8; y += .05 {
			free = append(free, local(Point{x, y}))
		}
	}
	results, err := MatchRoomWithFloor(context.Background(), room, scan, Point{}, free)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Position.Distance(position) > .15 || math.Abs(math.Remainder(results[0].Rotation-rotation, 2*math.Pi)) > .06 {
		t.Fatalf("clear floor did not recover correct pose: %+v", results)
	}
}

func TestObservedFloorExcludesObstaclesAndUnobservedCells(t *testing.T) {
	grid := Grid{Floor: map[Cell]bool{{0, 0}: true, {1, 0}: true, {2, 0}: false}, Obstacles: map[Cell]bool{{1, 0}: true}}
	points := grid.ObservedClearFloor()
	if len(points) != 1 || points[0] != center(Cell{0, 0}) {
		t.Fatalf("unmeasured or occupied floor used for matching: %v", points)
	}
}
