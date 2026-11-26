package chat

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestRobotLocalizeUsesPassiveArgumentsAndCanonicalRoom(t *testing.T) {
	base := &engineTestExecutor{tools: []Tool{robotCommandTestTool()}, run: func(context.Context, ToolCall) (string, error) {
		return `{"state":"completed","snapshot":{"Aligned":true}}`, nil
	}}
	tools := &robotCommandTools{base: base}
	ctx := robotTurnContext()
	for _, raw := range []string{`{"action":"localize","destination":" Living Room ","degrees":180}`, `{"action":"localize","destination":"living room"}`} {
		if _, err := tools.Execute(ctx, ToolCall{Name: "robot_command", Arguments: json.RawMessage(raw)}); err != nil {
			t.Fatal(err)
		}
	}
	if len(base.calls) != 1 {
		t.Fatalf("equivalent room name repeated localization: %d calls", len(base.calls))
	}
	var args map[string]any
	_ = json.Unmarshal(base.calls[0].Arguments, &args)
	if args["action"] != "localize" || args["destination"] != " Living Room " {
		t.Fatalf("changed passive goal: %v", args)
	}
	if _, exists := args["serial"]; exists {
		t.Fatal("invented a robot serial instead of leaving sole-pairing selection to the tool")
	}
	if _, exists := args["degrees"]; exists {
		t.Fatal("localization retained motion arguments")
	}
	commandIdentity(t, base.calls[0])
	if _, err := tools.Execute(robotTurnContext(), ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"localize"}`)}); err != nil {
		t.Fatalf("automatic room selection: %v", err)
	}
	for _, raw := range []string{`{"action":"localize","destination":""}`, `{"action":"localize","destination":"\n"}`, `{"action":"localize","destination":12}`} {
		if _, err := tools.Execute(robotTurnContext(), ToolCall{Name: "robot_command", Arguments: json.RawMessage(raw)}); err == nil {
			t.Fatalf("accepted invalid room: %s", raw)
		}
	}
	if len(base.calls) != 2 {
		t.Fatal("invalid room reached backend")
	}
}

func TestRobotLocalizeDoesNotBypassUnknownMovement(t *testing.T) {
	base := &engineTestExecutor{tools: []Tool{robotCommandTestTool()}, run: func(context.Context, ToolCall) (string, error) {
		return `{"state":"running"}`, nil
	}}
	tools := &robotCommandTools{base: base}
	_, _ = tools.Execute(robotTurnContext(), ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"turn","degrees":30}`)})
	if _, err := tools.Execute(robotTurnContext(), ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"localize"}`)}); err == nil {
		t.Fatal("localization changed the navigation frame during unresolved movement")
	}
	if len(base.calls) != 1 || tools.pending == nil {
		t.Fatal("localize dispatched or cleared the unresolved movement")
	}
}

func TestRobotLocalizeAfterFailureDoesNotRepeatMovement(t *testing.T) {
	base := &engineTestExecutor{tools: []Tool{robotCommandTestTool()}}
	base.run = func(_ context.Context, call ToolCall) (string, error) {
		var args map[string]any
		_ = json.Unmarshal(call.Arguments, &args)
		if args["action"] == "localize" {
			return `{"state":"completed","snapshot":{"Aligned":true,"ReadinessMessage":"Fresh observations","LastMotionFailure":"Previous turn failed"}}`, nil
		}
		if len(base.calls) > 1 && args["receipt_only"] != true {
			t.Fatal("localization caused a failed movement to execute again")
		}
		return `{"state":"failed","message":"Turn stopped before completion"}`, nil
	}
	tools := &robotCommandTools{base: base}
	ctx := robotTurnContext()
	turn := ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"turn","degrees":90}`)}
	for _, call := range []ToolCall{turn, {Name: "robot_command", Arguments: json.RawMessage(`{"action":"localize"}`)}, turn} {
		if _, err := tools.Execute(ctx, call); err != nil {
			t.Fatal(err)
		}
	}
	if len(base.calls) != 3 || commandIdentity(t, base.calls[0]) != commandIdentity(t, base.calls[2]) {
		t.Fatal("failed movement identity was lost during passive recovery")
	}
}

func TestRobotLocalizeUnknownOutcomeOnlyQueriesReceipt(t *testing.T) {
	base := &engineTestExecutor{tools: []Tool{robotCommandTestTool()}}
	base.run = func(_ context.Context, call ToolCall) (string, error) {
		if len(base.calls) == 1 {
			return "", errors.New("lost localize response")
		}
		var args map[string]any
		_ = json.Unmarshal(call.Arguments, &args)
		if args["action"] != "localize" || args["receipt_only"] != true {
			t.Fatal("uncertain localization repeated instead of querying receipt")
		}
		return `{"state":"completed","snapshot":{"Aligned":true}}`, nil
	}
	tools := &robotCommandTools{base: base}
	call := ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"localize"}`)}
	if _, err := tools.Execute(robotTurnContext(), call); err == nil {
		t.Fatal("lost response not reported")
	}
	if _, err := tools.Execute(robotTurnContext(), call); err != nil {
		t.Fatal(err)
	}
	if commandIdentity(t, base.calls[0]) != commandIdentity(t, base.calls[1]) {
		t.Fatal("localization retry changed identity")
	}
}
