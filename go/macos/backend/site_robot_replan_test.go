package backend

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/macos/backend/sitenav"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func TestSiteRobotReplansAroundNewObstacleAndArrives(t *testing.T) {
	for _, supervised := range []bool{false, true} {
		t.Run(map[bool]string{false: "observed-floor", true: "supervised"}[supervised], func(t *testing.T) {
			r, f := readySiteRobot()
			r.allowUnobservedFloor = supervised
			if err := r.Destination(1, "site/revision", 3, 0, true); err != nil {
				t.Fatal(err)
			}
			started := r.started
			r.stepLocked(time.Now())
			r.observed.Pose.Position.X = .2
			r.observed.Grid.Obstacles[sitenav.Cell{X: 15, Y: 0}] = true
			before := len(f.commands)
			r.stepLocked(time.Now())
			if !r.moving || len(r.route) < 2 || r.goal != (sitenav.Point{X: 3}) || r.started != started {
				t.Fatalf("lost destination or failed to replan: %s %+v", r.message, r.route)
			}
			for _, command := range f.commands[before:] {
				if command.Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
					t.Fatal("moved during planning")
				}
			}
			now := time.Now()
			for i := 0; i < 290 && r.moving; i++ {
				now = now.Add(100 * time.Millisecond)
				r.heartbeat, r.observed.Pose.Received, r.observed.Pose.Captured, r.observed.CloudAt = now, now, now, now
				r.stepLocked(now)
				c := f.commands[len(f.commands)-1]
				if c.Command == pb.UnitreeCommand_UNITREE_COMMAND_MOVE {
					r.observed.Pose.Yaw += c.Yaw * .1
					r.observed.Pose.Position.X += c.Forward * math.Cos(r.observed.Pose.Yaw) * .1
					r.observed.Pose.Position.Y += c.Forward * math.Sin(r.observed.Pose.Yaw) * .1
					if !r.navigationGridLocked(r.departure).ClearPose(r.observed.Pose.Position, r.observed.Pose.Yaw) {
						t.Fatal("detour collided")
					}
				}
			}
			if r.moving || r.message != "Arrived" {
				t.Fatalf("detour did not arrive: %s %+v route=%+v", r.message, r.observed.Pose, r.route)
			}
		})
	}
}

func TestSiteRobotReplanDoesNotResumeAfterFault(t *testing.T) {
	for _, fault := range []string{"stop-failed", "no-route", "stale", "user-stop", "timeout"} {
		t.Run(fault, func(t *testing.T) {
			r, f := readySiteRobot()
			r.allowUnobservedFloor = true
			if err := r.Destination(1, "site/revision", 3, 0, true); err != nil {
				t.Fatal(err)
			}
			r.observed.Grid.Obstacles[sitenav.Cell{X: 15, Y: 0}] = true
			if fault == "stop-failed" {
				f.stopError = errors.New("offline")
			}
			if fault == "no-route" {
				r.observed.Grid.Obstacles[sitenav.Cell{}] = true
			}
			r.stepLocked(time.Now())
			switch fault {
			case "stale":
				r.observed.CloudAt = time.Now().Add(-time.Second)
			case "user-stop":
				r.Stop(2)
			case "timeout":
				r.started = time.Now().Add(-31 * time.Second)
			}
			if r.moving {
				r.stepLocked(time.Now())
			}
			if r.moving {
				t.Fatal("resumed despite fault")
			}
			for _, c := range f.commands {
				if c.Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
					t.Fatal("issued move after fault")
				}
			}
		})
	}
}
