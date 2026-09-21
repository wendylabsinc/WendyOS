package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestProvider(t *testing.T, provider string, handler http.HandlerFunc) Provider {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	p, err := NewProvider(Config{Provider: provider, Model: "test-local-model", BaseURL: server.URL + "/v1"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOpenAICompatibleStreamingToolsAndHistory(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("OPENAI_API_KEY", "must-not-reach-local-server")
	var requests atomic.Int32
	p := newTestProvider(t, "local", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("x-api-key") != "" {
			t.Error("cloud credentials reached local endpoint")
		}
		var body struct {
			Model     string           `json:"model"`
			Stream    bool             `json:"stream"`
			MaxTokens int              `json:"max_tokens"`
			Messages  []openAIMessage  `json:"messages"`
			Tools     []openAIToolCall `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != "test-local-model" || !body.Stream || body.MaxTokens != 4096 || len(body.Tools) != 1 || string(body.Tools[0].Function.Parameters) != `{"type":"object"}` {
			t.Errorf("invalid local request: %+v", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if requests.Add(1) == 2 {
			if len(body.Messages) != 5 || len(body.Messages[2].ToolCalls) != 2 || body.Messages[3].ToolCallID != "first" || body.Messages[4].Content != "second result" {
				t.Errorf("history lost tool calls or results: %+v", body.Messages)
			}
			io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Finished\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		io.WriteString(w, `: keep-alive

data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"Checking "}}]}

data: {"choices":[{"index":0,"delta":{"content":"hardware","tool_calls":[{"index":1,"id":"second","type":"function","function":{"name":"device_list","arguments":"{"}},{"index":0,"id":"first","type":"function","function":{"name":"wendy_status","arguments":"{\"verbose\":"}}]}}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"true}"}},{"index":1,"function":{"arguments":"}"}}]}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`)
	})
	history := []Message{{Role: "system", Content: "Wendy agent"}, {Role: "user", Content: "Check hardware"}}
	tools := []Tool{{Name: "wendy_status", Description: "Connection state", Parameters: json.RawMessage(`{"type":"object"}`)}}
	var emitted strings.Builder
	message, err := p.Stream(context.Background(), history, tools, func(s string) { emitted.WriteString(s) })
	if err != nil {
		t.Fatal(err)
	}
	if message.Content != "Checking hardware" || emitted.String() != message.Content || len(message.ToolCalls) != 2 {
		t.Fatalf("unexpected streamed result: %+v, text=%q", message, emitted.String())
	}
	if message.ToolCalls[0].ID != "first" || message.ToolCalls[0].Name != "wendy_status" || string(message.ToolCalls[0].Arguments) != `{"verbose":true}` || string(message.ToolCalls[1].Arguments) != "{}" {
		t.Fatalf("indexed tool arguments were not reassembled: %+v", message.ToolCalls)
	}
	history = append(history, message, Message{Role: "tool", ToolCallID: "first", Content: "first result"}, Message{Role: "tool", ToolCallID: "second", Content: "second result"})
	message, err = p.Stream(context.Background(), history, tools, nil)
	if err != nil || message.Content != "Finished" {
		t.Fatalf("continuation failed: %+v, %v", message, err)
	}
}

func TestAnthropicStreamingAndParallelResults(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("WENDY_CHAT_API_KEY", "test-anthropic-key")
	p := newTestProvider(t, "anthropic", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "test-anthropic-key" || r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Error("incorrect Anthropic path or headers")
		}
		var body struct {
			System   string             `json:"system"`
			Messages []anthropicMessage `json:"messages"`
			Tools    []struct {
				Name   string          `json:"name"`
				Schema json.RawMessage `json:"input_schema"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.System != "Wendy system" || len(body.Messages) != 3 || len(body.Messages[2].Content) != 2 || body.Messages[2].Role != "user" || body.Messages[2].Content[1].ToolUseID != "second" {
			t.Errorf("invalid Anthropic history: %+v", body)
		}
		if len(body.Tools) != 1 || body.Tools[0].Name != "device_connect" || string(body.Tools[0].Schema) != `{"type":"object"}` {
			t.Errorf("tool schema missing: %+v", body.Tools)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `event: message_start
data: {"type":"message_start","message":{"role":"assistant","content":[]}}

event: ping
data: {"type":"ping"}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Connecting"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"connect","name":"device_connect","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"name\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"wendy\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}

event: message_stop
data: {"type":"message_stop"}

`)
	})
	var text strings.Builder
	message, err := p.Stream(context.Background(), []Message{
		{Role: "system", Content: "Wendy system"},
		{Role: "user", Content: "Connect"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "first", Name: "wendy_status", Arguments: json.RawMessage(`{}`)}, {ID: "second", Name: "device_list", Arguments: json.RawMessage(`{}`)}}},
		{Role: "tool", ToolCallID: "first", Content: "Disconnected"},
		{Role: "tool", ToolCallID: "second", Content: "wendy"},
	}, []Tool{{Name: "device_connect", Parameters: json.RawMessage(`{"type":"object"}`)}}, func(s string) { text.WriteString(s) })
	if err != nil {
		t.Fatal(err)
	}
	if message.Content != "Connecting" || message.Content != text.String() || len(message.ToolCalls) != 1 || string(message.ToolCalls[0].Arguments) != `{"name":"wendy"}` || message.ToolCalls[0].ID != "connect" {
		t.Fatalf("invalid Anthropic response: %+v", message)
	}
}

func TestProviderRejectsIncompleteAndInvalidStreams(t *testing.T) {
	clearProviderEnv(t)
	for _, test := range []struct{ name, provider, stream, want string }{
		{"openai truncated", "local", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n", "unexpectedly"},
		{"openai malformed", "local", "data: {not-json}\n\n", "invalid JSON"},
		{"openai error", "local", "data: {\"error\":{\"message\":\"model does not support tools\"}}\n\n", "does not support tools"},
		{"openai limit", "local", "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\n", "--max-tokens"},
		{"openai done without finish", "local", "data: [DONE]\n\n", "before a finish reason"},
		{"openai invalid arguments", "local", `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"run","arguments":"{\"broken\":"}}]},"finish_reason":"tool_calls"}]}` + "\n\ndata: [DONE]\n\n", "invalid JSON arguments"},
		{"openai invalid index", "local", `data: {"choices":[{"delta":{"tool_calls":[{"index":-1,"id":"a","function":{"name":"run","arguments":"{}"}}]}}]}` + "\n\n", "invalid tool call index"},
		{"anthropic truncated", "anthropic", "data: {\"type\":\"message_start\"}\n\n", "unexpectedly"},
		{"anthropic error", "anthropic", "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"Overloaded\"}}\n\n", "Overloaded"},
		{"anthropic unfinished block", "anthropic", "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n", "before content was complete"},
		{"anthropic limit", "anthropic", "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n", "--max-tokens"},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newTestProvider(t, test.provider, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, test.stream)
			})
			message, err := p.Stream(context.Background(), []Message{{Role: "user", Content: "test"}}, nil, nil)
			if err == nil || !strings.Contains(err.Error(), test.want) || len(message.ToolCalls) != 0 {
				t.Fatalf("Stream = %+v, %v; want %s without executable calls", message, err, test.want)
			}
		})
	}
}

func TestProviderHTTPAndSSEErrorsRedactKeys(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("WENDY_CHAT_API_KEY", "test-secret-key")
	for _, test := range []struct {
		name, contentType, body string
		status                  int
	}{
		{"http", "application/json", `{"error":{"message":"Invalid test-secret-key"}}`, http.StatusUnauthorized},
		{"stream", "text/event-stream", "data: {\"error\":{\"message\":\"Invalid test-secret-key\"}}\n\n", http.StatusOK},
		{"nonstream", "application/json", `{"error":"Invalid test-secret-key"}`, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newTestProvider(t, "local", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				w.WriteHeader(test.status)
				io.WriteString(w, test.body)
			})
			_, err := p.Stream(context.Background(), nil, nil, nil)
			if err == nil || strings.Contains(err.Error(), "test-secret-key") || !strings.Contains(err.Error(), "[redacted]") {
				t.Fatalf("error did not redact credential: %v", err)
			}
		})
	}
}

func TestProviderCancellation(t *testing.T) {
	clearProviderEnv(t)
	for _, provider := range []string{"local", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			started := make(chan struct{})
			p := newTestProvider(t, provider, func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				close(started)
				<-r.Context().Done()
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := make(chan error, 1)
			go func() {
				_, err := p.Stream(ctx, []Message{{Role: "user", Content: "test"}}, nil, nil)
				finished <- err
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("provider did not start")
			}
			cancel()
			select {
			case err := <-finished:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Stream returned %v after cancellation", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("stream did not honor cancellation")
			}
		})
	}
}

func TestProviderDoesNotForwardCredentialsOnRedirect(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("WENDY_CHAT_API_KEY", "test-key")
	var redirected atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Store(true)
	}))
	defer target.Close()
	p := newTestProvider(t, "anthropic", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	_, err := p.Stream(context.Background(), nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "307") || redirected.Load() {
		t.Fatalf("redirect followed or not reported: %v (followed=%v)", err, redirected.Load())
	}
}

func TestCompatibleSSEMultilineAndFinishWithoutDone(t *testing.T) {
	clearProviderEnv(t)
	p := newTestProvider(t, "ollama", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "\ufeffdata: {\"choices\":[{\"index\":0,\r\ndata: \"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}")
	})
	message, err := p.Stream(context.Background(), nil, nil, nil)
	if err != nil || message.Content != "ok" {
		t.Fatalf("compatible event stream = %+v, %v", message, err)
	}
}

type providerRoundTripFunc func(*http.Request) (*http.Response, error)

func (f providerRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestProviderTransportErrorsRedactKeys(t *testing.T) {
	clearProviderEnv(t)
	p, err := NewProvider(Config{Provider: "local", Model: "test", APIKey: "test-transport-secret"})
	if err != nil {
		t.Fatal(err)
	}
	p.(*httpProvider).client.Transport = providerRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("transport rejected header %s", r.Header.Get("Authorization"))
	})
	_, err = p.Stream(context.Background(), nil, nil, nil)
	if err == nil || strings.Contains(err.Error(), "test-transport-secret") || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("transport error leaked key: %v", err)
	}
}

func TestOpenAIUsesResponsesAPI(t *testing.T) {
	clearProviderEnv(t)
	p, err := NewProvider(Config{Provider: "openai", Model: "test-model", APIKey: "fake-key", MaxTokens: 512})
	if err != nil {
		t.Fatal(err)
	}
	// Intercept the official endpoint completely; this test makes no API call.
	p.(*httpProvider).client.Transport = providerRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if r.URL.Path != "/v1/responses" || body["max_output_tokens"] != float64(512) || body["store"] != false || body["max_completion_tokens"] != nil || body["max_tokens"] != nil || r.Header.Get("Authorization") != "Bearer fake-key" {
			t.Errorf("incorrect OpenAI request: %+v", body)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n"))}, nil
	})
	message, err := p.Stream(context.Background(), nil, nil, nil)
	if err != nil || message.Content != "ok" {
		t.Fatalf("intercepted OpenAI stream failed: %v", err)
	}
}

func TestLocalToolCallsMayOmitIDs(t *testing.T) {
	clearProviderEnv(t)
	p := newTestProvider(t, "local", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"wendy_status\",\"arguments\":\"{}\"}},{\"index\":1,\"function\":{\"name\":\"device_list\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
	})
	message, err := p.Stream(context.Background(), nil, nil, nil)
	if err != nil || len(message.ToolCalls) != 2 {
		t.Fatalf("local calls without IDs were rejected: %+v, %v", message, err)
	}
	if message.ToolCalls[0].ID == "" || message.ToolCalls[1].ID == "" || message.ToolCalls[0].ID == message.ToolCalls[1].ID {
		t.Fatalf("missing IDs were not assigned unique identities: %+v", message.ToolCalls)
	}
}
