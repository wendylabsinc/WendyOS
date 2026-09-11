package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// responsesInput replays native items in their original order, including opaque
// encrypted reasoning. With store:false, referring to previous server-side
// response IDs is insufficient to retain reasoning across tool calls.
func responsesInput(messages []Message) ([]any, error) {
	input := make([]any, 0, len(messages))
	for _, message := range messages {
		if message.Role == "tool" {
			input = append(input, map[string]any{"type": "function_call_output", "call_id": message.ToolCallID, "output": message.Content})
			continue
		}
		if message.Role == "assistant" && len(message.ResponseItems) > 0 {
			callIndex := 0
			for _, raw := range message.ResponseItems {
				var item map[string]json.RawMessage
				if err := json.Unmarshal(raw, &item); err != nil {
					return nil, fmt.Errorf("invalid saved OpenAI response item")
				}
				var kind string
				_ = json.Unmarshal(item["type"], &kind)
				if kind == "function_call" {
					if callIndex >= len(message.ToolCalls) {
						return nil, fmt.Errorf("saved OpenAI response has an unmatched function call")
					}
					// The engine may normalize duplicate IDs or rejected arguments.
					// Keep the native replay consistent with the ensuing tool result.
					call := message.ToolCalls[callIndex]
					item["call_id"], _ = json.Marshal(call.ID)
					item["name"], _ = json.Marshal(call.Name)
					item["arguments"], _ = json.Marshal(string(call.Arguments))
					input = append(input, item)
					callIndex++
				} else {
					input = append(input, raw)
				}
			}
			if callIndex != len(message.ToolCalls) {
				return nil, fmt.Errorf("saved OpenAI response is missing a function call")
			}
			continue
		}
		if message.Content != "" || message.Role != "assistant" {
			input = append(input, map[string]any{"role": message.Role, "content": message.Content})
		}
		for _, call := range message.ToolCalls {
			input = append(input, map[string]any{
				"type": "function_call", "call_id": call.ID,
				"name": call.Name, "arguments": string(call.Arguments),
			})
		}
	}
	return input, nil
}

type responsesOutputItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Status    string `json:"status"`
	Content   []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
}

type responsesStreamItem struct {
	item responsesOutputItem
	raw  json.RawMessage
	done bool
}

func (p *httpProvider) streamResponses(ctx context.Context, messages []Message, tools []Tool, emit func(string)) (Message, error) {
	input, err := responsesInput(messages)
	if err != nil {
		return Message{}, err
	}
	wireTools := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		parameters := tool.Parameters
		if len(parameters) == 0 {
			parameters = json.RawMessage(`{"type":"object"}`)
		}
		wireTools = append(wireTools, map[string]any{
			"type": "function", "name": tool.Name, "description": tool.Description,
			"parameters": parameters,
			// MCP schemas contain optional fields and are validated by the engine.
			// Responses otherwise normalizes omitted strict into strict mode.
			"strict": false,
		})
	}
	payload := map[string]any{
		"model": p.config.Model, "input": input, "stream": true,
		"store": false, "include": []string{"reasoning.encrypted_content"},
		"max_output_tokens": p.config.MaxTokens,
	}
	if len(wireTools) > 0 {
		payload["tools"] = wireTools
	}
	resp, err := p.post(ctx, "/responses", payload)
	if err != nil {
		return Message{}, err
	}
	defer resp.Body.Close()
	var streamed strings.Builder
	items := make(map[int]*responsesStreamItem)
	var output []json.RawMessage
	completed := false
	err = readEvents(ctx, resp.Body, func(event string, data []byte) error {
		var chunk struct {
			Type        string          `json:"type"`
			OutputIndex int             `json:"output_index"`
			ItemID      string          `json:"item_id"`
			Item        json.RawMessage `json:"item"`
			Delta       string          `json:"delta"`
			Arguments   string          `json:"arguments"`
			Message     string          `json:"message"`
			Response    struct {
				Status            string            `json:"status"`
				Output            []json.RawMessage `json:"output"`
				Error             json.RawMessage   `json:"error"`
				IncompleteDetails struct {
					Reason string `json:"reason"`
				} `json:"incomplete_details"`
			} `json:"response"`
		}
		if err := json.Unmarshal(data, &chunk); err != nil {
			return fmt.Errorf("invalid JSON in OpenAI Responses event stream")
		}
		if chunk.Type == "" {
			chunk.Type = event
		}
		switch chunk.Type {
		case "error":
			return fmt.Errorf("OpenAI Responses stream error: %s", p.safeError(firstValue(chunk.Message, p.errorDetail(data))))
		case "response.failed":
			failure, _ := json.Marshal(map[string]any{"error": chunk.Response.Error})
			return fmt.Errorf("OpenAI response failed: %s", p.errorDetail(failure))
		case "response.incomplete":
			if chunk.Response.IncompleteDetails.Reason == "max_output_tokens" {
				return fmt.Errorf("model reached its output token limit; increase --max-tokens and retry (no tool calls were executed)")
			}
			return fmt.Errorf("OpenAI response was incomplete: %s; no tool calls were executed", p.safeError(chunk.Response.IncompleteDetails.Reason))
		case "response.output_text.delta", "response.refusal.delta":
			streamed.WriteString(chunk.Delta)
			if chunk.Delta != "" {
				emit(chunk.Delta)
			}
		case "response.output_item.added", "response.output_item.done":
			if chunk.OutputIndex < 0 || chunk.OutputIndex >= 1024 {
				return fmt.Errorf("OpenAI returned an invalid response output index")
			}
			var item responsesOutputItem
			if err := json.Unmarshal(chunk.Item, &item); err != nil || item.Type == "" {
				return fmt.Errorf("OpenAI returned an invalid response output item")
			}
			items[chunk.OutputIndex] = &responsesStreamItem{item: item, raw: append(json.RawMessage(nil), chunk.Item...), done: chunk.Type == "response.output_item.done"}
		case "response.function_call_arguments.delta", "response.function_call_arguments.done":
			item := items[chunk.OutputIndex]
			if item == nil || item.item.Type != "function_call" || (chunk.ItemID != "" && item.item.ID != chunk.ItemID) || item.done {
				return fmt.Errorf("OpenAI returned arguments for an unknown or completed function call")
			}
			if chunk.Type == "response.function_call_arguments.delta" {
				item.item.Arguments += chunk.Delta
			} else {
				item.item.Arguments = chunk.Arguments
			}
		case "response.completed":
			if chunk.Response.Status != "completed" {
				return fmt.Errorf("OpenAI response ended without completed status; no tool calls were executed")
			}
			output = chunk.Response.Output
			if output == nil {
				// Complete output_item.done events also suffice when a compatible
				// stream omits the duplicate output snapshot in response.completed.
				indices := make([]int, 0, len(items))
				for index, item := range items {
					if !item.done {
						return fmt.Errorf("OpenAI response ended with an incomplete output item")
					}
					indices = append(indices, index)
				}
				sort.Ints(indices)
				for _, index := range indices {
					output = append(output, items[index].raw)
				}
			}
			completed = true
			return errStreamComplete
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStreamComplete) {
		return Message{}, err
	}
	if !completed {
		return Message{}, fmt.Errorf("OpenAI Responses stream ended unexpectedly before response.completed; no tool calls were executed")
	}
	result, err := completedResponse(output)
	if err != nil {
		return Message{}, err
	}
	// A completed snapshot can include text omitted from incremental events.
	if !strings.HasPrefix(result.Content, streamed.String()) {
		return Message{}, fmt.Errorf("OpenAI response text did not match its completed output")
	}
	if tail := strings.TrimPrefix(result.Content, streamed.String()); tail != "" {
		emit(tail)
	}
	return result, nil
}

func completedResponse(output []json.RawMessage) (Message, error) {
	result := Message{Role: "assistant"}
	var content strings.Builder
	seen := make(map[string]bool)
	for _, raw := range output {
		var item responsesOutputItem
		if err := json.Unmarshal(raw, &item); err != nil || item.Type == "" {
			return Message{}, fmt.Errorf("OpenAI returned an invalid completed output item")
		}
		if item.Status != "" && item.Status != "completed" {
			return Message{}, fmt.Errorf("OpenAI response contains an incomplete output item")
		}
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" {
					content.WriteString(part.Text)
				} else if part.Type == "refusal" {
					content.WriteString(part.Refusal)
				}
			}
		case "function_call":
			if item.CallID == "" || item.Name == "" || seen[item.CallID] || len(result.ToolCalls) >= 128 {
				return Message{}, fmt.Errorf("OpenAI returned an incomplete or duplicate function call")
			}
			var arguments map[string]any
			if json.Unmarshal([]byte(item.Arguments), &arguments) != nil || arguments == nil {
				return Message{}, fmt.Errorf("OpenAI returned invalid JSON arguments for a function call")
			}
			seen[item.CallID] = true
			result.ToolCalls = append(result.ToolCalls, ToolCall{ID: item.CallID, Name: item.Name, Arguments: json.RawMessage(item.Arguments)})
		}
		result.ResponseItems = append(result.ResponseItems, append(json.RawMessage(nil), raw...))
	}
	result.Content = content.String()
	return result, nil
}
