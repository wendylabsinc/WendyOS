package twinrobot

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func TestRecoveredMeasurementsDoNotRewriteFailedCommandReceipt(t *testing.T) {
	r, commands := readySiteRobot()
	r.frame = "" // A relative turn does not depend on map alignment.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	results := make(chan CommandResult, 1)
	go func() { results <- r.ExecuteCommand(ctx, Command{Action: "turn", Degrees: 180}) }()
	awaitCommand(t, r)
	r.mu.Lock()
	now := time.Now()
	r.observed.Pose.Received = now.Add(-time.Second)
	r.stepLocked(now)
	cause := r.motionFailure
	if cause != "Waiting for fresh Go2 odometry." {
		r.mu.Unlock()
		t.Fatalf("wrong stale-pose fixture: %q", cause)
	}
	// Recover before ExecuteCommand polls its terminal state. The result must
	// still report the aborted move, while current sensor status is healthy.
	deliverHealthyMotionTestCloud(t, r)
	r.mu.Unlock()
	result := <-results
	if result.State != "failed" || result.Message != cause {
		t.Fatalf("recovery changed the failed command receipt: %+v", result)
	}
	for _, status := range []SiteRobotSnapshot{result.Snapshot, r.Status(), r.Snapshot()} {
		if !status.MeasurementsReady || !status.Pose || status.Aligned || status.Ready || status.Moving || status.LookState != 3 {
			t.Fatalf("recovery resumed movement or changed readiness semantics: %+v", status)
		}
		if status.LastMotionFailure != cause || status.Message != cause || status.ReadinessMessage != "Fresh Go2 odometry and LiDAR measurements." {
			t.Fatalf("current readiness and historical failure were conflated: %+v", status)
		}
	}
	if len(commands.commands) != 1 || commands.commands[0].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
		t.Fatalf("telemetry recovery issued movement: %+v", commands.commands)
	}
	if r.motionFailure != cause {
		t.Fatal("reading status acknowledged the old failure")
	}
}

func TestReadinessMessageReportsCurrentMeasurementsIndependentlyOfMapAndFailure(t *testing.T) {
	for _, scenario := range []string{"fresh without map or floor", "stale pose receipt", "stale pose capture", "stale scan", "stop unconfirmed", "closed"} {
		t.Run(scenario, func(t *testing.T) {
			r, commands := readySiteRobot()
			r.motionFailure, r.message, r.lookState = "Earlier movement failed.", "Earlier movement failed.", 3
			wantReady, wantMessage := false, ""
			switch scenario {
			case "fresh without map or floor":
				r.frame = ""
				r.observed.FloorFault = "The scan does not show clear floor."
				wantReady, wantMessage = true, "Fresh Go2 odometry and LiDAR measurements."
			case "stale pose receipt":
				r.observed.Pose.Received = time.Now().Add(-time.Second)
				wantMessage = "Waiting for fresh Go2 odometry."
			case "stale pose capture":
				r.observed.Pose.Captured = time.Now().Add(-time.Second)
				wantMessage = "Go2 odometry has no fresh capture timestamp. Movement is unavailable."
			case "stale scan":
				r.observed.CloudAt = time.Now().Add(-time.Second)
				wantMessage = "Waiting for a timestamped floor and obstacle scan."
			case "stop unconfirmed":
				r.stopUnconfirmed = true
				wantMessage = stopUnconfirmedMessage
			case "closed":
				r.closed = true
				wantMessage = "Go2 connection closed."
			}
			heartbeat := r.heartbeat
			status := r.Status()
			if status.ReadinessMessage != wantMessage || status.MeasurementsReady != wantReady || status.LastMotionFailure != "Earlier movement failed." {
				t.Fatalf("wrong current readiness: %+v", status)
			}
			if status.Moving || len(commands.commands) != 0 || r.heartbeat != heartbeat {
				t.Fatal("readiness inspection changed motion or renewed its lease")
			}
			if scenario == "fresh without map or floor" && status.Ready {
				t.Fatal("measurement freshness bypassed alignment/floor checks")
			}
		})
	}
}

func TestNewAcceptedOperationAcknowledgesPreviousFailureFields(t *testing.T) {
	r, _ := readySiteRobot()
	r.motionFailure, r.message, r.lookState = "Old failure", "Old failure", 3
	if err := r.Look(1, 1); err != nil {
		t.Fatal(err)
	}
	status := r.Status()
	if status.LastMotionFailure != "" || !strings.HasPrefix(status.Message, "Turning") || !status.MeasurementsReady {
		t.Fatalf("accepted new operation did not acknowledge old failure: %+v", status)
	}
}

func TestReadinessDiagnosticsKeepMissingAndFutureTimestampsDistinct(t *testing.T) {
	r, _ := readySiteRobot()
	now := time.Now()
	r.observed.Pose.Received = now.Add(-time.Second)
	r.observed.Pose.Captured = time.Time{}
	r.observed.CloudAt = now.Add(200 * time.Millisecond)
	status := r.Status()
	if status.PoseReceivedAgeMS == nil || *status.PoseReceivedAgeMS < 1000 || status.PoseCaptureAgeMS != nil || status.ScanAgeMS == nil || *status.ScanAgeMS >= 0 {
		t.Fatalf("ages hid missing timestamps or clock skew: %+v", status)
	}
	if status.MeasurementsReady || status.Pose {
		t.Fatal("diagnostic ages weakened freshness checks")
	}
	*status.PoseReceivedAgeMS = 0
	if next := r.Status(); next.PoseReceivedAgeMS == nil || *next.PoseReceivedAgeMS < 1000 {
		t.Fatal("snapshot mutation changed observed timestamps")
	}
}

func TestLatestObservationFaultIsAvailableWithoutDiskOrChangingReadiness(t *testing.T) {
	r, _ := readySiteRobot()
	now := time.Now()
	r.lastObservationLog = now // Exercise in-memory evidence while disk writes are throttled.
	r.observed.ClockOffset = 20 * time.Millisecond
	r.recordObservationFault(sitenav.PoseTopic, errors.New("old rejected packet"), now.Add(-2*time.Second), now)
	r.recordObservationFault(sitenav.PoseTopic, errors.New("latest rejected packet"), now.Add(-time.Second), now)
	status := r.Status()
	fault := status.LatestObservationFault
	if fault == nil || !strings.Contains(fault.Error, "latest rejected packet") || fault.GatewayAgeMS != 1000 || fault.ClockOffsetMS != 20 || fault.PoseAgeMS == nil || fault.CaptureAgeMS == nil {
		t.Fatalf("latest packet rejection missing or throttled: %+v", fault)
	}
	if !status.MeasurementsReady || status.ReadinessMessage != "Fresh Go2 odometry and LiDAR measurements." {
		t.Fatal("historical packet fault replaced current readiness")
	}
	fault.Error, *fault.PoseAgeMS = "caller changed snapshot", 99999
	if next := r.Status().LatestObservationFault; !strings.Contains(next.Error, "latest rejected packet") || *next.PoseAgeMS == 99999 {
		t.Fatal("snapshot shared mutable diagnostic state")
	}
	r.observed.ClockOffset = 0
	deliverHealthyMotionTestCloud(t, r)
	if status := r.Status(); status.LastCloudProcessingMS <= 0 || status.LatestObservationFault == nil {
		t.Fatalf("cloud processing timing or historical evidence missing: %+v", status)
	}
}
