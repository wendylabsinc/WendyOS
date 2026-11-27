package twin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
)

func TestRobotCommandRecoversDeadObserverAndPreservesReceipts(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	saveLocalizeTestRoom(t, store, "Office", true)
	oldRobot := &setupTestRobot{state: twinrobot.SiteRobotSnapshot{Disconnected: true}}
	oldQ := robotCommandRequest{Serial: "go2", Action: "turn", Degrees: 90, RequestID: "old-turn-request"}
	oldReceipt := robotCommandReceipt{request: oldQ, result: twinrobot.CommandResult{State: "failed", Message: "Original odometry failure"}}
	old := &robotSession{robot: oldRobot, serial: "go2", commandReceipts: map[string]robotCommandReceipt{oldQ.RequestID: oldReceipt}}
	s := &Server{Context: context.Background(), Store: store, observers: map[string]*robotSession{"go2": old}, robots: map[string]*robotSession{"desktop-old": old}}
	fresh := &passiveLocalizeTestRobot{setupTestRobot: &setupTestRobot{state: twinrobot.SiteRobotSnapshot{MeasurementsReady: true, IdleMeasurementsReady: true}}}
	opens := 0
	open := func(context.Context, string) (robotControl, error) { opens++; return fresh, nil }
	// Retry before recovery is strictly a receipt lookup, even though the observer died.
	w := setupCommandCall(s, context.Background(), open, oldQ)
	if opens != 0 || !strings.Contains(w.Body.String(), "Original odometry failure") {
		t.Fatal(opens, w.Body.String())
	}
	q := robotCommandRequest{Serial: "go2", Action: "localize", RequestID: "fresh-localize"}
	w = setupCommandCall(s, context.Background(), open, q)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"completed"`) || opens != 1 || len(fresh.inputs) != 1 || len(fresh.recorded()) != 0 {
		t.Fatal(opens, w.Body.String())
	}
	if s.observers["go2"] == old || len(s.robots) != 0 || !old.commandRetired || !oldRobot.closed {
		t.Fatal("observer not replaced and old handles retired")
	}
	for _, request := range []robotCommandRequest{oldQ, {Serial: "go2", Action: "receipt", RequestID: oldQ.RequestID}} {
		w = setupCommandCall(s, context.Background(), open, request)
		if !strings.Contains(w.Body.String(), "Original odometry failure") || opens != 1 || len(fresh.recorded()) != 0 {
			t.Fatal(w.Body.String())
		}
	}
	if _, _, err := s.ensureCommandRobot(context.Background(), "go2", open); err != nil || opens != 1 {
		t.Fatal("healthy observer reopened", err)
	}
}

func TestRobotCommandRecoveryRefusesActiveOrUnknownAutomaticReplay(t *testing.T) {
	for _, mode := range []string{"moving", "intent", "search", "find", "running", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			robot := &passiveLocalizeTestRobot{setupTestRobot: &setupTestRobot{state: twinrobot.SiteRobotSnapshot{Disconnected: true}}}
			session := &robotSession{robot: robot, serial: "go2"}
			switch mode {
			case "moving":
				robot.state.Moving = true
			case "intent":
				robot.active = true
			case "search":
				session.searchState = "searching"
			case "find":
				session.find.State = "searching"
			default:
				session.commandReceipts = map[string]robotCommandReceipt{"unknown-id": {result: twinrobot.CommandResult{State: mode}}}
			}
			s := &Server{Context: context.Background(), observers: map[string]*robotSession{"go2": session}}
			opens := 0
			open := func(context.Context, string) (robotControl, error) { opens++; return nil, errors.New("must not open") }
			if _, _, err := s.ensureCommandRobot(context.Background(), "go2", open); err == nil || opens != 0 || robot.closed || session.commandRetired {
				t.Fatal("unsafe automatic reconnect", err, opens)
			}
		})
	}
}

func TestRobotCommandExplicitRecoveryKeepsUnknownReceiptAndAllowsPassiveLocalize(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	saveLocalizeTestRoom(t, store, "Office", true)
	oldQ := robotCommandRequest{Serial: "go2", Action: "forward", DistanceMeters: 1, RequestID: "unknown-forward"}
	old := &robotSession{robot: &setupTestRobot{state: twinrobot.SiteRobotSnapshot{Disconnected: true}}, serial: "go2", commandReceipts: map[string]robotCommandReceipt{oldQ.RequestID: {request: oldQ, result: twinrobot.CommandResult{State: "unknown", Message: "Prior outcome unknown"}}}}
	fresh := &passiveLocalizeTestRobot{setupTestRobot: &setupTestRobot{state: twinrobot.SiteRobotSnapshot{MeasurementsReady: true, IdleMeasurementsReady: true}}}
	s := &Server{Context: context.Background(), Store: store, observers: map[string]*robotSession{"go2": old}}
	open := func(context.Context, string) (robotControl, error) { return fresh, nil }
	w := setupCommandCall(s, context.Background(), open, robotCommandRequest{Serial: "go2", Action: "connect"})
	if w.Code != 200 || len(fresh.recorded()) != 1 || fresh.recorded()[0].Action != "stop" {
		t.Fatal(w.Body.String(), fresh.recorded())
	}
	w = setupCommandCall(s, context.Background(), open, oldQ)
	if !strings.Contains(w.Body.String(), `"state":"unknown"`) || len(fresh.recorded()) != 1 {
		t.Fatal("old motion replayed", w.Body.String())
	}
	w = setupCommandCall(s, context.Background(), open, robotCommandRequest{Serial: "go2", Action: "localize", RequestID: "new-passive-localize"})
	if !strings.Contains(w.Body.String(), `"state":"completed"`) || len(fresh.inputs) != 1 || len(fresh.recorded()) != 1 {
		t.Fatal(w.Body.String())
	}
}

func TestRobotCommandFailedRecoveryRetainsHistoryAndCanRetry(t *testing.T) {
	old := &robotSession{robot: &setupTestRobot{state: twinrobot.SiteRobotSnapshot{Disconnected: true}}, serial: "go2", commandReceipts: map[string]robotCommandReceipt{"failed-id": {result: twinrobot.CommandResult{State: "failed"}}}}
	s := &Server{Context: context.Background(), observers: map[string]*robotSession{"go2": old}, robots: map[string]*robotSession{"old-handle": old}}
	_, _, err := s.ensureCommandRobot(context.Background(), "go2", func(context.Context, string) (robotControl, error) { return nil, errors.New("offline") })
	if err == nil || s.observers["go2"] != old || old.commandRetired || len(s.robots) != 1 || len(old.commandReceipts) != 1 {
		t.Fatal("failed reconnect lost identity", err)
	}
	fresh := &setupTestRobot{}
	_, created, err := s.ensureCommandRobot(context.Background(), "go2", func(context.Context, string) (robotControl, error) { return fresh, nil })
	if err != nil || !created || s.observers["go2"].commandReceipts["failed-id"].result.State != "failed" {
		t.Fatal(created, err)
	}
}

type recoveryStopFailure struct{ *setupTestRobot }

func (r *recoveryStopFailure) ExecuteCommand(context.Context, twinrobot.Command) twinrobot.CommandResult {
	return twinrobot.CommandResult{State: "failed", Message: "Stop unacknowledged"}
}
func TestRobotCommandExplicitRecoveryRequiresAcknowledgedStop(t *testing.T) {
	old := &robotSession{robot: &setupTestRobot{state: twinrobot.SiteRobotSnapshot{Disconnected: true}}, serial: "go2", commandReceipts: map[string]robotCommandReceipt{"unknown-id": {result: twinrobot.CommandResult{State: "unknown"}}}}
	s := &Server{Context: context.Background(), observers: map[string]*robotSession{"go2": old}}
	fresh := &recoveryStopFailure{&setupTestRobot{}}
	w := setupCommandCall(s, context.Background(), func(context.Context, string) (robotControl, error) { return fresh, nil }, robotCommandRequest{Serial: "go2", Action: "connect"})
	if w.Code == 200 || !strings.Contains(w.Body.String(), "did not acknowledge Stop") || s.observers["go2"] != old || old.commandRetired || !fresh.closed {
		t.Fatal(w.Body.String())
	}
}

func TestRobotCommandStopCancelsDeadObserverRecoveryBeforePublication(t *testing.T) {
	oldRobot := &setupTestRobot{state: twinrobot.SiteRobotSnapshot{Disconnected: true}}
	old := &robotSession{robot: oldRobot, serial: "go2"}
	s := &Server{Context: context.Background(), observers: map[string]*robotSession{"go2": old}}
	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	fresh := &setupTestRobot{}
	open := func(ctx context.Context, _ string) (robotControl, error) {
		close(entered)
		<-release
		return fresh, nil
	}
	go func() {
		defer close(finished)
		setupCommandCall(s, context.Background(), open, robotCommandRequest{Serial: "go2", Action: "connect"})
	}()
	<-entered
	w := setupCommandCall(s, context.Background(), open, robotCommandRequest{Serial: "go2", Action: "stop", RequestID: "cancel-recovery"})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	close(release)
	<-finished
	if s.observers["go2"] != old || old.commandRetired || !fresh.closed || len(fresh.recorded()) != 0 {
		t.Fatal("canceled replacement published or dispatched")
	}
}

func TestRobotCommandExplicitRecoveryWaitsForRunningReceipt(t *testing.T) {
	old := &robotSession{robot: &setupTestRobot{state: twinrobot.SiteRobotSnapshot{Disconnected: true}}, serial: "go2", commandReceipts: map[string]robotCommandReceipt{"running-id": {result: twinrobot.CommandResult{State: "running"}}}}
	s := &Server{Context: context.Background(), observers: map[string]*robotSession{"go2": old}}
	opens := 0
	open := func(context.Context, string) (robotControl, error) { opens++; return &setupTestRobot{}, nil }
	w := setupCommandCall(s, context.Background(), open, robotCommandRequest{Serial: "go2", Action: "connect"})
	if w.Code == 200 || opens != 0 || old.commandRetired {
		t.Fatal("copied an unfinished receipt", w.Body.String())
	}
	// The old handler can finish into its original ledger before a later retry.
	old.commandMu.Lock()
	old.commandReceipts["running-id"] = robotCommandReceipt{result: twinrobot.CommandResult{State: "canceled", Message: "Setup canceled"}}
	old.commandMu.Unlock()
	w = setupCommandCall(s, context.Background(), open, robotCommandRequest{Serial: "go2", Action: "connect"})
	if w.Code != 200 || opens != 1 || s.observers["go2"].commandReceipts["running-id"].result.State != "canceled" {
		t.Fatal(w.Body.String())
	}
}
