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
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
)

type setupTestRobot struct {
	robotControl
	mu       sync.Mutex
	state    twinrobot.SiteRobotSnapshot
	commands []twinrobot.Command
	closed   bool
}

func (r *setupTestRobot) Status() twinrobot.SiteRobotSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}
func (r *setupTestRobot) CommandActive() bool { return false }
func (r *setupTestRobot) Close()              { r.mu.Lock(); defer r.mu.Unlock(); r.closed = true }
func (r *setupTestRobot) ExecuteCommand(ctx context.Context, q twinrobot.Command) twinrobot.CommandResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ctx.Err() != nil {
		return twinrobot.CommandResult{State: "canceled", Message: ctx.Err().Error()}
	}
	r.commands = append(r.commands, q)
	return twinrobot.CommandResult{State: "completed", Snapshot: r.state}
}
func (r *setupTestRobot) recorded() []twinrobot.Command {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]twinrobot.Command(nil), r.commands...)
}
func setupCommandCall(s *Server, ctx context.Context, open commandRobotOpener, q robotCommandRequest) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(q)
	w := httptest.NewRecorder()
	s.robotCommandUsing(w, httptest.NewRequest(http.MethodPost, "/v1/robot/command", bytes.NewReader(raw)).WithContext(ctx), open)
	return w
}
func waitSetupCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for !condition() {
		select {
		case <-deadline.C:
			t.Fatal("setup condition not reached")
		case <-poll.C:
		}
	}
}

func TestRobotCommandAutomaticallyStartsObservationsOnce(t *testing.T) {
	for _, action := range []string{"turn", "forward", "go_to"} {
		t.Run(action, func(t *testing.T) {
			store, _ := NewStore(t.TempDir())
			room, err := store.Put(room())
			if err != nil {
				t.Fatal(err)
			}
			frame := room.Site.ID + "/" + room.Site.ModelRevision
			if err := store.SaveWaypoint(frame, "kitchen", RoomObject{X: 2, Z: 1}); err != nil {
				t.Fatal(err)
			}
			robot := &setupTestRobot{state: twinrobot.SiteRobotSnapshot{MeasurementsReady: true, Pose: true, Aligned: true, Frame: frame}}
			s := &Server{Context: context.Background(), Store: store}
			opened := 0
			open := func(ctx context.Context, serial string) (robotControl, error) {
				opened++
				if serial != "go2" {
					t.Fatal(serial)
				}
				return robot, nil
			}
			q := robotCommandRequest{Serial: "go2", Action: action, RequestID: "same-request", Degrees: 45, DistanceMeters: 1, Destination: "kitchen"}
			for i := 0; i < 2; i++ {
				w := setupCommandCall(s, context.Background(), open, q)
				if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"completed"`) {
					t.Fatal(w.Body.String())
				}
			}
			if opened != 1 || len(robot.recorded()) != 1 {
				t.Fatalf("opened%d commands%+v", opened, robot.recorded())
			}
			s.mu.Lock()
			session := s.observers["go2"]
			s.mu.Unlock()
			if session.commandReceipts[q.RequestID].request != q {
				t.Fatal("request identity changed during setup")
			}
		})
	}
}

func TestRobotCommandStartupFailureDoesNotDispatch(t *testing.T) {
	robot := &setupTestRobot{}
	s := &Server{Context: context.Background()}
	opened := 0
	open := func(context.Context, string) (robotControl, error) {
		opened++
		return nil, errors.New("pairing is offline")
	}
	w := setupCommandCall(s, context.Background(), open, robotCommandRequest{Serial: "go2", Action: "turn", RequestID: "turn-request", Degrees: 45})
	if w.Code == 200 || !strings.Contains(w.Body.String(), "pairing is offline") || opened != 1 || len(robot.recorded()) != 0 || len(s.observers) != 0 {
		t.Fatalf("%d %s opened%d", w.Code, w.Body.String(), opened)
	}
}

func TestRobotCommandValidatesBeforeObservationSideEffects(t *testing.T) {
	cases := []robotCommandRequest{
		{Serial: "go2", Action: "unknown", RequestID: "invalid-1"},
		{Serial: "go2", Action: "turn", RequestID: "short", Degrees: 45},
		{Serial: "go2", Action: "turn", RequestID: "invalid-2", Degrees: 0},
		{Serial: "go2", Action: "turn", RequestID: "invalid-3", Degrees: 181},
		{Serial: "go2", Action: "forward", RequestID: "invalid-4", DistanceMeters: 0},
		{Serial: "go2", Action: "forward", RequestID: "invalid-5", DistanceMeters: 2.01},
		{Serial: "go2", Action: "go_to", RequestID: "invalid-6", Destination: " "},
		{Serial: "go2", Action: "go_to", RequestID: "invalid-7", Destination: "bad\nname"},
		{Serial: "bad serial", Action: "turn", RequestID: "invalid-8", Degrees: 45},
	}
	for _, q := range cases {
		s := &Server{Context: context.Background()}
		opened := 0
		w := setupCommandCall(s, context.Background(), func(context.Context, string) (robotControl, error) { opened++; return &setupTestRobot{}, nil }, q)
		if w.Code != 400 || opened != 0 || len(s.commandSetups) != 0 || len(s.observers) != 0 {
			t.Fatalf("%+v status%d opened%d", q, w.Code, opened)
		}
	}
}

func TestRobotCommandNeverAutoOpensReadStopOrObjectEvidence(t *testing.T) {
	for _, action := range []string{"status", "stop", "remember", "go_to_object"} {
		t.Run(action, func(t *testing.T) {
			s := &Server{Context: context.Background()}
			opened := 0
			setupCommandCall(s, context.Background(), func(context.Context, string) (robotControl, error) { opened++; return &setupTestRobot{}, nil }, robotCommandRequest{Serial: "go2", Action: action, RequestID: "no-auto-open", Destination: "kitchen", EvidenceID: "unknown"})
			if opened != 0 || len(s.observers) != 0 {
				t.Fatal("opened observations for", action)
			}
		})
	}
}

func TestRobotCommandStopOrCancellationDuringStartup(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(map[bool]string{false: "request-canceled", true: "stop"}[stop], func(t *testing.T) {
			s := &Server{Context: context.Background()}
			started := make(chan struct{})
			var opens atomic.Int32
			robot := &setupTestRobot{state: twinrobot.SiteRobotSnapshot{MeasurementsReady: true}}
			open := func(ctx context.Context, _ string) (robotControl, error) {
				opens.Add(1)
				close(started)
				<-ctx.Done()
				return robot, ctx.Err()
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				result <- setupCommandCall(s, ctx, open, robotCommandRequest{Serial: "go2", Action: "turn", RequestID: "turn-startup", Degrees: 45})
			}()
			<-started
			if stop {
				done := make(chan struct{})
				go func() {
					setupCommandCall(s, context.Background(), open, robotCommandRequest{Serial: "go2", Action: "stop", RequestID: "stop-startup"})
					close(done)
				}()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("Stop blocked on startup lock")
				}
			} else {
				cancel()
			}
			select {
			case w := <-result:
				if w.Code == 200 {
					t.Fatal(w.Body.String())
				}
			case <-time.After(time.Second):
				t.Fatal("startup survived cancellation")
			}
			robot.mu.Lock()
			closed := robot.closed
			robot.mu.Unlock()
			if opens.Load() != 1 || len(robot.recorded()) != 0 || len(s.observers) != 0 || !closed {
				t.Fatalf("opens%d commands%+v closed%v", opens.Load(), robot.recorded(), closed)
			}
		})
	}
}

func TestRobotCommandStopDuringInitialMeasurements(t *testing.T) {
	s := &Server{Context: context.Background()}
	robot := &setupTestRobot{}
	var opens atomic.Int32
	open := func(context.Context, string) (robotControl, error) { opens.Add(1); return robot, nil }
	q := robotCommandRequest{Serial: "go2", Action: "forward", RequestID: "forward-wait", DistanceMeters: 1}
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- setupCommandCall(s, context.Background(), open, q) }()
	waitSetupCondition(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.observers["go2"] != nil })
	w := setupCommandCall(s, context.Background(), open, robotCommandRequest{Serial: "go2", Action: "stop", RequestID: "stop-waiting"})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	select {
	case w = <-result:
		if !strings.Contains(w.Body.String(), `"state":"canceled"`) {
			t.Fatal(w.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("waiting command survived Stop")
	}
	robot.mu.Lock()
	robot.state.MeasurementsReady = true
	robot.mu.Unlock()
	setupCommandCall(s, context.Background(), open, q)
	commands := robot.recorded()
	if opens.Load() != 1 || len(commands) != 1 || commands[0].Action != "stop" {
		t.Fatalf("commands%+v opens%d", commands, opens.Load())
	}
}

func TestRobotCommandConcurrentStartupKeepsRequestIdentity(t *testing.T) {
	for _, sameID := range []bool{false, true} {
		t.Run(map[bool]string{false: "distinct", true: "duplicate"}[sameID], func(t *testing.T) {
			s := &Server{Context: context.Background()}
			robot := &setupTestRobot{state: twinrobot.SiteRobotSnapshot{MeasurementsReady: true}}
			entered, release := make(chan struct{}), make(chan struct{})
			var opens atomic.Int32
			open := func(context.Context, string) (robotControl, error) {
				opens.Add(1)
				close(entered)
				<-release
				return robot, nil
			}
			results := make(chan *httptest.ResponseRecorder, 2)
			q := robotCommandRequest{Serial: "go2", Action: "turn", RequestID: "concurrent-1", Degrees: 45}
			go func() { results <- setupCommandCall(s, context.Background(), open, q) }()
			<-entered
			second := q
			if !sameID {
				second.RequestID = "concurrent-2"
			}
			go func() { results <- setupCommandCall(s, context.Background(), open, second) }()
			waitSetupCondition(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.commandSetups["go2"]) == 2 })
			close(release)
			for i := 0; i < 2; i++ {
				w := <-results
				if w.Code != 200 {
					t.Fatal(w.Body.String())
				}
			}
			count := 2
			if sameID {
				count = 1
			}
			if opens.Load() != 1 || len(robot.recorded()) != count {
				t.Fatalf("opens%d commands%+v", opens.Load(), robot.recorded())
			}
		})
	}
}

func TestRobotCommandExplicitConnectStillOnlyObserves(t *testing.T) {
	s := &Server{Context: context.Background()}
	robot := &setupTestRobot{}
	opened := 0
	open := func(context.Context, string) (robotControl, error) { opened++; return robot, nil }
	for i := 0; i < 2; i++ {
		w := setupCommandCall(s, context.Background(), open, robotCommandRequest{Serial: "go2", Action: "connect"})
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	if opened != 1 || len(robot.recorded()) != 0 {
		t.Fatalf("opens%d commands%+v", opened, robot.recorded())
	}
}
