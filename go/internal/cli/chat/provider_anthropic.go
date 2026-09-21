package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   any             `json:"content,omitempty"`
	Source    *anthropicImage `json:"source,omitempty"`
}

type anthropicImage struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

func anthropicContent(message Message) []anthropicBlock {
	blocks := make([]anthropicBlock, 0, 1+len(message.Images))
	if message.Content != "" {
		blocks = append(blocks, anthropicBlock{Type: "text", Text: message.Content})
	}
	for _, image := range message.Images {
		blocks = append(blocks, anthropicBlock{Type: "image", Source: &anthropicImage{Type: "base64", MediaType: image.MIMEType, Data: image.Data}})
	}
	return blocks
}

func (p *httpProvider) streamAnthropic(ctx context.Context, messages []Message, tools []Tool, emit func(string)) (reply Message, err error) {
	if err := validateImageRoles(messages); err != nil {
		return Message{}, err
	}
	defer func() { err = imageRequestError(messages, err) }()
	var system []string
	wireMessages := make([]anthropicMessage, 0, len(messages))
	for _, message := range messages {
		if message.Role == "system" {
			system = append(system, message.Content)
			continue
		}
		m := anthropicMessage{Role: message.Role}
		if message.Role == "tool" {
			m.Role = "user"
			var content any = message.Content
			if len(message.Images) > 0 {
				content = anthropicContent(message)
			}
			m.Content = []anthropicBlock{{Type: "tool_result", ToolUseID: message.ToolCallID, Content: content}}
		} else {
			m.Content = anthropicContent(message)
			for _, call := range message.ToolCalls {
				m.Content = append(m.Content, anthropicBlock{Type: "tool_use", ID: call.ID, Name: call.Name, Input: call.Arguments})
			}
		}
		// Parallel tool results must be blocks in one user message.
		if n := len(wireMessages); n > 0 && wireMessages[n-1].Role == m.Role {
			wireMessages[n-1].Content = append(wireMessages[n-1].Content, m.Content...)
		} else {
			wireMessages = append(wireMessages, m)
		}
	}
	type wireTool struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	wireTools := make([]wireTool, 0, len(tools))
	for _, tool := range tools {
		wireTools = append(wireTools, wireTool{Name: tool.Name, Description: tool.Description, InputSchema: tool.Parameters})
	}
	payload := map[string]any{
		"model": p.config.Model, "messages": wireMessages,
		"stream": true, "max_tokens": p.config.MaxTokens,
	}
	if len(system) > 0 {
		payload["system"] = strings.Join(system, "\n\n")
	}
	if len(wireTools) > 0 {
		payload["tools"] = wireTools
	}
	resp, err := p.post(ctx, "/messages", payload)
	if err != nil {
		return Message{}, err
	}
	defer resp.Body.Close()
	result := Message{Role: "assistant"}
	var content strings.Builder
	calls := make(map[int]*ToolCall)
	partialArguments := make(map[int]bool)
	openBlocks := make(map[int]bool)
	finished := false
	stopReason := ""
	err = readEvents(ctx, resp.Body, func(event string, data []byte) error {
		var chunk struct {
			Type         string         `json:"type"`
			Index        int            `json:"index"`
			ContentBlock anthropicBlock `json:"content_block"`
			Delta        struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(data, &chunk); err != nil {
			return fmt.Errorf("invalid JSON in Anthropic event stream")
		}
		if chunk.Type == "" {
			chunk.Type = event
		}
		switch chunk.Type {
		case "error":
			return fmt.Errorf("anthropic stream error: %s", p.errorDetail(data))
		case "content_block_start":
			if chunk.Index < 0 || chunk.Index >= 1024 || openBlocks[chunk.Index] {
				return fmt.Errorf("Anthropic returned an invalid content block index")
			}
			openBlocks[chunk.Index] = true
			block := chunk.ContentBlock
			switch block.Type {
			case "text":
				content.WriteString(block.Text)
				if block.Text != "" {
					emit(block.Text)
				}
			case "tool_use":
				if len(calls) >= 128 || calls[chunk.Index] != nil {
					return fmt.Errorf("Anthropic returned too many or duplicate tool calls")
				}
				calls[chunk.Index] = &ToolCall{ID: block.ID, Name: block.Name, Arguments: block.Input}
			}
		case "content_block_delta":
			if !openBlocks[chunk.Index] {
				return fmt.Errorf("Anthropic returned a delta for an unopened content block")
			}
			switch chunk.Delta.Type {
			case "text_delta":
				content.WriteString(chunk.Delta.Text)
				if chunk.Delta.Text != "" {
					emit(chunk.Delta.Text)
				}
			case "input_json_delta":
				call := calls[chunk.Index]
				if call == nil {
					return fmt.Errorf("Anthropic returned arguments for an unknown tool call")
				}
				if !partialArguments[chunk.Index] {
					// The initial input object is a placeholder, not a prefix.
					call.Arguments = nil
					partialArguments[chunk.Index] = true
				}
				call.Arguments = append(call.Arguments, chunk.Delta.PartialJSON...)
			}
		case "content_block_stop":
			if !openBlocks[chunk.Index] {
				return fmt.Errorf("Anthropic stopped an unopened content block")
			}
			delete(openBlocks, chunk.Index)
		case "message_delta":
			if chunk.Delta.StopReason != "" {
				stopReason = chunk.Delta.StopReason
			}
		case "message_stop":
			if len(openBlocks) > 0 || stopReason == "" {
				return fmt.Errorf("Anthropic stream stopped before content was complete")
			}
			switch stopReason {
			case "end_turn", "tool_use", "stop_sequence", "refusal":
			case "max_tokens":
				return fmt.Errorf("model reached its output token limit; increase --max-tokens and retry (no tool calls were executed)")
			default:
				return fmt.Errorf("Anthropic stopped with unsupported reason %q", p.safeError(stopReason))
			}
			finished = true
			return errStreamComplete
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStreamComplete) {
		return Message{}, err
	}
	if !finished {
		return Message{}, fmt.Errorf("Anthropic stream ended unexpectedly before message_stop; no tool calls were executed")
	}
	result.Content = content.String()
	result.ToolCalls, err = assembledCalls(calls)
	return result, err
}
