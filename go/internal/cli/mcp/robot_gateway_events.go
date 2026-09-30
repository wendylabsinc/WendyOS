package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/yaml.v3"
)

func validateGatewayTrigger(t GatewayTrigger) error {
	if t.CampaignYAML == "" {
		return nil
	}
	c, err := data.ParseCampaign([]byte(t.CampaignYAML))
	if err != nil {
		return fmt.Errorf("trigger %s: %w", t.ID, err)
	}
	if c.Inference == nil || t.AppID != "sh.wendy.campaign."+c.Name || t.Event != c.Inference.Event || c.Notify != nil {
		return fmt.Errorf("trigger %s must identify its managed inference campaign and omit external notifications", t.ID)
	}
	return nil
}
func findGatewayTrigger(r *GatewayRobot, id string) (GatewayTrigger, error) {
	for _, t := range r.Triggers {
		if t.ID == id {
			return t, nil
		}
	}
	return GatewayTrigger{}, fmt.Errorf("trigger is not authorized for this device")
}

func (g *RobotGateway) registerEventTools() {
	prop := mcpgo.WithString("trigger_id", mcpgo.Required(), mcpgo.MinLength(1), mcpgo.MaxLength(64))
	g.protocol.AddTool(gatewayTool("list_device_triggers", "List approved detection triggers and whether their device campaign is deployed. Listing does not activate cameras.", readOnly(), robotArgument()), g.withRobot(RobotEventsScope, func(ctx context.Context, r *GatewayRobot, s *mcpServer, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		out := []map[string]any{}
		for _, t := range r.Triggers {
			row := map[string]any{"id": t.ID, "name": t.Name, "event": t.Event, "app_id": t.AppID, "managed": t.CampaignYAML != "", "state": "external_app", "can_configure": g.hasScope(ctx, RobotTriggerScope) && r.AllowCamera}
			if t.CampaignYAML != "" {
				c, _ := data.ParseCampaign([]byte(t.CampaignYAML))
				v, err := s.GetConn().DataService.CampaignInspect(ctx, &agentpbv2.DataCampaignInspectRequest{Name: c.Name})
				if err != nil {
					row["state"] = "not_verified"
				} else {
					row["state"] = v.State
					row["plan"] = json.RawMessage(v.PlanJson)
					row["warnings"] = v.Warnings
				}
			}
			out = append(out, row)
		}
		return okResult(map[string]any{"robot_id": r.ID, "triggers": out}), nil
	}))
	g.protocol.AddTool(gatewayTool("configure_device_trigger", "Enable or disable an approved persistent detection campaign. Enabling activates its cameras and inference on the device. Disabling stops inference; the approved capture policy can still retain or record data. External app triggers are managed by that app. This does not start a ChatGPT background turn.", mutating(), robotArgument(), prop, mcpgo.WithBoolean("enabled", mcpgo.Required())), g.withRobot(RobotTriggerScope, func(ctx context.Context, r *GatewayRobot, s *mcpServer, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		t, err := findGatewayTrigger(r, req.GetString("trigger_id", ""))
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		if !r.AllowCamera || t.CampaignYAML == "" {
			return mcpgo.NewToolResultError("This trigger cannot be configured through this gateway."), nil
		}
		enabled, ok := req.GetArguments()["enabled"].(bool)
		if !ok {
			return mcpgo.NewToolResultError("enabled must be boolean"), nil
		}
		c, err := data.ParseCampaign([]byte(t.CampaignYAML))
		if err != nil {
			return nil, err
		}
		c.Inference.Enabled = &enabled
		raw, err := yaml.Marshal(c)
		if err != nil {
			return nil, err
		}
		v, err := s.GetConn().DataService.CampaignDeploy(ctx, &agentpbv2.DataCampaignDeployRequest{CampaignYaml: raw})
		if err != nil {
			return mcpgo.NewToolResultError("Campaign deployment was not confirmed. Inspect its state before retrying."), nil
		}
		return okResult(map[string]any{"robot_id": r.ID, "trigger_id": t.ID, "enabled": enabled, "state": v.State, "revision": v.Revision, "warnings": v.Warnings, "readiness": "unknown"}), nil
	}))
	for _, name := range []string{"list_device_events", "wait_for_device_event"} {
		opts := []mcpgo.ToolOption{robotArgument(), prop, mcpgo.WithString("cursor", mcpgo.MaxLength(100)), mcpgo.WithBoolean("replay", mcpgo.Description("Read retained history instead of starting at the current tail"))}
		desc := "Read the device's retained detection events. Keep the returned cursor; gap means history was lost. Empty cursor starts now unless replay=true."
		if name == "wait_for_device_event" {
			opts = append(opts, mcpgo.WithInteger("timeout_seconds", mcpgo.Min(1), mcpgo.Max(300)), mcpgo.WithTaskSupport(mcpgo.TaskSupportOptional))
			desc = "Wait for a detection and return it as a tool result for the host to deliver to ChatGPT. Use task augmentation when supported for waits over 25 seconds. This cannot create a turn in a disconnected host. Resume with the returned cursor. Does not activate a trigger or capture an image."
		}
		g.protocol.AddTool(gatewayTool(name, desc, readOnly(), opts...), g.deviceEvents)
	}
}
func (g *RobotGateway) deviceEvents(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if req.Params.Task != nil {
		if req.Params.Meta == nil {
			return mcpgo.NewToolResultError("Missing task context"), nil
		}
		state, ok := req.Params.Meta.AdditionalFields[gatewayTaskStateKey].(*gatewayTaskState)
		if !ok {
			return mcpgo.NewToolResultError("Invalid task context"), nil
		}
		ctx = state.ctx
	}
	r, err := g.authorize(ctx, req.GetString("robot_id", ""), RobotEventsScope)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	t, err := findGatewayTrigger(r, req.GetString("trigger_id", ""))
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	seconds, err := snapshotInteger(req, "timeout_seconds", 25, 1, 300)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if req.Params.Task == nil {
		seconds = min(seconds, 25)
	}
	cursor := req.GetString("cursor", "")
	if len(cursor) > 100 {
		return mcpgo.NewToolResultError("Invalid cursor"), nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	// Long waits have a separate budget and never consume control-operation slots.
	select {
	case g.waitSlots <- struct{}{}:
		defer func() { <-g.waitSlots }()
	default:
		return mcpgo.NewToolResultError("Too many event waits; no wait started."), nil
	}
	conn, err := g.connect(ctx, r.Device)
	if err != nil {
		return mcpgo.NewToolResultError("Device connection unavailable."), nil
	}
	defer conn.Close()
	if conn.DataService == nil {
		return mcpgo.NewToolResultError("Device event API unavailable; update the agent."), nil
	}
	for {
		resp, err := conn.DataService.Events(ctx, &agentpbv2.DataEventsRequest{AppId: t.AppID, Event: t.Event, Cursor: cursor, Replay: req.GetBool("replay", false)})
		if err != nil {
			if ctx.Err() != nil {
				return okResult(map[string]any{"robot_id": r.ID, "trigger_id": t.ID, "status": "timeout", "cursor": cursor, "events": []any{}}), nil
			}
			if status.Code(err) == codes.Unimplemented {
				return mcpgo.NewToolResultError("Update this device's agent to enable its durable event inbox."), nil
			}
			return mcpgo.NewToolResultError("Event inbox unavailable. Resume with the previous cursor; do not treat this as no detections."), nil
		}
		var events []data.DeviceEvent
		if len(resp.EventsJson) > 4<<20 || json.Unmarshal(resp.EventsJson, &events) != nil {
			return mcpgo.NewToolResultError("Invalid event response"), nil
		}
		cursor = resp.Cursor
		if len(events) > 0 || resp.Gap || req.Params.Name == "list_device_events" {
			return okResult(map[string]any{"robot_id": r.ID, "trigger_id": t.ID, "status": "observed", "events": events, "cursor": cursor, "gap": resp.Gap, "evidence": "Detection-time event metadata. A fresh snapshot is a separate observation."}), nil
		}
		select {
		case <-ctx.Done():
			return okResult(map[string]any{"robot_id": r.ID, "trigger_id": t.ID, "status": "timeout", "events": []any{}, "cursor": cursor}), nil
		case <-time.After(time.Second):
		}
	}
}
