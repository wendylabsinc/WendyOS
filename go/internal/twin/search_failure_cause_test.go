package twin

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
)

func TestSearchOwnerExpiryPreservesBlockedFloorReason(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	robot := &searchRobot{ready: true, matchAfter: -1}
	robot.discoveryHook = func(int) error {
		return fmt.Errorf("%w: LiDAR floor coverage is incomplete", twinrobot.ErrDiscoveryBlocked)
	}
	robot.statusHook = func(state twinrobot.SiteRobotSnapshot) twinrobot.SiteRobotSnapshot {
		if robot.turns > 0 {
			cancel()
			state.Disconnected = true
			state.Message = context.DeadlineExceeded.Error()
		}
		return state
	}
	session := &robotSession{robot: robot, searchRoot: 1}
	(&Server{}).search(ctx, cancel, session, 1, twinrobot.SiteFusionRequest{Explore: true})
	if session.searchState != "failed" || !strings.Contains(session.searchMessage, "LiDAR floor coverage is incomplete") || !strings.Contains(session.searchMessage, "Several room positions still fit") {
		t.Fatalf("lost reason on owner expiry: %s", session.searchMessage)
	}
}

func TestSearchPreservesLatchedMotionCauseAfterLaterPacketRejection(t *testing.T) {
	for _, motion := range []string{"turn", "viewpoint"} {
		for _, cause := range []string{"latched", "legacy", "disconnected"} {
			t.Run(motion+"/"+cause, func(t *testing.T) {
				robot := &searchRobot{ready: true, matchAfter: -1}
				want := "Discovery stopped: an obstacle overlaps the movement area."
				if motion == "turn" {
					want = "Look turn stopped: obstacle in the turning footprint."
				}
				terminalCause := want
				robot.statusHook = func(state twinrobot.SiteRobotSnapshot) twinrobot.SiteRobotSnapshot {
					if robot.moves == 0 && robot.turns == 0 {
						return state
					}
					if motion == "turn" {
						state.LookState = 3
					} else {
						state.DiscoveryState = 3
					}
					state.LastMotionFailure = terminalCause
					// A later rejected packet changes the legacy message and retained
					// diagnostic, despite currently fresh usable telemetry.
					state.Message = "LiDAR needs fresh, timestamped Go2 odometry."
					state.LatestObservationFault = &twinrobot.SiteObservationFault{At: time.Now().Add(-time.Minute), Error: state.Message}
					state.ReadinessMessage = "Fresh Go2 odometry and LiDAR measurements."
					if cause == "legacy" {
						state.LastMotionFailure = ""
						state.Message = terminalCause
					}
					if cause == "disconnected" {
						state.Disconnected = true
						state.Message = "Go2 observation stream disconnected."
					}
					return state
				}
				if cause == "disconnected" {
					want = "Go2 observation stream disconnected."
				}
				session := &robotSession{robot: robot, searchRoot: 1000000}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				(&Server{}).search(ctx, cancel, session, 1000000, twinrobot.SiteFusionRequest{Explore: motion == "viewpoint"})
				if session.searchState != "failed" || session.searchMessage != want || robot.stopped == 0 {
					t.Fatalf("lost actual failure: state=%s message=%q want=%q", session.searchState, session.searchMessage, want)
				}
				if motion == "turn" && robot.turns != 1 || motion == "viewpoint" && robot.moves != 1 {
					t.Fatal("failure retried motion")
				}
			})
		}
	}
}
