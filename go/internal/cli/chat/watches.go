package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// WatchInfo is one of the session's watches, as watch_list reports it.
type WatchInfo struct {
	WatchID     string   `json:"watch_id"`
	Label       string   `json:"label"`
	CameraName  string   `json:"camera_name"`
	Classes     []string `json:"classes"`
	State       string   `json:"state"`
	Reason      string   `json:"reason,omitempty"`
	LastEventAt string   `json:"last_event_at,omitempty"`
}

// callWatchTool calls a watch tool without the model, as /watches does, and
// decodes its structured result into out.
func (t *Tools) callWatchTool(ctx context.Context, name string, args map[string]any, out any) error {
	if t.mcp == nil {
		return errors.New("Wendy MCP server is not connected")
	}
	request := mcpgo.CallToolRequest{}
	request.Params.Name, request.Params.Arguments = name, args
	result, err := t.mcp.CallTool(ctx, request)
	if err != nil {
		return err
	}
	if result == nil {
		return errors.New("Wendy MCP tool returned an empty response")
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return err
	}
	if result.IsError {
		var failure struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &failure)
		if failure.Message == "" {
			failure.Message = name + " failed"
		}
		return errors.New(failure.Message)
	}
	return json.Unmarshal(raw, out)
}

func (t *Tools) listWatches(ctx context.Context) ([]WatchInfo, error) {
	var listing struct {
		Watches []WatchInfo `json:"watches"`
	}
	if err := t.callWatchTool(ctx, "watch_list", map[string]any{}, &listing); err != nil {
		return nil, err
	}
	return listing.Watches, nil
}

// stopAllWatches stops every active watch and returns how many it stopped.
func (t *Tools) stopAllWatches(ctx context.Context) (int, error) {
	watches, err := t.listWatches(ctx)
	if err != nil {
		return 0, err
	}
	stopped := 0
	var errs []error
	for _, w := range watches {
		if w.State == "ENDED" {
			continue
		}
		var result map[string]any
		if err := t.callWatchTool(ctx, "watch_stop", map[string]any{"watch_id": w.WatchID}, &result); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", w.Label, err))
			continue
		}
		stopped++
	}
	return stopped, errors.Join(errs...)
}

func isWatchTool(name string) bool { return strings.HasPrefix(name, "watch_") }

// withoutWatchTools hides camera watches. A delegated child's MCP process
// ends with its task, taking its watches with it, and an unattended service
// has no one to tell (design §7.1, §3).
type withoutWatchTools struct{ base Executor }

func (w withoutWatchTools) ListTools(ctx context.Context) ([]Tool, error) {
	list, err := w.base.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Tool, 0, len(list))
	for _, tool := range list {
		if !isWatchTool(tool.Name) {
			out = append(out, tool)
		}
	}
	return out, nil
}

func (w withoutWatchTools) Execute(ctx context.Context, c ToolCall) (string, error) {
	r, err := w.ExecuteResult(ctx, c)
	return r.Text, err
}

func (w withoutWatchTools) ExecuteResult(ctx context.Context, c ToolCall) (ToolResult, error) {
	if isWatchTool(c.Name) {
		return ToolResult{}, fmt.Errorf("%s is not available here: camera watches belong to the interactive chat session", c.Name)
	}
	if m, ok := w.base.(MediaExecutor); ok {
		return m.ExecuteResult(ctx, c)
	}
	v, err := w.base.Execute(ctx, c)
	return ToolResult{Text: v}, err
}
