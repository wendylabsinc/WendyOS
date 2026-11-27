package mcp

import "github.com/mark3labs/mcp-go/server"

// World's secure local stores currently support Linux and macOS. Do not expose
// tools on Windows that cannot satisfy that storage contract.
func (s *mcpServer) registerWorldTools(srv *server.MCPServer) {}
