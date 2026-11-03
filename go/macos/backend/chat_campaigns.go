package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

var campaignChatTools = []chat.Tool{
	{Name: "enable_campaign_notifications", RequiresApproval: true, Description: "After a campaign has been created with notifications enabled, request the user's permission in the app to enable its Wendy Cloud notification grant. Applies to the campaign app across devices in the original device's organization. The signed-in account must have owner/admin permission. Does not upload recordings or prove delivery. Do not ask the user to visit Cloud Apps when this tool is available.", Parameters: json.RawMessage(`{"type":"object","properties":{"campaign":{"type":"string","minLength":1,"maxLength":80}},"required":["campaign"],"additionalProperties":false}`)},
	{Name: "device_campaign_sources", Description: "List recording sources and camera health on the original chat device. Use returned camera IDs when creating a detection campaign.", Parameters: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
	{Name: "device_campaign_list", Description: "List detection/recording campaigns on the original chat device, including inference state, warnings, and notification errors. Deployment alone does not verify detection or notification delivery.", Parameters: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
	{Name: "create_person_detection_campaign", RequiresApproval: true, Description: "Create and arm a NEW person-detection campaign on the original chat device using Wendy's pinned DETR model. Records local clips and may download the runtime/model on the device. Does not replace existing campaigns. When notify is true, the app automatically asks for separate Cloud notification permission after creation. The result reports that permission outcome; do not ask again after denial. Creation does not verify delivery. Requires review. Use camera IDs from device_campaign_sources; '*' means all present and future healthy cameras.", Parameters: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","minLength":1,"maxLength":80,"pattern":"^[A-Za-z0-9][A-Za-z0-9._-]*$","description":"A short identifier such as person-detection; no spaces"},"camera":{"type":"string","minLength":1},"notify":{"type":"boolean"},"upload":{"type":"boolean"},"threshold":{"type":"number","minimum":0,"maximum":1},"cooldown_seconds":{"type":"integer","minimum":0,"maximum":86400},"before_seconds":{"type":"integer","minimum":0,"maximum":300},"after_seconds":{"type":"integer","minimum":1,"maximum":86400}},"required":["name","camera","notify","upload","threshold","cooldown_seconds","before_seconds","after_seconds"],"additionalProperties":false}`)},
}

type deviceCampaignTools struct {
	chat.Executor
	conn                 *grpcclient.AgentConnection
	enableNotifications  func(context.Context, *grpcclient.AgentConnection, string) error
	approveNotifications chat.ApproveFunc
}

func (t *deviceCampaignTools) ListTools(ctx context.Context) ([]chat.Tool, error) {
	all, err := t.Executor.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	return append(all, campaignChatTools...), nil
}
func personCampaignInput(raw []byte) (commands.DesktopCampaignInput, error) {
	var args struct {
		Name      string   `json:"name"`
		Camera    string   `json:"camera"`
		Notify    *bool    `json:"notify"`
		Upload    *bool    `json:"upload"`
		Threshold *float64 `json:"threshold"`
		Cooldown  *uint32  `json:"cooldown_seconds"`
		Before    *uint32  `json:"before_seconds"`
		After     *uint32  `json:"after_seconds"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return commands.DesktopCampaignInput{}, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return commands.DesktopCampaignInput{}, errors.New("expected one campaign object")
	}
	if args.Notify == nil || args.Upload == nil || args.Threshold == nil || args.Cooldown == nil || args.Before == nil || args.After == nil {
		return commands.DesktopCampaignInput{}, errors.New("include every campaign review field")
	}
	if *args.Cooldown > 86400 {
		return commands.DesktopCampaignInput{}, errors.New("cooldown cannot exceed one day")
	}
	in := commands.DesktopCampaignInput{Name: args.Name, Camera: args.Camera, Notify: *args.Notify, Upload: *args.Upload,
		RunInference: true, Threshold: *args.Threshold, Cooldown: *args.Cooldown, Before: *args.Before, After: *args.After}
	_, err := commands.DesktopCampaignYAML(in)
	return in, err
}
func (t *deviceCampaignTools) Execute(ctx context.Context, call chat.ToolCall) (string, error) {
	switch call.Name {
	case "device_campaign_sources", "device_campaign_list", "create_person_detection_campaign", "enable_campaign_notifications":
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if t.conn == nil || t.conn.DataService == nil {
			return "", errors.New("reconnect the original chat device to use campaigns")
		}
	default:
		return t.Executor.Execute(ctx, call)
	}
	var result any
	switch call.Name {
	case "enable_campaign_notifications":
		var args struct {
			Campaign string `json:"campaign"`
		}
		decoder := json.NewDecoder(bytes.NewReader(call.Arguments))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&args); err != nil {
			return "", err
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return "", errors.New("expected one campaign object")
		}
		if args.Campaign == "" || len(args.Campaign) > 80 {
			return "", errors.New("choose an existing campaign")
		}
		enable := t.enableNotifications
		if enable == nil {
			enable = commands.DesktopEnableCampaignNotifications
		}
		if err := enable(ctx, t.conn, args.Campaign); err != nil {
			return "", err
		}
		result = map[string]any{"campaign": args.Campaign, "cloud_notification_permission_enabled": true, "notification_delivery_verified": false,
			"next_step": "Permission is enabled. Check campaign inference status; notification delivery still depends on detection and recipient push registration."}
	case "device_campaign_sources":
		sources, err := t.conn.DataService.Sources(ctx, &pb.DataSourcesRequest{})
		if err != nil {
			return "", err
		}
		result = sources.GetSources()
	case "device_campaign_list":
		campaigns, err := commands.DesktopCampaigns(ctx, t.conn)
		if err != nil {
			return "", err
		}
		result = campaigns
	case "create_person_detection_campaign":
		input, err := personCampaignInput(call.Arguments)
		if err != nil {
			return "", err
		}
		sources, err := t.conn.DataService.Sources(ctx, &pb.DataSourcesRequest{})
		if err != nil {
			return "", err
		}
		found := false
		for _, source := range sources.GetSources() {
			if source.GetKind() == "camera" && source.GetHealthy() && (input.Camera == "*" || input.Camera == source.GetId()) {
				found = true
			}
		}
		if !found {
			return "", errors.New("the selected camera is not currently healthy; refresh campaign sources")
		}
		if err := commands.DesktopDeployCampaign(ctx, t.conn, input); err != nil {
			return "", err
		}
		outcome := map[string]any{"created": input.Name, "notification_delivery_verified": false,
			"next_step": "Read device_campaign_list for inference status and errors. Do not claim notification delivery is verified."}
		if input.Notify {
			outcome["cloud_notification_permission"] = t.requestCreatedCampaignNotifications(ctx, input.Name)
		}
		result = outcome
	}
	raw, err := json.Marshal(result)
	return string(raw), err
}
func (t *deviceCampaignTools) ExecuteResult(ctx context.Context, call chat.ToolCall) (chat.ToolResult, error) {
	for _, tool := range campaignChatTools {
		if call.Name == tool.Name {
			text, err := t.Execute(ctx, call)
			return chat.ToolResult{Text: text}, err
		}
	}
	if media, ok := t.Executor.(chat.MediaExecutor); ok {
		return media.ExecuteResult(ctx, call)
	}
	text, err := t.Executor.Execute(ctx, call)
	return chat.ToolResult{Text: text}, err
}

// The creation flow owns this follow-up so it does not depend on another model
// tool call. The separate approval is scoped to the exact newly created campaign.
func (t *deviceCampaignTools) requestCreatedCampaignNotifications(ctx context.Context, name string) map[string]any {
	if t.approveNotifications == nil {
		return map[string]any{"status": "not_requested", "reason": "Chat approval is unavailable; no Cloud permission was changed."}
	}
	args, _ := json.Marshal(map[string]string{"campaign": name})
	allowed, err := t.approveNotifications(ctx, chat.ToolCall{Name: "enable_campaign_notifications", Arguments: args})
	if err != nil {
		return map[string]any{"status": "cancelled", "reason": err.Error()}
	}
	if !allowed {
		return map[string]any{"status": "denied", "next_step": "Respect this decision. Do not ask again or call enable_campaign_notifications unless the user explicitly requests it later."}
	}
	if err := ctx.Err(); err != nil {
		return map[string]any{"status": "cancelled", "reason": err.Error()}
	}
	enable := t.enableNotifications
	if enable == nil {
		enable = commands.DesktopEnableCampaignNotifications
	}
	if err := enable(ctx, t.conn, name); err != nil {
		return map[string]any{"status": "failed", "reason": err.Error(), "next_step": "Explain the failure. The campaign was created but notification permission was not enabled."}
	}
	return map[string]any{"status": "enabled", "notification_delivery_verified": false}
}
