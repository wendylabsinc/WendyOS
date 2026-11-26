package twin

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
)

// Receipt recovery must not even ask for current readiness. Embedding the
// interface with no implementation makes any unexpected robot call fail.
type receiptOnlyRobot struct{ robotControl }

func (*receiptOnlyRobot) Status() twinrobot.SiteRobotSnapshot {
	panic("receipt lookup asked for robot status")
}
func (*receiptOnlyRobot) CommandActive() bool { panic("receipt lookup asked for command readiness") }
func (*receiptOnlyRobot) ExecuteCommand(context.Context, twinrobot.Command) twinrobot.CommandResult {
	panic("receipt lookup executed a command")
}

func TestRobotCommandReceiptReadsOutcomeWithoutRobotOperations(t *testing.T) {
	for _, state := range []string{"running", "completed", "rejected", "failed", "superseded", "canceled"} {
		t.Run(state, func(t *testing.T) {
			request := robotCommandRequest{Serial: "go2", Action: "turn", RequestID: "receipt-original", Degrees: 45}
			original := twinrobot.CommandResult{State: state, Message: "Original command result", Snapshot: twinrobot.SiteRobotSnapshot{Frame: "saved-frame", X: 1.5, Moving: state == "running"}}
			session := &robotSession{robot: &receiptOnlyRobot{}, commandReceipts: map[string]robotCommandReceipt{request.RequestID: {request: request, result: original}}}
			s := &Server{observers: map[string]*robotSession{"go2": session}}
			opened := 0
			open := func(context.Context, string) (robotControl, error) { opened++; return &receiptOnlyRobot{}, nil }
			lookup := robotCommandRequest{Serial: "go2", Action: "receipt", RequestID: request.RequestID}
			for i := 0; i < 2; i++ {
				w := setupCommandCall(s, context.Background(), open, lookup)
				if w.Code != 200 {
					t.Fatal(w.Body.String())
				}
				var result twinrobot.CommandResult
				if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(result, original) {
					t.Fatalf("changed receipt: %+v, want %+v", result, original)
				}
			}
			if opened != 0 || len(s.commandSetups) != 0 || len(session.commandReceipts) != 1 || session.commandReceipts[request.RequestID].request != request {
				t.Fatal("receipt lookup changed controller/session state")
			}
		})
	}
}

func TestRobotCommandReceiptMissingOrReplacedObserverStaysUnknown(t *testing.T) {
	for _, mode := range []string{"missing-observer", "missing-id", "replaced-observer"} {
		t.Run(mode, func(t *testing.T) {
			old := &robotSession{robot: &receiptOnlyRobot{}, commandReceipts: map[string]robotCommandReceipt{"receipt-original": {result: twinrobot.CommandResult{State: "completed"}}}}
			s := &Server{observers: map[string]*robotSession{"go2": old}}
			id := "receipt-original"
			switch mode {
			case "missing-observer":
				delete(s.observers, "go2")
			case "missing-id":
				id = "receipt-absent"
			case "replaced-observer":
				s.observers["go2"] = &robotSession{robot: &receiptOnlyRobot{}}
			}
			opened := 0
			w := setupCommandCall(s, context.Background(), func(context.Context, string) (robotControl, error) { opened++; return &receiptOnlyRobot{}, nil }, robotCommandRequest{Serial: "go2", Action: "receipt", RequestID: id})
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			var result twinrobot.CommandResult
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.State != "unknown" || !strings.Contains(result.Message, "No command receipt") || opened != 0 || len(s.commandSetups) != 0 {
				t.Fatalf("%+v opened%d", result, opened)
			}
			if mode == "missing-observer" && len(s.observers) != 0 {
				t.Fatal("lookup created observer")
			}
			if mode == "replaced-observer" && s.observers["go2"].commandReceipts != nil {
				t.Fatal("lookup created receipt in replacement observer")
			}
			if len(old.commandReceipts) != 1 {
				t.Fatal("lookup changed old receipt history")
			}
		})
	}
}

func TestRobotCommandReceiptValidatesIdentityFirst(t *testing.T) {
	for _, q := range []robotCommandRequest{
		{Serial: "", Action: "receipt", RequestID: "valid-request"},
		{Serial: "bad serial", Action: "receipt", RequestID: "valid-request"},
		{Serial: "go2", Action: "receipt", RequestID: "short"},
		{Serial: "go2", Action: "receipt", RequestID: strings.Repeat("x", 81)},
	} {
		s := &Server{observers: map[string]*robotSession{"go2": {robot: &receiptOnlyRobot{}}}}
		opened := 0
		w := setupCommandCall(s, context.Background(), func(context.Context, string) (robotControl, error) { opened++; return &receiptOnlyRobot{}, nil }, q)
		if w.Code != 400 || opened != 0 || len(s.commandSetups) != 0 {
			t.Fatalf("%+v status%d opened%d", q, w.Code, opened)
		}
	}
}
