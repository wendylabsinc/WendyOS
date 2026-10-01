package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"slices"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func (g *RobotGateway) inspect(ctx context.Context, r *GatewayRobot, s *mcpServer, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	conn := s.GetConn()
	// These independent inventories share the same authenticated connection.
	// Start them together so Cloud round trips do not add to one another.
	type appResult struct {
		apps []map[string]any
		err  error
	}
	appsReady := make(chan appResult, 1)
	go func() {
		appCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		apps, err := gatewayApps(appCtx, r, s)
		appsReady <- appResult{apps, err}
	}()
	type cameraResult struct {
		response *agentpb.ListVideoDevicesResponse
		err      error
	}
	camerasReady := make(chan cameraResult, 1)
	if r.AllowCamera && g.hasScope(ctx, RobotCameraScope) {
		go func() {
			cameraCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			response, err := conn.VideoService.ListVideoDevices(cameraCtx, &agentpb.ListVideoDevicesRequest{})
			camerasReady <- cameraResult{response, err}
		}()
	} else {
		camerasReady <- cameraResult{}
	}
	versionCtx, finishVersion := context.WithTimeout(ctx, 10*time.Second)
	v, err := conn.AgentService.GetAgentVersion(versionCtx, &agentpb.GetAgentVersionRequest{})
	finishVersion()
	if err != nil {
		return mcpgo.NewToolResultError("Robot did not respond to the status check."), nil
	}
	out := map[string]any{"robot_id": r.ID, "name": r.Name, "connected": true, "agent_version": v.GetVersion(), "device_type": v.GetDeviceType(), "model": gatewayModel(*r, v.GetDeviceType()), "observed_at": time.Now().UTC().Format(time.RFC3339Nano)}
	warnings := []string{}
	appState := <-appsReady
	if appState.err != nil {
		warnings = append(warnings, "App state unavailable.")
	} else {
		if !g.hasScope(ctx, RobotControlScope) {
			for _, app := range appState.apps {
				app["can_control"] = false
			}
		}
		out["apps"] = appState.apps
	}
	cameras := []map[string]any{}
	cameraState := <-camerasReady
	if cameraState.err != nil {
		warnings = append(warnings, "Camera inventory unavailable.")
	} else {
		for _, c := range cameraState.response.GetDevices() {
			cameras = append(cameras, map[string]any{"id": c.GetId(), "name": c.GetName()})
		}
	}
	out["cameras"], out["warnings"] = cameras, warnings
	out["can_open_apps"] = ctx.Value(gatewayLocalContextKey{}) == true && g.hasScope(ctx, RobotToolsScope)
	return okResultBounded(out, 100000), nil
}

func gatewayApps(ctx context.Context, r *GatewayRobot, s *mcpServer) ([]map[string]any, error) {
	stream, err := s.GetConn().ContainerService.ListContainers(ctx, &agentpb.ListContainersRequest{})
	if err != nil {
		return nil, err
	}
	apps := []map[string]any{}
	for n := 0; n < 1000; n++ {
		resp, err := stream.Recv()
		if err == io.EOF {
			return apps, nil
		}
		if err != nil {
			return nil, err
		}
		c := resp.GetContainer()
		if c == nil || (!r.discovered && !r.ListAllApps && !r.AllowAllApps && !slices.Contains(r.Apps, c.GetAppName())) {
			continue
		}
		apps = append(apps, map[string]any{"name": c.GetAppName(), "version": c.GetAppVersion(), "state": c.GetRunningState().String(), "failure_count": c.GetFailureCount(), "readiness": "unknown", "http_port": c.GetHttpPort(), "can_control": r.AllowAllApps || slices.Contains(r.Apps, c.GetAppName())})
	}
	return nil, fmt.Errorf("app inventory exceeded the gateway limit")
}

func (g *RobotGateway) snapshot(ctx context.Context, r *GatewayRobot, s *mcpServer, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if !r.AllowCamera {
		return mcpgo.NewToolResultError("Camera access is disabled for this robot."), nil
	}
	id, err := snapshotInteger(req, "camera_id", -1, 0, 4294967295)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	var capture mcpgo.CallToolRequest
	capture.Params.Arguments = map[string]any{"device_id": id, "timeout_seconds": 15}
	result, err := s.handleCameraSnapshot(ctx, capture)
	if err != nil || result.IsError {
		return mcpgo.NewToolResultError("Camera capture failed. Check camera availability and GStreamer on the gateway."), nil
	}
	meta, ok := result.StructuredContent.(map[string]any)
	if !ok {
		return mcpgo.NewToolResultError("Camera returned invalid metadata."), nil
	}
	meta["robot_id"] = r.ID
	meta["camera_id"] = id
	encoded, _ := json.Marshal(meta)
	result.Content[0] = mcpgo.NewTextContent(string(encoded))
	return result, nil
}

func (g *RobotGateway) controlApp(ctx context.Context, r *GatewayRobot, s *mcpServer, req mcpgo.CallToolRequest, action string) (*mcpgo.CallToolResult, error) {
	app := req.GetString("app_name", "")
	if r.AllowAllApps {
		apps, err := gatewayApps(ctx, r, s)
		if err != nil {
			return mcpgo.NewToolResultError("Could not verify installed apps. No app operation was started."), nil
		}
		if !slices.ContainsFunc(apps, func(installed map[string]any) bool { return installed["name"] == app }) {
			return mcpgo.NewToolResultError("This app is not installed on the selected device."), nil
		}
	} else if !slices.Contains(r.Apps, app) {
		return mcpgo.NewToolResultError("App is not permitted for this robot."), nil
	}
	conn := s.GetConn()
	var operationErr error
	if action == "start" {
		// Stop reading after Started. Closing the attachment does not stop the app.
		startCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		stream, err := conn.ContainerService.StartContainer(startCtx, &agentpb.StartContainerRequest{AppName: app})
		operationErr = err
		if err == nil {
			for n := 0; n < 1000; n++ {
				resp, err := stream.Recv()
				if err != nil {
					if err != io.EOF {
						operationErr = err
					}
					break
				}
				if resp.GetStarted() != nil {
					break
				}
			}
		}
		cancel()
	} else {
		_, operationErr = conn.ContainerService.StopContainer(ctx, &agentpb.StopContainerRequest{AppName: app})
	}
	out := map[string]any{"robot_id": r.ID, "app_name": app, "action": action, "state": "unknown", "readiness": "unknown", "observed_at": time.Now().UTC().Format(time.RFC3339Nano)}
	apps, err := gatewayApps(ctx, r, s)
	if err == nil {
		for _, a := range apps {
			if a["name"] == app {
				out["state"] = a["state"]
				out["failure_count"] = a["failure_count"]
			}
		}
	}
	want := agentpb.AppRunningState_RUNNING.String()
	if action == "stop" {
		want = agentpb.AppRunningState_STOPPED.String()
	}
	out["state_verified"] = out["state"] == want
	result := okResult(out)
	if operationErr != nil || out["state"] != want {
		out["note"] = "Requested state was not confirmed. Inspect before deciding whether to retry."
		result = okResult(out)
		result.IsError = true
	}
	return result, nil
}

// No remote schema references: an app descriptor must not turn compilation
// into an HTTP request or filesystem read from the gateway.
type gatewaySchemaLoader struct{}

func (gatewaySchemaLoader) Load(string) (any, error) {
	return nil, fmt.Errorf("external schema references are not supported")
}

func (g *RobotGateway) registerExport(robot GatewayRobot, export GatewayExport) error {
	b, err := json.Marshal(export.Tool)
	if err != nil {
		return err
	}
	var descriptor map[string]any
	if err := json.Unmarshal(b, &descriptor); err != nil {
		return err
	}
	input, ok := descriptor["inputSchema"].(map[string]any)
	if !ok {
		return fmt.Errorf("export %s needs an object input schema", export.Name)
	}
	// Preserve local references such as #/$defs/Message when the app schema
	// moves under arguments. Do not mutate the descriptor used for drift checks.
	appSchema := make(map[string]any, len(input)+1)
	for key, value := range input {
		appSchema[key] = value
	}
	if _, exists := appSchema["$id"]; !exists {
		appSchema["$id"] = "urn:wendy:app-input:" + export.Name
	}
	schema := map[string]any{"type": "object", "properties": map[string]any{"robot_id": map[string]any{"type": "string", "const": robot.ID}, "arguments": appSchema}, "required": []string{"robot_id", "arguments"}, "additionalProperties": false}
	raw, _ := json.Marshal(schema)
	var jsonSchema any
	if err := json.Unmarshal(raw, &jsonSchema); err != nil {
		return err
	}
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(gatewaySchemaLoader{})
	const schemaURI = "urn:wendy:app-tool"
	if err := compiler.AddResource(schemaURI, jsonSchema); err != nil {
		return err
	}
	compiled, err := compiler.Compile(schemaURI)
	if err != nil {
		return fmt.Errorf("export %s schema: %w", export.Name, err)
	}
	t := mcpgo.NewToolWithRawSchema(export.Name, fmt.Sprintf("On robot %s, app %s: %s", robot.ID, export.App, export.Tool.Description), raw)
	t.Annotations = export.Tool.Annotations
	// App metadata cannot add UI entrypoints, identity claims, or hidden tools.
	g.protocol.AddTool(t, g.withRobot(RobotToolsScope, func(ctx context.Context, r *GatewayRobot, s *mcpServer, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		if r.ID != robot.ID {
			return mcpgo.NewToolResultError("This tool belongs to a different robot."), nil
		}
		if err := compiled.Validate(req.GetArguments()); err != nil {
			return mcpgo.NewToolResultError("Arguments do not match the approved app tool schema."), nil
		}
		client, err := g.connectApp(ctx, ctx, s.GetConn(), export.App)
		if err != nil {
			return mcpgo.NewToolResultError("App MCP server is unavailable. Inspect its state."), nil
		}
		defer client.Close()
		var list mcpgo.ListToolsRequest
		found := false
		for page := 0; page < 10; page++ {
			tools, err := client.ListTools(ctx, list)
			if err != nil {
				return mcpgo.NewToolResultError("Could not verify the app tool contract."), nil
			}
			for _, current := range tools.Tools {
				if current.Name != export.Tool.Name {
					continue
				}
				got, _ := json.Marshal(current)
				var currentDescriptor map[string]any
				if err := json.Unmarshal(got, &currentDescriptor); err != nil || !reflect.DeepEqual(descriptor, currentDescriptor) {
					return mcpgo.NewToolResultError("The app tool changed. The gateway operator must review and update its export before it can run."), nil
				}
				found = true
			}
			if found || tools.NextCursor == "" {
				break
			}
			list.Params.Cursor = tools.NextCursor
		}
		if !found {
			return mcpgo.NewToolResultError("The approved tool is no longer advertised by this app."), nil
		}
		var call mcpgo.CallToolRequest
		call.Params.Name = export.Tool.Name
		call.Params.Arguments = req.GetArguments()["arguments"]
		result, err := client.CallTool(ctx, call)
		if err != nil || result == nil {
			return mcpgo.NewToolResultError("App tool did not return a result. Its outcome is unknown; do not retry a write automatically."), nil
		}
		result.Meta = nil
		return capProxiedResult(result, defaultProxyMaxBytes), nil
	}))
	return nil
}
