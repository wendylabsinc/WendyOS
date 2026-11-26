package twin

import (
	"context"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
)

type passiveLocalizeTestRobot struct {
	*setupTestRobot
	inputs  []twinrobot.SiteFusionRequest
	active  bool
	outcome string
}

func (r *passiveLocalizeTestRobot) CommandActive() bool { return r.active }
func (r *passiveLocalizeTestRobot) LocalizeCommand(ctx context.Context, input twinrobot.SiteFusionRequest) twinrobot.CommandResult {
	if err := ctx.Err(); err != nil {
		return twinrobot.CommandResult{State: "canceled", Message: err.Error()}
	}
	r.inputs = append(r.inputs, input)
	if r.outcome != "" {
		return twinrobot.CommandResult{State: r.outcome, Message: "Several room positions still fit.", Snapshot: r.Status()}
	}
	r.mu.Lock()
	r.state.Frame = input.Frame
	r.state.Aligned = true
	r.state.X = 1.2
	r.state.Z = -2.3
	r.mu.Unlock()
	return twinrobot.CommandResult{State: "completed", Message: "Position confirmed without movement.", Snapshot: r.Status()}
}
func saveLocalizeTestRoom(t *testing.T, store *Store, name string, geometry bool) Room {
	t.Helper()
	r := room()
	r.Site.Name = name
	saved, err := store.Put(r)
	if err != nil {
		t.Fatal(err)
	}
	if geometry {
		if err := store.SaveLocalizationGeometry(localizationGeometryFixture(saved.Site.ID + "/" + saved.Site.ModelRevision)); err != nil {
			t.Fatal(err)
		}
	}
	return saved
}
func TestRobotCommandLocalizeUsesSavedGeometryAndReceipt(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	saved := saveLocalizeTestRoom(t, store, "Office", true)
	robot := &passiveLocalizeTestRobot{setupTestRobot: &setupTestRobot{state: twinrobot.SiteRobotSnapshot{MeasurementsReady: true, IdleMeasurementsReady: true}}}
	s := &Server{Store: store, Context: context.Background()}
	opened := 0
	open := func(context.Context, string) (robotControl, error) { opened++; return robot, nil }
	q := robotCommandRequest{Action: "localize", Serial: "go2", RequestID: "locate-once"}
	for i := 0; i < 2; i++ {
		w := setupCommandCall(s, context.Background(), open, q)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"completed"`) {
			t.Fatal(w.Body.String())
		}
	}
	if opened != 1 || len(robot.inputs) != 1 || len(robot.recorded()) != 0 {
		t.Fatalf("opens%d matches%d motion%v", opened, len(robot.inputs), robot.recorded())
	}
	input := robot.inputs[0]
	if input.Frame != saved.Site.ID+"/"+saved.Site.ModelRevision || len(input.Architecture) == 0 || input.Explore || input.Visual != nil {
		t.Fatalf("bad localization input %+v", input)
	}
}
func TestRobotCommandLocalizeRejectsMissingAmbiguousBusyOrUnconfirmed(t *testing.T) {
	for _, mode := range []string{"no-geometry", "multiple-rooms", "duplicate-name", "missing-name", "moving", "active", "pending", "ambiguous-fit", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			store, _ := NewStore(t.TempDir())
			saveLocalizeTestRoom(t, store, "Office", mode != "no-geometry")
			robot := &passiveLocalizeTestRobot{setupTestRobot: &setupTestRobot{state: twinrobot.SiteRobotSnapshot{MeasurementsReady: true, IdleMeasurementsReady: true}}}
			session := &robotSession{robot: robot, serial: "go2"}
			s := &Server{Store: store, Context: context.Background(), observers: map[string]*robotSession{"go2": session}}
			q := robotCommandRequest{Action: "localize", Serial: "go2", RequestID: "locate-reject"}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "multiple-rooms":
				saveLocalizeTestRoom(t, store, "Kitchen", true)
			case "duplicate-name":
				saveLocalizeTestRoom(t, store, "Office", true)
				q.Destination = "Office"
			case "missing-name":
				q.Destination = "Garage"
			case "moving":
				robot.state.Moving = true
			case "active":
				robot.active = true
			case "pending":
				session.commandReceipts = map[string]robotCommandReceipt{"pending-move": {result: twinrobot.CommandResult{State: "running"}}}
			case "ambiguous-fit":
				robot.outcome = "rejected"
			case "canceled":
				cancel()
			}
			w := setupCommandCall(s, ctx, func(context.Context, string) (robotControl, error) {
				t.Fatal("unexpected observer open")
				return nil, nil
			}, q)
			if w.Code != 200 || strings.Contains(w.Body.String(), `"state":"completed"`) || len(robot.recorded()) != 0 {
				t.Fatal(w.Body.String())
			}
			expected := 0
			if mode == "ambiguous-fit" {
				expected = 1
			}
			if len(robot.inputs) != expected {
				t.Fatalf("unexpected matching calls%d: %s", len(robot.inputs), w.Body.String())
			}
		})
	}
}
func TestRobotCommandLocalizeSelectsExactNamedRoom(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	saveLocalizeTestRoom(t, store, "Office", true)
	wanted := saveLocalizeTestRoom(t, store, "Kitchen", true)
	robot := &passiveLocalizeTestRobot{setupTestRobot: &setupTestRobot{state: twinrobot.SiteRobotSnapshot{MeasurementsReady: true, IdleMeasurementsReady: true}}}
	s := &Server{Store: store, Context: context.Background(), observers: map[string]*robotSession{"go2": {robot: robot, serial: "go2"}}}
	w := setupCommandCall(s, context.Background(), nil, robotCommandRequest{Action: "localize", Serial: "go2", RequestID: "locate-kitchen", Destination: " kitchen "})
	if w.Code != 200 || len(robot.inputs) != 1 || !strings.HasPrefix(robot.inputs[0].Frame, wanted.Site.ID+"/") {
		t.Fatal(w.Body.String())
	}
}
