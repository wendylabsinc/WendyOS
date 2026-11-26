package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

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
