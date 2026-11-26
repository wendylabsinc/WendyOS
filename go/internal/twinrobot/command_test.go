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

// Exercise the actual controller with measured pose updates and intercepted
// Unitree commands. No motor API is reachable from these tests.
func TestCommandTurnCompletionAndCancellation(t *testing.T) {
	for _, degrees := range []int{-180, -90, 1, 90, 180} {
		t.Run(fmt.Sprint(degrees), func(t *testing.T) {
			r, commands := readySiteRobot()
			r.frame = "" // A turn must not require clicking a position on the map.
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			result := make(chan CommandResult, 1)
			go func() { result <- r.ExecuteCommand(ctx, Command{Action: "turn", Degrees: degrees}) }()
			turns := 0
			var previous *siteLookTurn
			for {
				select {
				case got := <-result:
					cancel()
					if got.State != "completed" || got.Snapshot.Moving || got.Snapshot.ControlWarning != "" {
						t.Fatalf("%d: %+v", degrees, got)
					}
					if turns != int(math.Ceil(math.Abs(float64(degrees))/60)) {
						t.Fatalf("turn steps: %d", turns)
					}
					if len(commands.commands) == 0 || commands.commands[len(commands.commands)-1].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
						t.Fatal("missing stop")
					}
					return
				case <-time.After(5 * time.Millisecond):
					r.mu.Lock()
					r.observed.Pose.Received = time.Now()
					r.observed.CloudAt = time.Now()
					if turn := r.lookTurn; turn != nil && turn != previous {
						turns++
						previous = turn
						r.stepLocked(time.Now())
						r.observed.Pose.Yaw = math.Remainder(turn.targetYaw, 2*math.Pi)
						r.stepLocked(time.Now())
					}
					r.mu.Unlock()
				}
			}
		})
	}
}

func TestCommandCancellationStopsAndDoesNotResume(t *testing.T) {
	r, commands := readySiteRobot()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan CommandResult, 1)
	go func() { done <- r.ExecuteCommand(ctx, Command{Action: "turn", Degrees: 180}) }()
	awaitCommand(t, r)
	cancel()
	got := <-done
	if got.State != "canceled" || got.Snapshot.Moving || r.CommandActive() {
		t.Fatalf("%+v", got)
	}
	if len(commands.commands) == 0 || commands.commands[len(commands.commands)-1].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
		t.Fatal("no cancellation stop")
	}
	r.stepLocked(time.Now())
	if r.moving {
		t.Fatal("resumed after cancellation")
	}
}

func TestCommandManualStopSupersedesWithoutLateStop(t *testing.T) {
	r, commands := readySiteRobot()
	done := make(chan CommandResult, 1)
	go func() { done <- r.ExecuteCommand(context.Background(), Command{Action: "turn", Degrees: 180}) }()
	awaitCommand(t, r)
	r.Stop(7)
	count := len(commands.commands)
	got := <-done
	if got.State != "superseded" || len(commands.commands) != count {
		t.Fatalf("late command after manual stop: %+v", got)
	}
	if err := r.Look(8, 1); err != nil {
		t.Fatal("chat corrupted desktop order sequence:", err)
	}
}

func TestCommandRejectsBadGoalsAndBusyController(t *testing.T) {
	for _, q := range []Command{{Action: "turn", Degrees: 0}, {Action: "turn", Degrees: 181}, {Action: "navigate", Frame: "site/revision", X: math.NaN()}, {Action: "shell"}} {
		r, commands := readySiteRobot()
		got := r.ExecuteCommand(context.Background(), q)
		if got.State != "rejected" || len(commands.commands) != 0 {
			t.Fatalf("accepted invalid goal: %+v", got)
		}
	}
	r, _ := readySiteRobot()
	if err := r.Look(1, 1); err != nil {
		t.Fatal(err)
	}
	got := r.ExecuteCommand(context.Background(), Command{Action: "turn", Degrees: 60})
	if got.State != "rejected" || !r.moving {
		t.Fatal("chat took over an active manual command")
	}
}

func TestCommandNavigationCompletesFromMeasuredPosition(t *testing.T) {
	r, commands := readySiteRobot()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan CommandResult, 1)
	go func() { done <- r.ExecuteCommand(ctx, Command{Action: "navigate", Frame: "site/revision", X: 1}) }()
	awaitCommand(t, r)
	for i := 0; i <= 10; i++ {
		r.mu.Lock()
		r.observed.Pose.Received = time.Now()
		r.observed.CloudAt = time.Now()
		r.observed.Pose.Position.X = float64(i) / 10
		r.stepLocked(time.Now())
		r.mu.Unlock()
	}
	got := <-done
	if got.State != "completed" || got.Snapshot.Moving || got.Snapshot.Message != "Arrived" {
		t.Fatalf("%+v", got)
	}
	if commands.commands[len(commands.commands)-1].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
		t.Fatal("arrival did not stop")
	}
}

func TestCommandFailedStopBlocksFurtherMotion(t *testing.T) {
	r, commands := readySiteRobot()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan CommandResult, 1)
	go func() { done <- r.ExecuteCommand(ctx, Command{Action: "turn", Degrees: 90}) }()
	awaitCommand(t, r)
	r.mu.Lock()
	commands.stopError = errors.New("stop acknowledgement lost")
	r.mu.Unlock()
	cancel()
	got := <-done
	if got.State != "canceled" || got.Snapshot.ControlWarning == "" {
		t.Fatalf("lost stop failure: %+v", got)
	}
	got = r.ExecuteCommand(context.Background(), Command{Action: "turn", Degrees: 60})
	if got.State != "rejected" {
		t.Fatal("unconfirmed stop allowed a new movement")
	}
}

func TestCommandPassiveLocalizationAndRepeatedSettingsPreserveOwnership(t *testing.T) {
	for _, action := range []string{"turn", "forward"} {
		t.Run(action, func(t *testing.T) {
			r, commands := readySiteRobot()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			done := make(chan CommandResult, 1)
			go func() { done <- r.ExecuteCommand(ctx, Command{Action: action, Degrees: 60, DistanceMeters: 1}) }()
			awaitCommand(t, r)
			if err := r.SetUnderBodyAllowance(1, false); err != nil {
				t.Fatal(err)
			}
			if err := r.SetAllowUnobservedFloor(2, false); err != nil {
				t.Fatal(err)
			}
			if err := r.SetArrivalTiming(3, false); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Localize(4, "site/revision", nil); !errors.Is(err, errChatLocalizationBusy) {
				t.Fatalf("localization stole active goal: %v", err)
			}
			if _, err := r.FuseLocalization(5, SiteFusionRequest{Frame: "site/revision"}); !errors.Is(err, errChatLocalizationBusy) {
				t.Fatalf("fusion stole active goal: %v", err)
			}
			r.mu.Lock()
			if !r.moving || r.commandSequence != 0 || len(commands.commands) != 0 {
				r.mu.Unlock()
				t.Fatal("background operation interrupted movement")
			}
			r.observed.Pose.Received, r.observed.CloudAt = time.Now(), time.Now()
			if action == "turn" {
				r.observed.Pose.Yaw = r.lookTurn.targetYaw
			} else {
				r.observed.Pose.Position.X = 1
			}
			r.stepLocked(time.Now())
			r.mu.Unlock()
			if got := <-done; got.State != "completed" {
				t.Fatalf("%+v", got)
			}
			// Ignoring no-op settings must not consume future manual Stop orders.
			r.Stop(6)
			if r.commandSequence != 6 || len(commands.commands) != 2 {
				t.Fatal("manual Stop lost precedence")
			}
		})
	}
}

func TestCommandChangedSafetySettingsStillStopAndSupersede(t *testing.T) {
	for _, setting := range []string{"departure", "floor", "timing"} {
		t.Run(setting, func(t *testing.T) {
			r, commands := readySiteRobot()
			done, _ := startForwardCommand(t, r, 1)
			var err error
			switch setting {
			case "departure":
				err = r.SetUnderBodyAllowance(1, true)
			case "floor":
				err = r.SetAllowUnobservedFloor(1, true)
			case "timing":
				err = r.SetArrivalTiming(1, true)
			}
			if err != nil {
				t.Fatal(err)
			}
			got := <-done
			if got.State != "superseded" || got.Snapshot.Moving || len(commands.commands) != 1 || commands.commands[0].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
				t.Fatalf("policy change did not stop: %+v", got)
			}
		})
	}
}

func TestObjectApproachPolicyCannotUseUnobservedFloor(t *testing.T) {
	r, commands := readySiteRobot()
	r.allowUnobservedFloor, r.underBodyAllowance = true, true
	delete(r.observed.Grid.Floor, sitenav.Cell{X: 10, Y: 0})
	if err := r.destinationWithPolicyLocked("site/revision", 1, 0, true, true); err == nil || r.moving || r.strictMeasuredFloor {
		t.Fatal("strict dispatch accepted unknown destination floor or retained failed policy")
	}
	if err := r.Destination(1, "site/revision", 1, 0, true); err != nil {
		t.Fatal("object rejection changed manual policy:", err)
	}
	r.Stop(2)
	r.observed.Grid.Floor[sitenav.Cell{X: 10, Y: 0}] = true
	if err := r.destinationWithPolicyLocked("site/revision", 1, 0, true, true); err != nil {
		t.Fatal(err)
	}
	if !r.strictMeasuredFloor {
		t.Fatal("strict policy did not survive successful dispatch")
	}
	commands.commands = nil
	r.observed.Grid.Floor = map[sitenav.Cell]bool{}
	r.stepLocked(time.Now())
	if r.moving || r.strictMeasuredFloor || !r.allowUnobservedFloor || !r.underBodyAllowance {
		t.Fatal("lost measured floor did not stop or changed persistent operator policy")
	}
	for _, command := range commands.commands {
		if command.Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
			t.Fatal("replanning used permissive policy after measured floor vanished")
		}
	}
}

func awaitCommand(t *testing.T, r *SiteRobot) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		moving := r.moving
		r.mu.Unlock()
		if moving {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("command did not start")
}
