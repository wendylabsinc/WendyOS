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
	desc := "Return current MCP session connection state, cached CLI update details, and a plain-English suggested next step. Call this first to orient yourself. cli_update.available means a newer release is known; false does not confirm a successful release check."
	if s.relay {
		desc = "Return the current connection state and a plain-English suggested next step. Call this first to orient yourself."
	}
	statusOpts := []mcpgo.ToolOption{mcpgo.WithDescription(desc)}
	statusOpts = append(statusOpts, readOnly()...)
	statusOpts = append(statusOpts, localOnly()...)
	srv.AddTool(mcpgo.NewTool("wendy_status", statusOpts...), s.handleWendyStatus)
}

func (s *mcpServer) handleWendyStatus(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn, connType, target := s.connectionSnapshot()
	auth, loginErr := s.authState(time.Now())

	if conn == nil {
		next := "Call device_list, then device_connect with the returned device selector. For uninstalled hardware enable setup with wendy_tools, then use os_install_plan. An empty scan does not establish a network failure."
		if s.hostSimulators() {
			next = "Call device_list, then device_connect with the returned device selector. " +
				"If no physical device is available and the task needs no physical hardware, use a local simulator rather than skipping on-device checks: " +
				"device_connect boots a listed vm:NAME without asking; ask the user before simulator_create (simulator group) downloads a new one. " +
				"For uninstalled hardware enable setup with wendy_tools, then use os_install_plan. An empty scan does not establish a network failure."
		}
		if s.relay {
			next = "Call device_list, then device_connect with the returned device selector. For uninstalled hardware use os_install_plan. An empty scan does not establish a network failure."
		}
		// Signing in only reaches Cloud devices: gating discovery on it made
		// agents wait for a sign-in that local targets never needed.
		if auth == "logged_out" || auth == "expired" {
			signedOut := "Not signed in to Wendy Cloud"
			if auth == "expired" {
				signedOut = "The Wendy Cloud session has expired"
			}
			local := "LAN devices that are not Cloud-enrolled need no sign-in."
			if s.hostSimulators() {
				local = "Local simulators and " + local
			}
			next += " " + signedOut + ", so device_list omits Cloud devices. " + local +
				" Call auth_login and show the user the link only when the task needs a Cloud device or a device error asks for a sign-in."
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
		return s.statusResult(out), nil
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
	if s.relay {
		out["suggested_next_step"] = fmt.Sprintf("Connected to %s via %s. Use run or inspect containers and logs.", host, connType)
	}
	return s.statusResult(out), nil
}

// statusResult drops what a hosted chat has no use for: CLI upgrade details,
// the local command target, group state and app-proxy diagnostics.
func (s *mcpServer) statusResult(out map[string]any) *mcpgo.CallToolResult {
	s.mu.RLock()
	v1 := s.toolProfile == "workflow-v1"
	s.mu.RUnlock()
	if v1 && !s.relay {
		if out["connected"] == true {
			out["suggested_next_step"] = "Use the saved workflow with job_submit; batch dependent calls in the client. Keep terminal waits and read-only recovery outside repeated model decisions."
		} else {
			out["suggested_next_step"] = "Wendy MCP V1 requires an explicitly configured startup target (--device). Use the standard profile for discovery/sign-in, then restart with the selected target. No target or execution permission is inferred."
		}
	}
	if s.relay {
		for _, k := range []string{"cli_update", "command_target", "tool_groups", "proxy_diagnostics"} {
			delete(out, k)
		}
	}
	return okResult(out)
}
