package chat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestLocalizationAnnouncementContinuesThroughNormalApproval(t *testing.T) {
	base := &engineTestExecutor{tools: []Tool{navigationStatusTool(), robotCommandTestTool()}}
	base.run = func(_ context.Context, call ToolCall) (string, error) {
		if call.Name == "robot_status" {
			return `{"snapshot":{"Aligned":false}}`, nil
		}
		return `{"state":"completed","snapshot":{"Aligned":true}}`, nil
	}
	rounds, approvals := 0, 0
	provider := engineTestProvider(func(_ context.Context, messages []Message, _ []Tool, _ func(string)) (Message, error) {
		rounds++
		switch rounds {
		case 1:
			return Message{ToolCalls: []ToolCall{{Name: "robot_status", Arguments: json.RawMessage(`{}`)}}}, nil
		case 2:
			return Message{Content: "Executing: robot_command action localize_active now."}, nil
		case 3:
			if messages[len(messages)-1].Role != "system" || messages[len(messages)-1].Content != localizationContinuationReminder {
				t.Fatal("missing conditional reminder", messages)
			}
			return Message{ToolCalls: []ToolCall{{Name: "robot_command", Arguments: json.RawMessage(`{"action":"localize_active"}`)}}}, nil
		default:
			return Message{Content: "Alignment confirmed."}, nil
		}
	})
	engine := NewEngine(provider, &robotCommandTools{base: base}, "")
	engine.robotControlMode = true
	if err := engine.Turn(context.Background(), "Go to Desk", nil, func(context.Context, ToolCall) (bool, error) { approvals++; return true, nil }); err != nil {
		t.Fatal(err)
	}
	if rounds != 4 || approvals != 1 || len(base.calls) != 2 {
		t.Fatal(rounds, approvals, base.calls)
	}
}

func TestLocalizationContinuationIsBounded(t *testing.T) {
	base := &engineTestExecutor{tools: []Tool{navigationStatusTool()}, run: func(context.Context, ToolCall) (string, error) { return `{"snapshot":{"Aligned":false}}`, nil }}
	rounds := 0
	provider := engineTestProvider(func(_ context.Context, _ []Message, _ []Tool, _ func(string)) (Message, error) {
		rounds++
		if rounds == 1 {
			return Message{ToolCalls: []ToolCall{{Name: "robot_status", Arguments: json.RawMessage(`{}`)}}}, nil
		}
		return Message{Content: "I will perform active alignment."}, nil
	})
	engine := NewEngine(provider, base, "")
	engine.robotControlMode = true
	if err := engine.Turn(context.Background(), "Go to Desk", nil, nil); err == nil || !strings.Contains(err.Error(), "requested navigation has not completed") {
		t.Fatal("promised action was reported as completion", err)
	}
	nudges := 0
	for _, message := range engine.Messages() {
		if message.Content == localizationContinuationReminder {
			nudges++
		}
	}
	if rounds != 4 || nudges != 2 || len(base.calls) != 1 {
		t.Fatal(rounds, nudges, base.calls)
	}
}

func TestLocalizationContinuationLeavesOrdinaryStatusAlone(t *testing.T) {
	base := &engineTestExecutor{tools: []Tool{navigationStatusTool()}, run: func(context.Context, ToolCall) (string, error) { return `{"snapshot":{"Aligned":false}}`, nil }}
	rounds := 0
	provider := engineTestProvider(func(context.Context, []Message, []Tool, func(string)) (Message, error) {
		rounds++
		return Message{Content: "The robot is not aligned."}, nil
	})
	engine := NewEngine(provider, base, "")
	engine.robotControlMode = true
	if err := engine.Turn(context.Background(), "Is the robot aligned?", nil, nil); err != nil {
		t.Fatal(err)
	}
	if rounds != 1 || len(base.calls) != 1 {
		t.Fatal(rounds, base.calls)
	}
}

func TestLocalizationContinuationNeverRetriesAnAttempt(t *testing.T) {
	for _, outcome := range []string{"failed", "canceled", "unknown", "completed", "denied"} {
		t.Run(outcome, func(t *testing.T) {
			messages := []Message{
				{Role: "assistant", ToolCalls: []ToolCall{{ID: "status", Name: "robot_status"}}},
				{Role: "tool", ToolCallID: "status", Content: `{"snapshot":{"Aligned":false}}`},
				{Role: "assistant", ToolCalls: []ToolCall{{ID: "align", Name: "robot_command", Arguments: json.RawMessage(`{"action":"localize_active"}`)}}},
				{Role: "tool", ToolCallID: "align", Content: `{"state":"` + outcome + `"}`},
			}
			if needsLocalizationContinuation(messages, "I will perform active alignment.") {
				t.Fatal("attempted localization prompted again")
			}
		})
	}
	for _, text := range []string{"Alignment failed.", "I cannot localize_active because observations are unavailable.", "I will not align without permission."} {
		messages := []Message{{ToolCalls: []ToolCall{{ID: "status", Name: "robot_status"}}}, {Role: "tool", ToolCallID: "status", Content: `{"snapshot":{"Aligned":false}}`}}
		if needsLocalizationContinuation(messages, text) {
			t.Fatal("blocker treated as a promise", text)
		}
	}
	for _, guard := range []string{"only if authorized", "status question", "passive-only", "no-extra-motion", "Do not retry failed, canceled or unknown"} {
		if !strings.Contains(localizationContinuationReminder, guard) {
			t.Fatal("reminder lost scope restriction", guard)
		}
	}
}
