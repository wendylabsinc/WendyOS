package chat

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// metaRecordingMCP offers one read-only tool and records each call's _meta.
type metaRecordingMCP struct{ metas []*mcpgo.Meta }

func (m *metaRecordingMCP) ListTools(context.Context, mcpgo.ListToolsRequest) (*mcpgo.ListToolsResult, error) {
	return &mcpgo.ListToolsResult{Tools: []mcpgo.Tool{mcpgo.NewTool("device_info", mcpgo.WithReadOnlyHintAnnotation(true))}}, nil
}
func (m *metaRecordingMCP) CallTool(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	m.metas = append(m.metas, request.Params.Meta)
	return mcpgo.NewToolResultText("ok"), nil
}
func (*metaRecordingMCP) Close() error { return nil }

func TestWatchTurnMarksItsMCPCallsReadOnly(t *testing.T) {
	client := &metaRecordingMCP{}
	executor, err := newWorkspaceTools(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer executor.Close()
	executor.mcp = client
	provider := uiProviderFunc(func(_ context.Context, messages []Message, _ []Tool, _ func(string)) (Message, error) {
		if messages[len(messages)-1].Role == "tool" {
			return Message{Content: "done"}, nil
		}
		return Message{ToolCalls: []ToolCall{{ID: "info", Name: "device_info", Arguments: json.RawMessage(`{}`)}}}, nil
	})
	engine := NewEngine(provider, executor, "test")
	if err := engine.TurnWithOptions(context.Background(), "watch report", func(Event) {}, nil, TurnOptions{AppToolRefusal: "refused"}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Turn(context.Background(), "a user turn", func(Event) {}, nil); err != nil {
		t.Fatal(err)
	}
	if len(client.metas) != 2 {
		t.Fatalf("recorded %d calls; want 2", len(client.metas))
	}
	if meta := client.metas[0]; meta == nil || meta.AdditionalFields[ReadOnlyCallMeta] != true {
		t.Fatalf("watch turn call _meta = %+v; want %s: true", meta, ReadOnlyCallMeta)
	}
	if meta := client.metas[1]; meta != nil && meta.AdditionalFields[ReadOnlyCallMeta] != nil {
		t.Fatalf("user turn call _meta = %+v; want no read-only mark", meta)
	}
}
