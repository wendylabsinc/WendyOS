package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
)

type dataChatTool struct {
	chat.Tool
	run func(context.Context, *deviceDataTools, json.RawMessage) (any, error)
}

func dataChatTools() []dataChatTool {
	return []dataChatTool{eventHistoryTool, recordingsTool, recordingTool, trendsTool, samplesTool}
}

// All data queries remain pinned to the original native chat session, even if
// the model switches the embedded MCP connection to another device.
type deviceDataTools struct {
	chat.Executor
	conn *grpcclient.AgentConnection
	turn *ChatTurn
}

func (t *deviceDataTools) ListTools(ctx context.Context) ([]chat.Tool, error) {
	all, err := t.Executor.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	for _, tool := range dataChatTools() {
		all = append(all, tool.Tool)
	}
	return all, nil
}
func (t *deviceDataTools) Execute(ctx context.Context, call chat.ToolCall) (string, error) {
	for _, tool := range dataChatTools() {
		if call.Name != tool.Name {
			continue
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if t.conn == nil || t.conn.DataService == nil {
			return "", errors.New("reconnect the original chat device to query its data")
		}
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		result, err := tool.run(ctx, t, call.Arguments)
		if err != nil {
			return "", err
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		raw, err := json.Marshal(map[string]any{"device_scope": "original_chat_device", "observed_at": time.Now().UTC().Format(time.RFC3339Nano), "evidence": result})
		if len(raw) > 128<<10 {
			return "", errors.New("result exceeds chat limit; narrow the filters or reduce limit")
		}
		return string(raw), err
	}
	return t.Executor.Execute(ctx, call)
}
func (t *deviceDataTools) ExecuteResult(ctx context.Context, call chat.ToolCall) (chat.ToolResult, error) {
	for _, tool := range dataChatTools() {
		if call.Name == tool.Name {
			text, err := t.Execute(ctx, call)
			return chat.ToolResult{Text: text}, err
		}
	}
	if media, ok := t.Executor.(chat.MediaExecutor); ok {
		return media.ExecuteResult(ctx, call)
	}
	text, err := t.Executor.Execute(ctx, call)
	return chat.ToolResult{Text: text}, err
}

func decodeDataArgs(raw json.RawMessage, dst any) error {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("expected an argument object")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("expected one argument object")
	}
	return nil
}
func dataLimit(n int) (int, error) {
	if n == 0 {
		return 25, nil
	}
	if n < 1 || n > 100 {
		return 0, errors.New("limit must be between 1 and 100")
	}
	return n, nil
}

type dataWindow struct{ start, end time.Time }

func parseDataWindow(start, end string) (dataWindow, error) {
	now := time.Now().UTC()
	w := dataWindow{now.Add(-24 * time.Hour), now}
	var err error
	if end != "" {
		w.end, err = time.Parse(time.RFC3339Nano, end)
		if err != nil {
			return w, fmt.Errorf("end must include an RFC3339 timezone: %w", err)
		}
		w.start = w.end.Add(-24 * time.Hour)
	}
	if start != "" {
		w.start, err = time.Parse(time.RFC3339Nano, start)
		if err != nil {
			return w, fmt.Errorf("start must include an RFC3339 timezone: %w", err)
		}
	}
	if !w.start.Before(w.end) {
		return w, errors.New("start must be before end")
	}
	return w, nil
}
func (w dataWindow) includes(t time.Time) bool { return !t.Before(w.start) && t.Before(w.end) }
func (w dataWindow) evidence() map[string]string {
	return map[string]string{"start_inclusive": w.start.Format(time.RFC3339Nano), "end_exclusive": w.end.Format(time.RFC3339Nano)}
}
