package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
)

const (
	maxToolOutputBytes   = 32 * 1024
	maxToolArgumentBytes = 2 * 1024 * 1024
	defaultMaxRounds     = 40
)

// Engine maintains a conversation and runs model-requested tools sequentially.
// Reset and Messages are safe to call between turns; a turn holds the engine
// lock until the model and its tools have finished or acknowledged cancellation.
type Engine struct {
	mu        sync.Mutex
	provider  Provider
	executor  Executor
	system    string
	messages  []Message
	maxRounds int
	nextID    uint64
}

func NewEngine(provider Provider, executor Executor, system string) *Engine {
	e := &Engine{provider: provider, executor: executor, system: system, maxRounds: defaultMaxRounds}
	e.Reset()
	return e
}

func (e *Engine) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.messages = nil
	if e.system != "" {
		e.messages = []Message{{Role: "system", Content: e.system}}
	}
}

func (e *Engine) Messages() []Message {
	e.mu.Lock()
	defer e.mu.Unlock()
	return cloneMessages(e.messages)
}

func cloneMessages(messages []Message) []Message {
	out := append([]Message(nil), messages...)
	for i := range out {
		out[i].ResponseItems = append([]json.RawMessage(nil), out[i].ResponseItems...)
		for j := range out[i].ResponseItems {
			out[i].ResponseItems[j] = append(json.RawMessage(nil), out[i].ResponseItems[j]...)
		}
		out[i].Images = append([]Image(nil), out[i].Images...)
		out[i].ToolCalls = append([]ToolCall(nil), out[i].ToolCalls...)
		for j := range out[i].ToolCalls {
			out[i].ToolCalls[j].Arguments = append(json.RawMessage(nil), out[i].ToolCalls[j].Arguments...)
		}
	}
	return out
}

func (e *Engine) Turn(ctx context.Context, prompt string, emit func(Event), approve ApproveFunc) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.provider == nil || e.executor == nil {
		return errors.New("chat requires a model provider and tools")
	}
	if strings.TrimSpace(prompt) == "" {
		return errors.New("message must not be empty")
	}
	if emit == nil {
		emit = func(Event) {}
	}
	e.messages = append(e.messages, Message{Role: "user", Content: prompt})
	for round := 0; round < e.maxRounds; round++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Connecting to a device can expose a different set of MCP tools.
		available, err := e.executor.ListTools(ctx)
		if err != nil {
			return fmt.Errorf("listing tools: %w", err)
		}
		byName := make(map[string]Tool, len(available))
		for _, tool := range available {
			if _, exists := byName[tool.Name]; exists {
				return fmt.Errorf("duplicate tool name %q", tool.Name)
			}
			byName[tool.Name] = tool
		}
		emit(Event{Type: "status", Text: "Thinking…"})
		var streamed strings.Builder
		message, err := e.provider.Stream(ctx, cloneMessages(e.messages), available, func(chunk string) {
			streamed.WriteString(chunk)
			emit(Event{Type: "text", Text: chunk})
		})
		if err != nil {
			// Partial tool calls are never executed or recorded. Retaining only
			// streamed prose allows the next turn to resume a valid conversation.
			if streamed.Len() > 0 {
				e.messages = append(e.messages, Message{Role: "assistant", Content: streamed.String()})
			}
			return fmt.Errorf("model response: %w", err)
		}
		message.Role = "assistant"
		message.ToolCallID = ""
		if message.Content == "" {
			message.Content = streamed.String()
		} else if streamed.Len() == 0 {
			emit(Event{Type: "text", Text: message.Content})
		}
		validation := make([]error, len(message.ToolCalls))
		seenIDs := make(map[string]bool)
		for _, previous := range e.messages {
			for _, call := range previous.ToolCalls {
				seenIDs[call.ID] = true
			}
		}
		for i := range message.ToolCalls {
			call := &message.ToolCalls[i]
			if call.ID == "" || seenIDs[call.ID] {
				for {
					e.nextID++
					call.ID = fmt.Sprintf("wendy_call_%d", e.nextID)
					if !seenIDs[call.ID] {
						break
					}
				}
			}
			seenIDs[call.ID] = true
			tool, known := byName[call.Name]
			if !known {
				validation[i] = fmt.Errorf("unknown tool %q; use a tool from the current tool list", call.Name)
			} else {
				validation[i] = validateArguments(tool, call.Arguments)
			}
			// Invalid JSON cannot be sent back through providers that represent
			// tool inputs as JSON objects. The error result records the rejection.
			var object map[string]any
			if json.Unmarshal(call.Arguments, &object) != nil || object == nil {
				call.Arguments = json.RawMessage(`{}`)
			}
		}
		e.messages = append(e.messages, message)
		if len(message.ToolCalls) == 0 {
			return ctx.Err()
		}
		var turnErr error
		for i, call := range message.ToolCalls {
			var result string
			var images []Image
			emit(Event{Type: "tool_start", Call: &call})
			switch {
			case turnErr != nil:
				result = "Tool was not executed because the turn was interrupted."
			case ctx.Err() != nil:
				turnErr = ctx.Err()
				result = "Tool was not executed because the turn was canceled."
			case validation[i] != nil:
				result = "Tool error: " + validation[i].Error()
			default:
				allowed := !byName[call.Name].RequiresApproval
				if !allowed && approve != nil {
					emit(Event{Type: "status", Text: "Waiting for approval…", Call: &call})
					allowed, turnErr = approve(ctx, call)
				}
				switch {
				case turnErr != nil:
					result = "Tool was not executed: approval interrupted."
				case ctx.Err() != nil:
					turnErr = ctx.Err()
					result = "Tool was not executed because the turn was canceled."
				case !allowed:
					result = "User denied permission. Tool was not executed; do not retry this action without new user instructions."
				default:
					emit(Event{Type: "status", Text: "Running " + call.Name + "…", Call: &call})
					var output ToolResult
					var toolErr error
					if executor, ok := e.executor.(MediaExecutor); ok {
						output, toolErr = executor.ExecuteResult(ctx, call)
					} else {
						output.Text, toolErr = e.executor.Execute(ctx, call)
					}
					result = output.Text
					if toolErr == nil {
						images, toolErr = validateImages(output.Images)
					}
					if toolErr != nil {
						result = "Tool error: " + toolErr.Error()
						if output.Text != "" {
							result += "\n" + output.Text
						}
					}
					if ctx.Err() != nil {
						turnErr = ctx.Err()
					}
				}
			}
			if len(images) > 0 {
				result = fmt.Sprintf("[%d image(s) attached for visual inspection.]\n%s", len(images), result)
			}
			result = truncateOutput(result)
			e.messages = append(e.messages, Message{Role: "tool", ToolCallID: call.ID, Content: result, Images: images})
			e.trimImageHistory()
			emit(Event{Type: "tool_result", Text: result, Call: &call})
		}
		// Every announced tool call now has a result, including on cancellation.
		if turnErr != nil {
			return turnErr
		}
	}
	return fmt.Errorf("stopped after %d model rounds; send another message to continue", e.maxRounds)
}

func validateArguments(tool Tool, raw json.RawMessage) error {
	if len(raw) > maxToolArgumentBytes {
		return errors.New("tool arguments exceed the 2 MiB limit")
	}
	var arguments map[string]any
	if err := json.Unmarshal(raw, &arguments); err != nil {
		return fmt.Errorf("invalid JSON arguments: %w", err)
	}
	if arguments == nil {
		return errors.New("tool arguments must be a JSON object")
	}
	if len(tool.Parameters) == 0 {
		return nil
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(tool.Parameters, &schema); err != nil {
		return fmt.Errorf("invalid tool schema: %w", err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return fmt.Errorf("resolving tool schema: %w", err)
	}
	if err := resolved.Validate(arguments); err != nil {
		return fmt.Errorf("invalid arguments for %s: %w", tool.Name, err)
	}
	return nil
}

func truncateOutput(output string) string {
	output = strings.ToValidUTF8(output, "�")
	if len(output) <= maxToolOutputBytes {
		return output
	}
	const suffix = "\n[Output truncated at 32 KiB.]"
	end := maxToolOutputBytes - len(suffix)
	for end > 0 && !utf8.RuneStart(output[end]) {
		end--
	}
	return output[:end] + suffix
}
