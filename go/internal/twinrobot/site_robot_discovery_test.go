package twinrobot

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func TestDiscoveryMovesWithoutRoomAlignmentAndStopsAtNewView(t *testing.T) {
	r, commands := readySiteRobot()
	r.frame = ""
	if err := r.DiscoveryStep(1, 1); err != nil {
		t.Fatal(err)
	}
	if len(commands.commands) != 0 {
		t.Fatal("start bypassed watchdog")
	}
	r.stepLocked(time.Now())
	cmd := commands.commands[len(commands.commands)-1]
	if cmd.Command != pb.UnitreeCommand_UNITREE_COMMAND_MOVE || cmd.Forward != discoverySpeed || cmd.Yaw != 0 || cmd.Lateral != 0 {
		t.Fatalf("unexpected discovery motion: %+v", cmd)
	}
	r.observed.Pose.Position.X = discoveryDistance
	r.stepLocked(time.Now())
	if r.moving || r.discoveryMove != nil || r.discoveryState != 2 || r.frame != "" || commands.commands[len(commands.commands)-1].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
		t.Fatal("did not stop at viewpoint, or invented room alignment")
	}
	if r.Status().DiscoveryState != 2 {
		t.Fatal("missing completed status")
	}
	r.Stop(3)
	if err := r.DiscoveryStep(2, 1); err == nil {
		t.Fatal("stale order restarted discovery")
	}
}

func TestDiscoveryRequiresObservedFloorAndStoppingClearance(t *testing.T) {
	for _, fault := range []string{"obstacle", "unknown-floor", "side-clearance", "drop"} {
		t.Run(fault, func(t *testing.T) {
			r, commands := readySiteRobot()
			r.allowUnobservedFloor, r.underBodyAllowance = true, true
			cell := sitenav.Cell{X: 8, Y: 0} // front body and drift margin at the requested view
			switch fault {
			case "unknown-floor":
				delete(r.observed.Grid.Floor, cell)
			case "side-clearance":
				r.observed.Grid.Obstacles[sitenav.Cell{X: 6, Y: 2}] = true
			default:
				r.observed.Grid.Obstacles[cell] = true
			}
			err := discoveryClearanceError(r.observed.Grid, sitenav.Point{}, sitenav.Point{X: discoveryDistance}, 0)
			if err == nil || r.moving || len(commands.commands) != 0 {
				t.Fatalf("unsafe forward trajectory: %v", err)
			}
			reason := "obstacle or drop"
			if fault == "unknown-floor" {
				reason = "not observed all the floor"
			}
			if !strings.Contains(err.Error(), reason) {
				t.Fatalf("missing %q explanation: %v", reason, err)
			}
		})
	}
}

func TestDiscoveryFaultsStopWithoutAnotherMove(t *testing.T) {
	for _, fault := range []string{"pose", "cloud", "lease", "drift", "yaw", "wrong-way", "overshoot", "obstacle", "unknown-floor", "no-progress", "timeout", "epoch", "closed", "cancel", "stop-failed"} {
		t.Run(fault, func(t *testing.T) {
			r, commands := readySiteRobot()
			r.frame = ""
			if err := r.DiscoveryStep(1, 1); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			switch fault {
			case "pose":
				r.observed.Pose.Received = now.Add(-time.Second)
			case "cloud":
				r.observed.CloudAt = now.Add(-time.Second)
			case "lease":
				r.heartbeat = now.Add(-time.Second)
			case "drift":
				r.observed.Pose.Position.Y = .07
			case "yaw":
				r.observed.Pose.Yaw = .11
			case "wrong-way":
				r.observed.Pose.Position.X = -.06
			case "overshoot":
				r.observed.Pose.Position.X = .51
			case "obstacle":
				r.observed.Grid.Obstacles[sitenav.Cell{X: 7, Y: 0}] = true
			case "unknown-floor":
				delete(r.observed.Grid.Floor, sitenav.Cell{X: 7, Y: 0})
				r.allowUnobservedFloor = true
			case "no-progress":
				r.discoveryMove.progressAt = now.Add(-3 * time.Second)
			case "timeout":
				r.started = now.Add(-13 * time.Second)
			case "epoch":
				r.poseEpoch++
			case "closed":
				r.closed = true
			case "cancel":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				r.ctx = ctx
			case "stop-failed":
				commands.stopError = errors.New("lost stop")
				r.observed.Pose.Position.X = discoveryDistance
			}
			r.stepLocked(now)
			if r.moving || r.discoveryState != 3 || r.discoveryMove != nil || len(commands.commands) != 1 || commands.commands[0].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
				t.Fatalf("fault did not stop: state=%d commands=%+v", r.discoveryState, commands.commands)
			}
			if fault == "unknown-floor" && !strings.Contains(r.message, "not observed all the floor") {
				t.Fatalf("missing floor stop reason: %q", r.message)
			}
			if fault == "obstacle" && !strings.Contains(r.message, "obstacle or drop") {
				t.Fatalf("missing obstacle stop reason: %q", r.message)
			}
			if fault == "stop-failed" && !r.stopUnconfirmed {
				t.Fatal("lost unacknowledged stop")
			}
		})
	}
}

func TestDiscoveryRejectsCommandFailureAndStaleStart(t *testing.T) {
	r, commands := readySiteRobot()
	r.heartbeat = time.Time{}
	if err := r.DiscoveryStep(1, 1); err == nil || r.moving {
		t.Fatal("expired operator could start")
	}
	r.heartbeat = time.Now()
	commands.moveError = errors.New("move rejected")
	if err := r.DiscoveryStep(2, 1); err != nil {
		t.Fatal(err)
	}
	r.stepLocked(time.Now())
	if r.moving || r.discoveryState != 3 || len(commands.commands) != 2 || commands.commands[1].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
		t.Fatal("failed command did not stop")
	}
}

func TestDiscoveryBudgetsAndPreviouslyVisitedViews(t *testing.T) {
	r, _ := readySiteRobot()
	for i := 1; i <= 3; i++ {
		if err := r.DiscoveryStep(uint64(i), 1); err != nil {
			t.Fatal(err)
		}
		r.observed.Pose.Position.X += discoveryDistance
		r.stepLocked(time.Now())
		if r.discoveryState != 2 {
			t.Fatal("viewpoint failed")
		}
	}
	if err := r.DiscoveryStep(4, 1); !errors.Is(err, ErrDiscoveryBlocked) || !strings.Contains(err.Error(), "three-viewpoint") {
		t.Fatal("exceeded three moves", err)
	}
	r, _ = readySiteRobot()
	if err := r.DiscoveryStep(1, 1); err != nil {
		t.Fatal(err)
	}
	r.observed.Pose.Position.X = discoveryDistance
	r.stepLocked(time.Now())
	r.observed.Pose.Yaw = math.Pi
	if err := r.DiscoveryStep(2, 1); err != nil {
		t.Fatal("did not find an alternative to the previously visited view", err)
	}
	for _, previous := range r.discoveryHistory.visited {
		if r.discoveryMove.target.Distance(previous) < .25 {
			t.Fatal("returned to a previously sampled view")
		}
	}
	r.Stop(3)
	r.poseEpoch++
	if err := r.DiscoveryStep(4, 1); err == nil || errors.Is(err, ErrDiscoveryBlocked) {
		t.Fatal("changed odometry epoch did not end discovery", err)
	}
}

// Drive the real watchdog with simulated odometry at its 10 Hz command rate.
func TestDiscoveryClosedLoopReachesASeparatedViewAndStops(t *testing.T) {
	for _, yaw := range []float64{0, math.Pi / 2, -.6} {
		r, commands := readySiteRobot()
		r.frame = ""
		r.observed.Pose.Yaw = yaw
		if err := r.DiscoveryStep(1, 1); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		for tick := 0; tick < 20 && r.moving; tick++ {
			now := start.Add(time.Duration(tick) * 100 * time.Millisecond)
			r.heartbeat = now
			r.observed.Pose.Received, r.observed.Pose.Captured, r.observed.CloudAt = now, now, now
			r.stepLocked(now)
			last := commands.commands[len(commands.commands)-1]
			if last.Command == pb.UnitreeCommand_UNITREE_COMMAND_MOVE {
				r.observed.Pose.Position.X += math.Cos(yaw) * last.Forward * .1
				r.observed.Pose.Position.Y += math.Sin(yaw) * last.Forward * .1
			}
		}
		distance := r.observed.Pose.Position.Distance(sitenav.Point{})
		if r.moving || r.discoveryState != 2 || distance < .2 || distance > .5 {
			t.Fatalf("did not stop at a distinct bounded viewpoint: distance=%f state=%d", distance, r.discoveryState)
		}
		if commands.commands[len(commands.commands)-1].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
			t.Fatal("missing final STOP")
		}
	}
}

func TestDiscoveryExplainsUnobservedSafetyMarginBeyondUnderBodyAllowance(t *testing.T) {
	for _, allowance := range []bool{false, true} {
		r, commands := readySiteRobot()
		r.allowUnobservedFloor = true
		r.underBodyAllowance = allowance
		body := sitenav.Cell{X: 0, Y: 0}
		margin := sitenav.Cell{X: 0, Y: 2}
		delete(r.observed.Grid.Floor, body)
		if allowance {
			// Under-body permission covers the initial footprint, but cannot
			// fill the additional drift margin beside the robot.
			delete(r.observed.Grid.Floor, margin)
		}
		err := r.DiscoveryStep(1, 1)
		if !errors.Is(err, ErrDiscoveryBlocked) || !strings.Contains(err.Error(), "not observed all the floor") {
			t.Fatalf("allowance=%v: missing floor rejection reason: %v", allowance, err)
		}
		if r.moving || len(commands.commands) != 0 {
			t.Fatal("diagnostic allowed unsafe movement")
		}
		if r.observed.Grid.Floor[body] || (allowance && r.observed.Grid.Floor[margin]) {
			t.Fatal("diagnostic changed observed floor")
		}
	}
	r, _ := readySiteRobot()
	r.underBodyAllowance = true
	delete(r.observed.Grid.Floor, sitenav.Cell{})
	if err := r.DiscoveryStep(1, 1); err != nil {
		t.Fatalf("initial footprint allowance no longer works: %v", err)
	}
}

func TestDiscoveryBlockedDiagnosticsPreserveObstacleAndFloorEvidence(t *testing.T) {
	r, commands := readySiteRobot()
	cell := sitenav.Cell{X: 8, Y: 0}
	r.observed.Grid.Obstacles[cell] = true
	delete(r.observed.Grid.Floor, cell)
	err := discoveryClearanceError(r.observed.Grid, sitenav.Point{}, sitenav.Point{X: discoveryDistance}, 0)
	if err == nil || !strings.Contains(err.Error(), "not observed all the floor") {
		t.Fatalf("missing floor must remain a blocker even if an obstacle is removed: %v", err)
	}
	if !r.observed.Grid.Obstacles[cell] || r.observed.Grid.Floor[cell] || r.moving || len(commands.commands) != 0 {
		t.Fatal("diagnostic mutated observations or commanded motion")
	}
	r.observed.Grid.Floor[cell] = true
	err = discoveryClearanceError(r.observed.Grid, sitenav.Point{}, sitenav.Point{X: discoveryDistance}, 0)
	if err == nil || !strings.Contains(err.Error(), "obstacle or drop") {
		t.Fatalf("obstacle did not remain blocked after floor became observed: %v", err)
	}
}

func TestDiscoveryExplainsRadiusLimit(t *testing.T) {
	r, commands := readySiteRobot()
	r.discoveryHistory = discoveryHistory{root: 1, epoch: r.poseEpoch, visited: []sitenav.Point{{X: -2}}}
	err := r.DiscoveryStep(1, 1)
	if !errors.Is(err, ErrDiscoveryBlocked) || !strings.Contains(err.Error(), "1.3 m discovery radius") || r.moving || len(commands.commands) != 0 {
		t.Fatalf("missing bounded-radius rejection: %v", err)
	}
}

func TestDiscoveryChoosesAnotherHeadingWhenForwardIsBlocked(t *testing.T) {
	for _, yaw := range []float64{0, 3.05, -3.05} {
		r, commands := readySiteRobot()
		r.frame = ""
		r.observed.Pose.Yaw = yaw
		// A hazard beyond the turning footprint blocks the straight trajectory.
		point := sitenav.Point{X: .65 * math.Cos(yaw), Y: .65 * math.Sin(yaw)}
		r.observed.Grid.Obstacles[sitenav.Cell{X: int(math.Floor(point.X / sitenav.CellSize)), Y: int(math.Floor(point.Y / sitenav.CellSize))}] = true
		if err := r.DiscoveryStep(1, 1); err != nil {
			t.Fatal(err)
		}
		move := *r.discoveryMove
		if !move.turning || move.target.Distance(move.origin) < .25 || len(commands.commands) != 0 {
			t.Fatal("did not plan a separated alternative without issuing motion")
		}
		// Run the watchdog against simulated odometry. Crossing +/-pi must not
		// reverse the turn, and forward commands must wait for the turn's STOP.
		start := time.Now()
		turned, stoppedTurn, advanced := false, false, false
		for tick := 0; tick < 100 && r.moving; tick++ {
			now := start.Add(time.Duration(tick) * 100 * time.Millisecond)
			r.heartbeat = now
			r.observed.Pose.Received, r.observed.Pose.Captured, r.observed.CloudAt = now, now, now
			r.stepLocked(now)
			cmd := commands.commands[len(commands.commands)-1]
			if cmd.Command == pb.UnitreeCommand_UNITREE_COMMAND_STOP {
				stoppedTurn = stoppedTurn || turned && !advanced
				continue
			}
			if cmd.Yaw != 0 {
				turned = true
				if cmd.Forward != 0 || cmd.Yaw*move.turnDirection <= 0 {
					t.Fatal("unexpected combined or reversed motion")
				}
				r.observed.Pose.Yaw = math.Remainder(r.observed.Pose.Yaw+cmd.Yaw*.1, 2*math.Pi)
			}
			if cmd.Forward != 0 {
				if !stoppedTurn {
					t.Fatal("translated before acknowledging turn stop")
				}
				advanced = true
				r.observed.Pose.Position.X += math.Cos(r.observed.Pose.Yaw) * cmd.Forward * .1
				r.observed.Pose.Position.Y += math.Sin(r.observed.Pose.Yaw) * cmd.Forward * .1
			}
		}
		if !advanced || r.moving || r.discoveryState != 2 || r.frame != "" {
			t.Fatalf("alternative viewpoint failed: advanced=%v state=%d message=%s", advanced, r.discoveryState, r.message)
		}
	}
}

func TestDiscoveryChoosesShorterViewWithinRadius(t *testing.T) {
	r, _ := readySiteRobot()
	r.discoveryHistory = discoveryHistory{root: 1, epoch: r.poseEpoch, visited: []sitenav.Point{{X: -.95}}}
	if err := r.DiscoveryStep(1, 1); err != nil {
		t.Fatal(err)
	}
	if math.Abs(r.discoveryMove.target.X-.3) > 1e-9 || r.discoveryMove.turning {
		t.Fatalf("required the fixed 40 cm move: %+v", r.discoveryMove)
	}
}

func TestDiscoveryChecksCurrentStoppingHorizonBeforeEveryCommand(t *testing.T) {
	r, commands := readySiteRobot()
	// This obstacle is beyond the initial body sweep, but enters the live
	// stopping sweep near arrival. Gathering part of a view is OK.
	r.observed.Grid.Obstacles[sitenav.Cell{X: 9, Y: 0}] = true
	if err := r.DiscoveryStep(1, 1); err != nil {
		t.Fatal(err)
	}
	if r.discoveryMove.turning {
		t.Fatal("rejected clear forward stopping room")
	}
	r.stepLocked(time.Now())
	if commands.commands[len(commands.commands)-1].Command != pb.UnitreeCommand_UNITREE_COMMAND_MOVE {
		t.Fatal("did not gather data within clear stopping room")
	}
	// Before reaching the desired view, the live horizon reaches the obstacle.
	r.observed.Pose.Position.X = .34
	r.stepLocked(time.Now())
	if r.moving || commands.commands[len(commands.commands)-1].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP || !strings.Contains(r.message, "obstacle or drop") {
		t.Fatal("did not stop as the live stopping horizon reached a hazard")
	}
}

func TestDiscoveryAlternativeTurnStopsOnFaults(t *testing.T) {
	for _, fault := range []string{"pose", "cloud", "lease", "obstacle", "missing-floor", "drift", "wrong-way", "no-progress", "stop-failed", "move-failed"} {
		t.Run(fault, func(t *testing.T) {
			r, commands := readySiteRobot()
			r.underBodyAllowance = true
			r.observed.Grid.Obstacles[sitenav.Cell{X: 6, Y: 0}] = true
			if err := r.DiscoveryStep(1, 1); err != nil || !r.discoveryMove.turning {
				t.Fatalf("missing turn fixture: %v", err)
			}
			now := time.Now()
			switch fault {
			case "pose":
				r.observed.Pose.Received = now.Add(-time.Second)
			case "cloud":
				r.observed.CloudAt = now.Add(-time.Second)
			case "lease":
				r.heartbeat = now.Add(-time.Second)
			case "obstacle":
				r.observed.Grid.Obstacles[sitenav.Cell{}] = true
			case "missing-floor":
				r.observed.Grid.Floor = map[sitenav.Cell]bool{}
			case "drift":
				r.observed.Pose.Position.Y = .07
			case "wrong-way":
				r.observed.Pose.Yaw = -.1 * r.discoveryMove.turnDirection
			case "no-progress":
				r.discoveryMove.progressAt = now.Add(-3 * time.Second)
			case "stop-failed":
				r.observed.Pose.Yaw = r.discoveryMove.yaw
				commands.stopError = errors.New("lost stop")
			case "move-failed":
				commands.moveError = errors.New("rejected turn")
			}
			r.stepLocked(now)
			if r.moving || r.discoveryState != 3 || commands.commands[len(commands.commands)-1].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
				t.Fatalf("fault continued discovery: %s", r.message)
			}
			if fault == "stop-failed" && (!r.stopUnconfirmed || r.motionRecoveryAllowed) {
				t.Fatal("lost STOP allowed automatic recovery")
			}
		})
	}
}

func TestDiscoveryCannotPlanThroughUnobservedFloorOrBodyHazards(t *testing.T) {
	for _, fault := range []string{"missing-floor", "obstacle", "drop"} {
		t.Run(fault, func(t *testing.T) {
			r, commands := readySiteRobot()
			r.underBodyAllowance, r.allowUnobservedFloor = true, true
			if fault == "missing-floor" {
				r.observed.Grid.Floor = map[sitenav.Cell]bool{}
			} else {
				r.observed.Grid.Obstacles[sitenav.Cell{}] = true
			}
			if err := r.DiscoveryStep(1, 1); !errors.Is(err, ErrDiscoveryBlocked) || r.moving || len(commands.commands) > 0 {
				t.Fatalf("alternative viewpoint bypassed %s: %v", fault, err)
			}
		})
	}
}

func TestDiscoveryRetainsMeasuredStopReason(t *testing.T) {
	for _, test := range []struct {
		name      string
		x, y, yaw float64
		epoch     bool
		want      []string
	}{
		{name: "lateral", x: .1, y: .07, want: []string{"lateral drift 0.070 m exceeds 0.060 m", "progress 0.100/0.400 m"}},
		{name: "heading", x: .1, yaw: .11, want: []string{"heading drift 0.110 rad exceeds 0.100 rad"}},
		{name: "reverse", x: -.06, want: []string{"reverse progress 0.060 m exceeds 0.050 m"}},
		{name: "overshoot", x: .51, want: []string{"overshoot 0.110 m exceeds 0.100 m"}},
		{name: "combined", x: -.06, y: .07, yaw: .11, want: []string{"lateral drift", "heading drift", "reverse progress"}},
		{name: "epoch", epoch: true, want: []string{"odometry epoch changed from 7 to 8"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, commands := readySiteRobot()
			r.poseEpoch = 7
			if err := r.DiscoveryStep(1, 1); err != nil {
				t.Fatal(err)
			}
			r.observed.Pose.Position = sitenav.Point{X: test.x, Y: test.y}
			r.observed.Pose.Yaw = test.yaw
			if test.epoch {
				r.poseEpoch++
			}
			r.stepLocked(time.Now())
			if r.moving || r.motionRecoveryAllowed || r.discoveryMove != nil || r.discoveryState != 3 || len(commands.commands) != 1 || commands.commands[0].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
				t.Fatal("diagnostic altered protective stop")
			}
			status := r.Status()
			for _, want := range test.want {
				if !strings.Contains(status.LastMotionFailure, want) {
					t.Fatalf("missing %q in %q", want, status.LastMotionFailure)
				}
			}
			if status.Message != status.LastMotionFailure {
				t.Fatal("lost the measured stop reason")
			}
		})
	}
}

func measuredHalfMeterBeyondNose() sitenav.Grid {
	grid := sitenav.Grid{Floor: map[sitenav.Cell]bool{}, Obstacles: map[sitenav.Cell]bool{}}
	// Measured cells cover the current footprint, drift margin and 0.50 m
	// beyond its 0.35 m nose. Everything farther away stays unknown.
	for x := -5; x <= 8; x++ {
		for y := -3; y <= 2; y++ {
			grid.Floor[sitenav.Cell{X: x, Y: y}] = true
		}
	}
	return grid
}

func TestDiscoveryUsesSlowSpeedWithinMeasuredHalfMeterBeyondNose(t *testing.T) {
	for _, distance := range []float64{.3, .4} {
		t.Run(fmt.Sprint(distance), func(t *testing.T) {
			grid := measuredHalfMeterBeyondNose()
			if err := discoveryClearanceError(grid, sitenav.Point{}, sitenav.Point{X: distance}, 0); err != nil {
				t.Fatalf("slow view rejected measured patch: %v", err)
			}
			if err := discoveryClearanceErrorAtSpeed(grid, sitenav.Point{}, sitenav.Point{X: distance}, 0, sitenav.WalkingSpeed, 0); err == nil {
				t.Fatal("walking speed reused the slow stopping reservation")
			}
		})
	}
	r, commands := readySiteRobot()
	r.observed.Grid = measuredHalfMeterBeyondNose()
	if err := r.DiscoveryStep(1, 1); err != nil {
		t.Fatal(err)
	}
	r.stepLocked(time.Now())
	command := commands.commands[len(commands.commands)-1]
	if command.Command != pb.UnitreeCommand_UNITREE_COMMAND_MOVE || command.Forward != .20 {
		t.Fatalf("wrong nearby-view speed: %+v", command)
	}
	// A newly revealed hole or obstacle still stops the bounded movement.
	r.observed.Grid.Obstacles[sitenav.Cell{X: 6, Y: 0}] = true
	r.stepLocked(time.Now())
	if r.moving || commands.commands[len(commands.commands)-1].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
		t.Fatal("slow discovery ignored newly observed hazard")
	}
}

func TestSlowDiscoveryStillRejectsUnknownFloorAndDrops(t *testing.T) {
	for _, fault := range []string{"unknown-floor", "drop", "body-margin"} {
		t.Run(fault, func(t *testing.T) {
			grid := measuredHalfMeterBeyondNose()
			c := sitenav.Cell{X: 6, Y: 0}
			switch fault {
			case "unknown-floor":
				delete(grid.Floor, c)
			case "drop":
				grid.Obstacles[c] = true
			case "body-margin":
				delete(grid.Floor, sitenav.Cell{X: 0, Y: 2})
			}
			if err := discoveryClearanceError(grid, sitenav.Point{}, sitenav.Point{X: .3}, 0); err == nil {
				t.Fatal("slow speed bypassed floor/hazard evidence")
			}
		})
	}
}

func TestExplicitForwardKeepsWalkingSpeedAndStoppingHorizon(t *testing.T) {
	r, commands := readySiteRobot()
	if err := r.forwardLocked(.4); err != nil {
		t.Fatal(err)
	}
	r.stepLocked(time.Now())
	if command := commands.commands[len(commands.commands)-1]; command.Forward != .65 {
		t.Fatalf("explicit forward speed changed: %+v", command)
	}
	r, commands = readySiteRobot()
	r.observed.Grid = measuredHalfMeterBeyondNose()
	if err := r.forwardLocked(.4); err == nil || r.moving || len(commands.commands) != 0 {
		t.Fatal("explicit forward used slower discovery stopping distance")
	}
}

func TestDiscoveryFloorGapUsesCurrentBodyAxesForAngledPlan(t *testing.T) {
	r, _ := readySiteRobot()
	delete(r.observed.Grid.Floor, sitenav.Cell{X: 0, Y: 2})
	// Proposed translation points left, but report missing floor in the current
	// body axes, not the axes of that prospective turn.
	err := discoveryClearanceErrorAtSpeed(r.observed.Grid, sitenav.Point{}, sitenav.Point{Y: .3}, math.Pi/2, discoverySpeed, 0)
	if err == nil || !strings.Contains(err.Error(), "0.05 m ahead and 0.25 m left") {
		t.Fatalf("misleading body-relative gap: %v", err)
	}
}
