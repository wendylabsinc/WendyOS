package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAlignmentFollowsReceiptWithFrozenTarget(t *testing.T) {
	base := &commandMetadataExecutor{engineTestExecutor: &engineTestExecutor{tools: []Tool{robotCommandTestTool()}}}
	base.runResult = func(_ context.Context, call ToolCall) (ToolResult, error) {
		var args map[string]any
		_ = json.Unmarshal(call.Arguments, &args)
		if len(base.calls) == 1 {
			if args["receipt_only"] == true {
				t.Fatal("initial command was a lookup")
			}
			return ToolResult{Text: `{"state":"running","message":"Matching current view"}`, RobotSerial: "robot-a"}, nil
		}
		if args["receipt_only"] != true || args["serial"] != "robot-a" || commandIdentity(t, call) != commandIdentity(t, base.calls[0]) {
			t.Fatal("follow-through restarted motion or changed target/identity")
		}
		if len(base.calls) == 2 {
			return ToolResult{Text: `{"state":"running","message":"Turning to another view"}`, RobotSerial: "robot-a"}, nil
		}
		return ToolResult{Text: `{"state":"completed","message":"Alignment confirmed","snapshot":{"Aligned":true}}`, RobotSerial: "robot-a"}, nil
	}
	tools := &robotCommandTools{base: base, followInterval: time.Millisecond}
	var progress []Event
	ctx := context.WithValue(context.Background(), turnRuntimeKey{}, &turnRuntime{emit: func(e Event) { progress = append(progress, e) }})
	result, err := tools.ExecuteResult(ctx, ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"localize_active"}`)})
	if err != nil || !strings.Contains(result.Text, "Alignment confirmed") || len(base.calls) != 3 || tools.pending != nil || len(progress) != 2 {
		t.Fatalf("follow-through did not finish: %s %v calls=%d progress=%d", result.Text, err, len(base.calls), len(progress))
	}
	if !strings.Contains(progress[0].Text, "Esc cancels monitoring") || progress[1].Type != "tool_progress" {
		t.Fatal("progress hid cancellation semantics")
	}
}

func TestAlignmentFollowCancellationErrorsAndTerminalStates(t *testing.T) {
	for _, outcome := range []string{"failed", "rejected", "canceled", "superseded", "unknown", "transport", "cancel", "timeout", "false-completion"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithCancel(robotTurnContext())
			defer cancel()
			base := &engineTestExecutor{tools: []Tool{robotCommandTestTool()}}
			base.run = func(_ context.Context, call ToolCall) (string, error) {
				if len(base.calls) == 1 {
					if outcome == "cancel" {
						cancel()
					}
					return `{"state":"running","message":"Matching"}`, nil
				}
				if !strings.Contains(string(call.Arguments), `"receipt_only":true`) {
					t.Fatal("retry dispatched motion")
				}
				switch outcome {
				case "transport":
					return "", errors.New("lost receipt response")
				case "timeout":
					return `{"state":"running","message":"Matching"}`, nil
				case "false-completion":
					return `{"state":"completed","snapshot":{"Aligned":false}}`, nil
				default:
					return `{"state":"` + outcome + `","message":"Controller outcome"}`, nil
				}
			}
			tools := &robotCommandTools{base: base, followInterval: time.Millisecond, followLimit: 500 * time.Millisecond}
			if outcome == "timeout" {
				tools.followLimit = 8 * time.Millisecond
			}
			result, err := tools.ExecuteResult(ctx, ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"localize_active"}`)})
			terminal := outcome == "failed" || outcome == "rejected" || outcome == "canceled" || outcome == "superseded"
			if terminal {
				if err != nil || tools.pending != nil || !strings.Contains(result.Text, outcome) {
					t.Fatalf("lost terminal outcome: %+v %v", result, err)
				}
			} else if err == nil || tools.pending == nil {
				t.Fatalf("unverified outcome released pending: %v", err)
			}
			if outcome == "cancel" && len(base.calls) != 1 {
				t.Fatal("canceled monitor sent another request")
			}
			if outcome == "transport" && len(base.calls) != 4 {
				t.Fatalf("unbounded transport retries: %d", len(base.calls))
			}
		})
	}
}

func TestAcknowledgedStopReconcilesOnlyMatchingRobot(t *testing.T) {
	for _, stopTarget := range []string{"robot-a", "robot-b", ""} {
		for _, state := range []string{"completed", "failed", "unknown"} {
			base := &commandMetadataExecutor{engineTestExecutor: &engineTestExecutor{tools: []Tool{robotCommandTestTool()}}}
			base.runResult = func(_ context.Context, call ToolCall) (ToolResult, error) {
				if len(base.calls) == 1 {
					return ToolResult{RobotSerial: "robot-a"}, errors.New("lost dispatch reply")
				}
				return ToolResult{Text: `{"state":"` + state + `"}`, RobotSerial: stopTarget}, nil
			}
			tools := &robotCommandTools{base: base}
			ctx := robotTurnContext()
			move := ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"turn","degrees":90}`)}
			_, _ = tools.Execute(ctx, move)
			old := tools.pending.attempt
			_, _ = tools.Execute(ctx, ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"stop","serial":"` + stopTarget + `"}`)})
			cleared := state == "completed" && (stopTarget == "robot-a" || stopTarget == "")
			if (tools.pending == nil) != cleared || old.completed || old.stopped != cleared {
				t.Fatalf("incorrect Stop cleanup: target=%q state=%s", stopTarget, state)
			}
			if cleared {
				got, err := tools.Execute(ctx, move)
				if err != nil || !strings.Contains(got, "canceled") || len(base.calls) != 2 {
					t.Fatal("stopped attempt replayed or claimed completed", err)
				}
			}
		}
	}
}

func TestChangedGoalFirstResolvesTerminalReceipt(t *testing.T) {
	base := &engineTestExecutor{tools: []Tool{robotCommandTestTool()}}
	base.run = func(_ context.Context, call ToolCall) (string, error) {
		if len(base.calls) == 1 {
			return `{"state":"running"}`, nil
		}
		if len(base.calls) == 2 && !strings.Contains(string(call.Arguments), `"receipt_only":true`) {
			t.Fatal("new goal bypassed prior receipt")
		}
		return `{"state":"completed"}`, nil
	}
	tools := &robotCommandTools{base: base}
	_, _ = tools.Execute(robotTurnContext(), ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"turn","degrees":90}`)})
	if _, err := tools.Execute(robotTurnContext(), ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"forward","distance_meters":1}`)}); err != nil {
		t.Fatal(err)
	}
	if len(base.calls) != 3 || commandIdentity(t, base.calls[0]) != commandIdentity(t, base.calls[1]) || commandIdentity(t, base.calls[1]) == commandIdentity(t, base.calls[2]) {
		t.Fatal("incorrect receipt/new-goal identity")
	}
}
