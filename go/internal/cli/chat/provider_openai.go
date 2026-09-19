package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
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
	Content    any              `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIImageURL struct {
	URL string `json:"url"`
}

type openAIContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *openAIImageURL `json:"image_url,omitempty"`
}

func imageDataURL(image Image) string {
	return "data:" + image.MIMEType + ";base64," + image.Data
}

func validateImageRoles(messages []Message) error {
	for _, message := range messages {
		if len(message.Images) > 0 && message.Role != "user" && message.Role != "tool" {
			return fmt.Errorf("image attachments are supported on user and tool messages, not %q messages", message.Role)
		}
	}
	return nil
}

var imageDataURLPattern = regexp.MustCompile(`(?i)data:image/[a-z0-9.+-]+;base64,\s*[a-z0-9+/=_-]*`)

type imageProviderError struct {
	cause  error
	detail string
}

func (e *imageProviderError) Error() string { return e.detail }
func (e *imageProviderError) Unwrap() error { return e.cause }

// Images are always sent on the first attempt. Give guidance for servers that
// reject them, without retrying a text-only request that loses the observation.
func imageRequestError(messages []Message, err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	hasImages := false
	for _, message := range messages {
		hasImages = hasImages || len(message.Images) > 0
	}
	if !hasImages {
		return err
	}
	detail := imageDataURLPattern.ReplaceAllString(err.Error(), "[image omitted]")
	for _, message := range messages {
		for _, image := range message.Images {
			// Anthropic errors may echo bare data rather than a data URL.
			// Match a prefix because errorDetail may already have truncated it.
			prefix := image.Data[:min(32, len(image.Data))]
			if prefix == "" {
				continue
			}
			for offset := 0; offset < len(detail); {
				match := strings.Index(detail[offset:], prefix)
				if match < 0 {
					break
				}
				start := offset + match
				end := start + len(prefix)
				for end < len(detail) && strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/=", rune(detail[end])) {
					end++
				}
				detail = detail[:start] + "[image omitted]" + detail[end:]
				offset = start + len("[image omitted]")
			}
		}
	}
	lower := strings.ToLower(detail)
	rejected := strings.Contains(lower, "api returned http 400:") || strings.Contains(lower, "api returned http 415:") || strings.Contains(lower, "api returned http 422:")
	if strings.Contains(lower, "stream error:") || strings.Contains(lower, "response failed:") {
		for _, term := range []string{"image", "vision", "multimodal", "content must be a string"} {
			rejected = rejected || strings.Contains(lower, term)
		}
	}
	if rejected {
		detail += "; this request includes images: use /setup to choose a vision-capable model and a server that supports image input"
	}
	if detail != err.Error() {
		return &imageProviderError{cause: err, detail: detail}
	}
	return err
}

func openAIContent(text string, images []Image) []openAIContentPart {
	parts := make([]openAIContentPart, 0, 1+len(images))
	if text != "" {
		parts = append(parts, openAIContentPart{Type: "text", Text: text})
	}
	for _, image := range images {
		parts = append(parts, openAIContentPart{Type: "image_url", ImageURL: &openAIImageURL{URL: imageDataURL(image)}})
	}
	return parts
}

func openAIMessages(messages []Message) ([]openAIMessage, error) {
	if err := validateImageRoles(messages); err != nil {
		return nil, err
	}
	wireMessages := make([]openAIMessage, 0, len(messages))
	var toolImages []openAIContentPart
	flushToolImages := func() {
		if len(toolImages) > 0 {
			wireMessages = append(wireMessages, openAIMessage{Role: "user", Content: toolImages})
			toolImages = nil
		}
	}
	for _, message := range messages {
		if message.Role != "tool" {
			flushToolImages()
		}
		m := openAIMessage{Role: message.Role, Content: message.Content, ToolCallID: message.ToolCallID}
		if len(message.Images) > 0 {
			if message.Role == "tool" {
				// Compatible APIs accept images in user messages. Keep every
				// tool result adjacent to its assistant's parallel tool calls,
				// then attach images with their original call IDs as provenance.
				label := fmt.Sprintf("Images returned by tool call %q (tool output):", message.ToolCallID)
				toolImages = append(toolImages, openAIContent(label, message.Images)...)
			} else {
				m.Content = openAIContent(message.Content, message.Images)
			}
		}
		for _, call := range message.ToolCalls {
			m.ToolCalls = append(m.ToolCalls, openAIToolCall{ID: call.ID, Type: "function", Function: openAIFunction{
				Name: call.Name, Arguments: string(call.Arguments),
			}})
		}
		wireMessages = append(wireMessages, m)
	}
	flushToolImages()
	return wireMessages, nil
}

func (p *httpProvider) streamOpenAI(ctx context.Context, messages []Message, tools []Tool, emit func(string)) (reply Message, err error) {
	wireMessages, err := openAIMessages(messages)
	if err != nil {
		return Message{}, err
	}
	defer func() { err = imageRequestError(messages, err) }()
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
