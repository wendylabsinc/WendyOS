package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Groups control discovery, not authorization. All handlers stay registered so
// existing clients can call known tools. App-provided tools remain discoverable.
var toolGroups = map[string][]string{
	"core": {"wendy_status", "wendy_tools", "auth_login", "device_list", "device_connect", "device_disconnect", "device_info", "run",
		"container_list", "container_start", "container_stop", "container_delete", "container_stats", "container_exec", "telemetry_logs", "hardware_capabilities"},
	"setup": {"device_set_default", "project_validate", "os_install_plan", "os_list_drives", "os_install_start", "os_install_status", "os_install_resume", "os_install_verify", "os_update", "os_update_status", "device_update_agent",
		"cloud_enroll_device", "provisioning_start", "provisioning_status", "wifi_list", "wifi_connect", "wifi_disconnect", "wifi_status", "wifi_known_networks"},
	"hardware":      {"bluetooth_scan", "bluetooth_connect", "bluetooth_disconnect", "camera_list", "camera_controls", "camera_set_control", "camera_snapshot"},
	"robotics":      {"ros2_topics", "ros2_topic_info", "ros2_topic_sample", "ros2_topic_hz", "ros2_lidar_summary"},
	"observability": {"telemetry_metrics", "telemetry_traces", "app_inspect", "device_os_logs", "job_inspect", "job_artifacts_pull", "compute_status", "job_submit", "checkpoint_inspect", "experiment_plan"},
	"simulator":     {"simulator_list", "simulator_create", "simulator_start", "simulator_stop", "simulator_delete"},
	"cloud":         {"cloud_discover", "cloud_connect", "cloud_tunnel", "cloud_tunnel_list", "cloud_tunnel_close", "cloud_ping"},
}

var toolGroupNames = []string{"core", "setup", "simulator", "hardware", "robotics", "observability", "cloud", "all"}

// SetToolProfile selects an explicit task catalog without changing authorization.
// The default keeps the existing core/group discovery contract unchanged.
func (s *mcpServer) SetToolProfile(profile string) error {
	if profile != "" && profile != "training" && profile != "workflow" && profile != "workflow-v1" {
		return fmt.Errorf("unknown tool profile %q; choose training, workflow, workflow-v1 or omit", profile)
	}
	s.mu.Lock()
	s.toolProfile = profile
	s.mu.Unlock()
	return nil
}

var trainingTools = []string{"wendy_status", "auth_login", "device_list", "device_connect", "device_disconnect", "job_inspect", "job_artifacts_pull", "compute_status", "job_submit", "checkpoint_inspect", "experiment_plan"}

// Wendy MCP V1 is an experimental catalog for an explicitly configured target.
// It changes discovery and result rendering only, never execution authority.
var workflowV1Tools = []string{"wendy_status", "job_submit", "job_inspect", "job_artifacts_pull"}

// SetToolGroups sets the advertised built-in tools. Core is always included.
// Call before Start; the wendy_tools handler also uses it while serving.
func (s *mcpServer) SetToolGroups(groups []string) error {
	selected := []string{"core"}
	for _, group := range groups {
		if !slices.Contains(toolGroupNames, group) {
			return fmt.Errorf("unknown tool group %q; choose core, setup, simulator, hardware, robotics, observability, cloud, or all", group)
		}
		if !slices.Contains(selected, group) {
			selected = append(selected, group)
		}
	}
	if slices.Contains(selected, "all") {
		selected = []string{"all"}
	}
	sort.Strings(selected)
	s.mu.Lock()
	s.toolGroups = selected
	s.mu.Unlock()
	return nil
}

func (s *mcpServer) selectedToolGroups() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.toolGroups) == 0 {
		return []string{"core"}
	}
	return slices.Clone(s.toolGroups)
}

func (s *mcpServer) filterTools(_ context.Context, tools []mcpgo.Tool) []mcpgo.Tool {
	groups := s.selectedToolGroups()
	s.mu.RLock()
	profile := s.toolProfile
	s.mu.RUnlock()
	all := slices.Contains(groups, "all")
	binding := s.bindingPolicy()
	visible := make([]mcpgo.Tool, 0, len(tools))
	for _, tool := range tools {
		if binding != nil && !binding.allows(tool) {
			continue
		}
		if s.relay {
			if inManifest(tool.Name) {
				visible = append(visible, tool)
			}
			continue
		}
		if profile == "training" || profile == "workflow" || profile == "workflow-v1" {
			included := slices.Contains(trainingTools, tool.Name) && (profile != "workflow" || (tool.Name != "checkpoint_inspect" && tool.Name != "experiment_plan"))
			if profile == "workflow-v1" {
				included = slices.Contains(workflowV1Tools, tool.Name)
			}
			if included {
				if (profile == "workflow" || profile == "workflow-v1") && tool.Name == "job_submit" {
					tool.Description = "Run a saved wendy.workflow.v1 file on the connected target: bounded steps, terminal evidence, verified selected reports and a saved receipt. Identical retries reuse the job. Missing evidence stays unknown. Executes the declared argv with host-agent permissions. Use a long client tool wait; avoid short outer polling of this already-waiting call."
					tool.InputSchema = mcpgo.ToolInputSchema{Type: "object", Properties: map[string]any{"workflow_path": map[string]any{"type": "string", "description": "Absolute local workflow JSON path; contains job, wait_seconds, artifact selection/destination and scalar report fields"}}, Required: []string{"workflow_path"}}
				}
				visible = append(visible, tool)
			}
			continue
		}
		// The old name misleadingly suggests passive observation. Keep its
		// handler for old callers but never advertise it, even in all mode.
		if tool.Name == "container_attach" {
			continue
		}
		builtin, enabled := false, all
		for group, names := range toolGroups {
			if slices.Contains(names, tool.Name) {
				builtin = true
				enabled = enabled || slices.Contains(groups, group)
			}
		}
		if !builtin || enabled {
			visible = append(visible, tool)
		}
	}
	return visible
}

func (s *mcpServer) registerToolGroups(srv *server.MCPServer) {
	opts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Show or select tool groups. Core is always available. Enable setup, simulator, hardware, robotics, observability, cloud, or all for specialist tools."),
		mcpgo.WithArray("groups", mcpgo.Description("Replace enabled groups; omit to inspect. Core is always included."),
			mcpgo.Items(map[string]any{"type": "string", "enum": toolGroupNames})),
	}
	// Selecting visible definitions changes no host/device resources and grants
	// no authority to invoke them. Each operation keeps its own annotations.
	opts = append(opts, readOnly()...)
	opts = append(opts, localOnly()...)
	srv.AddTool(mcpgo.NewTool("wendy_tools", opts...), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		if value, present := req.GetArguments()["groups"]; present {
			encoded, err := json.Marshal(value)
			var groups []string
			if err != nil || json.Unmarshal(encoded, &groups) != nil || groups == nil {
				return errResult(errCodeInvalidArgument, "groups must be an array of group names"), nil
			}
			before := s.selectedToolGroups()
			if err := s.SetToolGroups(groups); err != nil {
				return errResult(errCodeInvalidArgument, err.Error()), nil
			}
			if !slices.Equal(before, s.selectedToolGroups()) {
				srv.SendNotificationToAllClients(mcpgo.MethodNotificationToolsListChanged, nil)
			}
		}
		var tools []mcpgo.Tool
		for _, entry := range srv.ListTools() {
			tools = append(tools, entry.Tool)
		}
		var names []string
		for _, tool := range s.filterTools(ctx, tools) {
			names = append(names, tool.Name)
		}
		sort.Strings(names)
		return okResult(map[string]any{"groups": s.selectedToolGroups(), "available_groups": toolGroupNames, "tools": names}), nil
	})
}
