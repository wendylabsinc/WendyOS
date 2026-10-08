package mcp

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudlink"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
)

//go:embed desktop_app.html
var robotPanelHTML string

// Hosts cache UI resources by URI. Each built bundle needs its own identity so
// an updated descriptor cannot reuse an older workspace's HTML or JavaScript.
var robotPanelURI = fmt.Sprintf("ui://wendy/device-workspace-%x.html", sha256.Sum256([]byte(robotPanelHTML)))

const legacyRobotPanelHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Refresh Wendy</title>
<style>:root{color-scheme:light dark}body{margin:0;padding:32px;font:16px/1.5 system-ui,sans-serif;color:light-dark(#171c23,#f1eee7);background:light-dark(#fff,#1e242d)}main{max-width:560px;margin:0 auto}h1{font-size:22px;font-weight:600}p{margin:16px 0}</style></head>
<body><main><h1>Refresh Wendy</h1><p>This saved Wendy view uses an older app version.</p><p>In ChatGPT, open Plugins → Wendy → Manage app → Refresh tools. Then close this view and open Wendy in a new conversation.</p></main>
<script>
(() => {
  const id = "wendy-upgrade-initialize";
  const ready = (event) => {
    if (event.source !== window.parent || event.data?.jsonrpc !== "2.0" || event.data.id !== id || !event.data.result) return;
    window.removeEventListener("message", ready);
    window.parent.postMessage({jsonrpc:"2.0",method:"ui/notifications/initialized"}, "*");
  };
  window.addEventListener("message", ready);
  window.parent.postMessage({jsonrpc:"2.0",id,method:"ui/initialize",params:{protocolVersion:"2026-01-26",appInfo:{name:"Wendy update",version:"1.0.0"},appCapabilities:{}}}, "*");
})();
</script></body></html>`

type gatewayPrincipal struct {
	Subject string
	Scopes  []string
}
type gatewayPrincipalKey struct{}

// RobotGateway has no mutable current device. Connections belong to one call.
type RobotGateway struct {
	webSlots          chan struct{}
	sceneMu           sync.Mutex
	scenes            map[string]*gatewaySimulatorScene
	previewMu         sync.Mutex
	previews          map[string]*gatewayCameraPreview
	lifecycle         *mcpServer
	jobMu             sync.Mutex
	workspaceMu       sync.Mutex
	jobCancels        map[string]context.CancelFunc
	cfg               RobotGatewayConfig
	connect           ConnectFunc
	protocol          *server.MCPServer
	mcpEvents         *gatewayMCPEventManager
	eventAuthenticate gatewayAuthenticate
	slots             chan struct{}
	waitSlots         chan struct{}
	connectApp        func(context.Context, context.Context, *grpcclient.AgentConnection, string) (appMCPClient, error)
	discover          GatewayCloudDiscoverFunc
	cloudLink         *cloudlink.Manager
}

func NewRobotGateway(cfg RobotGatewayConfig, connect ConnectFunc, options ...RobotGatewayOption) (*RobotGateway, error) {
	// Own the policy, including nested schemas, so caller mutation cannot widen it.
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var owned RobotGatewayConfig
	if err := json.Unmarshal(raw, &owned); err != nil {
		return nil, err
	}
	cfg = owned
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if connect == nil {
		return nil, fmt.Errorf("a device connector is required")
	}
	g := &RobotGateway{cfg: cfg, connect: connect, slots: make(chan struct{}, 16), connectApp: connectAppMCP}
	g.waitSlots = make(chan struct{}, 32)
	for _, option := range options {
		option(g)
	}
	if len(cfg.CloudSources) > 0 && g.discover == nil {
		return nil, fmt.Errorf("Cloud sources require an authenticated discovery provider")
	}
	hooks, taskHooks := gatewayTaskHooks()
	hooks.AddAfterInitialize(func(ctx context.Context, _ any, _ *mcpgo.InitializeRequest, result *mcpgo.InitializeResult) {
		if !g.hasScope(ctx, RobotSettingsScope) {
			// Capabilities and the filtered tool list must agree. Never offer a
			// host settings editor whose save tool the subject cannot discover.
			experimental := map[string]any{}
			for key, value := range result.Capabilities.Experimental {
				if key != "openai/settings" {
					experimental[key] = value
				}
			}
			result.Capabilities.Experimental = experimental
		}
	})
	g.protocol = server.NewMCPServer("wendy-robots", version.Version,
		server.WithHooks(hooks), server.WithTaskHooks(taskHooks),
		server.WithTaskCapabilities(false, true, true), server.WithMaxConcurrentTasks(32),
		server.WithToolCapabilities(false), server.WithResourceCapabilities(false, false),
		server.WithInputSchemaValidation(), server.WithStrictInputSchemaDefault(),
		server.WithToolFilter(g.filterTools),
		server.WithExperimental(map[string]any{"openai/settings": map[string]any{"readTool": "read_device_settings", "updateTool": "update_device_settings"}}),
		server.WithInstructions("Use list_robots to select an authorized robot_id. Call inspect_robot before operating apps. Camera images are finite snapshots; never describe them as live. App running state does not prove readiness. Only explicitly exported app tools are available. Never retry an uncertain write automatically."))
	if err := registerSkills(g.protocol); err != nil {
		return nil, err
	}
	g.registerTools()
	g.registerDesktopTools()
	g.registerCameraPreviewTools()
	g.registerAppWebTool()
	g.registerEventTools()
	g.registerLifecycleTools()
	g.registerSimulatorSceneResources()
	g.registerWorkspaceFileTools()
	g.registerYOLOTools()
	if err := g.initMCPEvents(); err != nil {
		return nil, err
	}
	for _, r := range cfg.Robots {
		for _, e := range r.Exports {
			if err := g.registerExport(r, e); err != nil {
				return nil, err
			}
		}
	}
	meta := map[string]any{"ui": map[string]any{"csp": map[string]any{"connectDomains": []string{}, "resourceDomains": []string{"data:", "blob:"}}}, "openai/ui": map[string]any{"availableDisplayModes": []string{"fullscreen"}, "preferredDisplayMode": "fullscreen"}}
	if cfg.AllowSimulators || cfg.AllowHostOperations {
		meta["ui"].(map[string]any)["csp"].(map[string]any)["connectDomains"] = []string{"http://127.0.0.1:*"}
	}
	resource := mcpgo.NewResource(robotPanelURI, "Wendy robot panel", mcpgo.WithMIMEType("text/html;profile=mcp-app"))
	resource.Meta = mcpgo.NewMetaFromMap(meta)
	g.protocol.AddResource(resource, func(ctx context.Context, _ mcpgo.ReadResourceRequest) ([]mcpgo.ResourceContents, error) {
		if !g.hasScope(ctx, RobotReadScope) {
			return nil, fmt.Errorf("robot access is not authorized")
		}
		return []mcpgo.ResourceContents{mcpgo.TextResourceContents{URI: robotPanelURI, MIMEType: resource.MIMEType, Text: robotPanelHTML, Meta: meta}}, nil
	})
	// Old descriptors may retain an old tool scope. Never run a new bundle under
	// their cached URI or issue newer helper calls from these compatibility views.
	legacyMeta := map[string]any{"ui": map[string]any{"csp": map[string]any{"connectDomains": []string{}, "resourceDomains": []string{}}}}
	for _, uri := range []string{"ui://wendy/robot-v1.html", "ui://wendy/device-workspace-v2.html", "ui://wendy/device-workspace-v3.html"} {
		legacy := mcpgo.NewResource(uri, "Wendy devices", mcpgo.WithMIMEType("text/html;profile=mcp-app"))
		legacy.Meta = mcpgo.NewMetaFromMap(legacyMeta)
		g.protocol.AddResource(legacy, func(ctx context.Context, req mcpgo.ReadResourceRequest) ([]mcpgo.ResourceContents, error) {
			if !g.hasScope(ctx, RobotReadScope) {
				return nil, fmt.Errorf("unauthorized")
			}
			return []mcpgo.ResourceContents{mcpgo.TextResourceContents{URI: req.Params.URI, MIMEType: legacy.MIMEType, Text: legacyRobotPanelHTML, Meta: legacyMeta}}, nil
		})
	}
	return g, nil
}

func (g *RobotGateway) StartStdio(ctx context.Context) error {
	if g.cfg.LocalSubject == "" {
		return fmt.Errorf("stdio requires local_subject with an explicit grant")
	}
	ctx = context.WithValue(ctx, gatewayLocalContextKey{}, true)
	return g.listenGatewayStdio(context.WithValue(ctx, gatewayPrincipalKey{}, gatewayPrincipal{g.cfg.LocalSubject, robotGatewayScopes}), os.Stdin, os.Stdout)
}

// StartEventDelivery binds durable subscription delivery to the server lifetime.
// HTTP callers invoke this after configuring authentication, before serving.
func (g *RobotGateway) StartEventDelivery(ctx context.Context) error {
	return g.mcpEvents.start(ctx)
}

func (g *RobotGateway) StopEventDelivery() {
	g.mcpEvents.close()
}

func (g *RobotGateway) CloseCloudLink() {
	if g.cloudLink != nil {
		g.cloudLink.Close()
	}
}

func (g *RobotGateway) grant(ctx context.Context) (*GatewayGrant, gatewayPrincipal) {
	p, _ := ctx.Value(gatewayPrincipalKey{}).(gatewayPrincipal)
	if g.cloudLink != nil && p.Subject != "" {
		return &GatewayGrant{Subject: p.Subject, Scopes: g.cfg.HTTP.CloudLink.Scopes}, p
	}
	for i := range g.cfg.Grants {
		if p.Subject != "" && g.cfg.Grants[i].Subject == p.Subject {
			return &g.cfg.Grants[i], p
		}
	}
	return nil, p
}

func (g *RobotGateway) hasScope(ctx context.Context, scope string) bool {
	grant, p := g.grant(ctx)
	return grant != nil && slices.Contains(grant.Scopes, scope) && slices.Contains(p.Scopes, scope)
}

func (g *RobotGateway) authorize(ctx context.Context, id, scope string) (*GatewayRobot, error) {
	grant, _ := g.grant(ctx)
	if !g.hasScope(ctx, scope) {
		return nil, fmt.Errorf("robot or operation is not authorized")
	}
	for i := range g.cfg.Robots {
		if g.cfg.Robots[i].ID == id {
			if !slices.Contains(grant.Robots, id) {
				return nil, fmt.Errorf("robot or operation is not authorized")
			}
			return &g.cfg.Robots[i], nil
		}
	}
	// Project access to discovered simulators still requires the caller's local
	// simulator permission. The workspace separately opts in to these targets.
	if (scope == RobotProjectScope || scope == RobotDeployScope) && g.localSimulatorAllowed(ctx) {
		rows, _ := g.catalog(ctx, false)
		for _, row := range rows {
			if row.ID == id && row.source == "simulator" {
				return &row.GatewayRobot, nil
			}
		}
	}
	// Discovery permits inspection by default. Camera capture and app control
	// each require an explicit source opt-in as well as the caller's scope.
	// App tool exports still require a reviewed, explicit robot configuration.
	if scope == RobotReadScope || scope == RobotCameraScope || scope == RobotControlScope || scope == RobotEventsScope || scope == RobotTriggerScope {
		rows, warnings := g.catalog(ctx, true)
		for _, row := range rows {
			allowed := scope == RobotReadScope || scope == RobotEventsScope || ((scope == RobotCameraScope || scope == RobotTriggerScope) && row.AllowCamera) || (scope == RobotControlScope && row.AllowAllApps)
			if row.ID == id && allowed {
				return &row.GatewayRobot, nil
			}
		}
		if len(warnings) > 0 {
			return nil, fmt.Errorf("Could not refresh Cloud authorization for this robot. Try discovery again.")
		}
	}
	return nil, fmt.Errorf("robot or operation is not authorized")
}

func (g *RobotGateway) toolScope(name string) string {
	if strings.HasPrefix(name, "simulator_") {
		return RobotSimulatorScope
	}
	if slices.Contains(gatewayHostTools, name) {
		return RobotHostScope
	}
	switch name {
	case "list_workspaces", "validate_device_project", "plan_fleet_deployment", "list_workspace_files", "read_workspace_file":
		return RobotProjectScope
	case "write_workspace_file":
		return RobotProjectWriteScope
	case "start_device_deployment", "get_deployment_job", "cancel_deployment_job":
		return RobotDeployScope
	case "identify_device", "list_robots", "open_robot", "inspect_robot", "open_devices", "search_devices", "read_device_metrics", "read_device_logs", "read_device_settings":
		return RobotReadScope
	case "list_device_events", "wait_for_device_event", "list_device_triggers", "inspect_yolo_detector", "read_device_notifications":
		return RobotEventsScope
	case "update_device_settings":
		return RobotSettingsScope
	case "configure_device_trigger", "deploy_yolo_detector", "stop_yolo_detector":
		return RobotTriggerScope
	case "capture_robot_image", "start_camera_preview", "read_camera_preview", "stop_camera_preview":
		return RobotCameraScope
	case "start_robot_app", "stop_robot_app":
		return RobotControlScope
	case "open_robot_app":
		return RobotToolsScope
	default:
		return RobotToolsScope
	}
}

func (g *RobotGateway) filterTools(ctx context.Context, tools []mcpgo.Tool) []mcpgo.Tool {
	out := make([]mcpgo.Tool, 0, len(tools))
	for _, t := range tools {
		if slices.Contains(gatewayWorkspaceFileTools, t.Name) && !g.localWorkspaceAllowed(ctx, g.toolScope(t.Name)) {
			continue
		}
		if t.Name == "open_robot_app" && ctx.Value(gatewayLocalContextKey{}) != true {
			continue
		}
		isSimulator := strings.HasPrefix(t.Name, "simulator_")
		if isSimulator && !g.localSimulatorAllowed(ctx) {
			continue
		}
		if !isSimulator && slices.Contains(gatewayHostTools, t.Name) && !g.localHostAllowed(ctx) {
			continue
		}
		if !g.hasScope(ctx, g.toolScope(t.Name)) && !(isSimulator && g.localHostAllowed(ctx)) {
			continue
		}
		allowed := true
		for _, r := range g.cfg.Robots {
			for _, e := range r.Exports {
				if e.Name == t.Name {
					_, err := g.authorize(ctx, r.ID, RobotToolsScope)
					allowed = err == nil
				}
			}
		}
		if allowed {
			// mcp-go preserves extension fields in _meta. Advertise the scope
			// through the documented compatibility field without mutating the
			// shared descriptor or claiming OAuth for a private stdio pilot.
			if g.cfg.HTTP != nil && g.cfg.HTTP.OAuth != nil {
				meta := map[string]any{}
				if t.Meta != nil {
					for k, v := range t.Meta.AdditionalFields {
						meta[k] = v
					}
				}
				scopes := []string{g.toolScope(t.Name)}
				if t.Name == "deploy_yolo_detector" {
					scopes = append(scopes, RobotCameraScope)
				}
				meta["securitySchemes"] = []map[string]any{{"type": "oauth2", "scopes": scopes}}
				t.Meta = mcpgo.NewMetaFromMap(meta)
			}
			out = append(out, t)
		}
	}
	return out
}

func gatewayTool(name, description string, behavior []mcpgo.ToolOption, props ...mcpgo.ToolOption) mcpgo.Tool {
	opts := append([]mcpgo.ToolOption{mcpgo.WithDescription(description)}, behavior...)
	opts = append(opts, localOnly()...)
	opts = append(opts, props...)
	t := mcpgo.NewTool(name, opts...)
	t.Meta = mcpgo.NewMetaFromMap(map[string]any{"ui": map[string]any{"visibility": []string{"model", "app"}}, "openai/widgetAccessible": true})
	return t
}

func robotArgument() mcpgo.ToolOption {
	return mcpgo.WithString("robot_id", mcpgo.Required(), mcpgo.MinLength(1), mcpgo.MaxLength(64), mcpgo.Description("Exact robot ID from list_robots"))
}

func (g *RobotGateway) registerTools() {
	g.protocol.AddTool(gatewayTool("list_robots", "Discover robots from authorized Wendy Cloud inventories and configured devices. Defaults to online Cloud devices; set include_offline for all enrolled devices. Follow next_offset for additional pages, or search by name with query. Report warnings when discovery_complete is false. Permission flags are not verified hardware capabilities. Does not probe agents or activate cameras.", readOnly(),
		mcpgo.WithBoolean("include_offline", mcpgo.Description("Include offline Cloud enrollments")),
		mcpgo.WithString("query", mcpgo.MaxLength(128), mcpgo.Description("Filter by robot name or ID")),
		mcpgo.WithInteger("offset", mcpgo.Min(0)), mcpgo.WithInteger("limit", mcpgo.Min(1), mcpgo.Max(200))), g.listRobots)
	open := gatewayTool("open_robot", "Open Wendy beside the conversation or in the sidebar. Optionally select a robot from list_robots. Does not activate cameras.", readOnly(), mcpgo.WithString("robot_id", mcpgo.MaxLength(64)))
	open.Meta = mcpgo.NewMetaFromMap(map[string]any{"ui": map[string]any{"resourceUri": robotPanelURI}, "openai/ui": map[string]any{"entrypoints": []map[string]string{{"type": "thread"}}}})
	g.protocol.AddTool(open, func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		result, _ := g.listRobots(ctx, req)
		if result.IsError {
			return result, nil
		}
		if id := req.GetString("robot_id", ""); id != "" {
			r, err := g.authorize(ctx, id, RobotReadScope)
			if err != nil {
				return mcpgo.NewToolResultError(err.Error()), nil
			}
			data := result.StructuredContent.(map[string]any)
			rows := data["robots"].([]map[string]any)
			if !slices.ContainsFunc(rows, func(row map[string]any) bool { return row["id"] == id }) {
				source := "configured"
				if strings.HasPrefix(r.Device, "vm:") {
					source = "simulator"
				} else if r.discovered {
					source = "cloud"
				}
				data["robots"] = append(rows, map[string]any{"id": r.ID, "name": r.Name, "source": source, "connection": "unknown", "can_capture": r.AllowCamera && g.hasScope(ctx, RobotCameraScope), "can_control_apps": (r.AllowAllApps || len(r.Apps) > 0) && g.hasScope(ctx, RobotControlScope)})
			}
			result.StructuredContent.(map[string]any)["selected_robot_id"] = id
			return okResult(result.StructuredContent), nil
		}
		return result, nil
	})
	g.protocol.AddTool(gatewayTool("inspect_robot", "Read this robot's connection, cameras, and permitted app states. Unknown readiness stays unknown; does not activate cameras.", readOnly(), robotArgument()), g.withRobot(RobotReadScope, g.inspect))
	g.protocol.AddTool(gatewayTool("capture_robot_image", "Activate the selected camera for one fresh still image, then close capture. Use when the user asks to look through the robot's camera. This is not live video.", mutating(), robotArgument(), mcpgo.WithInteger("camera_id", mcpgo.Required(), mcpgo.Min(0), mcpgo.Max(4294967295))), g.withRobot(RobotCameraScope, g.snapshot))
	for _, action := range []string{"start", "stop"} {
		g.protocol.AddTool(gatewayTool(action+"_robot_app", action+" an explicitly permitted robot app and inspect its resulting state. Starting may restart an existing app and interrupt work. Do not retry an uncertain result automatically.", destructive(), robotArgument(), mcpgo.WithString("app_name", mcpgo.Required(), mcpgo.MinLength(1), mcpgo.MaxLength(256))), g.withRobot(RobotControlScope, func(ctx context.Context, r *GatewayRobot, s *mcpServer, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			return g.controlApp(ctx, r, s, req, action)
		}))
	}
}

type gatewayOperation func(context.Context, *GatewayRobot, *mcpServer, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error)

func (g *RobotGateway) withRobot(scope string, op gatewayOperation) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		r, err := g.authorize(ctx, req.GetString("robot_id", ""), scope)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		select {
		case g.slots <- struct{}{}:
			defer func() { <-g.slots }()
		default:
			return mcpgo.NewToolResultError("Gateway is busy; no operation was started."), nil
		}
		conn, err := g.connect(ctx, r.Device)
		if err != nil {
			return mcpgo.NewToolResultError("Could not connect to this robot. Check its Wendy connection and Cloud credentials on the gateway."), nil
		}
		defer conn.Close()
		s := New(&config.Config{}, nil)
		s.SetConn(conn)
		return op(ctx, r, s, req)
	}
}
