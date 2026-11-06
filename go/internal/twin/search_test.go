package twin

import (
	"context"
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
	"testing"
	"time"
)

type searchRobot struct {
	*twinrobot.SiteRobot
	ready             bool
	turns, matchAfter int
	reject            bool
	stopped           uint64
}

func (r *searchRobot) Status() twinrobot.SiteRobotSnapshot {
	return twinrobot.SiteRobotSnapshot{MeasurementsReady: r.ready, LookState: 2}
}
func (r *searchRobot) FuseLocalization(sequence uint64, input twinrobot.SiteFusionRequest) (twinrobot.SiteFusionReport, error) {
	if r.matchAfter >= 0 && r.turns >= r.matchAfter {
		return twinrobot.SiteFusionReport{Candidates: []twinrobot.SiteRobotCandidate{{X: 1}}}, nil
	}
	return twinrobot.SiteFusionReport{}, nil
}
func (r *searchRobot) Look(sequence uint64, direction int) error {
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
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	(&Server{}).search(ctx, cancel, session, 1000000, twinrobot.SiteFusionRequest{Frame: "room/frame"})
	if robot.turns != 2 || session.searchState != "complete" || session.pendingSequence != 1000005 || session.pendingRoot != 1000000 {
		t.Fatalf("turns=%d state=%s sequence=%d", robot.turns, session.searchState, session.pendingSequence)
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	(&Server{}).search(ctx, cancel, session, 1000000, twinrobot.SiteFusionRequest{})
	if robot.turns != 12 || session.searchState != "failed" {
		t.Fatalf("unbounded search: %d %s", robot.turns, session.searchState)
	}
}
