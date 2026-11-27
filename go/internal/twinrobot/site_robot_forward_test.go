package twinrobot

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func startForwardCommand(t *testing.T, r *SiteRobot, distance float64) (<-chan CommandResult, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	done := make(chan CommandResult, 1)
	go func() { done <- r.ExecuteCommand(ctx, Command{Action: "forward", DistanceMeters: distance}) }()
	awaitCommand(t, r)
	return done, cancel
}

func TestCommandForwardMeasuredCompletionWithoutAlignment(t *testing.T) {
	for _, distance := range []float64{.1, 1, 2} {
		t.Run(fmt.Sprint(distance), func(t *testing.T) {
			r, commands := readySiteRobot()
			r.frame, r.poseEpoch = "", 7
			r.observed.Pose.Yaw = .15
			r.discoveryHistory = discoveryHistory{root: 10, epoch: 4, starts: 2, visited: []sitenav.Point{{X: -1}}}
			done, _ := startForwardCommand(t, r, distance)
			r.mu.Lock()
			for i := 0; i <= 10 && r.moving; i++ {
				now := time.Now()
				r.observed.Pose.Received, r.observed.CloudAt = now, now
				progress := distance * float64(i) / 10
				r.observed.Pose.Position = sitenav.Point{X: math.Cos(.15) * progress, Y: math.Sin(.15) * progress}
				r.stepLocked(now)
			}
			r.mu.Unlock()
			got := <-done
			if got.State != "completed" || got.Snapshot.Moving || got.Snapshot.Aligned || r.message != "Forward distance reached." {
				t.Fatalf("%+v", got)
			}
			if r.discoveryHistory.root != 10 || r.discoveryHistory.starts != 2 || len(r.discoveryHistory.visited) != 1 {
				t.Fatal("relative movement corrupted discovery history")
			}
			for i, command := range commands.commands {
				if i == len(commands.commands)-1 {
					if command.Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
						t.Fatal("no acknowledged arrival stop")
					}
				} else if command.Command != pb.UnitreeCommand_UNITREE_COMMAND_MOVE || command.Yaw != 0 || command.Lateral != 0 || command.Forward != sitenav.WalkingSpeed {
					t.Fatalf("unexpected relative command: %+v", command)
				}
			}
		})
	}
}

func TestCommandForwardRejectsInvalidDistanceOrUnsafeStart(t *testing.T) {
	for _, distance := range []float64{0, -.1, .099, 2.001, math.NaN(), math.Inf(1)} {
		r, commands := readySiteRobot()
		got := r.ExecuteCommand(context.Background(), Command{Action: "forward", DistanceMeters: distance})
		if got.State != "rejected" || len(commands.commands) != 0 {
			t.Fatalf("accepted distance %v: %+v", distance, got)
		}
	}
	for _, fault := range []string{"pose", "scan", "obstacle", "floor", "stopping-horizon"} {
		t.Run(fault, func(t *testing.T) {
			r, commands := readySiteRobot()
			r.allowUnobservedFloor = true // This policy never applies to relative forward goals.
			distance := 1.0
			switch fault {
			case "pose":
				r.observed.Pose.Received = time.Now().Add(-time.Second)
			case "scan":
				r.observed.CloudAt = time.Now().Add(-time.Second)
			case "obstacle":
				r.observed.Grid.Obstacles[sitenav.Cell{X: 12, Y: 0}] = true
			case "floor":
				delete(r.observed.Grid.Floor, sitenav.Cell{X: 12, Y: 0})
			case "stopping-horizon":
				distance = .1
				r.observed.Grid.Obstacles[sitenav.Cell{X: 12, Y: 0}] = true
			}
			got := r.ExecuteCommand(context.Background(), Command{Action: "forward", DistanceMeters: distance})
			if got.State != "rejected" || len(commands.commands) != 0 {
				t.Fatalf("unsafe start: %+v", got)
			}
		})
	}
}

func TestCommandForwardFaultsStopWithoutAnotherMove(t *testing.T) {
	for _, fault := range []string{"pose", "scan", "drift", "yaw", "wrong-way", "overshoot", "obstacle", "floor", "epoch", "no-progress", "timeout", "stop-failed"} {
		t.Run(fault, func(t *testing.T) {
			r, commands := readySiteRobot()
			done, _ := startForwardCommand(t, r, 1)
			r.mu.Lock()
			now := time.Now()
			switch fault {
			case "pose":
				r.observed.Pose.Received = now.Add(-time.Second)
			case "scan":
				r.observed.CloudAt = now.Add(-time.Second)
			case "drift":
				r.observed.Pose.Position.Y = .07
			case "yaw":
				r.observed.Pose.Yaw = .11
			case "wrong-way":
				r.observed.Pose.Position.X = -.06
			case "overshoot":
				r.observed.Pose.Position.X = 1.11
			case "obstacle":
				r.observed.Grid.Obstacles[sitenav.Cell{X: 9, Y: 0}] = true
			case "floor":
				delete(r.observed.Grid.Floor, sitenav.Cell{X: 9, Y: 0})
			case "epoch":
				r.poseEpoch++
			case "no-progress":
				r.discoveryMove.progressAt = now.Add(-3 * time.Second)
			case "timeout":
				r.started = now.Add(-13 * time.Second)
			case "stop-failed":
				commands.stopError = errors.New("lost acknowledgement")
				r.observed.Pose.Position.X = 1
			}
			r.stepLocked(now)
			r.mu.Unlock()
			got := <-done
			if got.State != "failed" || got.Snapshot.Moving || len(commands.commands) != 1 || commands.commands[0].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
				t.Fatalf("fault did not stop: %+v; commands: %+v", got, commands.commands)
			}
			if fault == "stop-failed" && got.Snapshot.ControlWarning == "" {
				t.Fatal("lost stop acknowledgement failure")
			}
		})
	}
}

func TestCommandForwardCancelAndManualStop(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(fmt.Sprint(manual), func(t *testing.T) {
			r, commands := readySiteRobot()
			done, cancel := startForwardCommand(t, r, 1)
			want := "canceled"
			if manual {
				r.Stop(5)
				want = "superseded"
			} else {
				cancel()
			}
			got := <-done
			if got.State != want || got.Snapshot.Moving || len(commands.commands) != 1 || commands.commands[0].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
				t.Fatalf("%+v; commands: %+v", got, commands.commands)
			}
		})
	}
}

func TestCommandForwardMovementAcknowledgementFailureStops(t *testing.T) {
	r, commands := readySiteRobot()
	done, _ := startForwardCommand(t, r, 1)
	r.mu.Lock()
	commands.moveError = errors.New("movement acknowledgement lost")
	r.stepLocked(time.Now())
	r.mu.Unlock()
	got := <-done
	if got.State != "failed" || got.Snapshot.Moving || len(commands.commands) != 2 ||
		commands.commands[0].Command != pb.UnitreeCommand_UNITREE_COMMAND_MOVE ||
		commands.commands[1].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
		t.Fatalf("movement failure did not stop: %+v; commands: %+v", got, commands.commands)
	}
}
