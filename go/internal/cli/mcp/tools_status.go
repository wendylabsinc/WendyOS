package mcp

import (
	"context"
	"fmt"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func (s *mcpServer) registerStatusTools(srv *server.MCPServer) {
	statusOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Return current MCP session connection state and a plain-English suggested next step. Call this first to orient yourself."),
	}
	statusOpts = append(statusOpts, readOnly()...)
	statusOpts = append(statusOpts, localOnly()...)
	srv.AddTool(mcpgo.NewTool("wendy_status", statusOpts...), s.handleWendyStatus)
}

func (s *mcpServer) handleWendyStatus(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	connType := s.GetConnType()

	if conn == nil {
		out := map[string]any{
			"connected":           false,
			"suggested_next_step": "not connected — call device_list for configured and online cloud devices (scan=true adds LAN discovery), then device_connect for local devices or cloud_connect for cloud devices",
			"proxy_diagnostics":   s.proxyDiagnostics(),
		}
		return okResult(out), nil
	}

	host := conn.Host
	if host == "" {
		host = "device"
	}
	out := map[string]any{
		"connected":           true,
		"device":              host,
		"connection_type":     connType,
		"suggested_next_step": fmt.Sprintf("connected to %s via %s — ready to use container, wifi, hardware, telemetry, and os tools", host, connType),
		"proxy_diagnostics":   s.proxyDiagnostics(),
	}
	return okResult(out), nil
}
