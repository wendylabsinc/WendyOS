package mcp

import (
	"context"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func (s *mcpServer) registerWiFiTools(srv *server.MCPServer) {}

func (s *mcpServer) handleWiFiList(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return mcpgo.NewToolResultError("not implemented"), nil
}
func (s *mcpServer) handleWiFiConnect(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return mcpgo.NewToolResultError("not implemented"), nil
}
func (s *mcpServer) handleWiFiStatus(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return mcpgo.NewToolResultError("not implemented"), nil
}
func (s *mcpServer) handleWiFiDisconnect(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return mcpgo.NewToolResultError("not implemented"), nil
}
func (s *mcpServer) handleWiFiKnownNetworks(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return mcpgo.NewToolResultError("not implemented"), nil
}
