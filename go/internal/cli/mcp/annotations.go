package mcp

import (
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// mcp-go's NewTool applies the MCP spec's pessimistic default annotations to
// every tool: ReadOnlyHint=false, DestructiveHint=true, IdempotentHint=false,
// OpenWorldHint=true. Relying on "absence" therefore advertises every tool as
// destructive and open-world. The helpers below set each axis EXPLICITLY so a
// tool's advertised behavior never depends on those hidden defaults.
//
// Convention: every registered tool applies exactly one behavior helper
// (readOnly / mutating / destructive) and exactly one world helper
// (localOnly / openWorld); idempotent() is an optional modifier for mutating
// tools (readOnly already implies idempotent).

// readOnly marks a tool that only reads state: not destructive, and idempotent
// by nature (repeated reads have no additional effect).
func readOnly() []mcpgo.ToolOption {
	return []mcpgo.ToolOption{
		mcpgo.WithReadOnlyHintAnnotation(true),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(true),
	}
}

// mutating marks a tool that changes state but not destructively — it does not
// remove or irreversibly alter existing data (e.g. starting an app, connecting).
func mutating() []mcpgo.ToolOption {
	return []mcpgo.ToolOption{
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(false),
	}
}

// destructive marks a tool that may perform irreversible or disruptive updates
// (e.g. deleting a container, updating the OS, dropping connectivity).
func destructive() []mcpgo.ToolOption {
	return []mcpgo.ToolOption{
		mcpgo.WithReadOnlyHintAnnotation(false),
		mcpgo.WithDestructiveHintAnnotation(true),
	}
}

// idempotent marks a mutating tool where repeated identical calls have no extra
// effect beyond the first. (readOnly already sets this; do not stack the two.)
func idempotent() []mcpgo.ToolOption {
	return []mcpgo.ToolOption{mcpgo.WithIdempotentHintAnnotation(true)}
}

// openWorld marks a tool that interacts with an open set of external entities
// the server does not own — a Wendy device, the local network, nearby radios,
// or the cloud broker (e.g. container control, mDNS scans, cloud discovery).
func openWorld() []mcpgo.ToolOption {
	return []mcpgo.ToolOption{mcpgo.WithOpenWorldHintAnnotation(true)}
}

// localOnly marks a tool whose domain of interaction is closed: it operates
// only on the local host (e.g. CLI status, local config, local simulators).
func localOnly() []mcpgo.ToolOption {
	return []mcpgo.ToolOption{mcpgo.WithOpenWorldHintAnnotation(false)}
}

// toolTitles are the human-readable names MCP clients and app directories show
// for each tool. Every registered tool needs one (TestEveryToolHasTitleAndHints).
var toolTitles = map[string]string{
	"experiment_plan":       "Plan Experiment",
	"job_submit":            "Submit Training Job",
	"compute_status":        "Inspect Compute Capacity",
	"checkpoint_inspect":    "Inspect Training Checkpoint",
	"job_inspect":           "Inspect Training Job",
	"job_artifacts_pull":    "Pull Job Artifacts",
	"app_inspect":           "Inspect App",
	"auth_login":            "Sign In to Wendy Cloud",
	"bluetooth_connect":     "Connect Bluetooth Device",
	"bluetooth_disconnect":  "Disconnect Bluetooth Device",
	"bluetooth_scan":        "Scan for Bluetooth Devices",
	"camera_controls":       "List Camera Controls",
	"camera_list":           "List Cameras",
	"camera_set_control":    "Set Camera Control",
	"camera_snapshot":       "Take Camera Snapshot",
	"cloud_connect":         "Connect to Cloud Device",
	"cloud_discover":        "Discover Cloud Devices",
	"cloud_enroll_device":   "Enroll Device in Wendy Cloud",
	"cloud_ping":            "Ping Cloud Device",
	"cloud_tunnel":          "Open Cloud Tunnel",
	"cloud_tunnel_close":    "Close Cloud Tunnel",
	"cloud_tunnel_list":     "List Cloud Tunnels",
	"container_attach":      "Attach to Container",
	"container_delete":      "Delete Container",
	"container_exec":        "Run Command in Container",
	"container_list":        "List Containers",
	"container_start":       "Start Container",
	"container_stats":       "Show Container Stats",
	"container_stop":        "Stop Container",
	"device_connect":        "Connect to Device",
	"device_disconnect":     "Disconnect from Device",
	"device_info":           "Show Device Info",
	"device_list":           "List Devices",
	"device_os_logs":        "Read Device OS Logs",
	"device_set_default":    "Set Default Device",
	"device_update_agent":   "Update Device Agent",
	"hardware_capabilities": "List Hardware Capabilities",
	"os_install_plan":       "Plan OS Install",
	"os_install_resume":     "Resume OS Install",
	"os_install_start":      "Start OS Install",
	"os_install_status":     "Show OS Install Status",
	"os_install_verify":     "Verify OS Install",
	"os_list_drives":        "List Drives",
	"os_update":             "Update Device OS",
	"os_update_status":      "Show OS Update Status",
	"project_validate":      "Validate Project",
	"provisioning_start":    "Start Device Provisioning",
	"provisioning_status":   "Show Provisioning Status",
	"ros2_lidar_summary":    "Summarize ROS 2 Lidar",
	"ros2_topic_hz":         "Measure ROS 2 Topic Rate",
	"ros2_topic_info":       "Inspect ROS 2 Topic",
	"ros2_topic_sample":     "Sample ROS 2 Topic",
	"ros2_topics":           "List ROS 2 Topics",
	"run":                   "Build and Run App",
	"simulator_create":      "Create Simulator",
	"simulator_delete":      "Delete Simulator",
	"simulator_list":        "List Simulators",
	"simulator_start":       "Start Simulator",
	"simulator_stop":        "Stop Simulator",
	"telemetry_logs":        "Read Device Logs",
	"telemetry_metrics":     "Read Device Metrics",
	"telemetry_traces":      "Read Device Traces",
	"wendy_status":          "Show Wendy Status",
	"wendy_tools":           "Manage Wendy Tool Groups",
	"wifi_connect":          "Connect Device to Wi-Fi",
	"wifi_disconnect":       "Disconnect Device from Wi-Fi",
	"wifi_known_networks":   "List Known Wi-Fi Networks",
	"wifi_list":             "Scan Wi-Fi Networks",
	"wifi_status":           "Show Wi-Fi Status",
}

// applyToolTitles sets each registered tool's title, both top level and in
// its annotations: clients and directories read either.
func applyToolTitles(srv *server.MCPServer) {
	var titled []server.ServerTool
	for name, t := range srv.ListTools() {
		t.Tool.Title = toolTitles[name]
		t.Tool.Annotations.Title = toolTitles[name]
		titled = append(titled, *t)
	}
	srv.AddTools(titled...)
}
