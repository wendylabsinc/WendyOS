package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type engineTestProvider func(context.Context, []Message, []Tool, func(string)) (Message, error)

func (p engineTestProvider) Stream(ctx context.Context, messages []Message, tools []Tool, emit func(string)) (Message, error) {
	return p(ctx, messages, tools, emit)
}

type engineTestExecutor struct {
	tools []Tool
	calls []ToolCall
	lists int
	run   func(context.Context, ToolCall) (string, error)
}

func (e *engineTestExecutor) ListTools(context.Context) ([]Tool, error) {
	e.lists++
	return e.tools, nil
}

func (e *engineTestExecutor) Execute(ctx context.Context, call ToolCall) (string, error) {
	e.calls = append(e.calls, call)
	if e.run != nil {
		return e.run(ctx, call)
	}
	return "ok", nil
}

func TestEngineRunsToolsAndRefreshesAfterConnection(t *testing.T) {
	executor := &engineTestExecutor{tools: []Tool{{Name: "connect", RequiresApproval: true}}}
	executor.run = func(context.Context, ToolCall) (string, error) {
		executor.tools = []Tool{{Name: "status"}}
		return "connected", nil
	}
	round := 0
	provider := engineTestProvider(func(_ context.Context, messages []Message, tools []Tool, emit func(string)) (Message, error) {
		round++
		if round == 1 {
			if messages[0].Role != "system" || tools[0].Name != "connect" {
				t.Fatalf("initial messages/tools = %+v %+v", messages, tools)
			}
			emit("Connecting. ")
			return Message{Content: "Connecting. ", ToolCalls: []ToolCall{{ID: "one", Name: "connect", Arguments: json.RawMessage(`{}`)}}}, nil
		}
		if tools[0].Name != "status" || messages[len(messages)-1].Content != "connected" {
			t.Fatalf("tools/results did not refresh: %+v %+v", tools, messages)
		}
		emit("Ready.")
		return Message{Content: "Ready."}, nil
	})
	engine := NewEngine(provider, executor, "help with hardware")
	var events []Event
	approved := 0
	err := engine.Turn(context.Background(), "connect", func(event Event) { events = append(events, event) }, func(context.Context, ToolCall) (bool, error) {
		approved++
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if approved != 1 || len(executor.calls) != 1 || executor.lists != 2 {
		t.Fatalf("approval/calls/lists = %d/%d/%d", approved, len(executor.calls), executor.lists)
	}
	var output string
	for _, event := range events {
		if event.Type == "text" {
			output += event.Text
		}
	}
	if output != "Connecting. Ready." {
		t.Fatalf("stream output = %q", output)
	}
	engine.Reset()
	if messages := engine.Messages(); len(messages) != 1 || messages[0].Role != "system" {
		t.Fatalf("reset messages = %+v", messages)
	}
}

func TestEngineDenialUnknownAndMalformedArgumentsNeverExecute(t *testing.T) {
	cases := []struct {
		name      string
		call      ToolCall
		approval  ApproveFunc
		wantError string
	}{
		{"nil approval", ToolCall{Name: "write", Arguments: json.RawMessage(`{"path":"a"}`)}, nil, "denied permission"},
		{"denied", ToolCall{Name: "write", Arguments: json.RawMessage(`{"path":"a"}`)}, func(context.Context, ToolCall) (bool, error) { return false, nil }, "denied permission"},
		{"unknown", ToolCall{Name: "invented", Arguments: json.RawMessage(`{}`)}, nil, "unknown tool"},
		{"invalid JSON", ToolCall{Name: "write", Arguments: json.RawMessage(`{"path":`)}, nil, "invalid JSON"},
		{"missing field", ToolCall{Name: "write", Arguments: json.RawMessage(`{}`)}, nil, "invalid arguments"},
		{"wrong type", ToolCall{Name: "write", Arguments: json.RawMessage(`{"path":1}`)}, nil, "invalid arguments"},
		{"null", ToolCall{Name: "write", Arguments: json.RawMessage(`null`)}, nil, "JSON object"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			executor := &engineTestExecutor{tools: []Tool{{Name: "write", RequiresApproval: true, Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)}}}
			round := 0
			provider := engineTestProvider(func(_ context.Context, messages []Message, _ []Tool, _ func(string)) (Message, error) {
				round++
				if round == 1 {
					return Message{ToolCalls: []ToolCall{test.call}}, nil
				}
				result := messages[len(messages)-1]
				if result.Role != "tool" || !strings.Contains(result.Content, test.wantError) {
					t.Fatalf("tool result = %+v, want %q", result, test.wantError)
				}
				call := messages[len(messages)-2].ToolCalls[0]
				if call.ID == "" || call.ID != result.ToolCallID || !json.Valid(call.Arguments) {
					t.Fatalf("invalid conversation call = %+v, result = %+v", call, result)
				}
				return Message{Content: "Stopped."}, nil
			})
			if err := NewEngine(provider, executor, "").Turn(context.Background(), "change file", nil, test.approval); err != nil {
				t.Fatal(err)
			}
			if len(executor.calls) != 0 {
				t.Fatalf("executed rejected tool: %+v", executor.calls)
			}
		})
	}
}

func TestEngineCancellationCompletesAllToolResults(t *testing.T) {
	for _, duringApproval := range []bool{true, false} {
		t.Run(map[bool]string{true: "approval", false: "execution"}[duringApproval], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			executor := &engineTestExecutor{tools: []Tool{{Name: "act", RequiresApproval: duringApproval}}}
			executor.run = func(ctx context.Context, _ ToolCall) (string, error) {
				cancel()
				return "partial", ctx.Err()
			}
			provider := engineTestProvider(func(context.Context, []Message, []Tool, func(string)) (Message, error) {
				return Message{ToolCalls: []ToolCall{{ID: "same", Name: "act", Arguments: json.RawMessage(`{}`)}, {ID: "same", Name: "act", Arguments: json.RawMessage(`{}`)}}}, nil
			})
			engine := NewEngine(provider, executor, "")
			err := engine.Turn(ctx, "do things", nil, func(context.Context, ToolCall) (bool, error) {
				cancel()
				return false, context.Canceled
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v", err)
			}
			messages := engine.Messages()
			if len(messages) != 4 {
				t.Fatalf("messages = %+v", messages)
			}
			calls := messages[1].ToolCalls
			if calls[0].ID == calls[1].ID || messages[2].ToolCallID != calls[0].ID || messages[3].ToolCallID != calls[1].ID {
				t.Fatalf("unpaired tool results: %+v", messages)
			}
			wantCalls := 1
			if duringApproval {
				wantCalls = 0
			}
			if len(executor.calls) != wantCalls {
				t.Fatalf("executed %d calls, want %d", len(executor.calls), wantCalls)
			}
		})
	}
}

func TestEngineToolErrorsAndOutputLimits(t *testing.T) {
	executor := &engineTestExecutor{tools: []Tool{{Name: "read"}}, run: func(context.Context, ToolCall) (string, error) {
		return strings.Repeat("€", maxToolOutputBytes), errors.New("read failed")
	}}
	round := 0
	provider := engineTestProvider(func(_ context.Context, messages []Message, _ []Tool, _ func(string)) (Message, error) {
		round++
		if round == 1 {
			return Message{ToolCalls: []ToolCall{{ID: "read", Name: "read", Arguments: json.RawMessage(`{}`)}}}, nil
		}
		output := messages[len(messages)-1].Content
		if len(output) > maxToolOutputBytes || !strings.Contains(output, "read failed") || !strings.Contains(output, "truncated") || strings.ContainsRune(output, '�') {
			t.Fatalf("bad truncated output length=%d prefix=%q", len(output), output[:min(100, len(output))])
		}
		return Message{Content: "Handled the failure."}, nil
	})
	if err := NewEngine(provider, executor, "").Turn(context.Background(), "read", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestEngineBoundedRoundsAndPartialProviderFailure(t *testing.T) {
	executor := &engineTestExecutor{tools: []Tool{{Name: "read"}}}
	provider := engineTestProvider(func(context.Context, []Message, []Tool, func(string)) (Message, error) {
		return Message{ToolCalls: []ToolCall{{Name: "read", Arguments: json.RawMessage(`{}`)}}}, nil
	})
	engine := NewEngine(provider, executor, "")
	engine.maxRounds = 2
	if err := engine.Turn(context.Background(), "loop", nil, nil); err == nil || !strings.Contains(err.Error(), "2 model rounds") {
		t.Fatalf("round-limit error = %v", err)
	}
	if len(executor.calls) != 2 || engine.Messages()[4].Role != "tool" {
		t.Fatalf("round-limit left incomplete calls: %+v", engine.Messages())
	}
	engine.provider = engineTestProvider(func(_ context.Context, _ []Message, _ []Tool, emit func(string)) (Message, error) {
		emit("Partial thought")
		return Message{ToolCalls: []ToolCall{{Name: "read"}}}, errors.New("stream interrupted")
	})
	if err := engine.Turn(context.Background(), "continue", nil, nil); err == nil {
		t.Fatal("expected provider failure")
	}
	last := engine.Messages()[len(engine.Messages())-1]
	if last.Content != "Partial thought" || len(last.ToolCalls) != 0 {
		t.Fatalf("partial model response = %+v", last)
	}
}
