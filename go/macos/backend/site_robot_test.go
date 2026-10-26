package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wendylabsinc/wendy/go/macos/backend/sitenav"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"math"
	"testing"
	"time"
)

func TestSiteRobotRejectedPoseDoesNotEraseContinuity(t *testing.T) {
	for _, kind := range []string{"delayed", "future-arrival", "stale-capture", "duplicate", "jump", "clock-reset"} {
		t.Run(kind, func(t *testing.T) {
			r, _ := readySiteRobot()
			previous := r.observed.Pose
			r.poseHistory = []sitenav.Pose{previous}
			now := previous.Received.Add(20 * time.Millisecond)
			received, capture := now, now
			position := []float64{0, 0, .3}
			switch kind {
			case "delayed":
				received = now.Add(-time.Second)
			case "future-arrival":
				received = now.Add(100 * time.Millisecond)
			case "stale-capture":
				capture = now.Add(-time.Second)
			case "duplicate":
				capture = previous.Captured
			case "jump":
				position[0] = 5
			case "clock-reset":
				capture = previous.Captured.Add(-time.Millisecond)
			}
			raw, _ := json.Marshal(map[string]any{"topic": sitenav.PoseTopic, "data": map[string]any{
				"position": position, "imu_state": map[string]any{"quaternion": []float64{1, 0, 0, 0}},
				"stamp": map[string]any{"sec": capture.Unix(), "nanosec": capture.Nanosecond()},
			}})
			r.lastObservationLog = now // Unit tests do not write support diagnostics.
			r.receiveObservationLocked(sitenav.PoseTopic, raw, false, received, now)
			if kind == "jump" || kind == "clock-reset" {
				if r.frame != "" || r.poseEpoch != 1 || !r.observed.Pose.Received.IsZero() {
					t.Fatal("discontinuity retained alignment")
				}
				return
			}
			if r.frame != "site/revision" || r.poseEpoch != 0 || r.observed.Pose != previous || len(r.poseHistory) != 1 {
				t.Fatal("rejected packet erased or renewed accepted pose")
			}
			if !r.Snapshot().Pose {
				t.Fatal("fresh last-known pose flickered off")
			}
			// Replaying an identical packet never renews the pose lease.
			if r.observed.Ready(now.Add(time.Second)) == nil {
				t.Fatal("rejected packet extended freshness")
			}
			// The next fresh sample must still be compared against the retained pose.
			next := now.Add(20 * time.Millisecond)
			raw, _ = json.Marshal(map[string]any{"topic": sitenav.PoseTopic, "data": map[string]any{
				"position": []float64{5, 0, .3}, "imu_state": map[string]any{"quaternion": []float64{1, 0, 0, 0}},
				"stamp": map[string]any{"sec": next.Unix(), "nanosec": next.Nanosecond()},
			}})
			r.receiveObservationLocked(sitenav.PoseTopic, raw, false, next, next)
			if r.frame != "" {
				t.Fatal("packet rejection allowed subsequent pose jump")
			}
		})
	}
}

type robotCommands struct {
	pb.WendyUnitreeRobotServiceClient
	commands  []*pb.UnitreeCommandRequest
	stopError error
	minYaw    float64
}

func (f *robotCommands) Command(_ context.Context, r *pb.UnitreeCommandRequest, _ ...grpc.CallOption) (*pb.UnitreeCommandResponse, error) {
	f.commands = append(f.commands, r)
	if r.Command == pb.UnitreeCommand_UNITREE_COMMAND_MOVE && r.Yaw != 0 && math.Abs(r.Yaw) < f.minYaw {
		return nil, errors.New("SDK rejected rotation below its minimum rate")
	}
	if r.Command == pb.UnitreeCommand_UNITREE_COMMAND_STOP {
		return nil, f.stopError
	}
	return &pb.UnitreeCommandResponse{}, nil
}
func readySiteRobot() (*SiteRobot, *robotCommands) {
	now := time.Now()
	f := &robotCommands{}
	g := sitenav.Grid{Floor: map[sitenav.Cell]bool{}, Obstacles: map[sitenav.Cell]bool{}}
	for x := -20; x < 70; x++ {
		for y := -20; y < 20; y++ {
			g.Floor[sitenav.Cell{X: x, Y: y}] = true
		}
	}
	r := &SiteRobot{ctx: context.Background(), client: f, serial: "ONLY-THIS-ROBOT", frame: "site/revision", heartbeat: now, observed: sitenav.Observations{Pose: sitenav.Pose{Position: sitenav.Point{}, Height: .3, Received: now, Captured: now}, Grid: g, CloudAt: now}}
	return r, f
}
func TestSiteRobotPreviewDoesNotMoveAndStopCancels(t *testing.T) {
	r, f := readySiteRobot()
	if err := r.Destination(1, "site/revision", 2, 0, false); err != nil {
		t.Fatal(err)
	}
	if r.moving || len(f.commands) != 0 {
		t.Fatal("preview moved robot")
	}
	if err := r.Destination(2, "other/revision", 2, 0, true); err == nil {
		t.Fatal("wrong site accepted")
	}
	if err := r.Destination(3, "site/revision", 2, 0, true); err != nil {
		t.Fatal(err)
	}
	r.stepLocked(time.Now())
	if len(f.commands) != 1 || f.commands[0].Command != pb.UnitreeCommand_UNITREE_COMMAND_MOVE || f.commands[0].Serial != "ONLY-THIS-ROBOT" {
		t.Fatal("wrong target or command", f.commands)
	}
	r.Stop(4)
	r.stepLockedForTest()
	if r.moving || f.commands[len(f.commands)-1].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
		t.Fatal("stop did not cancel")
	}
}
func (r *SiteRobot) stepLockedForTest() {
	if r.moving {
		r.stepLocked(time.Now())
	}
}

func TestSiteRobotTimingChangeStopsAndInvalidatesScan(t *testing.T) {
	r, f := readySiteRobot()
	if err := r.Destination(1, "site/revision", 2, 0, true); err != nil {
		t.Fatal(err)
	}
	if err := r.SetArrivalTiming(2, true); err != nil {
		t.Fatal(err)
	}
	s := r.Snapshot()
	if r.moving || len(r.preview) != 0 || s.Ready || !s.ArrivalTiming || !r.observed.CloudAt.IsZero() {
		t.Fatal("timing change retained movement or old scan")
	}
	if len(f.commands) != 1 || f.commands[0].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
		t.Fatal("timing change did not stop")
	}
	if r.SetArrivalTiming(1, false) == nil || !r.observed.AllowArrivalTime {
		t.Fatal("superseded timing change accepted")
	}
	r, f = readySiteRobot()
	r.moving = true
	f.stopError = errors.New("offline")
	if r.SetArrivalTiming(1, true) == nil || r.observed.AllowArrivalTime || r.Snapshot().Ready {
		t.Fatal("timing changed despite failed stop")
	}
}

func TestSiteRobotLocalizationConfirmationIsBoundToPoseAndRequest(t *testing.T) {
	for _, fault := range []string{"valid", "moved", "expired", "wrong-site", "superseded", "stale-scan"} {
		t.Run(fault, func(t *testing.T) {
			r, f := readySiteRobot()
			for i := 0; i < 40; i++ {
				r.observed.Landmarks = append(r.observed.Landmarks, sitenav.Point{X: float64(i) * .1, Y: float64(i%2) * 2})
			}
			r.frame = ""
			r.commandSequence = 5
			r.localizationSequence = 5
			r.localizations = []siteRobotLocalization{{alignment: sitenav.Alignment{Origin: r.observed.Pose.Position, Site: sitenav.Point{X: 2, Y: 3}, Rotation: .7}, pose: r.observed.Pose, frame: "site/revision", created: time.Now()}}
			frame := "site/revision"
			switch fault {
			case "moved":
				r.observed.Pose.Position.X += .1
			case "expired":
				r.localizations[0].created = time.Now().Add(-31 * time.Second)
			case "wrong-site":
				frame = "other/revision"
			case "superseded":
				r.commandSequence = 6
			case "stale-scan":
				r.observed.CloudAt = time.Now().Add(-time.Second)
			}
			err := r.ConfirmLocalization(7, 5, frame, 0)
			if fault == "valid" {
				if err != nil || r.frame != "site/revision" {
					t.Fatalf("confirmation failed: %v", err)
				}
			} else if err == nil || r.frame != "" {
				t.Fatal("invalid candidate became navigation alignment")
			}
			if r.moving || len(f.commands) != 0 {
				t.Fatal("confirming position commanded motion")
			}
		})
	}
}
func TestSiteRobotStopsOnLostUIStaleSensorsAndTimeout(t *testing.T) {
	for _, fault := range []string{"ui", "pose", "capture", "cloud", "timeout", "obstacle", "connection"} {
		t.Run(fault, func(t *testing.T) {
			r, f := readySiteRobot()
			if err := r.Destination(3, "site/revision", 2, 0, true); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			switch fault {
			case "ui":
				r.heartbeat = now.Add(-time.Second)
			case "pose":
				r.observed.Pose.Received = now.Add(-time.Second)
			case "capture":
				r.observed.Pose.Captured = time.Time{}
			case "cloud":
				r.observed.CloudAt = now.Add(-time.Second)
			case "timeout":
				r.started = now.Add(-31 * time.Second)
			case "obstacle":
				r.observed.Grid.Obstacles[sitenav.Cell{X: 1, Y: 0}] = true
			case "connection":
				r.closed = true
			}
			r.stepLocked(now)
			if r.moving || len(f.commands) == 0 {
				t.Fatal("did not stop")
			}
			for _, c := range f.commands {
				if c.Command == pb.UnitreeCommand_UNITREE_COMMAND_MOVE {
					t.Fatal("moved after fault")
				}
			}
		})
	}
}

func TestSiteRobotStopSupersedesQueuedMove(t *testing.T) {
	r, f := readySiteRobot()
	r.Stop(9)
	if err := r.Destination(8, "site/revision", 2, 0, true); err == nil {
		t.Fatal("old order accepted after stop")
	}
	if r.moving || len(f.commands) != 1 || f.commands[0].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
		t.Fatal("superseded order moved")
	}
}
func TestSiteRobotFailedStopPreventsReplacementOrder(t *testing.T) {
	r, f := readySiteRobot()
	if err := r.Destination(1, "site/revision", 2, 0, true); err != nil {
		t.Fatal(err)
	}
	f.stopError = errors.New("no acknowledgement")
	if err := r.Destination(2, "site/revision", 3, 0, true); err == nil {
		t.Fatal("replacement accepted despite failed stop")
	}
	if err := r.Destination(3, "site/revision", 3, 0, true); err == nil {
		t.Fatal("unconfirmed stop forgotten")
	}
	if r.moving || r.Snapshot().Ready {
		t.Fatal("ready after failed stop")
	}
	f.stopError = nil
	r.Stop(4)
	if !r.Snapshot().Ready {
		t.Fatal("acknowledged stop did not recover")
	}
}
func TestSiteRobotClosedLoopReachesTurnedDestination(t *testing.T) {
	r, f := readySiteRobot()
	f.minYaw = .5
	// Begin facing away from a destination, and integrate commanded velocities.
	// Sensors and UI remain fresh at each simulated 100 ms update.
	r.observed.Pose.Yaw = math.Pi
	if err := r.Destination(1, "site/revision", 2, -1, true); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i := 0; i < 300 && r.moving; i++ {
		now = now.Add(100 * time.Millisecond)
		r.heartbeat = now
		r.observed.Pose.Received = now
		r.observed.Pose.Captured = now
		r.observed.CloudAt = now
		r.stepLocked(now)
		c := f.commands[len(f.commands)-1]
		if c.Command != pb.UnitreeCommand_UNITREE_COMMAND_MOVE {
			continue
		}
		if math.Abs(c.Forward) > sitenav.WalkingSpeed || math.Abs(c.Yaw) > sitenav.TurningSpeed {
			t.Fatal("unbounded command")
		}
		r.observed.Pose.Yaw += c.Yaw * .1
		r.observed.Pose.Position.X += c.Forward * math.Cos(r.observed.Pose.Yaw) * .1
		r.observed.Pose.Position.Y += c.Forward * math.Sin(r.observed.Pose.Yaw) * .1
	}
	if r.moving || r.message != "Arrived" || r.observed.Pose.Position.Distance(sitenav.Point{X: 2, Y: 1}) >= .22 {
		t.Fatalf("did not arrive: %s, %+v", r.message, r.observed.Pose.Position)
	}
	if f.commands[len(f.commands)-1].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
		t.Fatal("arrival must stop")
	}
}

func TestSiteRobotNarrowAisleApproachAndDynamicHazards(t *testing.T) {
	for _, fault := range []string{"none", "new-obstacle", "unknown-floor", "stale-scan"} {
		t.Run(fault, func(t *testing.T) {
			r, f := readySiteRobot()
			for x := -20; x < 60; x++ {
				r.observed.Grid.Obstacles[sitenav.Cell{X: x, Y: 3}] = true
				r.observed.Grid.Obstacles[sitenav.Cell{X: x, Y: -4}] = true
			}
			// Side clearance stays at 10 cm. The higher, executable walking rate
			// also requires room ahead for its command lease and braking horizon.
			for y := -3; y <= 2; y++ {
				r.observed.Grid.Obstacles[sitenav.Cell{X: 33, Y: y}] = true
			}
			if err := r.Destination(1, "site/revision", 2, 0, false); err != nil {
				t.Fatal(err)
			}
			if len(f.commands) != 0 {
				t.Fatal("preview issued a command")
			}
			if err := r.Destination(2, "site/revision", 2, 0, true); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			for i := 0; i < 300 && r.moving; i++ {
				now = now.Add(100 * time.Millisecond)
				r.heartbeat = now
				r.observed.Pose.Received = now
				r.observed.Pose.Captured = now
				r.observed.CloudAt = now
				if i == 15 {
					switch fault {
					case "new-obstacle":
						r.observed.Grid.Obstacles[sitenav.Cell{X: 13, Y: 0}] = true
					case "unknown-floor":
						delete(r.observed.Grid.Floor, sitenav.Cell{X: 13, Y: 0})
					case "stale-scan":
						r.observed.CloudAt = now.Add(-time.Second)
					}
				}
				before := len(f.commands)
				r.stepLocked(now)
				c := f.commands[len(f.commands)-1]
				if i >= 15 && fault != "none" {
					for _, command := range f.commands[before:] {
						if command.Command == pb.UnitreeCommand_UNITREE_COMMAND_MOVE {
							t.Fatal("moved after hazard")
						}
					}
				}
				if c.Command == pb.UnitreeCommand_UNITREE_COMMAND_MOVE {
					r.observed.Pose.Yaw += c.Yaw * .1
					r.observed.Pose.Position.X += c.Forward * math.Cos(r.observed.Pose.Yaw) * .1
					r.observed.Pose.Position.Y += c.Forward * math.Sin(r.observed.Pose.Yaw) * .1
				}
			}
			if r.moving {
				t.Fatal("failed to stop")
			}
			if fault == "none" && (r.message != "Arrived" || r.observed.Pose.Position.Distance(sitenav.Point{X: 2}) >= .08) {
				t.Fatalf("tight-space approach failed: %s %+v", r.message, r.observed.Pose.Position)
			}
			if fault != "none" && r.message == "Arrived" {
				t.Fatal("hazard was ignored")
			}
		})
	}
}

func TestSiteRobotRejectsRotationInsideNarrowAisle(t *testing.T) {
	r, f := readySiteRobot()
	for x := -20; x < 60; x++ {
		r.observed.Grid.Obstacles[sitenav.Cell{X: x, Y: 3}] = true
		r.observed.Grid.Obstacles[sitenav.Cell{X: x, Y: -4}] = true
	}
	if !r.Snapshot().Ready {
		t.Fatal("lengthways pose should fit")
	}
	r.observed.Pose.Yaw = math.Pi / 2
	if r.Snapshot().Ready {
		t.Fatal("sideways footprint should not fit")
	}
	if r.Destination(1, "site/revision", 2, 0, true) == nil || len(f.commands) != 0 {
		t.Fatal("unsafe start pose issued movement")
	}
}

func TestSiteRobotUnderBodyAllowanceIsExplicitAndStopsOnChange(t *testing.T) {
	r, f := readySiteRobot()
	for x := -3; x <= 2; x++ {
		for y := -2; y <= 1; y++ {
			delete(r.observed.Grid.Floor, sitenav.Cell{X: x, Y: y})
		}
	}
	if r.Snapshot().Ready || r.Snapshot().UnderBodyAllowance {
		t.Fatal("hidden departure floor was ready by default")
	}
	if r.Destination(1, "site/revision", 2, 0, false) == nil {
		t.Fatal("default allowed hidden floor")
	}
	if err := r.SetUnderBodyAllowance(2, true); err != nil {
		t.Fatal(err)
	}
	if !r.Snapshot().Ready || !r.Snapshot().UnderBodyAllowance {
		t.Fatal("explicit departure allowance not ready")
	}
	if err := r.Destination(3, "site/revision", 2, 0, false); err != nil {
		t.Fatal(err)
	}
	if r.moving || len(f.commands) != 0 {
		t.Fatal("toggle or preview caused motion")
	}
	if r.SetUnderBodyAllowance(1, false) == nil {
		t.Fatal("stale setting accepted")
	}
	if err := r.Destination(4, "site/revision", 2, 0, true); err != nil {
		t.Fatal(err)
	}
	if err := r.SetUnderBodyAllowance(5, false); err != nil {
		t.Fatal(err)
	}
	if r.moving || r.departure != nil || len(r.preview) != 0 || r.underBodyAllowance {
		t.Fatal("mode change retained motion/allowance")
	}
	if len(f.commands) != 1 || f.commands[0].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
		t.Fatal("mode change did not stop")
	}
	r.moving = true
	f.stopError = errors.New("offline")
	if r.SetUnderBodyAllowance(6, true) == nil || r.underBodyAllowance {
		t.Fatal("mode enabled after failed stop")
	}
}

func TestSiteRobotCanConfirmPositionWithoutAllowingUnsafeNavigation(t *testing.T) {
	r, f := readySiteRobot()
	for i := 0; i < 40; i++ {
		r.observed.Landmarks = append(r.observed.Landmarks, sitenav.Point{X: float64(i) * .1, Y: float64(i%2) * 2})
	}
	r.observed.FloorFault = "Uncertain floor"
	r.observed.Grid = sitenav.Grid{}
	r.frame = ""
	r.commandSequence = 5
	r.localizationSequence = 5
	r.localizations = []siteRobotLocalization{{pose: r.observed.Pose, frame: "site/revision", created: time.Now()}}
	if !r.Snapshot().ScanReady || r.Snapshot().Ready {
		t.Fatal("localization and navigation readiness not separated")
	}
	if err := r.ConfirmLocalization(6, 5, "site/revision", 0); err != nil {
		t.Fatal(err)
	}
	if !r.Snapshot().Aligned || r.Snapshot().Ready {
		t.Fatal("confirmed pose bypassed navigation floor check")
	}
	if r.Destination(7, "site/revision", 1, 0, true) == nil || r.moving || len(f.commands) != 0 {
		t.Fatal("uncertain floor allowed motion")
	}
}

func TestSiteRobotVideoDoesNotTakeNavigationLockOrSendCommands(t *testing.T) {
	r, commands := readySiteRobot()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.ctx = ctx
	r.videoFrames = make(chan []byte, 2)
	r.videoFrames <- []byte{0, 0, 1, 0x65}
	r.mu.Lock()
	done := make(chan error, 1)
	go func() {
		frame, err := r.NextVideo()
		if err == nil && len(frame) != 4 {
			err = errors.New("missing video bytes")
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		r.mu.Unlock()
		t.Fatal("video blocked on motion lock")
	}
	r.mu.Unlock()
	cancel()
	if _, err := r.NextVideo(); err == nil {
		t.Fatal("closed session returned video")
	}
	if len(commands.commands) != 0 {
		t.Fatal("camera issued motion commands")
	}
}

func TestSiteRobotReportsMissingLocalizationStructure(t *testing.T) {
	r, _ := readySiteRobot()
	r.frame = ""
	r.message = "Live LiDAR and odometry."
	s := r.Snapshot()
	if s.ScanReady || s.Message == r.message {
		t.Fatal("missing localization structure was hidden by live telemetry status")
	}
}

func TestWalkingDeadbandAndStoppingHorizon(t *testing.T) {
	r, f := readySiteRobot()
	if err := r.Destination(1, "site/revision", 2, 0, true); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i := 0; i < 100 && r.moving; i++ {
		now = now.Add(100 * time.Millisecond)
		r.heartbeat = now
		r.observed.Pose.Received = now
		r.observed.Pose.Captured = now
		r.observed.CloudAt = now
		r.stepLocked(now)
		command := f.commands[len(f.commands)-1]
		if command.Command == pb.UnitreeCommand_UNITREE_COMMAND_MOVE && command.Forward >= .5 {
			r.observed.Pose.Position.X += command.Forward * .1
		}
	}
	if r.moving || r.message != "Arrived" {
		t.Fatal("robot with a walking deadband did not arrive", r.message)
	}
	r, f = readySiteRobot()
	if err := r.Destination(1, "site/revision", .2, 0, true); err != nil {
		t.Fatal(err)
	}
	// The target footprint is clear but the 0.55m/s stopping envelope is not.
	r.observed.Grid.Obstacles[sitenav.Cell{X: 9, Y: 0}] = true
	r.stepLocked(time.Now())
	if r.moving || len(f.commands) != 1 || f.commands[0].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
		t.Fatal("speed increase bypassed forward stopping clearance")
	}
}

func TestSupervisedUnobservedFloorStillStopsForHazardsAndLostTelemetry(t *testing.T) {
	for _, fault := range []string{"obstacle", "stale-pose", "stale-scan", "stop", "mode-change"} {
		t.Run(fault, func(t *testing.T) {
			r, f := readySiteRobot()
			r.observed.Grid.Floor = map[sitenav.Cell]bool{}
			if r.Snapshot().Ready || r.Snapshot().AllowUnobservedFloor {
				t.Fatal("unknown floor allowed by default")
			}
			if err := r.SetAllowUnobservedFloor(1, true); err != nil {
				t.Fatal(err)
			}
			if !r.Snapshot().Ready || !r.Snapshot().AllowUnobservedFloor {
				t.Fatal("supervised mode did not admit unknown floor")
			}
			if len(r.observed.Grid.Floor) != 0 || len(r.observed.Grid.ObservedClearFloor()) != 0 {
				t.Fatal("invented measured floor")
			}
			if err := r.Destination(2, "site/revision", 2, 0, false); err != nil {
				t.Fatal(err)
			}
			if r.moving || len(f.commands) > 0 {
				t.Fatal("mode or preview caused motion")
			}
			if err := r.Destination(3, "site/revision", 2, 0, true); err != nil {
				t.Fatal(err)
			}
			r.stepLocked(time.Now())
			if len(f.commands) != 1 || f.commands[0].Command != pb.UnitreeCommand_UNITREE_COMMAND_MOVE {
				t.Fatal("unknown floor still blocked movement")
			}
			switch fault {
			case "obstacle":
				r.observed.Grid.Obstacles[sitenav.Cell{X: 5, Y: 0}] = true
			case "stale-pose":
				r.observed.Pose.Received = time.Now().Add(-time.Second)
			case "stale-scan":
				r.observed.CloudAt = time.Now().Add(-time.Second)
			case "stop":
				r.Stop(4)
			case "mode-change":
				if err := r.SetAllowUnobservedFloor(4, false); err != nil {
					t.Fatal(err)
				}
			}
			r.stepLocked(time.Now())
			if r.moving || f.commands[len(f.commands)-1].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
				t.Fatal("incident did not stop movement", fault)
			}
		})
	}
	r, f := readySiteRobot()
	r.moving = true
	f.stopError = errors.New("offline")
	if r.SetAllowUnobservedFloor(1, true) == nil || r.allowUnobservedFloor {
		t.Fatal("enabled mode without confirmed stop")
	}
}

func TestTransientPoseRejectionUsesOnlyStillFreshAcceptedObservations(t *testing.T) {
	for _, expired := range []bool{false, true} {
		for _, kind := range []string{"duplicate", "delayed", "stale-capture", "future-arrival"} {
			t.Run(fmt.Sprintf("%s/expired=%v", kind, expired), func(t *testing.T) {
				r, f := readySiteRobot()
				r.moving = true
				r.started = time.Now()
				r.message = "Moving"
				previous := r.observed.Pose
				now := previous.Received.Add(20 * time.Millisecond)
				if expired {
					now = now.Add(time.Second)
				}
				received, captured := now, previous.Captured
				switch kind {
				case "delayed":
					received = now.Add(-time.Second)
				case "stale-capture":
					captured = now.Add(-time.Second)
				case "future-arrival":
					received = now.Add(time.Second)
				}
				raw, _ := json.Marshal(map[string]any{"topic": sitenav.PoseTopic, "data": map[string]any{"position": []float64{0, 0, .3}, "imu_state": map[string]any{"quaternion": []float64{1, 0, 0, 0}}, "stamp": map[string]any{"sec": captured.Unix(), "nanosec": captured.Nanosecond()}}})
				r.lastObservationLog = now
				r.receiveObservationLocked(sitenav.PoseTopic, raw, false, received, now)
				if r.observed.Pose != previous {
					t.Fatal("rejection changed accepted pose or freshness")
				}
				if expired {
					if r.moving || len(f.commands) != 1 || f.commands[0].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
						t.Fatal("expired pose did not stop")
					}
				} else {
					if !r.moving || len(f.commands) != 0 || r.message != "Moving" {
						t.Fatal("fresh accepted pose was poisoned")
					}
					r.stepLocked(now.Add(time.Second))
					if r.moving || len(f.commands) != 1 {
						t.Fatal("rejection renewed freshness")
					}
				}
			})
		}
	}
}

func TestRouteTurnChecksClearanceBeyondItsTargetHeading(t *testing.T) {
	for _, direction := range []float64{-1, 1} {
		r, f := readySiteRobot()
		r.allowUnobservedFloor = true
		target := sitenav.Point{X: 2 * math.Cos(.06), Y: 2 * math.Sin(.06) * direction}
		found := false
		for x := -8; x <= 8 && !found; x++ {
			for y := -8; y <= 8; y++ {
				cell := sitenav.Cell{X: x, Y: y}
				r.observed.Grid.Obstacles[cell] = true
				g := r.observed.Grid
				if g.Transition(sitenav.Point{}, 0, target) && !g.Turn(sitenav.Point{}, 0, direction*sitenav.TurnStoppingAngle) {
					found = true
					break
				}
				delete(g.Obstacles, cell)
			}
		}
		if !found {
			t.Fatal("missing obstacle just beyond requested turn")
		}
		if err := r.Destination(1, "site/revision", target.X, -target.Y, true); err != nil {
			t.Fatal(err)
		}
		r.stepLocked(time.Now())
		if r.moving || len(f.commands) != 1 || f.commands[0].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
			t.Fatal("turn ignored its stopping sweep")
		}
	}
}
