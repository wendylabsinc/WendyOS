package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func headlessFixture(t *testing.T) (*Engine, *engineTestExecutor) {
	t.Helper()
	executor := &engineTestExecutor{tools: []Tool{
		{Name: "device_info"},
		{Name: "workspace_exec", RequiresApproval: true},
	}}
	round := 0
	provider := engineTestProvider(func(_ context.Context, messages []Message, _ []Tool, emit func(string)) (Message, error) {
		round++
		switch round {
		case 1:
			emit("Checking. ")
			return Message{Content: "Checking. ", ToolCalls: []ToolCall{
				{ID: "a", Name: "device_info", Arguments: json.RawMessage(`{}`)},
				{ID: "b", Name: "workspace_exec", Arguments: json.RawMessage(`{"command":"ls"}`)},
			}}, nil
		default:
			last := messages[len(messages)-1].Content
			emit("Done: " + last)
			return Message{Content: "Done: " + last}, nil
		}
	})
	return NewEngine(provider, executor, "system"), executor
}

func decodeLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line %q is not JSON: %v", line, err)
		}
		events = append(events, ev)
	}
	return events
}

func TestHeadlessJSONStreamsEventsAndDeniesWithoutYes(t *testing.T) {
	engine, executor := headlessFixture(t)
	out := new(bytes.Buffer)
	if err := RunHeadless(context.Background(), HeadlessOptions{Engine: engine, Prompt: "what is up", JSON: true, Output: out}); err != nil {
		t.Fatal(err)
	}
	events := decodeLines(t, out.String())
	var types []string
	for _, ev := range events {
		types = append(types, ev["type"].(string))
	}
	joined := strings.Join(types, " ")
	for _, want := range []string{"status", "text", "tool_start", "approval", "tool_result", "done"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q event in %s", want, joined)
		}
	}
	if types[len(types)-1] != "done" {
		t.Errorf("stream must end with done, got %s", joined)
	}
	if len(executor.calls) != 1 || executor.calls[0].Name != "device_info" {
		t.Fatalf("only the read tool may run without --yes, ran %+v", executor.calls)
	}
	for _, ev := range events {
		switch ev["type"] {
		case "approval":
			if ev["approved"] != false || ev["tool"] != "workspace_exec" {
				t.Errorf("approval event = %v", ev)
			}
		case "tool_start":
			if ev["tool"] == "workspace_exec" && ev["arguments"].(map[string]any)["command"] != "ls" {
				t.Errorf("tool_start must carry the arguments: %v", ev)
			}
		case "done":
			if !strings.HasPrefix(ev["text"].(string), "Checking. Done: User denied permission") {
				t.Errorf("done text = %q", ev["text"])
			}
		}
	}
}

func TestHeadlessYesRunsApprovedToolsAndPrintsPlainText(t *testing.T) {
	engine, executor := headlessFixture(t)
	out := new(bytes.Buffer)
	if err := RunHeadless(context.Background(), HeadlessOptions{Engine: engine, Prompt: "run it", Output: out, AutoApprove: true}); err != nil {
		t.Fatal(err)
	}
	if len(executor.calls) != 2 {
		t.Fatalf("--yes must run both tools, ran %+v", executor.calls)
	}
	if got := out.String(); got != "Checking. Done: ok\n" {
		t.Fatalf("plain output = %q", got)
	}
}

func TestHeadlessReportsErrorsAsEvents(t *testing.T) {
	provider := engineTestProvider(func(context.Context, []Message, []Tool, func(string)) (Message, error) {
		return Message{}, context.DeadlineExceeded
	})
	engine := NewEngine(provider, &engineTestExecutor{}, "")
	out := new(bytes.Buffer)
	err := RunHeadless(context.Background(), HeadlessOptions{Engine: engine, Prompt: "x", JSON: true, Output: out})
	if err == nil {
		t.Fatal("expected the provider error")
	}
	events := decodeLines(t, out.String())
	last := events[len(events)-1]
	if last["type"] != "error" || !strings.Contains(last["text"].(string), "deadline") {
		t.Fatalf("last event = %v", last)
	}
	if err := RunHeadless(context.Background(), HeadlessOptions{Engine: engine, Prompt: "  "}); err == nil {
		t.Fatal("empty prompt must be rejected")
	}
}
