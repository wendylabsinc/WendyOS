//go:build !windows

package mcp

import (
	"context"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func TestWorldToolsKeepProposalsSeparateAndProfileHostSelected(t *testing.T) {
	t.Setenv("WENDY_WORLD_AGENT_CONFIG", "")
	srv := server.NewMCPServer("test", "1")
	s := &mcpServer{}
	s.registerWorldTools(srv)
	for name, tool := range srv.ListTools() {
		read := name != "world_task_propose"
		if tool.Tool.Annotations.ReadOnlyHint == nil || *tool.Tool.Annotations.ReadOnlyHint != read {
			t.Fatalf("wrong review classification: %s", name)
		}
		result, err := tool.Handler(context.Background(), mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{Name: name, Arguments: map[string]any{}}})
		if err != nil || result == nil || !result.IsError {
			t.Fatalf("missing host profile reported success: %s %v", name, err)
		}
		for _, content := range result.Content {
			if text, ok := content.(mcpgo.TextContent); ok && !strings.Contains(text.Text, "WENDY_WORLD_AGENT_CONFIG") {
				t.Fatal("missing actionable profile diagnostic")
			}
		}
	}
	if len(srv.ListTools()) != 12 {
		t.Fatal("unexpected executable tools exposed")
	}
}
