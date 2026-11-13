package chat

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func TestEmbeddedMCPToolsPreserveSchemaApprovalAndCancellation(t *testing.T) {
	protocol := server.NewMCPServer("test", "1", server.WithToolCapabilities(false))
	readOnly := true
	protocol.AddTool(mcpgo.NewTool("inspect_device", mcpgo.WithToolAnnotation(mcpgo.ToolAnnotation{ReadOnlyHint: &readOnly})), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("device online"), nil
	})
	protocol.AddTool(mcpgo.NewTool("stop_app"), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		t.Fatal("unapproved write was executed")
		return nil, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tools, err := NewEmbeddedMCPTools(ctx, protocol, "")
	if err != nil {
		t.Fatal(err)
	}
	defer tools.Close()
	definitions, err := tools.ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]Tool{}
	for _, tool := range definitions {
		found[tool.Name] = tool
	}
	if found["inspect_device"].RequiresApproval || !found["stop_app"].RequiresApproval {
		t.Fatal("tool approval annotations were lost")
	}
	if _, ok := found["workspace_exec"]; ok {
		t.Fatal("embedded device tools exposed a host shell")
	}
	if _, ok := found["wendy_docs"]; !ok {
		t.Fatal("bundled documentation is missing")
	}
	result, err := tools.Execute(ctx, ToolCall{Name: "inspect_device", Arguments: json.RawMessage(`{}`)})
	if err != nil || result != "device online\n" {
		t.Fatalf("read = %q, %v", result, err)
	}
	cancel()
	if _, err := tools.Execute(ctx, ToolCall{Name: "inspect_device", Arguments: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("canceled session accepted a call")
	}
	if err := tools.Close(); err != nil {
		t.Fatal(err)
	}
}
