package twinrobot

import (
	"errors"
	"math"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
)

func findGrid(obstacles ...sitenav.Point) sitenav.Grid {
	g := sitenav.Grid{Floor: map[sitenav.Cell]bool{}, Obstacles: map[sitenav.Cell]bool{}}
	for x := -60; x <= 60; x++ {
		for y := -60; y <= 60; y++ {
			g.Floor[sitenav.Cell{X: x, Y: y}] = true
		}
	}
	for _, p := range obstacles {
		g.Obstacles[sitenav.Cell{X: int(math.Floor(p.X / sitenav.CellSize)), Y: int(math.Floor(p.Y / sitenav.CellSize))}] = true
	}
	return g
}

func TestViewTargetUsesNearerOfLiDARAndFloorContact(t *testing.T) {
	pose := sitenav.Pose{}
	centre := go2LensCenterX
	// LiDAR sees the object 2 m ahead; its box ends above the horizon.
	p, err := viewTarget(findGrid(sitenav.Point{X: 2.35, Y: .05}), pose, centre, .4)
	if err != nil || math.Abs(p.X-2.35) > .1 || math.Abs(p.Y) > .1 {
		t.Fatalf("LiDAR range: %+v %v", p, err)
	}
	// LiDAR sees only the wall at 5 m; the box touches the floor about 1.5 m away.
	bottom := go2LensCenterY + go2CameraHeight*go2LensFocalY/1.5
	p, err = viewTarget(findGrid(sitenav.Point{X: 5.35, Y: .05}), pose, centre, bottom)
	if err != nil || math.Abs(p.X-(go2CameraForward+1.5)) > .05 {
		t.Fatalf("floor contact: %+v %v", p, err)
	}
	// A detection right of centre lies at negative Y (robot's right).
	p, err = viewTarget(findGrid(), pose, .9, bottom)
	if err != nil || p.Y >= -.3 {
		t.Fatalf("bearing sign: %+v %v", p, err)
	}
	if _, err = viewTarget(findGrid(), pose, centre, .4); !errors.Is(err, ErrNoViewRange) {
		t.Fatalf("unranged detection: %v", err)
	}
	if _, err = viewTarget(findGrid(), pose, math.NaN(), .4); err == nil {
		t.Fatal("accepted an invalid detection")
	}
}

func TestExploreTargetPrefersUnvisitedMeasuredFloor(t *testing.T) {
	pose := sitenav.Pose{}
	first, err := exploreTarget(findGrid(), pose, []sitenav.Point{{}})
	if err != nil || first.Distance(pose.Position) < 1.2 || first.Distance(pose.Position) > 4 {
		t.Fatalf("first viewpoint: %+v %v", first, err)
	}
	second, err := exploreTarget(findGrid(), pose, []sitenav.Point{{}, first})
	if err != nil || second.Distance(first) < 1.2 {
		t.Fatalf("second viewpoint repeats the first: %+v %v", second, err)
	}
	// No measured floor means no viewpoint, whatever the site model contains.
	if _, err = exploreTarget(sitenav.Grid{}, pose, nil); err == nil {
		t.Fatal("chose a viewpoint without measured floor")
	}
}
