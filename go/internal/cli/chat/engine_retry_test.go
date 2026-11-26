package chat

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestEngineStopsRepeatedToolFailureAndCompletesHistory(t *testing.T) {
	base := &engineTestExecutor{tools: []Tool{{Name: "robot_command"}}}
	wantError := errors.New("invalid request")
	base.run = func(context.Context, ToolCall) (string, error) { return "", wantError }
	rounds := 0
	provider := engineTestProvider(func(context.Context, []Message, []Tool, func(string)) (Message, error) {
		rounds++
		calls := []ToolCall{{Name: "robot_command", Arguments: json.RawMessage(`{}`)}}
		if rounds == 3 {
			calls = append(calls, calls[0])
		}
		return Message{Role: "assistant", ToolCalls: calls}, nil
	})
	engine := NewEngine(provider, base, "")
	err := engine.Turn(context.Background(), "request", nil, nil)
	if !errors.Is(err, wantError) || rounds != 3 || len(base.calls) != 3 {
		t.Fatalf("unbounded retry: rounds=%d calls=%d error=%v", rounds, len(base.calls), err)
	}
	for _, m := range engine.Messages() {
		for _, c := range m.ToolCalls {
			found := false
			for _, result := range engine.Messages() {
				found = found || result.ToolCallID == c.ID
			}
			if !found {
				t.Fatalf("unpaired tool call %s", c.ID)
			}
		}
	}
}

func TestEngineAllowsRecoveryAfterSuccessfulIntervention(t *testing.T) {
	base := &engineTestExecutor{tools: []Tool{{Name: "robot_command"}, {Name: "repair"}}}
	base.run = func(_ context.Context, call ToolCall) (string, error) {
		if call.Name == "repair" {
			return "repaired", nil
		}
		return "", errors.New("not ready")
	}
	round := 0
	provider := engineTestProvider(func(context.Context, []Message, []Tool, func(string)) (Message, error) {
		round++
		if round == 6 {
			return Message{Content: "Still blocked."}, nil
		}
		name := "robot_command"
		if round == 3 {
			name = "repair"
		}
		return Message{ToolCalls: []ToolCall{{Name: name, Arguments: json.RawMessage(`{}`)}}}, nil
	})
	if err := NewEngine(provider, base, "").Turn(context.Background(), "request", nil, nil); err != nil || round != 6 {
		t.Fatalf("recovery incorrectly stopped: round=%d error=%v", round, err)
	}
}
