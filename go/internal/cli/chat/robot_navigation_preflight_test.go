package chat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func navigationStatusTool() Tool {
	return Tool{Name: "robot_status", Parameters: json.RawMessage(`{"type":"object","properties":{"serial":{"type":"string"}},"additionalProperties":false}`)}
}

func TestNavigationPreflightAllowsLocalizationBeforeOriginalDispatch(t *testing.T) {
	aligned := false
	base := &commandMetadataExecutor{engineTestExecutor: &engineTestExecutor{tools: []Tool{robotCommandTestTool(), navigationStatusTool()}}}
	base.runResult = func(_ context.Context, call ToolCall) (ToolResult, error) {
		if call.Name == "robot_status" {
			data, _ := json.Marshal(map[string]any{"snapshot": map[string]bool{"Aligned": aligned}})
			return ToolResult{Text: string(data), RobotSerial: "robot-a"}, nil
		}
		var args map[string]any
		_ = json.Unmarshal(call.Arguments, &args)
		if args["action"] == "localize_active" {
			aligned = true
			return ToolResult{Text: `{"state":"completed","snapshot":{"Aligned":true}}`, RobotSerial: "robot-a"}, nil
		}
		if !aligned || args["serial"] != "robot-a" {
			t.Fatal("unverified or unpinned navigation", args)
		}
		return ToolResult{Text: `{"state":"completed"}`, RobotSerial: "robot-a"}, nil
	}
	tools := &robotCommandTools{base: base}
	ctx := robotTurnContext()
	move := ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"go_to","destination":"Desk"}`)}
	result, err := tools.ExecuteResult(ctx, move)
	if err == nil || !result.CommandNotStarted || !strings.Contains(err.Error(), "localize_active") || tools.pending != nil || len(base.calls) != 1 {
		t.Fatal("preflight dispatched movement or lost recovery", result, err, base.calls)
	}
	if _, err = tools.ExecuteResult(ctx, ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"localize_active"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err = tools.ExecuteResult(ctx, move); err != nil {
		t.Fatal(err)
	}
	if _, err = tools.ExecuteResult(ctx, move); err != nil {
		t.Fatal(err)
	}
	if len(base.calls) != 4 {
		t.Fatal("completed movement replayed or re-preflighted", base.calls)
	}
	if commandIdentity(t, base.calls[1]) == commandIdentity(t, base.calls[3]) {
		t.Fatal("prerequisite reused movement identity")
	}
}

func TestNavigationPreflightFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		offered      bool
		serial       string
	}{
		{"unaligned", `{"snapshot":{"Aligned":false}}`, true, "robot-a"},
		{"invalid", `not json`, true, "robot-a"},
		{"missing", `{"snapshot":{}}`, true, "robot-a"},
		{"policy", `{"snapshot":{"Aligned":true}}`, false, "robot-a"},
		{"target changed", `{"snapshot":{"Aligned":true}}`, true, "robot-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := &commandMetadataExecutor{engineTestExecutor: &engineTestExecutor{tools: []Tool{robotCommandTestTool()}}}
			if tc.offered {
				base.tools = append(base.tools, navigationStatusTool())
			}
			base.runResult = func(_ context.Context, call ToolCall) (ToolResult, error) {
				if call.Name != "robot_status" {
					t.Fatal("movement sent", call)
				}
				return ToolResult{Text: tc.status, RobotSerial: tc.serial}, nil
			}
			tools := &robotCommandTools{base: base}
			result, err := tools.ExecuteResult(robotTurnContext(), ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"go_to","destination":"Desk","serial":"robot-a"}`)})
			if err == nil || !result.CommandNotStarted || tools.pending != nil {
				t.Fatal(result, err, tools.pending)
			}
		})
	}
}

func TestNavigationPreflightSkipsPendingReceipt(t *testing.T) {
	base := &engineTestExecutor{tools: []Tool{robotCommandTestTool(), navigationStatusTool()}}
	base.run = func(_ context.Context, call ToolCall) (string, error) {
		if len(base.calls) == 1 {
			if call.Name != "robot_status" {
				t.Fatal(call)
			}
			return `{"snapshot":{"Aligned":true}}`, nil
		}
		if call.Name != "robot_command" {
			t.Fatal("pending motion was preflighted", call)
		}
		if len(base.calls) == 2 {
			return `{"state":"running"}`, nil
		}
		if !strings.Contains(string(call.Arguments), `"receipt_only":true`) {
			t.Fatal("pending motion replayed", call)
		}
		return `{"state":"completed"}`, nil
	}
	tools := &robotCommandTools{base: base}
	move := ToolCall{Name: "robot_command", Arguments: json.RawMessage(`{"action":"go_to","destination":"Desk"}`)}
	if _, err := tools.ExecuteResult(robotTurnContext(), move); err != nil {
		t.Fatal(err)
	}
	if _, err := tools.ExecuteResult(robotTurnContext(), move); err != nil {
		t.Fatal(err)
	}
	if len(base.calls) != 3 {
		t.Fatal(base.calls)
	}
}

func TestNavigationStatusPrerequisiteIsConditional(t *testing.T) {
	for _, text := range []string{`{"snapshot":{"Aligned":true}}`, `{"snapshot":{}}`, `not json`} {
		result := navigationPrerequisiteEvidence(ToolResult{Text: text})
		if result.Text != text {
			t.Fatal("invented an alignment prerequisite", result)
		}
	}
	result := navigationPrerequisiteEvidence(ToolResult{Text: `{"snapshot":{"Aligned":false},"message":"not aligned"}`, RobotSerial: "robot-a"})
	for _, phrase := range []string{"localize_active", "required_before", "No motion for status-only, passive alignment or no-extra-motion requests", "pending movement receipt", "Failure, cancellation or unknown outcome blocks"} {
		if !strings.Contains(result.Text, phrase) {
			t.Fatal("missing conditional prerequisite", phrase, result)
		}
	}
	if result.RobotSerial != "robot-a" {
		t.Fatal("status target lost")
	}
}
