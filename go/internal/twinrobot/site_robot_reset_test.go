package twinrobot

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func TestResetLocalizationStopsAndDiscardsConfirmedPosition(t *testing.T) {
	r, commands := readySiteRobot()
	r.commandSequence, r.localizationSequence = 5, 4
	r.alignment = sitenav.Alignment{Site: sitenav.Point{X: 8}, Rotation: 1}
	r.floorY = 3
	r.localizations = []siteRobotLocalization{{frame: r.frame}}
	r.fusionFrame, r.fusionEpoch = r.frame, 7
	r.fusionTracker.Update(sitenav.Pose{}, sitenav.SurfaceCloud{}, nil, time.Now())
	r.discoveryHistory = discoveryHistory{root: 3, starts: 3, visited: []sitenav.Point{{X: 1}}}
	r.moving, r.stopUnconfirmed = true, true
	r.route, r.preview = []sitenav.Point{{X: 2}}, []sitenav.Point{{X: 3}}
	r.motionFailure = "old failure"
	r.lookState, r.discoveryState = 3, 3
	pose, grid := r.observed.Pose, r.observed.Grid
	if err := r.ResetLocalization(6, "site/revision"); err != nil {
		t.Fatal(err)
	}
	if len(commands.commands) != 1 || commands.commands[0].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
		t.Fatalf("reset must only STOP: %+v", commands.commands)
	}
	if r.frame != "" || r.alignment != (sitenav.Alignment{}) || r.floorY != 0 || len(r.localizations) != 0 || r.localizationSequence != 0 || r.fusionFrame != "" || r.fusionEpoch != 0 || !reflect.DeepEqual(r.fusionTracker, sitenav.PoseTracker{}) {
		t.Fatal("old alignment or localization evidence survived reset")
	}
	if r.moving || r.stopUnconfirmed || r.motionFailure != "" || len(r.route) != 0 || len(r.preview) != 0 || r.lookState != 0 || r.discoveryState != 0 || !reflect.DeepEqual(r.discoveryHistory, discoveryHistory{}) {
		t.Fatal("old motion state survived reset")
	}
	if r.observed.Pose != pose || !reflect.DeepEqual(r.observed.Grid, grid) {
		t.Fatal("reset erased live odometry or measured floor")
	}
	if err := r.ConfirmLocalization(7, 4, "site/revision", 0); err == nil {
		t.Fatal("confirmed an old match after reset")
	}
	if err := r.Destination(8, "site/revision", 2, 0, true); err == nil || r.moving {
		t.Fatal("old alignment authorized a destination after reset")
	}
}

func TestResetLocalizationRequiresFreshOrderAndAcknowledgedStop(t *testing.T) {
	for _, fault := range []string{"stale", "foreign-frame", "stop-failure"} {
		t.Run(fault, func(t *testing.T) {
			r, commands := readySiteRobot()
			r.commandSequence, r.localizationSequence = 10, 9
			r.localizations = []siteRobotLocalization{{frame: r.frame}}
			r.fusionFrame = r.frame
			r.motionFailure = "previous failure"
			sequence, frame := uint64(11), r.frame
			switch fault {
			case "stale":
				sequence = 10
			case "foreign-frame":
				frame = "other/revision"
			case "stop-failure":
				commands.stopError = errors.New("transport unavailable")
			}
			if err := r.ResetLocalization(sequence, frame); err == nil {
				t.Fatal("invalid reset succeeded")
			}
			if r.frame != "site/revision" || r.localizationSequence != 9 || len(r.localizations) != 1 || r.fusionFrame != r.frame || r.motionFailure == "" {
				t.Fatal("failed reset erased localization or motion failure")
			}
			if fault == "stop-failure" {
				if !r.stopUnconfirmed || len(commands.commands) != 1 {
					t.Fatal("failed STOP was not retained")
				}
				if err := r.DiscoveryStep(12, 12); err == nil || r.moving {
					t.Fatal("unacknowledged STOP allowed discovery to restart")
				}
				commands.stopError = nil
				if err := r.ResetLocalization(13, frame); err != nil || r.frame != "" || r.stopUnconfirmed {
					t.Fatalf("explicit retry failed: %v", err)
				}
			} else if len(commands.commands) != 0 || r.commandSequence != 10 {
				t.Fatal("stale or foreign reset issued STOP")
			}
		})
	}
}
