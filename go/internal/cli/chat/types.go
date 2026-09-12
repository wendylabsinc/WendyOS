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
	Images     []Image
	// ResponseItems preserves native OpenAI response items, including opaque
	// reasoning state required when continuing a tool-calling conversation.
	ResponseItems []json.RawMessage
}

// Image is a validated, bounded image attached to a tool result. Data is base64
// encoded; it is sent as provider media content, never as transcript text.
type Image struct {
	MIMEType string
	Data     string
}

type ToolResult struct {
	Text   string
	Images []Image
}

// MediaExecutor extends text-only executors without requiring every tool to
// handle media. The engine preserves attachments separately from text limits.
type MediaExecutor interface {
	ExecuteResult(context.Context, ToolCall) (ToolResult, error)
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
