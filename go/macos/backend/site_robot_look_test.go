package backend

import (
	"math"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/macos/backend/sitenav"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func TestLookTurnWithoutSiteAlignmentAndAcrossYawWrap(t *testing.T) {
	for _, direction := range []int{-1, 1} {
		r, f := readySiteRobot()
		r.frame = ""
		r.observed.Pose.Yaw = float64(direction) * 3.05
		if err := r.Look(1, direction); err != nil {
			t.Fatal(err)
		}
		if len(f.commands) != 0 {
			t.Fatal("look start bypassed motion watchdog")
		}
		r.stepLocked(time.Now())
		if len(f.commands) != 1 || f.commands[0].Command != pb.UnitreeCommand_UNITREE_COMMAND_MOVE || f.commands[0].Forward != 0 || f.commands[0].Lateral != 0 || f.commands[0].Yaw*float64(direction) <= 0 || math.Abs(f.commands[0].Yaw) != lookSpeed {
			t.Fatal("incorrect turn command", f.commands)
		}
		r.observed.Pose.Yaw = math.Remainder(r.observed.Pose.Yaw+float64(direction)*lookAngle, 2*math.Pi)
		r.stepLocked(time.Now())
		if r.moving || r.lookState != 2 || r.lookTurn != nil || f.commands[len(f.commands)-1].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
			t.Fatal("turn did not stop at target")
		}
		r.Stop(2)
		if err := r.Look(1, direction); err == nil {
			t.Fatal("stale order restarted turn")
		}
	}
}

func TestLookTurnRejectsBlockedSweepAndUnknownFloor(t *testing.T) {
	r, f := readySiteRobot()
	// Find a cell outside the starting footprint that is inside the turn sweep.
	var blocked sitenav.Cell
	found := false
	for x := -8; x <= 8 && !found; x++ {
		for y := -8; y <= 8; y++ {
			cell := sitenav.Cell{X: x, Y: y}
			r.observed.Grid.Obstacles[cell] = true
			if r.observed.Grid.ClearPose(sitenav.Point{}, 0) && !r.observed.Grid.Turn(sitenav.Point{}, 0, lookAngle+lookStopMargin) {
				blocked = cell
				found = true
				break
			}
			delete(r.observed.Grid.Obstacles, cell)
		}
	}
	if !found {
		t.Fatal("missing swept corner fixture")
	}
	if err := r.Look(1, 1); err == nil || r.moving || len(f.commands) != 0 {
		t.Fatal("accepted obstacle in swept corner")
	}
	delete(r.observed.Grid.Obstacles, blocked)
	delete(r.observed.Grid.Floor, blocked)
	r.underBodyAllowance = true
	if err := r.Look(2, 1); err == nil || r.moving {
		t.Fatal("departure allowance expanded to unseen turning floor")
	}
}

func TestLookTurnStopsOnFaults(t *testing.T) {
	for _, fault := range []string{"stale-pose", "stale-cloud", "lease", "drift", "wrong-way", "obstacle", "unknown-floor", "no-progress", "timeout", "closed", "stop-failed"} {
		t.Run(fault, func(t *testing.T) {
			r, f := readySiteRobot()
			if err := r.Look(1, 1); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			switch fault {
			case "stale-pose":
				r.observed.Pose.Received = now.Add(-time.Second)
			case "stale-cloud":
				r.observed.CloudAt = now.Add(-time.Second)
			case "lease":
				r.heartbeat = now.Add(-time.Second)
			case "drift":
				r.observed.Pose.Position.X = .09
			case "wrong-way":
				r.observed.Pose.Yaw = -.1
			case "obstacle":
				r.observed.Grid.Obstacles[sitenav.Cell{}] = true
			case "unknown-floor":
				delete(r.observed.Grid.Floor, sitenav.Cell{})
			case "no-progress":
				r.lookTurn.progressAt = now.Add(-3 * time.Second)
			case "timeout":
				r.started = now.Add(-9 * time.Second)
			case "closed":
				r.closed = true
			case "stop-failed":
				f.stopError = errLookTest{}
				r.closed = true
			}
			r.stepLocked(now)
			if r.moving || r.lookState != 3 || r.lookTurn != nil || len(f.commands) != 1 || f.commands[0].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
				t.Fatal("fault did not stop turn", f.commands)
			}
			if fault == "stop-failed" && !r.stopUnconfirmed {
				t.Fatal("unacknowledged stop lost")
			}
		})
	}
}

type errLookTest struct{}

func (errLookTest) Error() string { return "stop failed" }

func TestLookTurnRequiresFreshObservationsAndValidDirection(t *testing.T) {
	for _, kind := range []string{"direction", "pose", "cloud", "lease"} {
		r, f := readySiteRobot()
		direction := 1
		switch kind {
		case "direction":
			direction = 2
		case "pose":
			r.observed.Pose.Captured = time.Time{}
		case "cloud":
			r.observed.CloudAt = time.Time{}
		case "lease":
			r.heartbeat = time.Time{}
		}
		if err := r.Look(1, direction); err == nil || r.moving || len(f.commands) != 0 {
			t.Fatal("unsafe start", kind)
		}
	}
}

// Verify that the command stream never tapers below the existing manual
// control rate near the target; transport success alone cannot prove actuation.
func TestLookTurnDoesNotTaperIntoLowYawRates(t *testing.T) {
	for _, direction := range []int{-1, 1} {
		r, f := readySiteRobot()
		if err := r.Look(1, direction); err != nil {
			t.Fatal(err)
		}
		for _, remaining := range []float64{.4, .2, .1, .04} {
			r.observed.Pose.Yaw = float64(direction) * (lookAngle - remaining)
			r.stepLocked(time.Now())
			command := f.commands[len(f.commands)-1]
			if command.Command != pb.UnitreeCommand_UNITREE_COMMAND_MOVE || math.Abs(command.Yaw) < .5 || command.Yaw*float64(direction) <= 0 {
				t.Fatal("sub-threshold turn", command)
			}
		}
		r.observed.Pose.Yaw = float64(direction) * lookAngle
		r.stepLocked(time.Now())
		if r.moving || f.commands[len(f.commands)-1].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
			t.Fatal("expected acknowledged stop, not sub-threshold velocity")
		}
	}
	if lookStopMargin < lookSpeed*.5+lookSpeed*lookSpeed/2 {
		t.Fatal("turning clearance does not cover command lease and braking")
	}
}

func TestAutomaticLookChoosesClearSide(t *testing.T) {
	r, f := readySiteRobot()
	found := false
	for x := -8; x <= 8 && !found; x++ {
		for y := -8; y <= 8; y++ {
			cell := sitenav.Cell{X: x, Y: y}
			r.observed.Grid.Obstacles[cell] = true
			g := r.observed.Grid
			if !g.Turn(sitenav.Point{}, 0, lookAngle+lookStopMargin) && g.Turn(sitenav.Point{}, 0, -lookAngle-lookStopMargin) {
				found = true
				break
			}
			delete(g.Obstacles, cell)
		}
	}
	if !found {
		t.Fatal("missing one-sided clearance fixture")
	}
	if err := r.Look(1, 0); err != nil {
		t.Fatal(err)
	}
	if r.lookTurn.direction != -1 || len(f.commands) != 0 {
		t.Fatal("automatic look did not choose clear right turn")
	}
	r.stepLocked(time.Now())
	if len(f.commands) != 1 || f.commands[0].Yaw != -.5 {
		t.Fatal("wrong automatic turn rate")
	}
}
