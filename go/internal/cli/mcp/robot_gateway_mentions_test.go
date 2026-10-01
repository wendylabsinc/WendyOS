package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
)

func mentionItems(t *testing.T, result *mcpgo.CallToolResult, err error) []mcpgo.ResourceLink {
	t.Helper()
	if err != nil || result == nil || result.IsError {
		t.Fatalf("mention search: %v %v", result, err)
	}
	if len(result.Content) != 0 {
		t.Fatalf("mention content must be an empty array: %#v", result.Content)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil || len(envelope) != 1 || envelope["items"] == nil {
		t.Fatalf("mention result must contain only items: %s", raw)
	}
	var output deviceMentionResult
	if err := json.Unmarshal(raw, &output); err != nil || output.Items == nil {
		t.Fatalf("items must be an array: %s, %v", raw, err)
	}
	for _, item := range output.Items {
		if item.Type != "resource_link" || !strings.HasPrefix(item.URI, deviceMentionPrefix) || item.Name == "" || item.MIMEType != "application/json" {
			t.Fatalf("invalid mention resource: %+v", item)
		}
	}
	return output.Items
}

func TestGatewayDeviceMentionsContract(t *testing.T) {
	g, err := NewRobotGateway(gatewayTestConfig(), func(context.Context, string) (*grpcclient.AgentConnection, error) {
		t.Error("mention search or resource read connected to a device")
		return nil, fmt.Errorf("unexpected connection")
	})
	if err != nil {
		t.Fatal(err)
	}
	h := gatewayTestHTTP(t, g)
	alice := gatewayHTTPClient(t, h.URL, gatewayTestEnv("ALICE_TOKEN"))
	bob := gatewayHTTPClient(t, h.URL, gatewayTestEnv("BOB_TOKEN"))
	ctx := context.Background()
	catalog, err := alice.ListTools(ctx, mcpgo.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var search *mcpgo.Tool
	for i := range catalog.Tools {
		if catalog.Tools[i].Name == "search_devices" {
			search = &catalog.Tools[i]
		}
	}
	if search == nil {
		t.Fatal("missing mention search tool")
	}
	wire, _ := json.Marshal(search)
	var descriptor struct {
		Meta struct {
			Extensions map[string]json.RawMessage `json:"openai/extensions"`
			UI         struct {
				Visibility []string `json:"visibility"`
			} `json:"ui"`
		} `json:"_meta"`
		OutputSchema struct {
			Type       string                     `json:"type"`
			Required   []string                   `json:"required"`
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"outputSchema"`
	}
	if err := json.Unmarshal(wire, &descriptor); err != nil {
		t.Fatal(err)
	}
	if string(descriptor.Meta.Extensions["mentions/search"]) != "{}" || !slices.Equal(descriptor.Meta.UI.Visibility, []string{"app"}) {
		t.Fatalf("invalid extension metadata: %s", wire)
	}
	if !slices.Contains(search.InputSchema.Required, "query") || len(search.InputSchema.Properties) != 1 {
		t.Fatalf("query must be the sole required input: %s", wire)
	}
	if descriptor.OutputSchema.Type != "object" || !slices.Contains(descriptor.OutputSchema.Required, "items") || descriptor.OutputSchema.Properties["items"] == nil {
		t.Fatalf("missing mention result schema: %s", wire)
	}
	if search.Annotations.ReadOnlyHint == nil || !*search.Annotations.ReadOnlyHint {
		t.Fatal("search must be read only")
	}
	for _, query := range []string{"", "  aLPHa  ", "alpha"} {
		result, err := alice.CallTool(ctx, callToolReq("search_devices", map[string]any{"query": query}))
		items := mentionItems(t, result, err)
		if len(items) != 1 || items[0].URI != "wendy://devices/alpha" || items[0].Name != "Alpha" || !strings.Contains(items[0].Description, "alpha") {
			t.Fatalf("search %q: %+v", query, items)
		}
		var read mcpgo.ReadResourceRequest
		read.Params.URI = items[0].URI
		resource, err := alice.ReadResource(ctx, read)
		if err != nil || len(resource.Contents) != 1 {
			t.Fatalf("read selected mention: %v %v", resource, err)
		}
		content, ok := resource.Contents[0].(mcpgo.TextResourceContents)
		if !ok || content.URI != items[0].URI || content.MIMEType != items[0].MIMEType {
			t.Fatalf("resource identity changed: %+v", content)
		}
		var identity map[string]string
		if err := json.Unmarshal([]byte(content.Text), &identity); err != nil || identity["robot_id"] != "alpha" || identity["name"] != "Alpha" || identity["connection"] != "unknown" {
			t.Fatalf("invalid identity: %s %v", content.Text, err)
		}
		if strings.Contains(content.Text, ".local") || !strings.Contains(identity["instruction"], "inspect_robot") {
			t.Fatalf("unsafe or unusable mention context: %s", content.Text)
		}
		if _, err := bob.ReadResource(ctx, read); err == nil {
			t.Fatal("mention crossed principal boundary")
		}
	}
	for _, query := range []string{"beta", "no match"} {
		result, err := alice.CallTool(ctx, callToolReq("search_devices", map[string]any{"query": query}))
		if items := mentionItems(t, result, err); len(items) != 0 {
			t.Fatalf("unexpected matches for %q: %+v", query, items)
		}
	}
	for _, args := range []map[string]any{{}, {"query": nil}, {"query": 1}, {"query": strings.Repeat("x", 129)}} {
		result, err := alice.CallTool(ctx, callToolReq("search_devices", args))
		if err == nil && !result.IsError {
			t.Fatalf("invalid search accepted: %v", args)
		}
	}
	for _, uri := range []string{"wendy://devices/missing", "wendy://devices/../alpha", "wendy://devices/alpha?x=1", "wendy://devices/alpha/extra", "other://devices/alpha"} {
		var read mcpgo.ReadResourceRequest
		read.Params.URI = uri
		if _, err := alice.ReadResource(ctx, read); err == nil {
			t.Fatalf("invalid mention reference accepted: %s", uri)
		}
	}
}

func TestGatewayDeviceMentionsCloudInventory(t *testing.T) {
	cfg := gatewayTestConfig()
	cfg.CloudSources = []GatewayCloudSource{{ID: "fleet", Endpoint: "cloud.example:443", OrganizationID: 1}}
	cfg.Grants[0].CloudSources = []string{"fleet"}
	var discoveryCalls atomic.Int32
	var removed, fail atomic.Bool
	g, err := NewRobotGateway(cfg, func(context.Context, string) (*grpcclient.AgentConnection, error) {
		t.Error("mention connected to a device")
		return nil, fmt.Errorf("unexpected connection")
	}, WithRobotCloudDiscovery(func(_ context.Context, _ GatewayCloudSource, onlineOnly bool) ([]GatewayCloudDevice, error) {
		discoveryCalls.Add(1)
		if onlineOnly {
			t.Error("mention search must include offline enrollments")
		}
		if fail.Load() {
			return nil, fmt.Errorf("private backend error with credentials")
		}
		if removed.Load() {
			return nil, nil
		}
		rows := make([]GatewayCloudDevice, 105)
		for i := range rows {
			rows[i] = GatewayCloudDevice{Device: fmt.Sprintf("private-selector-%d", i), Name: fmt.Sprintf("Device %03d", i)}
		}
		rows[0].Name, rows[1].Name = "Duplicate", "Duplicate"
		return rows, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	h := gatewayTestHTTP(t, g)
	c := gatewayHTTPClient(t, h.URL, gatewayTestEnv("ALICE_TOKEN"))
	ctx := context.Background()
	search := func(query string) (*mcpgo.CallToolResult, []mcpgo.ResourceLink) {
		t.Helper()
		result, err := c.CallTool(ctx, callToolReq("search_devices", map[string]any{"query": query}))
		return result, mentionItems(t, result, err)
	}
	result, items := search("")
	if len(items) != deviceMentionLimit || result.Meta.AdditionalFields["truncated"] != true || result.Meta.AdditionalFields["total_count"] != float64(106) {
		t.Fatalf("unbounded or incorrectly counted results: %v", result)
	}
	_, repeated := search("")
	for i := range items {
		if items[i].URI != repeated[i].URI {
			t.Fatal("empty query order is unstable")
		}
	}
	_, duplicates := search("duplicate")
	if len(duplicates) != 2 || duplicates[0].URI == duplicates[1].URI || duplicates[0].Description == duplicates[1].Description {
		t.Fatalf("duplicate names lost distinct identities: %+v", duplicates)
	}
	_, narrowed := search("device 104")
	if len(narrowed) != 1 || narrowed[0].URI != deviceMentionPrefix+discoveredRobotID("private-selector-104") {
		t.Fatalf("filter must run before truncation: %+v", narrowed)
	}
	id := strings.TrimPrefix(narrowed[0].URI, deviceMentionPrefix)
	_, byID := search(id)
	if len(byID) != 1 || byID[0].URI != narrowed[0].URI {
		t.Fatalf("stable ID search failed: %+v", byID)
	}
	var read mcpgo.ReadResourceRequest
	read.Params.URI = narrowed[0].URI
	if _, err := c.ReadResource(ctx, read); err != nil {
		t.Fatal(err)
	}
	removed.Store(true)
	if _, err := c.ReadResource(ctx, read); err == nil {
		t.Fatal("saved mention retained revoked Cloud access")
	}
	_, absent := search("device 104")
	if len(absent) != 0 {
		t.Fatal("search retained removed Cloud enrollment")
	}
	fail.Store(true)
	result, items = search("")
	if len(items) != 1 || result.Meta.AdditionalFields["discovery_complete"] != false {
		t.Fatal("discovery failure discarded configured devices or claimed a complete inventory")
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), "credentials") || strings.Contains(string(raw), "private-selector") || len(result.Meta.AdditionalFields["warnings"].([]any)) == 0 {
		t.Fatalf("missing or private warning: %s", raw)
	}
	before := discoveryCalls.Load()
	for _, principal := range []gatewayPrincipal{{}, {Subject: "alice", Scopes: []string{RobotControlScope}}, {Subject: "unknown", Scopes: robotGatewayScopes}} {
		unauthorized := context.WithValue(ctx, gatewayPrincipalKey{}, principal)
		result, err := g.searchDeviceMentions(unauthorized, callToolReq("search_devices", map[string]any{"query": ""}))
		if err != nil || !result.IsError {
			t.Fatalf("unscoped search allowed: %v %v", result, err)
		}
		read.Params.URI = "wendy://devices/alpha"
		if _, err := g.readDeviceMention(unauthorized, read); err == nil {
			t.Fatal("unscoped resource read allowed")
		}
		if len(g.filterTools(unauthorized, []mcpgo.Tool{g.protocol.ListTools()["search_devices"].Tool})) != 0 {
			t.Fatal("unscoped mention tool advertised")
		}
	}
	if discoveryCalls.Load() != before {
		t.Fatal("unscoped request reached Cloud discovery")
	}
}
