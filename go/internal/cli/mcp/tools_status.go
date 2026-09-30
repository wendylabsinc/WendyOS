package mcp

import (
	"context"
	"fmt"
	"os"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/pluginmode"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
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
	conn, connType, target := s.connectionSnapshot()

	if conn == nil {
		out := map[string]any{
			"connected":             false,
			"suggested_next_step":   "Call device_list, then device_connect with the returned device selector. For uninstalled hardware enable setup with wendy_tools, then use os_install_plan. An empty scan does not establish a network failure.",
			"tool_groups":           s.selectedToolGroups(),
			"cli_version":           version.Version,
			"installation_planning": s.installation.Plan != nil,
			"installation_jobs":     s.installation.Start != nil,
			"simulator_management":  s.simulators.List != nil,
			"proxy_diagnostics":     s.proxyDiagnostics(),
		}
		addPluginStatus(out)
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
		"installation_planning": s.installation.Plan != nil,
		"installation_jobs":     s.installation.Start != nil,
		"simulator_management":  s.simulators.List != nil,
		"device":                host,
		"connection_type":       connType,
		"suggested_next_step":   fmt.Sprintf("Connected to %s via %s. Use run or inspect containers and logs; enable specialist groups with wendy_tools.", host, connType),
		"proxy_diagnostics":     s.proxyDiagnostics(),
	}
	if target.Device != "" {
		out["command_target"] = target
	}
	addPluginStatus(out)
	return okResult(out), nil
}

// addPluginStatus adds a "plugin" block to a wendy_status result when the
// Wendy plugin started this server: the client it runs in and, when that
// client also has a user-level wendy server — which lists every Wendy tool a
// second time — the file configuring it and how to remove it.
func addPluginStatus(out map[string]any) {
	client := pluginmode.Client()
	if client == "" {
		return
	}
	plugin := map[string]any{"client": client}
	if home, err := os.UserHomeDir(); err == nil {
		if path, found := pluginmode.UserLevelServer(client, home); found {
			plugin["duplicate_server"] = path
			if pluginmode.SetupRemovesUserLevelServer(client, home) {
				plugin["fix"] = fmt.Sprintf("%s also configures a user-level wendy MCP server, so every Wendy tool is listed twice. Run '%s mcp setup' (it removes that entry because the plugin provides the server), then restart this client.", path, pluginmode.CLIInvocation())
			} else {
				removeCmd := "`claude mcp remove wendy -s user`"
				if client == "codex" {
					removeCmd = "`codex mcp remove wendy`"
				}
				plugin["fix"] = fmt.Sprintf("%s also configures a user-level wendy MCP server, so every Wendy tool is listed twice. `wendy mcp setup` keeps that entry (it was customized, or the plugin is not installed for your user and enabled). If you use Wendy only through the plugin, remove it with %s, then restart this client.", path, removeCmd)
			}
		}
	}
	out["plugin"] = plugin
}
