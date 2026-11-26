package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRobotConnectionQuestionsSelectPairings(t *testing.T) {
	available := []Tool{{Name: "wendy_status"}, {Name: "robot_status"}, {Name: "unitree_pairings"}}
	for _, tc := range []struct {
		prompt string
		prior  bool
		want   bool
	}{
		{"Is the robot online?", false, true},
		{"Is my Go2 paired with the device?", false, true},
		{"Is the Go2 connected after reboot?", false, true},
		{"Check whether the robot is offline", false, true},
		{"Are my Unitree robots disconnected?", false, true},
		{"Is it still online?", true, true},
		{"Are they paired?", true, true},
		{"Is it online?", false, false},
		{"Is my Orin online?", true, false},
		{"Is my Orin still online?", true, false},
		{"Is the gateway online or is it disconnected?", true, false},
		{"Is the device connected?", true, false},
		{"How do I get my Go2 connected?", true, false},
		{"Connect the robot", true, false},
	} {
		t.Run(tc.prompt, func(t *testing.T) {
			e := NewEngine(nil, nil, "")
			if tc.prior {
				e.messages = []Message{{ToolCalls: []ToolCall{{Name: "unitree_pairings"}}}}
			}
			got := e.robotEvidenceCall(tc.prompt, available, 0)
			if tc.want {
				if got == nil || got.Name != "unitree_pairings" || string(got.Arguments) != "{}" {
					t.Fatalf("connection question did not read pairings: %+v", got)
				}
			} else if got != nil {
				t.Fatalf("unrelated question forced robot evidence: %+v", got)
			}
		})
	}
}

func TestRobotConnectionEvidenceHonorsOfferedReadTools(t *testing.T) {
	e := NewEngine(nil, nil, "")
	for _, available := range [][]Tool{
		{{Name: "wendy_status"}, {Name: "robot_status"}},
		{{Name: "unitree_pairings", RequiresApproval: true}},
	} {
		if got := e.robotEvidenceCall("Is the robot still connected?", available, 0); got != nil {
			t.Fatalf("unavailable read tool replaced or policy bypassed: %+v", got)
		}
	}
	if got := e.robotEvidenceCall("Is the robot online?", []Tool{{Name: "unitree_pairings"}}, 1); got != nil {
		t.Fatal("pairing evidence repeated on later model rounds")
	}
}

func TestRobotOnlineReadsPairingEvenWhenGatewayConnected(t *testing.T) {
	base := &engineTestExecutor{tools: []Tool{
		{Name: "wendy_status", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Name: "unitree_pairings", Parameters: json.RawMessage(`{"type":"object"}`)},
	}}
	base.run = func(_ context.Context, call ToolCall) (string, error) {
		if call.Name != "unitree_pairings" {
			t.Fatalf("robot online question used gateway status: %s", call.Name)
		}
		return `{"robots":[{"serial":"robot-a","connected":false}]}`, nil
	}
	provider := engineTestProvider(func(_ context.Context, messages []Message, _ []Tool, _ func(string)) (Message, error) {
		if len(base.calls) != 1 || messages[len(messages)-1].Role != "tool" || !strings.Contains(messages[len(messages)-1].Content, `"connected":false`) {
			t.Fatal("model answered without current robot pairing evidence")
		}
		return Message{Content: "The gateway is connected, but the Go2 pairing reports disconnected."}, nil
	})
	e := NewEngine(provider, base, "")
	e.messages = []Message{{Role: "assistant", Content: "The Orin gateway is connected."}}
	if err := e.Turn(context.Background(), "Is the robot online?", nil, func(context.Context, ToolCall) (bool, error) {
		t.Fatal("read-only pairing evidence requested approval")
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRobotStatusEvidencePrecedesModelAndStopKeepsApproval(t *testing.T) {
	for _, prompt := range []string{"Is it aligned?", "status?", "It's not moving", "stop"} {
		base := &engineTestExecutor{tools: []Tool{robotCommandTestTool(), {Name: "robot_status", Parameters: json.RawMessage(`{"type":"object"}`)}}}
		base.run = func(context.Context, ToolCall) (string, error) {
			return `{"state":"completed","snapshot":{"Aligned":false}}`, nil
		}
		provider := engineTestProvider(func(_ context.Context, messages []Message, _ []Tool, _ func(string)) (Message, error) {
			if len(base.calls) != 1 || messages[len(messages)-1].Role != "tool" {
				t.Fatal("model answered without fresh evidence")
			}
			return Message{Content: "Current alignment is unconfirmed."}, nil
		})
		e := NewEngine(provider, &robotCommandTools{base: base}, "")
		e.robotControlMode = true
		approvals := 0
		if err := e.Turn(context.Background(), prompt, nil, func(context.Context, ToolCall) (bool, error) { approvals++; return true, nil }); err != nil {
			t.Fatal(err)
		}
		if (approvals == 1) != (prompt == "stop") {
			t.Fatal("Stop bypassed approval or status requested it")
		}
	}
	for _, prompt := range []string{"stop", "how do I align a robot?", "what is localization?", "explain progress indicators"} {
		e := NewEngine(nil, nil, "")
		if e.robotEvidenceCall(prompt, []Tool{robotCommandTestTool(), {Name: "robot_status"}}, 0) != nil {
			t.Fatalf("general question forced robot action: %q", prompt)
		}
	}
}

func TestExactStopInterruptsMonitoringQueue(t *testing.T) {
	m := newChatModel(context.Background(), UIOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	m.active = true
	m.cancelTurn = cancel
	m.queuedPrompts = []string{"old followup"}
	m.submit("stop")
	if ctx.Err() == nil || len(m.queuedPrompts) != 1 || m.queuedPrompts[0] != "stop" {
		t.Fatal("Stop queued behind active monitor")
	}
	m.handleEvent(Event{Type: "tool_progress", Text: "Turning. Esc cancels monitoring; search continues until Stop."})
	last := m.transcript[len(m.transcript)-1]
	if isActivityEntry(last) || !strings.Contains(last.text, "Esc cancels monitoring") {
		t.Fatal("progress hidden by collapsed tools")
	}
}

func TestAlignmentMonitorCannotSwitchToNewPendingGoal(t *testing.T) {
	base := &commandMetadataExecutor{engineTestExecutor: &engineTestExecutor{tools: []Tool{robotCommandTestTool()}}}
	tools := &robotCommandTools{base: base, followInterval: time.Millisecond}
	base.runResult = func(_ context.Context, call ToolCall) (ToolResult, error) {
		var args struct{ Action string }
		_ = json.Unmarshal(call.Arguments, &args)
		switch args.Action {
		case "localize_active":
			if _, err := tools.Execute(robotTurnContext(), ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"stop","serial":"robot-a"}`)}); err != nil {
				t.Fatal(err)
			}
			if _, err := tools.Execute(robotTurnContext(), ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"forward","distance_meters":1,"serial":"robot-a"}`)}); err != nil {
				t.Fatal(err)
			}
			return ToolResult{Text: `{"state":"running","message":"Matching"}`, RobotSerial: "robot-a"}, nil
		case "stop":
			return ToolResult{Text: `{"state":"completed"}`, RobotSerial: "robot-a"}, nil
		case "forward":
			return ToolResult{Text: `{"state":"running"}`, RobotSerial: "robot-a"}, nil
		default:
			return ToolResult{}, errors.New("unexpected command")
		}
	}
	got, err := tools.Execute(robotTurnContext(), ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"localize_active","serial":"robot-a"}`)})
	if err != nil || !strings.Contains(got, "canceled") || len(base.calls) != 3 || tools.pending == nil || !strings.Contains(tools.pending.key, "forward") {
		t.Fatalf("monitor switched owners: %s %v", got, err)
	}
}

func TestDelayedStopReplyCannotReleaseNewerGoal(t *testing.T) {
	base := &commandMetadataExecutor{engineTestExecutor: &engineTestExecutor{tools: []Tool{robotCommandTestTool()}}}
	tools := &robotCommandTools{base: base}
	base.runResult = func(ctx context.Context, call ToolCall) (ToolResult, error) {
		var args struct {
			Action      string
			ReceiptOnly bool `json:"receipt_only"`
		}
		_ = json.Unmarshal(call.Arguments, &args)
		if args.ReceiptOnly {
			return ToolResult{Text: `{"state":"completed"}`, RobotSerial: "robot-a"}, nil
		}
		if args.Action == "stop" {
			old := tools.pending.attempt
			if _, err := tools.lookupRobotAttempt(ctx, ToolCall{Name: "robot_command"}, old); err != nil {
				t.Fatal(err)
			}
			if _, err := tools.Execute(robotTurnContext(), ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"forward","distance_meters":1,"serial":"robot-a"}`)}); err != nil {
				t.Fatal(err)
			}
			return ToolResult{Text: `{"state":"completed"}`, RobotSerial: "robot-a"}, nil
		}
		return ToolResult{Text: `{"state":"running"}`, RobotSerial: "robot-a"}, nil
	}
	_, _ = tools.Execute(robotTurnContext(), ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"turn","degrees":90,"serial":"robot-a"}`)})
	if _, err := tools.Execute(robotTurnContext(), ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"stop"}`)}); err != nil {
		t.Fatal(err)
	}
	if tools.pending == nil || !strings.Contains(tools.pending.key, "forward") || tools.pending.attempt.stopped || tools.pending.attempt.completed {
		t.Fatal("delayed Stop acknowledgement released the newer running goal")
	}
}
