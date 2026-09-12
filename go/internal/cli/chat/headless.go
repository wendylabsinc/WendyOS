package chat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
)

// HeadlessOptions runs one turn without the terminal UI, for scripts and for
// other programs that want the agent's reasoning and tools but not its screen.
type HeadlessOptions struct {
	Engine *Engine
	Prompt string
	// JSON writes one event per line as it happens; otherwise only the final
	// assistant text is written, followed by a newline.
	JSON   bool
	Output io.Writer
	// AutoApprove runs tools that would ask a person. Without it, such tools
	// are denied: there is nobody to ask, and silently running a shell command
	// or a device change is worse than declining it.
	AutoApprove bool
}

// headlessEvent is the wire form of Event. It carries the fields a consumer
// needs to reconstruct the turn and nothing that names a terminal.
type headlessEvent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Approved  *bool           `json:"approved,omitempty"`
}

// RunHeadless sends Prompt to the engine and reports what happened.
//
// With JSON, events are written in order as newline-delimited objects:
// status, text (streamed chunks), tool_start, approval, tool_result, then a
// final done or error. Without JSON the assistant's complete text is written
// once the turn ends. Denied approvals are reported, not hidden: a consumer
// that sees approved:false knows to rerun with --yes or to ask a person.
func RunHeadless(ctx context.Context, opts HeadlessOptions) error {
	if opts.Engine == nil {
		return errors.New("chat requires an engine")
	}
	if strings.TrimSpace(opts.Prompt) == "" {
		return errors.New("prompt must not be empty")
	}
	if opts.Output == nil {
		opts.Output = io.Discard
	}
	var mu sync.Mutex
	var full strings.Builder
	write := func(ev headlessEvent) {
		if !opts.JSON {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		line, err := json.Marshal(ev)
		if err != nil {
			return
		}
		_, _ = opts.Output.Write(append(line, '\n'))
	}
	emit := func(event Event) {
		ev := headlessEvent{Type: event.Type, Text: event.Text}
		if event.Call != nil {
			ev.ID, ev.Tool = event.Call.ID, event.Call.Name
			if event.Type == "tool_start" {
				ev.Arguments = compactArguments(event.Call.Arguments)
			}
		}
		if event.Type == "text" {
			mu.Lock()
			full.WriteString(event.Text)
			mu.Unlock()
		}
		write(ev)
	}
	approve := func(ctx context.Context, call ToolCall) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		allowed := opts.AutoApprove
		write(headlessEvent{Type: "approval", ID: call.ID, Tool: call.Name, Approved: &allowed,
			Text: approvalText(allowed)})
		return allowed, nil
	}
	err := opts.Engine.Turn(ctx, opts.Prompt, emit, approve)
	mu.Lock()
	text := full.String()
	mu.Unlock()
	if err != nil {
		write(headlessEvent{Type: "error", Text: err.Error()})
		return err
	}
	if opts.JSON {
		write(headlessEvent{Type: "done", Text: text})
		return nil
	}
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	_, werr := io.WriteString(opts.Output, text)
	return werr
}

func approvalText(allowed bool) string {
	if allowed {
		return "approved by --yes"
	}
	return "denied: this tool needs approval and headless mode has nobody to ask; rerun with --yes to allow it"
}

func compactArguments(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || !json.Valid(raw) {
		return json.RawMessage(`{}`)
	}
	return raw
}
