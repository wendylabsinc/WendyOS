package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

const deviceMentionPrefix = "wendy://devices/"
const deviceMentionLimit = 100

type deviceMentionResult struct {
	Items []mcpgo.ResourceLink `json:"items"`
}

func (g *RobotGateway) registerDeviceMentions() {
	search := gatewayTool("search_devices", "Find authorized Wendy devices by name or ID to mention in a conversation, including offline Cloud enrollments. An empty query lists devices; narrow the query for fleets larger than 100 devices.", readOnly(),
		mcpgo.WithString("query", mcpgo.Required(), mcpgo.MaxLength(128), mcpgo.Description("Device name or ID; may be empty.")),
		mcpgo.WithOutputSchema[deviceMentionResult]())
	search.Meta.AdditionalFields["ui"] = map[string]any{"visibility": []string{"app"}}
	search.Meta.AdditionalFields["openai/extensions"] = map[string]any{"mentions/search": map[string]any{}}
	g.protocol.AddTool(search, g.searchDeviceMentions)
	g.protocol.AddResourceTemplate(mcpgo.NewResourceTemplate(deviceMentionPrefix+"{id}", "Wendy device",
		mcpgo.WithTemplateMIMEType("application/json"),
		mcpgo.WithTemplateDescription("Authorized device identity for a composer mention. Use its robot_id for device tools.")), g.readDeviceMention)
}

func (g *RobotGateway) searchDeviceMentions(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if !g.hasScope(ctx, RobotReadScope) {
		return mcpgo.NewToolResultError("robot access is not authorized"), nil
	}
	// A mention can refer to an offline enrollment. Listing identity must not
	// probe an agent or change a connection shared with another conversation.
	rows, warnings := g.catalog(ctx, true)
	query := strings.ToLower(strings.TrimSpace(req.GetString("query", "")))
	items := make([]mcpgo.ResourceLink, 0, min(len(rows), deviceMentionLimit))
	matches := 0
	for _, row := range rows {
		if query != "" && !strings.Contains(strings.ToLower(row.Name), query) && !strings.Contains(strings.ToLower(row.ID), query) {
			continue
		}
		matches++
		if len(items) < deviceMentionLimit {
			// Names need not be unique; keep the stable ID visible without
			// exposing transport selectors, addresses, or credentials.
			items = append(items, mcpgo.NewResourceLink(deviceMentionPrefix+row.ID, row.Name,
				fmt.Sprintf("Wendy device %s (%s)", row.ID, row.source), "application/json"))
		}
	}
	return &mcpgo.CallToolResult{
		Content:           []mcpgo.Content{},
		StructuredContent: deviceMentionResult{Items: items},
		// Keep the extension's structured result exactly {items: [...]}, as in
		// the OpenAI SDK. Discovery diagnostics belong to host-only metadata.
		Result: mcpgo.Result{Meta: mcpgo.NewMetaFromMap(map[string]any{
			"discovery_complete": len(warnings) == 0, "warnings": listOrEmpty(warnings),
			"total_count": matches, "truncated": matches > len(items),
		})},
	}, nil
}

func (g *RobotGateway) readDeviceMention(ctx context.Context, req mcpgo.ReadResourceRequest) ([]mcpgo.ResourceContents, error) {
	id, ok := strings.CutPrefix(req.Params.URI, deviceMentionPrefix)
	if !ok || !gatewayIdentifier.MatchString(id) {
		return nil, fmt.Errorf("invalid device reference")
	}
	// Always reauthorize: a saved mention must not retain access after the
	// enrollment disappears from the caller's Cloud inventory.
	r, err := g.authorize(ctx, id, RobotReadScope)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(map[string]any{
		"robot_id": r.ID, "name": r.Name, "connection": "unknown",
		"instruction": "Use this robot_id for device tools. Call inspect_robot for current state, or open_robot to open this device's workspace.",
	})
	if err != nil {
		return nil, err
	}
	return []mcpgo.ResourceContents{mcpgo.TextResourceContents{URI: req.Params.URI, MIMEType: "application/json", Text: string(b)}}, nil
}
