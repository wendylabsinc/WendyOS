package mcp

import (
	"context"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func (s *mcpServer) registerTelemetryTools(srv *server.MCPServer) {}

func (s *mcpServer) handleTelemetryLogs(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return mcpgo.NewToolResultError("not implemented"), nil
}
func (s *mcpServer) handleTelemetryMetrics(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return mcpgo.NewToolResultError("not implemented"), nil
}
func (s *mcpServer) handleTelemetryTraces(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return mcpgo.NewToolResultError("not implemented"), nil
}
