package twin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
)

type activeOwnerTest struct {
	mu                                      sync.Mutex
	robot                                   *setupTestRobot
	cancel                                  context.CancelFunc
	ctx                                     context.Context
	report                                  twinrobot.SiteFusionReport
	checkErr, confirmErr                    error
	confirms, matches, moves, turns, closes int
	supersedeOnMove                         bool
	matchAfterMove                          bool
	hold                                    bool
}

func (o *activeOwnerTest) Status() twinrobot.SiteRobotSnapshot {
	state := o.robot.Status()
	state.DiscoveryState = 2
	return state
}
func (o *activeOwnerTest) Check() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.ctx.Err(); err != nil {
		return err
	}
	return o.checkErr
}
func (o *activeOwnerTest) FuseLocalization(_ uint64, _ twinrobot.SiteFusionRequest) (twinrobot.SiteFusionReport, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.matches++
	if o.matchAfterMove && o.moves == 0 {
		return twinrobot.SiteFusionReport{}, nil
	}
	if o.hold {
		return twinrobot.SiteFusionReport{WaitingForObservations: true}, nil
	}
	return o.report, nil
}
func (o *activeOwnerTest) Look(uint64, int) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.turns++
	return errors.New("test turn must not run")
}
func (o *activeOwnerTest) DiscoveryStep(uint64, uint64) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.moves++
	if o.supersedeOnMove {
		o.checkErr = twinrobot.ErrLocalizationSearchSuperseded
		return o.checkErr
	}
	if o.matchAfterMove {
		return nil
	}
	return errors.New("test move must not run")
}
func (o *activeOwnerTest) ConfirmLocalization(_, _ uint64, frame string, _ int) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.ctx.Err(); err != nil {
		return err
	}
	if o.confirmErr != nil {
		return o.confirmErr
	}
	o.confirms++
	o.robot.mu.Lock()
	defer o.robot.mu.Unlock()
	o.robot.state.Frame = frame
	o.robot.state.Aligned = true
	return nil
}
func (o *activeOwnerTest) Close()      { o.mu.Lock(); defer o.mu.Unlock(); o.closes++ }
func (o *activeOwnerTest) Stop(uint64) { o.Close() }
func activeCommandCall(s *Server, ctx context.Context, factory activeSearchOwnerFactory, q robotCommandRequest) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(q)
	w := httptest.NewRecorder()
	s.robotCommandUsingSearch(w, httptest.NewRequest(http.MethodPost, "/v1/robot/command", bytes.NewReader(raw)).WithContext(ctx), nil, factory)
	return w
}
func activeFixture(t *testing.T) (*Server, *robotSession, *activeOwnerTest, robotCommandRequest, activeSearchOwnerFactory) {
	t.Helper()
	store, _ := NewStore(t.TempDir())
	saveLocalizeTestRoom(t, store, "Office", true)
	robot := &setupTestRobot{state: twinrobot.SiteRobotSnapshot{MeasurementsReady: true}}
	session := &robotSession{robot: robot, serial: "go2"}
	life, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &Server{Store: store, Context: life, observers: map[string]*robotSession{"go2": session}}
	owner := &activeOwnerTest{robot: robot, report: twinrobot.SiteFusionReport{Candidates: []twinrobot.SiteRobotCandidate{{X: 1}}, Tracking: sitenav.TrackingResult{Selected: 0, Fits: []sitenav.RegistrationFit{{Valid: true}}}}}
	q := robotCommandRequest{Serial: "go2", Action: "localize_active", RequestID: "active-search"}
	factory := func(ctx context.Context, _ robotControl) (activeSearchOwner, error) {
		owner.ctx, owner.cancel = context.WithCancel(ctx)
		return owner, nil
	}
	return s, session, owner, q, factory
}
func activeReceipt(session *robotSession, id string) twinrobot.CommandResult {
	session.commandMu.Lock()
	defer session.commandMu.Unlock()
	return session.commandReceipts[id].result
}

func TestActiveLocalizationAcknowledgesThenConfirmsAndNeverReplays(t *testing.T) {
	s, session, owner, q, factory := activeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	w := activeCommandCall(s, ctx, factory, q)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"running"`) {
		t.Fatal(w.Body.String())
	}
	cancel() // Acknowledged work survives normal HTTP request completion.
	waitSetupCondition(t, func() bool { return activeReceipt(session, q.RequestID).State != "running" })
	result := activeReceipt(session, q.RequestID)
	if result.State != "completed" || !result.Snapshot.Aligned {
		t.Fatalf("%+v", result)
	}
	for _, action := range []string{"localize_active", "receipt"} {
		retry := q
		retry.Action = action
		w = activeCommandCall(s, context.Background(), func(context.Context, robotControl) (activeSearchOwner, error) {
			t.Fatal("restarted search")
			return nil, nil
		}, retry)
		if !strings.Contains(w.Body.String(), `"state":"completed"`) {
			t.Fatal(w.Body.String())
		}
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.matches != 1 || owner.confirms != 1 || owner.moves != 0 || owner.turns != 0 || owner.closes != 1 {
		t.Fatalf("unexpected calls %+v", owner)
	}
}
func TestActiveLocalizationRejectsUnconfirmedCandidate(t *testing.T) {
	for _, mode := range []string{"ambiguous", "unselected", "invalid", "confirm-rejected"} {
		t.Run(mode, func(t *testing.T) {
			s, session, owner, q, factory := activeFixture(t)
			switch mode {
			case "ambiguous":
				owner.report.Candidates = append(owner.report.Candidates, twinrobot.SiteRobotCandidate{X: 2})
			case "unselected":
				owner.report.Tracking.Selected = -1
			case "invalid":
				owner.report.Tracking.Fits[0].Valid = false
			case "confirm-rejected":
				owner.confirmErr = errors.New("odometry epoch changed")
			}
			activeCommandCall(s, context.Background(), factory, q)
			waitSetupCondition(t, func() bool { return activeReceipt(session, q.RequestID).State != "running" })
			result := activeReceipt(session, q.RequestID)
			if result.State != "rejected" || result.Snapshot.Aligned {
				t.Fatalf("%+v", result)
			}
			owner.mu.Lock()
			defer owner.mu.Unlock()
			if owner.confirms != 0 || owner.moves != 0 || owner.turns != 0 {
				t.Fatalf("unexpected calls %+v", owner)
			}
		})
	}
}
func TestActiveLocalizationStopAndSupersessionEndSameReceipt(t *testing.T) {
	for _, mode := range []string{"stop", "superseded", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			s, session, owner, q, factory := activeFixture(t)
			owner.hold = true
			w := activeCommandCall(s, context.Background(), factory, q)
			if !strings.Contains(w.Body.String(), `"state":"running"`) {
				t.Fatal(w.Body.String())
			}
			waitSetupCondition(t, func() bool { owner.mu.Lock(); defer owner.mu.Unlock(); return owner.matches > 0 })
			wanted := "canceled"
			switch mode {
			case "stop":
				activeCommandCall(s, context.Background(), factory, robotCommandRequest{Serial: "go2", Action: "stop", RequestID: "stop-active"})
			case "superseded":
				owner.mu.Lock()
				owner.checkErr = twinrobot.ErrLocalizationSearchSuperseded
				owner.mu.Unlock()
				wanted = "superseded"
			case "shutdown":
				session.searchMu.Lock()
				session.searchCancel()
				session.searchMu.Unlock()
			}
			waitSetupCondition(t, func() bool { return activeReceipt(session, q.RequestID).State != "running" })
			if result := activeReceipt(session, q.RequestID); result.State != wanted {
				t.Fatalf("%+v", result)
			}
			owner.mu.Lock()
			defer owner.mu.Unlock()
			if owner.confirms != 0 || owner.moves != 0 || owner.turns != 0 {
				t.Fatalf("unexpected calls %+v", owner)
			}
		})
	}
}
func TestActiveLocalizationCanceledBeforeLaunchCannotMove(t *testing.T) {
	s, session, owner, q, factory := activeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	initial, launch := s.prepareActiveLocalization(ctx, session, q, owner.robot, factory)
	if initial.State != "running" || launch == nil {
		t.Fatalf("%+v", initial)
	}
	session.commandReceipts = map[string]robotCommandReceipt{q.RequestID: {request: q, result: initial}}
	cancel()
	launch()
	if got := activeReceipt(session, q.RequestID); got.State != "canceled" {
		t.Fatalf("%+v", got)
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.matches != 0 || owner.confirms != 0 || owner.moves != 0 || owner.turns != 0 {
		t.Fatalf("unexpected calls %+v", owner)
	}
}
func TestActiveLocalizationPendingMoveBlocksOwner(t *testing.T) {
	s, session, _, q, _ := activeFixture(t)
	session.commandReceipts = map[string]robotCommandReceipt{"pending-move": {result: twinrobot.CommandResult{State: "running"}}}
	w := activeCommandCall(s, context.Background(), func(context.Context, robotControl) (activeSearchOwner, error) {
		t.Fatal("claimed owner while command pending")
		return nil, nil
	}, q)
	if !strings.Contains(w.Body.String(), `"state":"rejected"`) {
		t.Fatal(w.Body.String())
	}
}

func TestActiveLocalizationUsesMeasuredViewpointBeforeConfirming(t *testing.T) {
	s, session, owner, q, factory := activeFixture(t)
	owner.matchAfterMove = true
	w := activeCommandCall(s, context.Background(), factory, q)
	if !strings.Contains(w.Body.String(), `"state":"running"`) {
		t.Fatal(w.Body.String())
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for activeReceipt(session, q.RequestID).State == "running" {
		select {
		case <-deadline.C:
			t.Fatal("search did not finish")
		case <-poll.C:
		}
	}
	if got := activeReceipt(session, q.RequestID); got.State != "completed" {
		t.Fatalf("%+v", got)
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.matches != 2 || owner.moves != 1 || owner.turns != 0 || owner.confirms != 1 {
		t.Fatalf("unexpected search operations %+v", owner)
	}
}

func TestActiveLocalizationRequiresStrictMeasurementsAndValidRoomBeforeOwnership(t *testing.T) {
	for _, mode := range []string{"idle-only", "missing-room", "invalid-room", "moving"} {
		t.Run(mode, func(t *testing.T) {
			s, _, owner, q, _ := activeFixture(t)
			ctx := context.Background()
			switch mode {
			case "idle-only":
				owner.robot.state.MeasurementsReady = false
				owner.robot.state.IdleMeasurementsReady = true
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 30*time.Millisecond)
				defer cancel()
			case "missing-room":
				q.Destination = "Unmapped garage"
			case "invalid-room":
				q.Destination = "Invalid\nRoom"
			case "moving":
				owner.robot.state.Moving = true
			}
			w := activeCommandCall(s, ctx, func(context.Context, robotControl) (activeSearchOwner, error) {
				t.Fatal("unready request claimed search owner")
				return nil, nil
			}, q)
			if strings.Contains(w.Body.String(), `"state":"running"`) || strings.Contains(w.Body.String(), `"state":"completed"`) {
				t.Fatal(w.Body.String())
			}
		})
	}
}

func TestActiveLocalizationSupersededDuringViewpointDispatch(t *testing.T) {
	s, session, owner, q, factory := activeFixture(t)
	owner.report = twinrobot.SiteFusionReport{}
	owner.supersedeOnMove = true
	activeCommandCall(s, context.Background(), factory, q)
	waitSetupCondition(t, func() bool { return activeReceipt(session, q.RequestID).State != "running" })
	if result := activeReceipt(session, q.RequestID); result.State != "superseded" {
		t.Fatalf("%+v", result)
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.moves != 1 || owner.confirms != 0 || owner.turns != 0 {
		t.Fatalf("unexpected calls %+v", owner)
	}
}
