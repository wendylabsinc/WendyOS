package mcp

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
)

//go:embed desktop_assets/*.glb
var gatewayAssets embed.FS
var gatewayModels = map[string]bool{"thor": true, "go2": true, "orin": true, "dragonwing": true, "dgx": true, "macbook": true, "generic": true}
var gatewaySettingsMu sync.Mutex
var desktopToolNames = []string{"identify_device", "open_devices", "search_devices", "get_device_model", "read_device_settings", "update_device_settings", "read_device_metrics", "read_device_logs", "list_device_triggers", "configure_device_trigger", "list_device_events", "wait_for_device_event"}

func gatewayModel(r GatewayRobot, deviceType string) string {
	if r.Model != "" {
		return r.Model
	}
	t := strings.NewReplacer("-", " ", "_", " ").Replace(strings.ToLower(deviceType))
	for _, pair := range [][2]string{{"unitree g1", "g1"}, {"dgxspark", "dgx"}, {"dgx spark", "dgx"}, {"raspberry pi", "rpi"}, {"linux desktop", "desktop"}, {"agx thor", "thor"}, {"go2", "go2"}, {"orin nano", "orin"}, {"dragonwing", "dragonwing"}, {"macbook", "macbook"}} {
		if strings.Contains(t, pair[0]) {
			return pair[1]
		}
	}
	return "generic"
}
func (g *RobotGateway) registerDesktopTools() {
	identity := g.withRobot(RobotReadScope, func(ctx context.Context, r *GatewayRobot, s *mcpServer, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		v, err := s.GetConn().AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
		if err != nil {
			return mcpgo.NewToolResultError("Device identity unavailable"), nil
		}
		return okResult(map[string]any{"robot_id": r.ID, "device_type": v.GetDeviceType(), "model": gatewayModel(*r, v.GetDeviceType()), "agent_version": v.GetVersion()}), nil
	})
	g.protocol.AddTool(gatewayTool("identify_device", "Read only agent identity to select an accurate display model. Does not activate cameras or inspect apps.", readOnly(), robotArgument()), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return identity(ctx, req)
	})

	t := gatewayTool("open_devices", "Open the Wendy fleet workspace. Browse authorized devices, inspect a device, and work with its apps and detection events.", readOnly())
	t.Meta = mcpgo.NewMetaFromMap(map[string]any{"ui": map[string]any{"resourceUri": robotPanelURI}, "openai/ui": map[string]any{"entrypoints": []map[string]string{{"type": "global"}}}})
	t.Title = "Devices"
	t.Annotations.Title = "Devices"
	t.Icons = []mcpgo.Icon{{Src: "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(`<svg xmlns="http://www.w3.org/2000/svg" width="20" height="20" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.5"><rect x="2" y="2" width="6" height="6" rx="1.5"/><rect x="12" y="2" width="6" height="6" rx="1.5"/><rect x="2" y="12" width="6" height="6" rx="1.5"/><rect x="12" y="12" width="6" height="6" rx="1.5"/></svg>`)), MIMEType: "image/svg+xml"}}
	g.protocol.AddTool(t, g.listRobots)
	search := gatewayTool("search_devices", "Find authorized devices to mention in a conversation.", readOnly(), mcpgo.WithString("query", mcpgo.MaxLength(128)))
	search.Meta = mcpgo.NewMetaFromMap(map[string]any{"ui": map[string]any{"visibility": []string{"app"}}, "openai/extensions": map[string]any{"mentions/search": map[string]any{}}})
	g.protocol.AddTool(search, func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		result, err := g.listRobots(ctx, req)
		if err != nil || result.IsError {
			return result, err
		}
		rows := result.StructuredContent.(map[string]any)["robots"].([]map[string]any)
		items := []map[string]any{}
		for _, r := range rows {
			items = append(items, map[string]any{"type": "resource_link", "uri": "wendy://devices/" + r["id"].(string), "name": r["name"], "mimeType": "application/json"})
		}
		return okResult(map[string]any{"items": items}), nil
	})
	g.protocol.AddResourceTemplate(mcpgo.NewResourceTemplate("wendy://devices/{id}", "Wendy device"), func(ctx context.Context, req mcpgo.ReadResourceRequest) ([]mcpgo.ResourceContents, error) {
		id := strings.TrimPrefix(req.Params.URI, "wendy://devices/")
		if !gatewayIdentifier.MatchString(id) {
			return nil, fmt.Errorf("invalid device reference")
		}
		r, err := g.authorize(ctx, id, RobotReadScope)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(map[string]any{"robot_id": r.ID, "name": r.Name, "connection": "unknown", "instruction": "Inspect the device for current state."})
		return []mcpgo.ResourceContents{mcpgo.TextResourceContents{URI: req.Params.URI, MIMEType: "application/json", Text: string(b)}}, nil
	})
	model := gatewayTool("get_device_model", "Load an embedded display model. Models are illustrations and do not establish device hardware capabilities.", readOnly(), mcpgo.WithString("model", mcpgo.Required(), mcpgo.Enum("go2", "orin", "dragonwing", "dgx", "macbook", "thor")))
	model.Meta = mcpgo.NewMetaFromMap(map[string]any{"ui": map[string]any{"visibility": []string{"app"}}})
	g.protocol.AddTool(model, func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		if !g.hasScope(ctx, RobotReadScope) {
			return mcpgo.NewToolResultError("Unauthorized"), nil
		}
		id := req.GetString("model", "")
		if !gatewayModels[id] || id == "generic" {
			return mcpgo.NewToolResultError("Unknown model"), nil
		}
		b, err := gatewayAssets.ReadFile("desktop_assets/" + id + ".glb")
		if err != nil {
			return nil, err
		}
		res := okResult(map[string]any{"model": id})
		res.Meta = mcpgo.NewMetaFromMap(map[string]any{"glb": base64.StdEncoding.EncodeToString(b)})
		return res, nil
	})
	for _, kind := range []string{"metrics", "logs"} {
		g.protocol.AddTool(gatewayTool("read_device_"+kind, "Read a bounded current device telemetry sample.", readOnly(), robotArgument()), g.withRobot(RobotReadScope, func(ctx context.Context, r *GatewayRobot, s *mcpServer, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			q := mcpgo.CallToolRequest{}
			q.Params.Arguments = map[string]any{"max_records": 30, "max_batches": 1, "timeout_seconds": 5, "max_bytes": 16000}
			if kind == "logs" {
				return s.handleTelemetryLogs(ctx, q)
			}
			q.Params.Arguments = map[string]any{"last_n": 20, "max_batches": 20, "max_records": 1000, "max_bytes": 200000}
			return s.handleTelemetryMetrics(ctx, q)
		}))
	}
	g.registerGatewaySettings()
}

type gatewaySettingsResult struct {
	Schema    map[string]any `json:"schema"`
	Values    map[string]any `json:"values"`
	CanUpdate bool           `json:"can_update"`
}

func (g *RobotGateway) settingsPath(ctx context.Context) (string, error) {
	_, p := g.grant(ctx)
	base := g.cfg.StateDirectory
	if base == "" {
		d, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(d, "wendy", "robot-gateway")
	}
	h := sha256.Sum256([]byte(p.Subject))
	return filepath.Join(base, fmt.Sprintf("settings-%x.json", h[:16])), nil
}
func (g *RobotGateway) settingsValues(ctx context.Context) (map[string]any, error) {
	values := map[string]any{"show_3d": true, "include_offline": false}
	path, err := g.settingsPath(ctx)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return values, nil
	}
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(b, &values)
	return values, err
}
func (g *RobotGateway) registerGatewaySettings() {
	read := gatewayTool("read_device_settings", "Read your device workspace preferences.", readOnly(), mcpgo.WithOutputSchema[gatewaySettingsResult]())
	g.protocol.AddTool(read, func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		if !g.hasScope(ctx, RobotReadScope) {
			return mcpgo.NewToolResultError("Unauthorized"), nil
		}
		gatewaySettingsMu.Lock()
		defer gatewaySettingsMu.Unlock()
		v, err := g.settingsValues(ctx)
		if err != nil {
			return nil, err
		}
		return okResult(gatewaySettingsResult{Schema: map[string]any{"type": "object", "properties": map[string]any{"include_offline": map[string]any{"type": "boolean", "title": "Include offline enrollments"}}}, Values: v, CanUpdate: g.hasScope(ctx, RobotSettingsScope)}), nil
	})
	update := mcpgo.NewToolWithRawSchema("update_device_settings", "Save your device workspace preferences.", json.RawMessage(`{"type":"object","properties":{"set":{"type":"object","properties":{"show_3d":{"type":"boolean"},"include_offline":{"type":"boolean"}},"additionalProperties":false}},"required":["set"],"additionalProperties":false}`))
	update.Annotations = mcpgo.ToolAnnotation{ReadOnlyHint: mcpgo.ToBoolPtr(false), DestructiveHint: mcpgo.ToBoolPtr(false), IdempotentHint: mcpgo.ToBoolPtr(true), OpenWorldHint: mcpgo.ToBoolPtr(false)}
	update.Meta = mcpgo.NewMetaFromMap(map[string]any{"ui": map[string]any{"visibility": []string{"model", "app"}}, "openai/widgetAccessible": true})
	g.protocol.AddTool(update, func(ctx context.Context, r mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		if !g.hasScope(ctx, RobotSettingsScope) {
			return mcpgo.NewToolResultError("Unauthorized"), nil
		}
		gatewaySettingsMu.Lock()
		defer gatewaySettingsMu.Unlock()
		v, err := g.settingsValues(ctx)
		if err != nil {
			return nil, err
		}
		set, ok := r.GetArguments()["set"].(map[string]any)
		if !ok {
			return mcpgo.NewToolResultError("set must be an object"), nil
		}
		for k, value := range set {
			if _, ok := v[k]; !ok {
				return mcpgo.NewToolResultError("Unknown setting"), nil
			}
			if _, ok := value.(bool); !ok {
				return mcpgo.NewToolResultError("Settings must be boolean"), nil
			}
			v[k] = value
		}
		path, err := g.settingsPath(ctx)
		if err != nil {
			return nil, err
		}
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
		b, _ := json.Marshal(v)
		if err = atomicfile.Write(path, b, 0600); err != nil {
			return nil, err
		}
		return okResult(map[string]any{"values": v}), nil
	})
}
