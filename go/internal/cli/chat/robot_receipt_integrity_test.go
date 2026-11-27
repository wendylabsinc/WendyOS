package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func TestLargeMCPReceiptFollowsOriginalLocalization(t *testing.T) {
	base := &commandMetadataExecutor{engineTestExecutor: &engineTestExecutor{tools: []Tool{robotCommandTestTool()}}}
	base.runResult = func(_ context.Context, call ToolCall) (ToolResult, error) {
		state, aligned := "running", false
		if len(base.calls) > 1 {
			state, aligned = "completed", true
			if !strings.Contains(string(call.Arguments), `"receipt_only":true`) || commandIdentity(t, call) != commandIdentity(t, base.calls[0]) {
				t.Fatal("restarted instead of polling original receipt")
			}
		}
		raw, _ := json.Marshal(map[string]any{"localization_camera": map[string]any{"message": strings.Repeat("x", maxToolOutputBytes+100)}, "snapshot": map[string]bool{"Aligned": aligned}, "state": state, "message": "Controller receipt"})
		return robotControlToolResult(mcpgo.NewToolResultText(string(raw)))
	}
	tracker := &robotCommandTools{base: base, followInterval: time.Millisecond}
	got, err := tracker.ExecuteResult(robotTurnContext(), ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"localize_active"}`)})
	var receipt robotReceipt
	if err != nil || json.Unmarshal([]byte(got.Text), &receipt) != nil || receipt.State != "completed" || len(base.calls) != 2 || tracker.pending != nil {
		t.Fatalf("large receipt failed: state=%s calls=%d err=%v", receipt.State, len(base.calls), err)
	}
}

func TestUnresolvedReceiptEndsInitialAndRetryTurnBeforeNarration(t *testing.T) {
	for _, outcome := range []string{"malformed", "missing-state", "false-completion", "transport"} {
		t.Run(outcome, func(t *testing.T) {
			base := &commandMetadataExecutor{engineTestExecutor: &engineTestExecutor{tools: []Tool{robotCommandTestTool()}}}
			base.runResult = func(_ context.Context, call ToolCall) (ToolResult, error) {
				if len(base.calls) > 1 && (!strings.Contains(string(call.Arguments), `"receipt_only":true`) || commandIdentity(t, call) != commandIdentity(t, base.calls[0])) {
					t.Fatal("retry dispatched another motion")
				}
				switch outcome {
				case "missing-state":
					return ToolResult{Text: `{"snapshot":{"Aligned":true}}`}, nil
				case "false-completion":
					return ToolResult{Text: `{"state":"completed","snapshot":{"Aligned":false}}`}, nil
				case "transport":
					return ToolResult{}, errors.New("connection lost after dispatch")
				default:
					return ToolResult{Text: `{"localization_camera":`}, nil
				}
			}
			tracker := &robotCommandTools{base: base}
			modelCalls := 0
			engine := NewEngine(engineTestProvider(func(context.Context, []Message, []Tool, func(string)) (Message, error) {
				modelCalls++
				return Message{ToolCalls: []ToolCall{{Name: "robot_command", Arguments: json.RawMessage(`{"action":"localize_active"}`)}}}, nil
			}), tracker, "")
			for turn, prompt := range []string{"Localize with motion", "Retry"} {
				err := engine.Turn(context.Background(), prompt, nil, func(context.Context, ToolCall) (bool, error) { return true, nil })
				var monitoring *robotMonitoringError
				if !errors.As(err, &monitoring) || modelCalls != turn+1 || tracker.pending == nil {
					t.Fatalf("unresolved result reached model narration: turn=%d calls=%d err=%v", turn, modelCalls, err)
				}
			}
		})
	}
}

func TestOversizedRobotReceiptIsRejectedWithoutPartialJSON(t *testing.T) {
	got, err := robotControlToolResult(mcpgo.NewToolResultText(strings.Repeat("x", (1<<20)+1)))
	if err == nil || got.Text != "" {
		t.Fatal("oversized receipt returned partial control evidence")
	}
}

func TestRetryResolvesOriginalReceiptAfterMalformedResponse(t *testing.T) {
	base := &commandMetadataExecutor{engineTestExecutor: &engineTestExecutor{tools: []Tool{robotCommandTestTool()}}}
	base.runResult = func(_ context.Context, call ToolCall) (ToolResult, error) {
		if len(base.calls) == 1 {
			return ToolResult{Text: `{"localization_camera":`}, nil
		}
		if !strings.Contains(string(call.Arguments), `"receipt_only":true`) || commandIdentity(t, call) != commandIdentity(t, base.calls[0]) {
			t.Fatal("recovery restarted motion")
		}
		return ToolResult{Text: `{"state":"completed","message":"Alignment confirmed","snapshot":{"Aligned":true}}`}, nil
	}
	tracker := &robotCommandTools{base: base}
	call := ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"localize_active"}`)}
	if _, err := tracker.ExecuteResult(robotTurnContext(), call); err == nil || tracker.pending == nil {
		t.Fatal("invalid response did not retain unresolved receipt")
	}
	got, err := tracker.ExecuteResult(robotTurnContext(), call)
	if err != nil || tracker.pending != nil || len(base.calls) != 2 || !strings.Contains(got.Text, `"state":"completed"`) {
		t.Fatalf("original receipt could not recover: %v", err)
	}
}
