package mcp

import (
	"context"
	"fmt"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
)

func (s *mcpServer) registerStatusTools(srv *server.MCPServer) {
	statusOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Return current MCP session connection state, cached CLI update details, and a plain-English suggested next step. Call this first to orient yourself. cli_update.available means a newer release is known; false does not confirm a successful release check."),
	}
	statusOpts = append(statusOpts, readOnly()...)
	statusOpts = append(statusOpts, localOnly()...)
	srv.AddTool(mcpgo.NewTool("wendy_status", statusOpts...), s.handleWendyStatus)
}

func (s *mcpServer) handleWendyStatus(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn, connType, target := s.connectionSnapshot()
	auth, loginErr := s.authState(time.Now())

	if conn == nil {
		next := "Call device_list, then device_connect with the returned device selector. For uninstalled hardware enable setup with wendy_tools, then use os_install_plan. An empty scan does not establish a network failure."
		if auth == "logged_out" || auth == "expired" {
			next = "Sign in first: call auth_login and show the user the link (devices need a signed-in session). Then " + next
		}
		out := map[string]any{
			"connected":             false,
			"suggested_next_step":   next,
			"auth":                  auth,
			"tool_groups":           s.selectedToolGroups(),
			"cli_version":           version.Version,
			"cli_update":            s.cliUpdateStatus(),
			"installation_planning": s.installation.Plan != nil,
			"installation_jobs":     s.installation.Start != nil,
			"simulator_management":  s.simulators.List != nil,
			"proxy_diagnostics":     s.proxyDiagnostics(),
		}
		if loginErr != nil {
			out["auth_login_error"] = loginErr.Error()
		}
		return okResult(out), nil
	}

	host := conn.Host
	if host == "" {
		host = "device"
	}
	out := map[string]any{
		"connected":             true,
		"tool_groups":           s.selectedToolGroups(),
		"cli_version":           version.Version,
		"cli_update":            s.cliUpdateStatus(),
		"installation_planning": s.installation.Plan != nil,
		"installation_jobs":     s.installation.Start != nil,
		"simulator_management":  s.simulators.List != nil,
		"device":                host,
		"connection_type":       connType,
		"suggested_next_step":   fmt.Sprintf("Connected to %s via %s. Use run or inspect containers and logs; enable specialist groups with wendy_tools.", host, connType),
		"proxy_diagnostics":     s.proxyDiagnostics(),
	}
	out["auth"] = auth
	if loginErr != nil {
		out["auth_login_error"] = loginErr.Error()
	}
	if target.Device != "" {
		out["command_target"] = target
	}
	return okResult(out), nil
}
