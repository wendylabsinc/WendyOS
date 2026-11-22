package twin

import (
	"context"
	"errors"
	"fmt"
	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
	"strings"
	"testing"
	"time"
)

type searchRobot struct {
	*twinrobot.SiteRobot
	ready                                          bool
	turns, matchAfter                              int
	reject                                         bool
	stopped                                        uint64
	directions                                     []int
	visuals                                        []*sitenav.VisualHint
	moves, discoveryAttempts                       int
	discoveryBlocked, discoveryFailed, matchOnMove bool
	discoveryAllowedTurns                          map[int]bool
	fusionTimeoutAfter                             int
}

func (r *searchRobot) Status() twinrobot.SiteRobotSnapshot {
	return twinrobot.SiteRobotSnapshot{MeasurementsReady: r.ready, LookState: 2, LookDirection: -1, DiscoveryState: func() int {
		if r.discoveryFailed {
			return 3
		}
		return 2
	}()}
}
func (r *searchRobot) FuseLocalization(sequence uint64, input twinrobot.SiteFusionRequest) (twinrobot.SiteFusionReport, error) {
	r.visuals = append(r.visuals, input.Visual)
	if r.fusionTimeoutAfter > 0 && len(r.visuals) > r.fusionTimeoutAfter {
		return twinrobot.SiteFusionReport{}, context.DeadlineExceeded
	}
	if r.matchOnMove && r.moves > 0 || !r.matchOnMove && r.matchAfter >= 0 && r.turns >= r.matchAfter {
		return twinrobot.SiteFusionReport{Candidates: []twinrobot.SiteRobotCandidate{{X: 1}}}, nil
	}
	return twinrobot.SiteFusionReport{Message: "Several room positions still fit.", Accepted: 42}, nil
}
func (r *searchRobot) Look(sequence uint64, direction int) error {
	r.directions = append(r.directions, direction)
	r.turns++
	if r.reject {
		return errors.New("obstacle blocks turn")
	}
	return nil
}
func (r *searchRobot) Stop(sequence uint64) { r.stopped = sequence }
func TestLinuxSearchOwnsTurnsAndCandidateSequence(t *testing.T) {
	robot := &searchRobot{ready: true, matchAfter: 2}
	session := &robotSession{robot: robot, searchRoot: 1000000}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	(&Server{}).search(ctx, cancel, session, 1000000, twinrobot.SiteFusionRequest{Frame: "room/frame"})
	if robot.turns != 2 || session.searchState != "complete" || session.pendingSequence != 1000005 || session.pendingRoot != 1000000 {
		t.Fatalf("turns=%d state=%s sequence=%d", robot.turns, session.searchState, session.pendingSequence)
	}
	if len(robot.directions) != 2 || robot.directions[0] != 0 || robot.directions[1] != -1 {
		t.Fatalf("lost turn direction: %v", robot.directions)
	}
	if robot.stopped != 0 {
		t.Fatal("successful preview stopped with a superseding order")
	}
}
func TestLinuxSearchRejectsUnsafeTurnAndNeverRetries(t *testing.T) {
	robot := &searchRobot{ready: true, matchAfter: -1, reject: true}
	session := &robotSession{robot: robot, searchRoot: 1000000}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	(&Server{}).search(ctx, cancel, session, 1000000, twinrobot.SiteFusionRequest{})
	if robot.turns != 1 || robot.stopped != 1999999 || session.searchState != "failed" || session.searchMessage != "obstacle blocks turn" {
		t.Fatalf("unexpected search: %+v", session)
	}
}
func TestLinuxSearchWaitsForMeasurementsAndCancels(t *testing.T) {
	robot := &searchRobot{ready: false}
	session := &robotSession{robot: robot, searchRoot: 1000000}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	(&Server{}).search(ctx, cancel, session, 1000000, twinrobot.SiteFusionRequest{})
	if robot.turns != 0 || robot.stopped != 1999999 || session.searchState != "failed" {
		t.Fatalf("unexpected search: %+v", session)
	}
}
func TestLinuxSearchHasBoundedFullRotation(t *testing.T) {
	robot := &searchRobot{ready: true, matchAfter: -1}
	session := &robotSession{robot: robot, searchRoot: 1000000}
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Second)
	defer cancel()
	(&Server{}).search(ctx, cancel, session, 1000000, twinrobot.SiteFusionRequest{})
	if robot.moves != 0 || robot.discoveryAttempts != 0 || robot.turns != 12 || session.searchState != "failed" || !strings.Contains(session.searchMessage, "Several room positions") || !strings.Contains(string(session.searchReport), `"Accepted":42`) {
		t.Fatalf("unbounded search: %d %s", robot.turns, session.searchState)
	}
}

func TestLinuxSearchUsesCameraHintOnlyBeforeTheFirstTurn(t *testing.T) {
	robot := &searchRobot{ready: true, matchAfter: 1}
	session := &robotSession{robot: robot, searchRoot: 1000000}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hint := &sitenav.VisualHint{Position: sitenav.Point{X: 2, Y: 3}, Inliers: 80, Residual: 1}
	(&Server{}).search(ctx, cancel, session, 1000000, twinrobot.SiteFusionRequest{Frame: "room/frame", Visual: hint})
	if session.searchState != "complete" || robot.turns != 1 || len(robot.visuals) != 2 {
		t.Fatalf("search did not match after one turn: state=%s turns=%d matches=%d", session.searchState, robot.turns, len(robot.visuals))
	}
	if robot.visuals[0] != hint || robot.visuals[1] != nil {
		t.Fatalf("camera hint missing initially or reused after turning: %v", robot.visuals)
	}
}

func (r *searchRobot) DiscoveryStep(sequence, root uint64) error {
	r.discoveryAttempts++
	if r.discoveryBlocked || r.discoveryAllowedTurns != nil && !r.discoveryAllowedTurns[r.turns] {
		return fmt.Errorf("%w: floor ahead is unobserved", twinrobot.ErrDiscoveryBlocked)
	}
	r.moves++
	return nil
}

func TestDiscoverySearchMatchesTranslatedViewBeforeAnotherTurn(t *testing.T) {
	robot := &searchRobot{ready: true, matchOnMove: true}
	session := &robotSession{robot: robot, searchRoot: 1000000}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	(&Server{}).search(ctx, cancel, session, 1000000, twinrobot.SiteFusionRequest{Explore: true})
	if session.searchState != "complete" || robot.turns != 0 || robot.moves != 1 || session.searchMoves != 1 {
		t.Fatalf("did not gather a translated view: state=%s turns=%d moves=%d", session.searchState, robot.turns, robot.moves)
	}
	if len(robot.visuals) != 2 {
		t.Fatalf("did not re-match immediately after moving: %d", len(robot.visuals))
	}
}

func TestDiscoverySearchIsBoundedAndBlockedViewpointsAreSkipped(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(fmt.Sprint(blocked), func(t *testing.T) {
			t.Parallel()
			robot := &searchRobot{ready: true, matchAfter: -1, discoveryBlocked: blocked}
			if blocked {
				robot.fusionTimeoutAfter = 1
			}
			session := &robotSession{robot: robot, searchRoot: 1000000}
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			(&Server{}).search(ctx, cancel, session, 1000000, twinrobot.SiteFusionRequest{Explore: true})
			wantTurns, wantMoves, wantAttempts := 2, 3, 3
			if blocked {
				wantTurns, wantMoves, wantAttempts = 11, 0, 12
				for _, reason := range []string{"Several room positions still fit", "exceeded its time budget", "blocked at 12 headings", "floor ahead is unobserved"} {
					if !strings.Contains(session.searchMessage, reason) {
						t.Errorf("lost %q in %q", reason, session.searchMessage)
					}
				}
				if !strings.Contains(string(session.searchReport), `"Accepted":42`) {
					t.Error("timeout discarded the last matching evidence")
				}
			}
			if session.searchState != "failed" || robot.turns != wantTurns || robot.discoveryAttempts != wantAttempts || robot.moves != wantMoves || robot.stopped == 0 {
				t.Fatalf("unbounded search: state=%s turns=%d moves=%d attempts=%d", session.searchState, robot.turns, robot.moves, robot.discoveryAttempts)
			}
			if len(robot.visuals) != robot.turns+robot.moves+1 {
				t.Fatal("matched the same viewpoint repeatedly")
			}
		})
	}
}

func TestDiscoveryMovementFailureEndsSearch(t *testing.T) {
	robot := &searchRobot{ready: true, matchAfter: -1, discoveryFailed: true}
	session := &robotSession{robot: robot, searchRoot: 1000000}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	(&Server{}).search(ctx, cancel, session, 1000000, twinrobot.SiteFusionRequest{Explore: true})
	if session.searchState != "failed" || robot.turns != 0 || robot.moves != 1 || robot.stopped == 0 {
		t.Fatal("failed movement continued discovery")
	}
}

func TestDiscoverySearchFindsAnOpeningAtTheNextHeading(t *testing.T) {
	robot := &searchRobot{ready: true, matchOnMove: true, discoveryAllowedTurns: map[int]bool{1: true}}
	session := &robotSession{robot: robot, searchRoot: 1000000}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	(&Server{}).search(ctx, cancel, session, 1000000, twinrobot.SiteFusionRequest{Explore: true})
	if session.searchState != "complete" || robot.turns != 1 || robot.moves != 1 || robot.discoveryAttempts != 2 || len(robot.visuals) != 3 {
		t.Fatalf("missed the clear 30-degree viewpoint: state=%s turns=%d moves=%d attempts=%d matches=%d", session.searchState, robot.turns, robot.moves, robot.discoveryAttempts, len(robot.visuals))
	}
}

func TestDiscoverySearchAlreadyMatchedDoesNotMove(t *testing.T) {
	robot := &searchRobot{ready: true, matchAfter: 0}
	session := &robotSession{robot: robot, searchRoot: 1000000}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	(&Server{}).search(ctx, cancel, session, 1000000, twinrobot.SiteFusionRequest{Explore: true})
	if session.searchState != "complete" || robot.turns != 0 || robot.moves != 0 || robot.discoveryAttempts != 0 || len(robot.visuals) != 1 {
		t.Fatal("moved despite sufficient initial observations")
	}
}
