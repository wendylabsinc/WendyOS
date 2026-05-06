package mcp

import (
	"context"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func (s *mcpServer) registerFileSyncTools(srv *server.MCPServer) {
	srv.AddTool(mcpgo.NewTool("filesync_sync",
		mcpgo.WithDescription("Sync files to a container app on the connected device (requires binary file data; not available via MCP)"),
	), s.handleFileSyncSync)
}

func (s *mcpServer) handleFileSyncSync(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return mcpgo.NewToolResultError(
		"filesync_sync requires binary file transfer and is not available via the MCP interface. " +
			"Use the wendy CLI directly: wendy run <path>",
	), nil
}
