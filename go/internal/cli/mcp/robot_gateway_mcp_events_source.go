package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

const gatewayMCPNotificationEvent = "wendy.data.notification"

func (g *RobotGateway) registerMCPNotificationTool() {
	g.protocol.AddTool(gatewayTool("read_device_notifications", "Read durable immediate Wendy Data campaign notifications (notify:on detection or event). Metadata only, without starting inference or cameras. An empty cursor starts at the current tail unless replay is true; gap means retained history was lost. Use MCP event wendy.data.notification for ChatGPT background monitoring. Cloud episode_committed notifications are separate.", readOnly(), robotArgument(),
		mcpgo.WithString("campaign", mcpgo.MaxLength(128)), mcpgo.WithString("event", mcpgo.MaxLength(128)), mcpgo.WithString("cursor", mcpgo.MaxLength(100)), mcpgo.WithBoolean("replay")), func(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		arguments, _ := json.Marshal(gatewayMCPEventArguments{RobotID: request.GetString("robot_id", ""), Campaign: request.GetString("campaign", ""), Event: request.GetString("event", "")})
		batch, err := g.readMCPEventSource(ctx, gatewayMCPNotificationEvent, arguments, request.GetString("cursor", ""), request.GetBool("replay", false))
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		items := make([]map[string]any, 0, len(batch.Events))
		for _, event := range batch.Events {
			items = append(items, event.Data)
		}
		return okResult(map[string]any{"robot_id": request.GetString("robot_id", ""), "notifications": items, "cursor": batch.Cursor, "gap": batch.Truncated}), nil
	})
}

type gatewayMCPEventArguments struct {
	RobotID  string `json:"robot_id"`
	Campaign string `json:"campaign,omitempty"`
	Event    string `json:"event,omitempty"`
}

type gatewayMCPEventOccurrence struct {
	ID        string
	Timestamp string
	Data      map[string]any
	Cursor    string
}
type gatewayMCPEventBatch struct {
	Events    []gatewayMCPEventOccurrence
	Cursor    string
	Truncated bool
}

func (g *RobotGateway) mcpEventDefinitions(ctx context.Context) []map[string]any {
	definitions := []map[string]any{}
	if !g.hasScope(ctx, RobotEventsScope) {
		return definitions
	}
	properties := map[string]any{}
	for _, name := range []string{"robot_id", "notification_id", "campaign", "event", "source_id", "model", "model_revision", "occurred_at"} {
		properties[name] = map[string]any{"type": "string"}
	}
	properties["count"] = map[string]any{"type": "integer", "minimum": 0}
	definitions = append(definitions, map[string]any{
		"name":        gatewayMCPNotificationEvent,
		"description": "An immediate Wendy Data campaign notification emitted by notify:on detection or notify:on event, including YOLO detections. Metadata only; subscribing does not start a camera or deploy inference. Requires a device agent supporting the notification journal. Cloud episode_committed notifications are not included.",
		"delivery":    []string{"webhook"},
		"inputSchema": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"robot_id"}, "properties": map[string]any{
			"robot_id": map[string]any{"type": "string", "minLength": 1, "maxLength": 64, "description": "Exact authorized device ID from list_robots."},
			"campaign": map[string]any{"type": "string", "minLength": 1, "maxLength": 128, "description": "Optional exact Wendy Data campaign name; omit to monitor all campaigns on this device."},
			"event":    map[string]any{"type": "string", "minLength": 1, "maxLength": 128, "description": "Optional exact notification event name, for example person_detected."},
		}},
		"payloadSchema": map[string]any{"type": "object", "additionalProperties": false, "properties": properties, "required": []string{"robot_id", "notification_id", "campaign", "event", "source_id", "model", "model_revision", "count", "occurred_at"}},
	})
	return definitions
}

func gatewayMCPDecode(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid event arguments")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("expected one JSON object")
	}
	return nil
}

var gatewayMCPEventFilterPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

func gatewayMCPParseArguments(name string, raw json.RawMessage) (gatewayMCPEventArguments, error) {
	var args gatewayMCPEventArguments
	if name != gatewayMCPNotificationEvent {
		return args, fmt.Errorf("unknown event")
	}
	if err := gatewayMCPDecode(raw, &args); err != nil {
		return args, err
	}
	var supplied map[string]json.RawMessage
	_ = json.Unmarshal(raw, &supplied)
	for _, key := range []string{"campaign", "event"} {
		if value, ok := supplied[key]; ok && (string(value) == "null" || string(value) == `""`) {
			return args, fmt.Errorf("optional event filters must be nonempty strings")
		}
	}
	if !gatewayIdentifier.MatchString(args.RobotID) || (args.Campaign != "" && !gatewayMCPEventFilterPattern.MatchString(args.Campaign)) || (args.Event != "" && !gatewayMCPEventFilterPattern.MatchString(args.Event)) {
		return args, fmt.Errorf("invalid device, campaign, or event filter")
	}
	return args, nil
}

func (g *RobotGateway) authorizeMCPEvent(ctx context.Context, name string, raw json.RawMessage) error {
	args, err := gatewayMCPParseArguments(name, raw)
	if err != nil {
		return err
	}
	_, err = g.authorize(ctx, args.RobotID, RobotEventsScope)
	return err
}

func (g *RobotGateway) readMCPEventSource(ctx context.Context, name string, raw json.RawMessage, cursor string, replay bool) (gatewayMCPEventBatch, error) {
	var batch gatewayMCPEventBatch
	args, err := gatewayMCPParseArguments(name, raw)
	if err != nil {
		return batch, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	robot, err := g.authorize(ctx, args.RobotID, RobotEventsScope)
	if err != nil {
		return batch, err
	}
	connection, err := g.connect(ctx, robot.Device)
	if err != nil {
		return batch, fmt.Errorf("device notification source unavailable")
	}
	defer connection.Close()
	if connection.DataService == nil {
		return batch, fmt.Errorf("update this device agent to expose Wendy Data notifications")
	}
	appID := ""
	if args.Campaign != "" {
		appID = "sh.wendy.campaign." + args.Campaign
	}
	response, err := connection.DataService.Events(ctx, &agentpbv2.DataEventsRequest{AppId: appID, Event: args.Event, Cursor: cursor, Replay: replay, NotificationsOnly: true})
	if err != nil {
		return batch, fmt.Errorf("device notification source unavailable")
	}
	// An older agent silently ignores unknown protobuf fields. Never mistake
	// its ordinary event journal for the actual Wendy Data notification stream.
	if !response.Notifications {
		return batch, fmt.Errorf("update this device agent to expose Wendy Data notifications")
	}
	var notifications []data.CampaignNotification
	if len(response.EventsJson) > 4<<20 || json.Unmarshal(response.EventsJson, &notifications) != nil || len(notifications) > 512 {
		return batch, fmt.Errorf("invalid device notification response")
	}
	epoch, _, ok := strings.Cut(response.Cursor, ":")
	if !ok || len(response.Cursor) > 100 {
		return batch, fmt.Errorf("invalid device notification cursor")
	}
	batch.Cursor, batch.Truncated = response.Cursor, response.Gap
	for _, notification := range notifications {
		if (args.Campaign != "" && notification.Campaign != args.Campaign) || (args.Event != "" && notification.Event != args.Event) {
			return gatewayMCPEventBatch{}, fmt.Errorf("device ignored notification filter")
		}
		if notification.ID == "" || notification.Sequence == 0 || notification.Count < 0 {
			return gatewayMCPEventBatch{}, fmt.Errorf("invalid device notification")
		}
		if _, err := time.Parse(time.RFC3339Nano, notification.OccurredAt); err != nil {
			return gatewayMCPEventBatch{}, fmt.Errorf("invalid device notification time")
		}
		batch.Events = append(batch.Events, gatewayMCPEventOccurrence{ID: notification.ID, Timestamp: notification.OccurredAt, Cursor: epoch + ":" + strconv.FormatUint(notification.Sequence, 10), Data: map[string]any{
			"robot_id": args.RobotID, "notification_id": notification.ID, "campaign": notification.Campaign, "event": notification.Event, "source_id": notification.SourceID, "model": notification.Model, "model_revision": notification.Revision, "count": notification.Count, "occurred_at": notification.OccurredAt,
		}})
	}
	return batch, nil
}
