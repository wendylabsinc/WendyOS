package twinrobot

import (
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func TestRoomCorrectionPreservesSiteDestinationAndMotionLimits(t *testing.T) {
	r, commands := readySiteRobot()
	if err := r.Destination(1, r.frame, 2, 0, true); err != nil {
		t.Fatal(err)
	}
	destination := r.alignment.ToSite(r.goal)
	started, heartbeat, sequence := r.started, r.heartbeat, r.commandSequence
	origin, departure := r.alignment.Origin, r.departure
	r.applyRoomFitLocked(r.observed.Pose, sitenav.PoseCandidate{Position: sitenav.Point{X: .1}, Rotation: .03})
	if r.alignment.ToSite(r.goal).Distance(destination) > 1e-8 {
		t.Fatal("correction changed user's destination")
	}
	if !r.moving || len(r.route) == 0 {
		t.Fatal("clear corrected route was not replanned")
	}
	if r.started != started || r.heartbeat != heartbeat || r.commandSequence != sequence || r.alignment.Origin != origin || r.departure != departure {
		t.Fatal("correction extended movement authorization")
	}
	if len(commands.commands) != 1 || commands.commands[0].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
		t.Fatal("refit must stop before replanning, never issue MOVE")
	}
}

func TestRoomCorrectionCannotMoveOrClearFaults(t *testing.T) {
	r, commands := readySiteRobot()
	r.heartbeat = time.Now().Add(-time.Second)
	r.motionFailure = "existing failure"
	r.preview = []sitenav.Point{{X: 2}}
	heartbeat := r.heartbeat
	r.applyRoomFitLocked(r.observed.Pose, sitenav.PoseCandidate{Position: sitenav.Point{X: .1}})
	if len(commands.commands) != 0 || r.moving || r.heartbeat != heartbeat || r.motionFailure != "existing failure" || len(r.preview) != 0 {
		t.Fatal("background fit altered control authorization or retained stale preview")
	}
}

func TestRoomCorrectionStopsWhenReplanningFails(t *testing.T) {
	for _, stopFailure := range []bool{false, true} {
		r, commands := readySiteRobot()
		if err := r.Destination(1, r.frame, 2, 0, true); err != nil {
			t.Fatal(err)
		}
		if stopFailure {
			commands.stopError = errors.New("stop unavailable")
		} else {
			r.observed.Grid = sitenav.Grid{}
		}
		r.applyRoomFitLocked(r.observed.Pose, sitenav.PoseCandidate{Position: sitenav.Point{X: .1}})
		if r.moving || len(r.route) != 0 {
			t.Fatal("continued after replan failure")
		}
		for _, c := range commands.commands {
			if c.Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
				t.Fatal("sent movement after correction failure")
			}
		}
		if stopFailure && !r.stopUnconfirmed {
			t.Fatal("lost stop failure")
		}
	}
}

func TestInFlightRoomCorrectionCannotSurviveInvalidation(t *testing.T) {
	for _, fault := range []string{"none", "stop", "epoch", "model", "frame", "alignment", "old-scan", "stale-pose", "stop-unconfirmed", "closed"} {
		t.Run(fault, func(t *testing.T) {
			r, commands := readySiteRobot()
			r.observed.Surfaces.Surfaces = make([]sitenav.Surface, 24)
			g := &roomTracking{frame: r.frame}
			r.roomTracking = g
			pose, alignment, sequence, captured := r.observed.Pose, r.alignment, r.commandSequence, r.observed.CloudAt
			switch fault {
			case "stop":
				r.Stop(1)
			case "epoch":
				r.poseEpoch++
			case "model":
				r.roomTracking = &roomTracking{frame: r.frame}
			case "frame":
				r.frame = "other/revision"
			case "alignment":
				r.alignment.Site.X = 5
			case "old-scan":
				captured = captured.Add(-time.Second)
			case "stale-pose":
				r.observed.Pose.Captured = captured.Add(-time.Second)
			case "stop-unconfirmed":
				r.stopUnconfirmed = true
			case "closed":
				r.closed = true
			}
			before, commandCount := r.alignment, len(commands.commands)
			r.commitRoomFitLocked(g, pose, alignment, sequence, captured, sitenav.PoseCandidate{Position: sitenav.Point{X: .1}})
			if (r.alignment != before) != (fault == "none") {
				t.Fatal("invalidated fit applied, or fresh fit ignored")
			}
			if len(commands.commands) != commandCount {
				t.Fatal("fit issued an unsolicited command")
			}
		})
	}
}
