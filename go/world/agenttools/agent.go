// Package agenttools connects Wendy Chat to the bounded World agent protocol.
// The host selects the profile; model-supplied arguments cannot change it.
package agenttools

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wendylabsinc/wendy/go/world/internal/worldmcp"
)

var ErrUnavailable = errors.New("World agent profile unavailable; configure WENDY_WORLD_AGENT_CONFIG with a private read/propose agent profile and restart Wendy Chat")
var slots = make(chan struct{}, worldmcp.MaxConcurrent)

// Call retains the original tool schema, credential checks, output limits and
// non-executable proposal contract. It never uses an operator signing profile.
func Call(ctx context.Context, path, name string, arguments json.RawMessage) (*mcp.CallToolResult, error) {
	switch name {
	case "world_query", "world_watch", "task_catalog", "task_status", "task_propose":
	default:
		known := false
		for _, d := range worldmcp.ReadDescriptors() {
			known = known || name == d.Name
		}
		if !known {
			return nil, errors.New("unknown World agent operation")
		}
	}
	if len(arguments) > worldmcp.MaxInputBytes || !json.Valid(arguments) {
		return nil, errors.New("invalid World arguments")
	}
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return nil, errors.New("World agent busy")
	}
	ctx, cancel := context.WithTimeout(ctx, worldmcp.CallTimeout)
	defer cancel()
	cfg, err := loadConfig(path)
	if err != nil {
		return nil, ErrUnavailable
	}
	service, err := worldmcp.New(cfg)
	if err != nil {
		return nil, ErrUnavailable
	}
	st, ct := mcp.NewInMemoryTransports()
	ss, err := service.Connect(ctx, st, nil)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "wendy-chat", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer cs.Close()
	return cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
}

// ReadTool describes additional bounded reads exposed identically in World MCP
// and the Wendy CLI MCP adapter. All are read-only and use the fixed profile.
type ReadTool struct {
	Name, Title, Description string
	Schema                   json.RawMessage
}

func ReadTools() []ReadTool {
	var out []ReadTool
	for _, d := range worldmcp.ReadDescriptors() {
		schema := map[string]any{"type": "object", "additionalProperties": false, "properties": d.Properties}
		if len(d.Required) > 0 {
			schema["required"] = d.Required
		}
		raw, _ := json.Marshal(schema)
		out = append(out, ReadTool{Name: d.Name, Title: d.Title, Description: d.Description, Schema: raw})
	}
	return out
}
