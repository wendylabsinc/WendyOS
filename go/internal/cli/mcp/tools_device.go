package mcp

import (
	"context"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func (s *mcpServer) registerDeviceTools(srv *server.MCPServer) {}

func (s *mcpServer) handleDeviceList(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return mcpgo.NewToolResultError("not implemented"), nil
}
func (s *mcpServer) handleDeviceConnect(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return mcpgo.NewToolResultError("not implemented"), nil
}
func (s *mcpServer) handleDeviceDisconnect(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return mcpgo.NewToolResultError("not implemented"), nil
}
func (s *mcpServer) handleDeviceInfo(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return mcpgo.NewToolResultError("not implemented"), nil
}
func (s *mcpServer) handleDeviceSetDefault(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return mcpgo.NewToolResultError("not implemented"), nil
}
