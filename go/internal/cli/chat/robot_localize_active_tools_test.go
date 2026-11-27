package chat

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestRobotActiveLocalizationIsDistinctAndReceiptOnlyOnRetry(t *testing.T) {
	base := &engineTestExecutor{tools: []Tool{robotCommandTestTool()}}
	base.run = func(_ context.Context, call ToolCall) (string, error) {
		var args map[string]any
		_ = json.Unmarshal(call.Arguments, &args)
		switch len(base.calls) {
		case 1:
			if args["action"] != "localize" {
				t.Fatal("passive request became active")
			}
			return `{"state":"rejected","message":"No reliable match from this view"}`, nil
		case 2:
			if args["action"] != "localize_active" || args["receipt_only"] == true || args["destination"] != "Living Room" {
				t.Fatalf("active search not dispatched as requested: %v", args)
			}
			if _, exists := args["degrees"]; exists {
				t.Fatal("active localization became an arbitrary turn")
			}
			return `{"state":"running","message":"Matching the current view"}`, nil
		default:
			if args["action"] != "localize_active" || args["receipt_only"] != true {
				t.Fatal("retry restarted movement-assisted localization")
			}
			return `{"state":"completed","snapshot":{"Aligned":true}}`, nil
		}
	}
	tools := &robotCommandTools{base: base, followInterval: time.Millisecond}
	ctx := robotTurnContext()
	passive := ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"localize","destination":"Living Room"}`)}
	active := ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"localize_active","destination":"Living Room","degrees":180}`)}
	for _, call := range []ToolCall{passive, active} {
		if _, err := tools.Execute(ctx, call); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tools.Execute(ctx, active); err != nil {
		t.Fatal(err)
	}
	if len(base.calls) != 3 || commandIdentity(t, base.calls[0]) == commandIdentity(t, base.calls[1]) || commandIdentity(t, base.calls[1]) != commandIdentity(t, base.calls[2]) {
		t.Fatal("passive/active identity separation or active retry identity failed")
	}
}

func TestRobotActiveLocalizationCannotBypassUnresolvedMovement(t *testing.T) {
	base := &engineTestExecutor{tools: []Tool{robotCommandTestTool()}, run: func(context.Context, ToolCall) (string, error) {
		return `{"state":"running"}`, nil
	}}
	tools := &robotCommandTools{base: base}
	_, _ = tools.Execute(robotTurnContext(), ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"forward","distance_meters":1}`)})
	if _, err := tools.Execute(robotTurnContext(), ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"localize_active"}`)}); err == nil {
		t.Fatal("active localization bypassed an unresolved movement")
	}
	if len(base.calls) != 2 || tools.pending == nil {
		t.Fatal("active localization reached the backend or released the prior goal")
	}
}
