// Package chat implements the provider-neutral agent behind wendy chat.
package chat

import (
	"context"
	"encoding/json"
)

// Message is the common conversation format used by model providers.
type Message struct {
	Role       string
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string
	// ResponseItems preserves native OpenAI response items, including opaque
	// reasoning state required when continuing a tool-calling conversation.
	ResponseItems []json.RawMessage
}

type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

type Tool struct {
	Name             string
	Description      string
	Parameters       json.RawMessage
	RequiresApproval bool
}

// Provider streams assistant text and returns the complete assistant message,
// including any requested tool calls. Implementations must honor cancellation.
type Provider interface {
	Stream(context.Context, []Message, []Tool, func(string)) (Message, error)
}

type Executor interface {
	ListTools(context.Context) ([]Tool, error)
	Execute(context.Context, ToolCall) (string, error)
}

type ApproveFunc func(context.Context, ToolCall) (bool, error)

// Event types are text, tool_start, tool_result, and status.
type Event struct {
	Type string
	Text string
	Call *ToolCall
}
