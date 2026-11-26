package chat

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Ollama's OpenAI-compatible endpoint does not accept a per-request context
// size. Use its native endpoint for the full MCP catalog, without changing the
// server's settings or affecting other clients. Other local servers keep /v1.
type deviceProvider struct {
	fallback *httpProvider
	once     sync.Once
	ollama   bool
}

func (p *deviceProvider) Stream(ctx context.Context, messages []Message, tools []Tool, emit func(string)) (Message, error) {
	p.once.Do(func() {
		probe, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		var version struct {
			Version string `json:"version"`
		}
		p.ollama = discoveryJSON(probe, p.fallback, http.MethodGet, "http://wendy-device/api/version", nil, &version) == nil && version.Version != ""
	})
	// Native Ollama already separates message.thinking from message.content.
	// Treating all Qwen content as an implicit thinking preamble holds ordinary
	// answers until completion and makes a working local model look stalled.
	// Only the compatible endpoint needs the legacy template heuristic.
	filter := &answerStream{implicit: !p.ollama && strings.Contains(strings.ToLower(p.fallback.config.Model), "qwen3"), emit: emit}
	var result Message
	var err error
	if !p.ollama {
		result, err = p.fallback.Stream(ctx, messages, tools, filter.append)
	} else {
		result, err = p.stream(ctx, messages, tools, filter)
	}
	filter.finish(err == nil)
	result.Content = filter.visible.String()
	if err == nil && p.ollama && strings.TrimSpace(result.Content) == "" && len(result.ToolCalls) == 0 {
		err = fmt.Errorf("local model completed without an answer or tool call. %s", p.instructModelHint())
	}
	return result, err
}
func (p *deviceProvider) stream(ctx context.Context, messages []Message, tools []Tool, filter *answerStream) (Message, error) {
	type function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	type call struct {
		Function function `json:"function"`
	}
	type message struct {
		Role      string   `json:"role"`
		Content   string   `json:"content"`
		ToolName  string   `json:"tool_name,omitempty"`
		ToolCalls []call   `json:"tool_calls,omitempty"`
		Images    []string `json:"images,omitempty"`
	}
	wire := make([]message, 0, len(messages))
	names := map[string]string{}
	for _, m := range messages {
		v := message{Role: m.Role, Content: m.Content}
		for _, c := range m.ToolCalls {
			v.ToolCalls = append(v.ToolCalls, call{Function: function{Name: c.Name, Arguments: c.Arguments}})
			names[c.ID] = c.Name
		}
		if m.Role == "tool" {
			v.ToolName = names[m.ToolCallID]
		}
		for _, image := range m.Images {
			data, err := ollamaSceneImage(ctx, image)
			if err != nil {
				return Message{}, fmt.Errorf("preparing local chat image: %w", err)
			}
			v.Images = append(v.Images, data)
		}
		wire = append(wire, v)
	}
	wireTools := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		wireTools = append(wireTools, map[string]any{"type": "function", "function": map[string]any{"name": tool.Name, "description": tool.Description, "parameters": tool.Parameters}})
	}
	payload := map[string]any{"model": p.fallback.config.Model, "messages": wire, "stream": true, "tools": wireTools, "options": map[string]any{"num_ctx": 32768, "num_predict": p.fallback.config.MaxTokens}, "keep_alive": "1h"}
	// Suppress optional thinking output only for models advertising that feature.
	var details struct {
		Capabilities []string `json:"capabilities"`
	}
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	if discoveryJSON(probe, p.fallback, http.MethodPost, "http://wendy-device/api/show", map[string]string{"model": p.fallback.config.Model}, &details) == nil {
		hasTools := false
		for _, c := range details.Capabilities {
			if c == "thinking" {
				payload["think"] = false
			}
			if c == "tools" {
				hasTools = true
			}
		}
		if details.Capabilities != nil && len(tools) > 0 && !hasTools {
			cancel()
			return Message{}, errors.New("this model does not support function tools; choose a tool-capable model such as Qwen")
		}
	}
	cancel()
	response, err := discoveryRequest(ctx, p.fallback, http.MethodPost, "http://wendy-device/api/chat", payload)
	if err != nil {
		return Message{}, err
	}
	defer response.Body.Close()
	scanner := bufio.NewScanner(io.LimitReader(response.Body, 8<<20))
	scanner.Buffer(make([]byte, 4096), 2<<20)
	result := Message{Role: "assistant"}
	var text strings.Builder
	for scanner.Scan() {
		var chunk struct {
			// Deliberately do not decode or emit message.thinking: it is a
			// separate private channel, never an answer or executable tool call.
			Message    message `json:"message"`
			Done       bool    `json:"done"`
			DoneReason string  `json:"done_reason"`
			Failure    string  `json:"error"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &chunk); err != nil {
			return result, fmt.Errorf("reading model response: %w", err)
		}
		if chunk.Failure != "" {
			return result, errors.New(p.fallback.safeError(chunk.Failure))
		}
		if chunk.Message.Content != "" {
			text.WriteString(chunk.Message.Content)
			filter.append(chunk.Message.Content)
		}
		for _, c := range chunk.Message.ToolCalls {
			result.ToolCalls = append(result.ToolCalls, ToolCall{Name: c.Function.Name, Arguments: c.Function.Arguments})
		}
		if chunk.Done {
			result.Content = text.String()
			if chunk.DoneReason == "length" {
				return result, fmt.Errorf("local model reached its %d-token generation limit before completing; partial tool calls were not executed. %s", p.fallback.config.MaxTokens, p.instructModelHint())
			}
			return result, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return result, err
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, errors.New("model stream ended without a completion acknowledgement")
}

func (p *deviceProvider) instructModelHint() string {
	if strings.Contains(strings.ToLower(p.fallback.config.Model), "qwen3-vl") {
		return "Use an explicit instruct variant such as qwen3-vl:4b-instruct; this runtime may still generate only thinking with think:false."
	}
	return "Use a tool-capable instruct model if the runtime spends the generation budget on thinking."
}
