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
	statusHook                                     func(twinrobot.SiteRobotSnapshot) twinrobot.SiteRobotSnapshot
	fusionHook                                     func() (twinrobot.SiteFusionReport, error)
	discoveryHook                                  func(int) error
	matchTimes, discoveryTimes                     []time.Time
}

func (r *searchRobot) Status() twinrobot.SiteRobotSnapshot {
	state := twinrobot.SiteRobotSnapshot{MeasurementsReady: r.ready, ScanReady: r.ready, ScanReceivedUnixNanos: time.Now().UnixNano(), LookState: 2, LookDirection: -1, DiscoveryState: func() int {
		if r.discoveryFailed {
			return 3
		}
		return 2
	}()}
	if r.statusHook != nil {
		return r.statusHook(state)
	}
	return state
}
func (r *searchRobot) FuseLocalization(sequence uint64, input twinrobot.SiteFusionRequest) (twinrobot.SiteFusionReport, error) {
	r.visuals = append(r.visuals, input.Visual)
	r.matchTimes = append(r.matchTimes, time.Now())
	if r.fusionHook != nil {
		return r.fusionHook()
	}
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
	if robot.moves != 0 || robot.discoveryAttempts != 0 || robot.turns != 6 || session.searchState != "failed" || !strings.Contains(session.searchMessage, "Several room positions") || !strings.Contains(string(session.searchReport), `"Accepted":42`) {
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
	r.discoveryTimes = append(r.discoveryTimes, time.Now())
	if r.discoveryHook != nil {
		if err := r.discoveryHook(r.discoveryAttempts); err != nil {
			return err
		}
	}
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
			// Observe the bounded stationary phase, then exercise cancellation.
			// The production search can keep matching until its five-minute deadline.
			robot.statusHook = func(state twinrobot.SiteRobotSnapshot) twinrobot.SiteRobotSnapshot {
				if session.searchPhase == "waiting" {
					cancel()
				}
				return state
			}
			(&Server{}).search(ctx, cancel, session, 1000000, twinrobot.SiteFusionRequest{Explore: true})
			wantTurns, wantMoves, wantAttempts := 2, 3, 3
			if blocked {
				wantTurns, wantMoves, wantAttempts = 5, 0, 6
				for _, reason := range []string{"Several room positions still fit", "exceeded its time budget", "blocked at 6 headings", "floor ahead is unobserved"} {
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
		t.Fatalf("missed the clear 60-degree viewpoint: state=%s turns=%d moves=%d attempts=%d matches=%d", session.searchState, robot.turns, robot.moves, robot.discoveryAttempts, len(robot.visuals))
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

func TestSearchCannotPublishCandidateAfterLocalizationReset(t *testing.T) {
	robot := &searchRobot{ready: true, matchAfter: 0}
	session := &robotSession{robot: robot, searchRoot: 2000000, pendingSequence: 2000000, pendingRoot: 2000000}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	(&Server{}).search(ctx, cancel, session, 1000000, twinrobot.SiteFusionRequest{Explore: true})
	if len(session.pending.Candidates) != 0 || session.pendingSequence != 2000000 || session.searchState == "complete" {
		t.Fatal("superseded search restored a discarded position")
	}
	if robot.turns != 0 || robot.moves != 0 {
		t.Fatal("superseded search moved the robot")
	}
}

func TestDiscoveryRecoversPreflightSensorsOnlyAfterNewScanAndDelay(t *testing.T) {
	t.Parallel()
	robot := &searchRobot{ready: true, matchOnMove: true}
	robot.discoveryHook = func(attempt int) error {
		if attempt == 1 {
			return fmt.Errorf("%w: waiting for floor", twinrobot.ErrObservationUnavailable)
		}
		return nil
	}
	robot.statusHook = func(state twinrobot.SiteRobotSnapshot) twinrobot.SiteRobotSnapshot {
		// A new receipt is useful even when the geometric fingerprint is unchanged.
		state.ObservationRevision = 42
		state.ScanReceivedUnixNanos = 10
		if len(robot.discoveryTimes) > 0 && time.Since(robot.discoveryTimes[0]) >= 2200*time.Millisecond {
			state.ScanReceivedUnixNanos = 11
		}
		return state
	}
	session := &robotSession{robot: robot, searchRoot: 1000000}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	(&Server{}).search(ctx, cancel, session, 1000000, twinrobot.SiteFusionRequest{Explore: true})
	if session.searchState != "complete" || robot.turns != 0 || robot.moves != 1 || robot.discoveryAttempts != 2 || len(robot.visuals) != 3 {
		t.Fatalf("sensor recovery failed: state=%s turns=%d moves=%d attempts=%d matches=%d", session.searchState, robot.turns, robot.moves, robot.discoveryAttempts, len(robot.visuals))
	}
	if robot.matchTimes[1].Sub(robot.discoveryTimes[0]) < 2200*time.Millisecond {
		t.Fatal("re-matched before both the new scan and recovery delay")
	}
}

func TestDiscoveryDoesNotRetryStaleScanAndStopCancelsWait(t *testing.T) {
	t.Parallel()
	robot := &searchRobot{ready: true, matchAfter: -1}
	robot.discoveryHook = func(int) error { return twinrobot.ErrObservationUnavailable }
	var revision uint64
	robot.statusHook = func(state twinrobot.SiteRobotSnapshot) twinrobot.SiteRobotSnapshot {
		state.ScanReceivedUnixNanos = 10
		revision++
		state.ObservationRevision = revision // Fingerprint changes alone are not fresh receipts.
		return state
	}
	session := &robotSession{robot: robot, searchRoot: 1000000}
	ctx, cancel := context.WithTimeout(context.Background(), 2700*time.Millisecond)
	defer cancel()
	(&Server{}).search(ctx, cancel, session, 1000000, twinrobot.SiteFusionRequest{Explore: true})
	if session.searchState != "failed" || robot.stopped != 1999999 || robot.discoveryAttempts != 1 || len(robot.visuals) != 1 || robot.turns != 0 || robot.moves != 0 {
		t.Fatalf("stale scan restarted work or cancellation missed STOP: state=%s attempts=%d matches=%d turns=%d moves=%d stop=%d", session.searchState, robot.discoveryAttempts, len(robot.visuals), robot.turns, robot.moves, robot.stopped)
	}
}

func TestDiscoveryCannotRecoverAfterControlLostDuringWait(t *testing.T) {
	t.Parallel()
	robot := &searchRobot{ready: true, matchAfter: -1}
	var abortedAt time.Time
	robot.statusHook = func(state twinrobot.SiteRobotSnapshot) twinrobot.SiteRobotSnapshot {
		if robot.moves == 1 {
			if abortedAt.IsZero() {
				abortedAt = time.Now()
			}
			state.DiscoveryState = 3
			state.MotionRecoveryAllowed = time.Since(abortedAt) < 250*time.Millisecond
			state.Message = "Stopped for changed floor observations"
		}
		return state
	}
	session := &robotSession{robot: robot, searchRoot: 1000000}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	(&Server{}).search(ctx, cancel, session, 1000000, twinrobot.SiteFusionRequest{Explore: true})
	if session.searchState != "failed" || !strings.Contains(session.searchMessage, "cannot resume automatically") || robot.stopped != 1999999 || robot.turns != 0 || robot.moves != 1 || robot.discoveryAttempts != 1 || len(robot.visuals) != 1 {
		t.Fatalf("control loss was cleared by a retry: state=%s message=%q moves=%d attempts=%d matches=%d", session.searchState, session.searchMessage, robot.moves, robot.discoveryAttempts, len(robot.visuals))
	}
}

func TestDiscoveryRecoversAcknowledgedTransientMotionAbort(t *testing.T) {
	t.Parallel()
	robot := &searchRobot{ready: true, matchAfter: -1}
	var abortedAt time.Time
	robot.statusHook = func(state twinrobot.SiteRobotSnapshot) twinrobot.SiteRobotSnapshot {
		if robot.moves == 1 {
			if abortedAt.IsZero() {
				abortedAt = time.Now()
			}
			state.DiscoveryState = 3
			state.MotionRecoveryAllowed = true
			state.Message = "Discovery stopped: floor observations changed."
		}
		return state
	}
	robot.fusionHook = func() (twinrobot.SiteFusionReport, error) {
		if robot.moves == 2 {
			return twinrobot.SiteFusionReport{Candidates: []twinrobot.SiteRobotCandidate{{X: 1}}}, nil
		}
		return twinrobot.SiteFusionReport{Message: "Need a new viewpoint"}, nil
	}
	session := &robotSession{robot: robot, searchRoot: 1000000}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	(&Server{}).search(ctx, cancel, session, 1000000, twinrobot.SiteFusionRequest{Explore: true})
	if session.searchState != "complete" || robot.turns != 0 || robot.moves != 2 || len(robot.visuals) != 3 || robot.stopped != 0 {
		t.Fatalf("transient abort did not recover: state=%s turns=%d moves=%d matches=%d", session.searchState, robot.turns, robot.moves, len(robot.visuals))
	}
	if robot.matchTimes[1].Sub(abortedAt) < 2*time.Second {
		t.Fatal("motion recovery skipped the settle delay")
	}
}

func TestDiscoveryKeepsMatchingAtMovementLimitWithoutMoreMotion(t *testing.T) {
	t.Parallel()
	robot := &searchRobot{ready: true, matchAfter: -1}
	passiveMatches := 0
	robot.fusionHook = func() (twinrobot.SiteFusionReport, error) {
		if robot.moves == 3 {
			passiveMatches++
			if passiveMatches == 2 {
				return twinrobot.SiteFusionReport{Candidates: []twinrobot.SiteRobotCandidate{{X: 1}}}, nil
			}
		}
		return twinrobot.SiteFusionReport{Message: "Need more observations"}, nil
	}
	session := &robotSession{robot: robot, searchRoot: 1000000}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	(&Server{}).search(ctx, cancel, session, 1000000, twinrobot.SiteFusionRequest{Explore: true})
	if session.searchState != "complete" || robot.moves != 3 || robot.discoveryAttempts != 3 || robot.turns != 2 || passiveMatches != 2 {
		t.Fatalf("movement budget reset during recovery: state=%s moves=%d attempts=%d turns=%d passive=%d", session.searchState, robot.moves, robot.discoveryAttempts, robot.turns, passiveMatches)
	}
	count := len(robot.matchTimes)
	if robot.matchTimes[count-1].Sub(robot.matchTimes[count-2]) < 2*time.Second {
		t.Fatal("passive matching exceeded recovery cadence")
	}
}

func TestDiscoveryRetriesLastBlockedHeadingWithFreshFloorWithoutAnotherTurn(t *testing.T) {
	t.Parallel()
	robot := &searchRobot{ready: true, matchOnMove: true}
	robot.discoveryHook = func(attempt int) error {
		if attempt <= 6 {
			return fmt.Errorf("%w: waiting for floor", twinrobot.ErrDiscoveryBlocked)
		}
		return nil
	}
	session := &robotSession{robot: robot, searchRoot: 1000000}
	ctx, cancel := context.WithTimeout(context.Background(), 22*time.Second)
	defer cancel()
	(&Server{}).search(ctx, cancel, session, 1000000, twinrobot.SiteFusionRequest{Explore: true})
	if session.searchState != "complete" || robot.turns != 5 || robot.moves != 1 || robot.discoveryAttempts != 7 {
		t.Fatalf("did not retry the current heading within turn budget: state=%s turns=%d moves=%d attempts=%d", session.searchState, robot.turns, robot.moves, robot.discoveryAttempts)
	}
	if robot.discoveryTimes[6].Sub(robot.discoveryTimes[5]) < 2*time.Second {
		t.Fatal("retried blocked movement without recovery delay")
	}
}
