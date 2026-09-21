package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Preserve the official hostname for adapter selection and redirect all actual
// network traffic to an httptest server. No provider credential or API is used.
func newResponsesTestProvider(t *testing.T, handler http.HandlerFunc) Provider {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewProvider(Config{Provider: "openai", Model: "gpt-6-astra", BaseURL: "https://api.openai.com/v1", APIKey: "test-responses-key", MaxTokens: 1024})
	if err != nil {
		t.Fatal(err)
	}
	p.(*httpProvider).client.Transport = providerRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.openai.com" || r.URL.Path != "/v1/responses" {
			t.Errorf("official OpenAI tools used incorrect endpoint: %s", r.URL)
		}
		local := r.Clone(r.Context())
		local.URL.Scheme, local.URL.Host = endpoint.Scheme, endpoint.Host
		local.Host = endpoint.Host
		return http.DefaultTransport.RoundTrip(local)
	})
	return p
}

func responseEvent(w io.Writer, value any) {
	data, _ := json.Marshal(value)
	fmt.Fprintf(w, "data: %s\n\n", data)
}

func responseTextItem(id, text string) map[string]any {
	return map[string]any{"id": id, "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
}

func TestOpenAIResponsesToolsAndReasoningReplay(t *testing.T) {
	clearProviderEnv(t)
	var requests atomic.Int32
	reasoning := map[string]any{"id": "rs_first", "type": "reasoning", "summary": []any{}, "encrypted_content": "opaque-first-reasoning"}
	statusCall := map[string]any{"id": "fc_status", "type": "function_call", "call_id": "call_status", "name": "wendy_status", "arguments": `{"verbose":true}`, "status": "completed"}
	listCall := map[string]any{"id": "fc_list", "type": "function_call", "call_id": "call_list", "name": "device_list", "arguments": `{}`, "status": "completed"}
	p := newResponsesTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model            string           `json:"model"`
			Stream           bool             `json:"stream"`
			Store            bool             `json:"store"`
			Include          []string         `json:"include"`
			MaxOutputTokens  int              `json:"max_output_tokens"`
			PreviousResponse string           `json:"previous_response_id"`
			Input            []map[string]any `json:"input"`
			Tools            []map[string]any `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != "gpt-6-astra" || !body.Stream || body.Store || body.PreviousResponse != "" || body.MaxOutputTokens != 1024 || len(body.Include) != 1 || body.Include[0] != "reasoning.encrypted_content" {
			t.Errorf("incorrect stateless Responses request: %+v", body)
		}
		if len(body.Tools) != 2 || body.Tools[0]["type"] != "function" || body.Tools[0]["name"] == nil || body.Tools[0]["function"] != nil || body.Tools[0]["strict"] != false {
			t.Errorf("tools were not translated to flat non-strict Responses definitions: %+v", body.Tools)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		switch requests.Add(1) {
		case 1:
			if len(body.Input) != 2 || body.Input[0]["role"] != "system" || body.Input[1]["role"] != "user" {
				t.Errorf("initial input lost system/user messages: %+v", body.Input)
			}
			responseEvent(w, map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "rs_first", "type": "reasoning", "summary": []any{}}})
			responseEvent(w, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": reasoning})
			responseEvent(w, map[string]any{"type": "response.output_text.delta", "output_index": 1, "delta": "Checking "})
			responseEvent(w, map[string]any{"type": "response.output_text.delta", "output_index": 1, "delta": "hardware. "})
			responseEvent(w, map[string]any{"type": "response.output_item.done", "output_index": 1, "item": responseTextItem("msg_first", "Checking hardware. ")})
			// Interleaved calls exercise output-index and item-ID association.
			responseEvent(w, map[string]any{"type": "response.output_item.added", "output_index": 3, "item": map[string]any{"type": "function_call", "id": "fc_list", "call_id": "call_list", "name": "device_list", "arguments": ""}})
			responseEvent(w, map[string]any{"type": "response.output_item.added", "output_index": 2, "item": map[string]any{"type": "function_call", "id": "fc_status", "call_id": "call_status", "name": "wendy_status", "arguments": ""}})
			responseEvent(w, map[string]any{"type": "response.function_call_arguments.delta", "output_index": 2, "item_id": "fc_status", "delta": `{"verbose":`})
			responseEvent(w, map[string]any{"type": "response.function_call_arguments.delta", "output_index": 3, "item_id": "fc_list", "delta": `{}`})
			responseEvent(w, map[string]any{"type": "response.function_call_arguments.delta", "output_index": 2, "item_id": "fc_status", "delta": `true}`})
			responseEvent(w, map[string]any{"type": "response.function_call_arguments.done", "output_index": 2, "item_id": "fc_status", "arguments": `{"verbose":true}`})
			responseEvent(w, map[string]any{"type": "response.output_item.done", "output_index": 2, "item": statusCall})
			responseEvent(w, map[string]any{"type": "response.function_call_arguments.done", "output_index": 3, "item_id": "fc_list", "arguments": `{}`})
			responseEvent(w, map[string]any{"type": "response.output_item.done", "output_index": 3, "item": listCall})
			responseEvent(w, map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{reasoning, responseTextItem("msg_first", "Checking hardware. "), statusCall, listCall}}})
		case 2:
			if len(body.Input) != 8 {
				t.Errorf("tool continuation has %d items, want 8", len(body.Input))
			} else if body.Input[2]["encrypted_content"] != "opaque-first-reasoning" || body.Input[4]["call_id"] != "call_status" || body.Input[5]["call_id"] != "call_list" || body.Input[6]["type"] != "function_call_output" || body.Input[6]["call_id"] != "call_status" || body.Input[7]["output"] != "result-device_list" {
				t.Errorf("reasoning/call IDs/results were not replayed in order: %+v", body.Input)
			}
			responseEvent(w, map[string]any{"type": "response.output_text.delta", "output_index": 0, "delta": "Ready."})
			responseEvent(w, map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{responseTextItem("msg_second", "Ready.")}}})
		case 3:
			if len(body.Input) != 10 || body.Input[2]["encrypted_content"] != "opaque-first-reasoning" || body.Input[8]["id"] != "msg_second" || body.Input[9]["content"] != "Continue" {
				t.Errorf("next user turn lost native history: %+v", body.Input)
			}
			responseEvent(w, map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{responseTextItem("msg_third", "Done.")}}})
		default:
			t.Error("unexpected extra request")
		}
	})
	executor := &engineTestExecutor{tools: []Tool{
		{Name: "wendy_status", Parameters: json.RawMessage(`{"type":"object","properties":{"verbose":{"type":"boolean"}},"additionalProperties":false}`)},
		{Name: "device_list", Parameters: json.RawMessage(`{"type":"object"}`)},
	}, run: func(_ context.Context, call ToolCall) (string, error) { return "result-" + call.Name, nil }}
	engine := NewEngine(p, executor, "Develop hardware")
	var emitted strings.Builder
	if err := engine.Turn(context.Background(), "Inspect hardware", func(event Event) {
		if event.Type == "text" {
			emitted.WriteString(event.Text)
		}
	}, nil); err != nil {
		t.Fatal(err)
	}
	if len(executor.calls) != 2 || executor.calls[0].ID != "call_status" || string(executor.calls[0].Arguments) != `{"verbose":true}` || executor.calls[1].Name != "device_list" || emitted.String() != "Checking hardware. Ready." {
		t.Fatalf("incorrect Responses tool loop: calls=%+v, text=%q", executor.calls, emitted.String())
	}
	if err := engine.Turn(context.Background(), "Continue", nil, nil); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 3 {
		t.Fatalf("request count = %d", requests.Load())
	}
}

func TestResponsesRejectIncompleteAndMalformedOutput(t *testing.T) {
	clearProviderEnv(t)
	for _, test := range []struct{ name, stream, want string }{
		{"truncated", `data: {"type":"response.output_text.delta","delta":"partial"}` + "\n\n", "unexpectedly"},
		{"invalid JSON", "data: broken\n\n", "invalid JSON"},
		{"token limit", `data: {"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}` + "\n\n", "--max-tokens"},
		{"incomplete status", `data: {"type":"response.completed","response":{"status":"incomplete","output":[]}}` + "\n\n", "without completed status"},
		{"stream error", `data: {"type":"error","message":"Invalid test-responses-key"}` + "\n\n", "[redacted]"},
		{"failed", `data: {"type":"response.failed","response":{"status":"failed","error":{"message":"Invalid test-responses-key"}}}` + "\n\n", "[redacted]"},
		{"unknown call delta", `data: {"type":"response.function_call_arguments.delta","output_index":2,"item_id":"fc_missing","delta":"{}"}` + "\n\n", "unknown or completed function call"},
		{"bad arguments", `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","name":"run","call_id":"a","arguments":"{broken"}]}}` + "\n\n", "invalid JSON arguments"},
		{"missing call ID", `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","name":"run","arguments":"{}"}]}}` + "\n\n", "incomplete or duplicate function call"},
		{"incomplete item", `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","status":"in_progress","name":"run","call_id":"a","arguments":"{}"}]}}` + "\n\n", "incomplete output item"},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newResponsesTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, test.stream)
			})
			message, err := p.Stream(context.Background(), []Message{{Role: "user", Content: "test"}}, nil, nil)
			if err == nil || !strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), "test-responses-key") || len(message.ToolCalls) != 0 {
				t.Fatalf("Stream = %+v, %v; want %s without secrets/executable calls", message, err, test.want)
			}
		})
	}
}

func TestResponsesCancellation(t *testing.T) {
	clearProviderEnv(t)
	started := make(chan struct{})
	p := newResponsesTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := p.Stream(ctx, nil, nil, nil); done <- err }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not stop")
	}
}

func TestCustomOpenAIEndpointKeepsChatCompletions(t *testing.T) {
	clearProviderEnv(t)
	p := newTestProvider(t, "openai", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("custom compatible endpoint changed protocol: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"local\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	})
	message, err := p.Stream(context.Background(), nil, nil, nil)
	if err != nil || message.Content != "local" {
		t.Fatalf("custom compatible model failed: %+v, %v", message, err)
	}
}

func TestResponsesReplayUsesNormalizedToolCallIDs(t *testing.T) {
	messages := []Message{{Role: "assistant", ToolCalls: []ToolCall{{ID: "normalized", Name: "run", Arguments: json.RawMessage(`{}`)}}, ResponseItems: []json.RawMessage{json.RawMessage(`{"type":"function_call","id":"fc_original","call_id":"duplicate","name":"run","arguments":"{}"}`)}}, {Role: "tool", ToolCallID: "normalized", Content: "denied"}}
	input, err := responsesInput(messages)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(input)
	if strings.Contains(string(encoded), "duplicate") || strings.Count(string(encoded), `"call_id":"normalized"`) != 2 {
		t.Fatalf("replayed calls do not match normalized outputs: %s", encoded)
	}
}
