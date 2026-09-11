package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

type openAIFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Arguments   string          `json:"arguments,omitempty"`
}

type openAIToolCall struct {
	Index    int            `json:"index,omitempty"`
	ID       string         `json:"id,omitempty"`
	Type     string         `json:"type,omitempty"`
	Function openAIFunction `json:"function"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

func (p *httpProvider) streamOpenAI(ctx context.Context, messages []Message, tools []Tool, emit func(string)) (Message, error) {
	wireMessages := make([]openAIMessage, 0, len(messages))
	for _, message := range messages {
		m := openAIMessage{Role: message.Role, Content: message.Content, ToolCallID: message.ToolCallID}
		for _, call := range message.ToolCalls {
			m.ToolCalls = append(m.ToolCalls, openAIToolCall{ID: call.ID, Type: "function", Function: openAIFunction{
				Name: call.Name, Arguments: string(call.Arguments),
			}})
		}
		wireMessages = append(wireMessages, m)
	}
	wireTools := make([]openAIToolCall, 0, len(tools))
	for _, tool := range tools {
		wireTools = append(wireTools, openAIToolCall{Type: "function", Function: openAIFunction{
			Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters,
		}})
	}
	payload := map[string]any{"model": p.config.Model, "messages": wireMessages, "stream": true}
	if len(wireTools) > 0 {
		payload["tools"] = wireTools
	}
	// Most compatible local servers use max_tokens; OpenAI's reasoning models
	// require the newer max_completion_tokens parameter instead.
	u, _ := url.Parse(p.config.BaseURL)
	if strings.EqualFold(u.Hostname(), "api.openai.com") {
		payload["max_completion_tokens"] = p.config.MaxTokens
	} else {
		payload["max_tokens"] = p.config.MaxTokens
	}
	resp, err := p.post(ctx, "/chat/completions", payload)
	if err != nil {
		return Message{}, err
	}
	defer resp.Body.Close()
	result := Message{Role: "assistant"}
	var content strings.Builder
	calls := make(map[int]*ToolCall)
	finished := false
	err = readEvents(ctx, resp.Body, func(event string, data []byte) error {
		if string(data) == "[DONE]" {
			if !finished {
				return fmt.Errorf("chat stream ended before a finish reason; no tool calls were executed")
			}
			return errStreamComplete
		}
		var chunk struct {
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Content   string           `json:"content"`
					Refusal   string           `json:"refusal"`
					ToolCalls []openAIToolCall `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(data, &chunk); err != nil {
			return fmt.Errorf("invalid JSON in chat event stream")
		}
		if event == "error" || (len(chunk.Error) > 0 && string(chunk.Error) != "null") {
			return fmt.Errorf("%s stream error: %s", p.config.Provider, p.errorDetail(data))
		}
		for _, choice := range chunk.Choices {
			if choice.Index != 0 {
				continue
			}
			text := choice.Delta.Content + choice.Delta.Refusal
			content.WriteString(text)
			if text != "" {
				emit(text)
			}
			for _, delta := range choice.Delta.ToolCalls {
				if delta.Index < 0 || delta.Index >= 128 {
					return fmt.Errorf("model returned an invalid tool call index")
				}
				call := calls[delta.Index]
				if call == nil {
					call = &ToolCall{}
					calls[delta.Index] = call
				}
				call.ID += delta.ID
				call.Name += delta.Function.Name
				call.Arguments = append(call.Arguments, delta.Function.Arguments...)
			}
			switch choice.FinishReason {
			case "":
			case "stop", "tool_calls":
				finished = true
			case "length":
				return fmt.Errorf("model reached its output token limit; increase --max-tokens and retry (no tool calls were executed)")
			case "content_filter":
				return fmt.Errorf("model response was stopped by the provider's content filter")
			default:
				return fmt.Errorf("model stopped with unsupported finish reason %q", p.safeError(choice.FinishReason))
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStreamComplete) {
		return Message{}, err
	}
	// Some compatible servers close after the finish_reason without [DONE].
	// A response cut off before either terminal marker is never executable.
	if !finished {
		return Message{}, fmt.Errorf("chat stream ended unexpectedly before completion; no tool calls were executed")
	}
	result.Content = content.String()
	result.ToolCalls, err = assembledCalls(calls)
	return result, err
}
