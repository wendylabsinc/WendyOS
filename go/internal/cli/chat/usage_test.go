package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestUsageOpenAICompatibleIncludesCacheAndReasoning(t *testing.T) {
	clearProviderEnv(t)
	p := newTestProvider(t, "local", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		if string(body["stream_options"]) != `{"include_usage":true}` {
			t.Error("usage was not requested")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20,\"prompt_tokens_details\":{\"cached_tokens\":80},\"completion_tokens_details\":{\"reasoning_tokens\":10}}}\n\ndata: [DONE]\n\n")
	})
	message, err := p.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	u := message.Usage
	if u == nil || !u.Complete || u.InputTokens != 100 || u.CachedInputTokens != 80 || u.OutputTokens != 20 || u.ReasoningTokens != 10 {
		t.Fatalf("usage: %+v", u)
	}
}

func TestUsageAnthropicCumulativeOutputAndDisjointCache(t *testing.T) {
	clearProviderEnv(t)
	p := newTestProvider(t, "anthropic", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":80,"cache_creation_input_tokens":20}}}

data: {"type":"message_delta","delta":{},"usage":{"output_tokens":5}}

data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":8}}

data: {"type":"message_stop"}

`)
	})
	message, err := p.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	u := message.Usage
	if u == nil || !u.Complete || u.InputTokens != 110 || u.CachedInputTokens != 80 || u.CacheWriteTokens != 20 || u.OutputTokens != 8 {
		t.Fatalf("usage: %+v", u)
	}
}

func TestUsageResponses(t *testing.T) {
	clearProviderEnv(t)
	p := newResponsesTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		responseEvent(w, map[string]any{"type": "response.completed", "response": map[string]any{
			"status": "completed", "output": []any{}, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}})
	})
	message, err := p.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err != nil || message.Usage == nil || !message.Usage.Complete {
		t.Fatalf("zero usage lost: %+v, %v", message.Usage, err)
	}
}

func TestUsageAnthropicRequiresFinalOutputCount(t *testing.T) {
	clearProviderEnv(t)
	p := newTestProvider(t, "anthropic", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
	})
	message, err := p.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if message.Usage == nil || message.Usage.Complete {
		t.Fatalf("initial usage mistaken for final usage: %+v", message.Usage)
	}
}

func TestUsageMissingAndInterruptedAreIncomplete(t *testing.T) {
	for _, stream := range []string{
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
		"data: {\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":3}}\n\n",
	} {
		t.Run(stream, func(t *testing.T) {
			clearProviderEnv(t)
			p := newTestProvider(t, "local", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, stream)
			})
			message, _ := p.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil)
			if message.Usage == nil || message.Usage.Complete {
				t.Fatalf("unreported usage presented as complete: %+v", message.Usage)
			}
		})
	}
}

func TestHeadlessUsagePerRound(t *testing.T) {
	round := 0
	provider := engineTestProvider(func(context.Context, []Message, []Tool, func(string)) (Message, error) {
		round++
		m := Message{Usage: &TokenUsage{Model: "test", InputTokens: int64(round * 100), OutputTokens: 10, Complete: true}}
		if round == 1 {
			m.ToolCalls = []ToolCall{{ID: "a", Name: "device_info", Arguments: json.RawMessage(`{}`)}}
		}
		return m, nil
	})
	e := NewEngine(provider, &engineTestExecutor{tools: []Tool{{Name: "device_info"}}}, "system")
	out := new(bytes.Buffer)
	if err := RunHeadless(context.Background(), HeadlessOptions{Engine: e, Prompt: "inspect", JSON: true, Output: out}); err != nil {
		t.Fatal(err)
	}
	var total float64
	for _, event := range decodeLines(t, out.String()) {
		if event["type"] == "usage" {
			total += event["usage"].(map[string]any)["input_tokens"].(float64)
		}
	}
	if total != 300 {
		t.Fatalf("per-request usage missing or duplicated: %v", total)
	}
}
