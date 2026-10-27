package backend

import (
	"github.com/wendylabsinc/wendy/go/macos/backend/sitenav"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"math"
	"testing"
	"time"
)

func TestRouteDoesNotTurnBackToPassedWaypoint(t *testing.T) {
	r, commands := readySiteRobot()
	if err := r.Destination(1, "site/revision", 2, 0, true); err != nil {
		t.Fatal(err)
	}
	r.route = []sitenav.Point{{X: .5}, {X: 2}}
	// A delayed observation can miss the old four-centimetre acceptance disk.
	r.observed.Pose.Position.X = .57
	r.stepLocked(time.Now())
	command := commands.commands[len(commands.commands)-1]
	if command.Command != pb.UnitreeCommand_UNITREE_COMMAND_MOVE || command.Forward <= 0 || command.Yaw != 0 || len(r.route) != 1 {
		t.Fatalf("turned back toward a passed waypoint: route=%+v command=%+v", r.route, command)
	}
}

func TestDestinationTransformAgreesWithDisplayedNoseInEveryQuadrant(t *testing.T) {
	for _, odomYaw := range []float64{0, math.Pi / 2, math.Pi, -math.Pi / 2} {
		for _, siteYaw := range []float64{0, math.Pi / 2, math.Pi, -math.Pi / 2} {
			r, commands := readySiteRobot()
			r.observed.Pose.Yaw = odomYaw
			for x := -50; x <= 50; x++ {
				for y := -50; y <= 50; y++ {
					r.observed.Grid.Floor[sitenav.Cell{X: x, Y: y}] = true
				}
			}
			if err := r.Align(1, "site/revision", 1, -2, 0, siteYaw); err != nil {
				t.Fatal(err)
			}
			// SceneKit's nose vector is (cos(yaw), 0, -sin(yaw)).
			if err := r.Destination(2, "site/revision", 1+math.Cos(siteYaw), -2-math.Sin(siteYaw), true); err != nil {
				t.Fatal(err)
			}
			expected := sitenav.Point{X: math.Cos(odomYaw), Y: math.Sin(odomYaw)}
			if r.goal.Distance(expected) > 1e-9 {
				t.Fatalf("flipped goal: %v want %v", r.goal, expected)
			}
			r.stepLocked(time.Now())
			command := commands.commands[len(commands.commands)-1]
			if command.Forward <= 0 || command.Yaw != 0 {
				t.Fatalf("nose-forward target caused turn: %v", command)
			}
		}
	}
}
