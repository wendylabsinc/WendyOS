package mcp

import (
	"context"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func (s *mcpServer) registerProvisioningTools(srv *server.MCPServer) {}

func (s *mcpServer) handleProvisioningStatus(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return mcpgo.NewToolResultError("not implemented"), nil
}
func (s *mcpServer) handleProvisioningStart(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return mcpgo.NewToolResultError("not implemented"), nil
}
