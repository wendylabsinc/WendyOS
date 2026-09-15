package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/assets"
)

const guideText = `Wendy MCP Guide
===============

Wendy manages remote Linux devices (edge servers, embedded boards, cloud VMs).
Every MCP session has at most one active device connection at a time.

## Getting started

Call wendy_status first — it tells you whether you are connected and what to do next.

## Connecting to a device

Local/direct devices:
1. device_list          — lists devices from config (add scan=true for live mDNS scan)
2. device_connect       — connect by address (host:port), e.g. "mydevice.local:50051"

Cloud-enrolled devices:
1. cloud_discover       — finds devices enrolled in your Wendy Cloud account
2. cloud_connect        — opens a secure tunnel to a cloud device by name

## Tools available once connected

- device_info — agent, OS, hardware, storage, and battery information
- container_list / container_start / container_stop / container_delete / container_attach / container_exec / container_stats
- wifi_list / wifi_connect / wifi_disconnect / wifi_status / wifi_known_networks
- telemetry_logs / telemetry_metrics / telemetry_traces
- hardware_capabilities
- ros2_topics / ros2_topic_info / ros2_topic_sample / ros2_topic_hz / ros2_lidar_summary
- os_update
- provisioning_start / provisioning_status

Use container_exec with app_name and an explicit command argument array to run
a bounded command in an existing container through the active direct or cloud
connection. Use telemetry_logs for passive logs. container_attach starts or
restarts the app and can interrupt its running task; it is not a command shell.

Host↔device file sync happens automatically as part of ` + "`wendy run`" + `'s
fast redeploy path — there is no standalone file-sync CLI command or MCP tool.

## Robot and sensor diagnostics

For battery level and charge state, call device_info. Its battery object contains
percent (0–100), state, and optional seconds_remaining until empty (discharging)
or full (charging). A missing battery means the agent has no reading; a missing
seconds_remaining means no estimate is available. This uses the agent's battery
API and does not require a running ROS 2 app or container.

Use ros2_topics to discover sensor and odometry topics and ros2_topic_info to
inspect a topic's publishers and QoS. Prefer ros2_lidar_summary for standard
PointCloud2 and LaserScan messages: it subscribes with sensor-compatible QoS,
decodes points on the device, and returns compact sector nearest returns,
bounded XYZ samples, and frame, source timestamp, clock, and coverage diagnostics.
Use ros2_topic_sample or ros2_topic_hz for other finite observations.
Scope defaults to app and preserves its isolation.
If no ROS 2 app is running but the robot/host publishes sensor DDS data, explicitly
set scope="host" and the known domain_id on each inspection tool. This uses a
standalone ROS Humble/FastRTPS inspector; first use may download its image.
A domain override alone never enables host scope. Custom message samples require
compatible typesupport in an app image or a robot adapter.

Discover actual LiDAR topics; /utlidar/cloud_deskewed and /scan are examples,
not assumed capabilities. For robot-relative sectors, verify a body frame and
request it with target_frame; the tool resolves TF at the measurement timestamp.
An odom/map cloud's axes are not necessarily the robot's axes. Choose min_z/max_z
in the output frame and inspect filtering, point limits, and missing sectors.
Do not assign directional meanings to undocumented /utlidar/range_info fields.
source_age_seconds is a signed offset against clock_basis, not proof of clock
synchronization or freshness. Use use_sim_time=true for a verified ROS simulation
clock; never compare ROS source timestamps to the chat/session clock.
The LiDAR tool requires an updated local CLI and device agent. Read
wendy://docs/integrations/ros2.mdx for argument examples.

No samples means unknown, not an empty obstacle field. A received sample or
rate measurement does not establish sensor acquisition freshness, localization
quality, coverage, or readiness to move. Reception times are not sensor times.
Use the robot app's readiness and navigation tools when it provides them.

Examples/RobotNavigation is a deployable app providing robot_status, supervised
navigation_goal/status/renew/cancel, robot_stop, robot_observe, robot_targets,
and approach_person. Its default configuration disables motor output. It needs
standard scan/odometry/IMU inputs, calibrated RGB-D for person targets, and a
commissioned local driver/watchdog before motion. Read-only status calls do not
renew navigation leases; cancellation is confirmed only after the action ends
and fresh odometry establishes rest. Use fresh app target IDs for person goals.

Apps with an mcp entitlement expose tools under an app-name prefix. Their tools
are refreshed as the active device and running apps change. Check wendy_status
or wendy://diagnostics when an expected app tool is unavailable.

Read wendy://docs/integrations/robot-diagnostics.mdx for the diagnostic workflow
and the boundary between remote observation and local robot control.

## Deploying a workload

Use the run tool to build and deploy a local project to a cloud-enrolled device:
  run(project_path="/path/to/project", device_name="mydevice")

## Disconnecting

device_disconnect — closes the active connection and frees resources.

## Entitlements

Apps declare the device capabilities they need (gpu, network, persistence,
camera, bluetooth, etc.) in their project's wendy.json. The device only
grants what's declared there — if a container needs a capability that isn't
entitled, the operation fails (or the container exits) with error_code
ENTITLEMENT_DENIED, and container_list surfaces it in termination_reason for
stopped containers.

## Result shape & error codes

Tools that return data include a machine-readable structuredContent object
alongside human-readable text. List-shaped tools nest their rows under a
single key — devices (device_list, cloud_discover, bluetooth_scan),
containers, stats, capabilities, networks (wifi_list, wifi_known_networks),
batches (telemetry_*) — never as a bare array. Errors include an error_code you can branch
on — e.g. NOT_CONNECTED, DEVICE_UNREACHABLE, ENTITLEMENT_DENIED,
INVALID_ARGUMENT, NOT_FOUND, MULTIPLE_SESSIONS, UNSUPPORTED, TIMEOUT,
INTERNAL. Tool annotations mark read-only vs destructive vs mutating
operations and whether a tool reaches beyond the connected device (open-world).

## Documentation

Detailed documentation is available as MCP resources under wendy://docs/.
Run resources/list to see all available docs.
`

func (s *mcpServer) registerGuideResource(srv *server.MCPServer) {
	srv.AddResource(
		mcpgo.NewResource("wendy://guide", "Wendy Guide",
			mcpgo.WithResourceDescription("Overview of Wendy MCP tools, connection model, and common workflows. Read this first."),
			mcpgo.WithMIMEType("text/plain"),
		),
		s.handleGuideResource,
	)
	s.registerDocResources(srv)
}

func (s *mcpServer) handleGuideResource(_ context.Context, _ mcpgo.ReadResourceRequest) ([]mcpgo.ResourceContents, error) {
	return []mcpgo.ResourceContents{
		mcpgo.TextResourceContents{URI: "wendy://guide", MIMEType: "text/plain", Text: guideText},
	}, nil
}

// registerDiagnosticsResource registers wendy://diagnostics, which reports
// container-MCP proxy failures that would otherwise only appear on stderr.
func (s *mcpServer) registerDiagnosticsResource(srv *server.MCPServer) {
	srv.AddResource(
		mcpgo.NewResource("wendy://diagnostics", "Wendy MCP Diagnostics",
			mcpgo.WithResourceDescription("Container-MCP proxy failures recorded during this session (app name, stage, error, time), as JSON."),
			mcpgo.WithMIMEType("application/json"),
		),
		s.handleDiagnosticsResource,
	)
}

func (s *mcpServer) handleDiagnosticsResource(_ context.Context, _ mcpgo.ReadResourceRequest) ([]mcpgo.ResourceContents, error) {
	data, err := json.Marshal(s.proxyDiagnostics())
	if err != nil {
		return nil, err
	}
	return []mcpgo.ResourceContents{
		mcpgo.TextResourceContents{URI: "wendy://diagnostics", MIMEType: "application/json", Text: string(data)},
	}, nil
}

func (s *mcpServer) registerDocResources(srv *server.MCPServer) {
	_ = fs.WalkDir(assets.FS, "docs", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		// Both plain-Markdown reference docs (.md) and the embedded MDX guide
		// content (.mdx, e.g. docs/integrations/ros2.mdx) are exposed here —
		// an MCP client has no other way to read either.
		ext := path.Ext(p)
		if ext != ".md" && ext != ".mdx" {
			return nil
		}
		relPath := strings.TrimPrefix(p, "docs/")
		uri := "wendy://docs/" + relPath
		name := docTitle(relPath)
		resource := mcpgo.NewResource(uri, name,
			mcpgo.WithResourceDescription(fmt.Sprintf("Wendy documentation: %s", relPath)),
			mcpgo.WithMIMEType("text/markdown"),
		)
		embeddedPath := p
		srv.AddResource(resource, func(_ context.Context, req mcpgo.ReadResourceRequest) ([]mcpgo.ResourceContents, error) {
			data, readErr := assets.FS.ReadFile(embeddedPath)
			if readErr != nil {
				return nil, readErr
			}
			return []mcpgo.ResourceContents{
				mcpgo.TextResourceContents{URI: req.Params.URI, MIMEType: "text/markdown", Text: string(data)},
			}, nil
		})
		return nil
	})
}

// docTitle converts a relative doc path to a human-readable title.
func docTitle(relPath string) string {
	base := path.Base(relPath)
	base = strings.TrimSuffix(base, ".mdx")
	base = strings.TrimSuffix(base, ".md")
	base = strings.ReplaceAll(base, "-", " ")
	base = strings.ReplaceAll(base, "_", " ")
	dir := path.Dir(relPath)
	if dir == "." {
		return base
	}
	return dir + " / " + base
}
