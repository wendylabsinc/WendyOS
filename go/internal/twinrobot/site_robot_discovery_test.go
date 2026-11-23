package twinrobot

import (
	"context"
	"errors"
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
			cell := sitenav.Cell{X: 12, Y: 0} // beyond the target, inside stopping sweep
			switch fault {
			case "unknown-floor":
				delete(r.observed.Grid.Floor, cell)
			case "side-clearance":
				r.observed.Grid.Obstacles[sitenav.Cell{X: 6, Y: 2}] = true
			default:
				r.observed.Grid.Obstacles[cell] = true
			}
			err := r.DiscoveryStep(1, 1)
			if !errors.Is(err, ErrDiscoveryBlocked) || r.moving || len(commands.commands) != 0 {
				t.Fatalf("unsafe discovery start: %v", err)
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
				r.observed.Grid.Obstacles[sitenav.Cell{X: 9, Y: 0}] = true
			case "unknown-floor":
				delete(r.observed.Grid.Floor, sitenav.Cell{X: 9, Y: 0})
				r.allowUnobservedFloor = true
			case "no-progress":
				r.discoveryMove.progressAt = now.Add(-3 * time.Second)
			case "timeout":
				r.started = now.Add(-5 * time.Second)
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
	if err := r.DiscoveryStep(2, 1); !errors.Is(err, ErrDiscoveryBlocked) || !strings.Contains(err.Error(), "previously visited") {
		t.Fatal("returned to the original viewpoint", err)
	}
	r.poseEpoch++
	if err := r.DiscoveryStep(3, 1); err == nil || errors.Is(err, ErrDiscoveryBlocked) {
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
	cell := sitenav.Cell{X: 12, Y: 0}
	r.observed.Grid.Obstacles[cell] = true
	delete(r.observed.Grid.Floor, cell)
	err := r.DiscoveryStep(1, 1)
	if !errors.Is(err, ErrDiscoveryBlocked) || !strings.Contains(err.Error(), "not observed all the floor") {
		t.Fatalf("missing floor must remain a blocker even if an obstacle is removed: %v", err)
	}
	if !r.observed.Grid.Obstacles[cell] || r.observed.Grid.Floor[cell] || r.moving || len(commands.commands) != 0 {
		t.Fatal("diagnostic mutated observations or commanded motion")
	}
	r.observed.Grid.Floor[cell] = true
	err = r.DiscoveryStep(2, 1)
	if !errors.Is(err, ErrDiscoveryBlocked) || !strings.Contains(err.Error(), "obstacle or drop") {
		t.Fatalf("obstacle did not remain blocked after floor became observed: %v", err)
	}
}

func TestDiscoveryExplainsRadiusLimit(t *testing.T) {
	r, commands := readySiteRobot()
	r.discoveryHistory = discoveryHistory{root: 1, epoch: r.poseEpoch, visited: []sitenav.Point{{X: -1}}}
	err := r.DiscoveryStep(1, 1)
	if !errors.Is(err, ErrDiscoveryBlocked) || !strings.Contains(err.Error(), "1.3 m discovery radius") || r.moving || len(commands.commands) != 0 {
		t.Fatalf("missing bounded-radius rejection: %v", err)
	}
}
