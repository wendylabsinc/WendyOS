package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type gatewayEventFixture struct {
	agentpbv2.DataServiceClient
	fired atomic.Bool
	calls atomic.Int32
}

func (f *gatewayEventFixture) Events(ctx context.Context, r *agentpbv2.DataEventsRequest, _ ...grpc.CallOption) (*agentpbv2.DataEventsResponse, error) {
	f.calls.Add(1)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw := []byte(`[]`)
	cursor := "epoch:1"
	if f.fired.Load() {
		raw = []byte(`[{"id":"epoch:2","sequence":2,"app_id":"vision","name":"person_detected"}]`)
		cursor = "epoch:2"
	}
	return &agentpbv2.DataEventsResponse{EventsJson: raw, Cursor: cursor}, nil
}
func TestGatewayTaskSurvivesHTTPRequestAndIsolatesPrincipal(t *testing.T) {
	cfg := gatewayTestConfig()
	cfg.Robots[0].Triggers = []GatewayTrigger{{ID: "people", Name: "People", AppID: "vision", Event: "person_detected"}}
	cfg.Grants[1].Scopes = append(cfg.Grants[1].Scopes, RobotEventsScope)
	addr, _ := gatewayFixture(t, "test")
	fixture := &gatewayEventFixture{}
	g, err := NewRobotGateway(cfg, func(context.Context, string) (*grpcclient.AgentConnection, error) {
		cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return nil, err
		}
		c := grpcclient.NewFromConn(cc)
		c.DataService = fixture
		return c, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	host := gatewayTestHTTP(t, g)
	rpc := func(token, method string, params any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		req, _ := http.NewRequest(http.MethodPost, host.URL+"/mcp", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+gatewayTestEnv(token))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	init := map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "1"}}
	rpc("ALICE_TOKEN", "initialize", init)
	create := rpc("ALICE_TOKEN", "tools/call", map[string]any{"name": "wait_for_device_event", "arguments": map[string]any{"robot_id": "alpha", "trigger_id": "people", "timeout_seconds": 10}, "task": map[string]any{}})
	result, ok := create["result"].(map[string]any)
	if !ok {
		t.Fatalf("create: %v", create)
	}
	id := result["task"].(map[string]any)["taskId"].(string)
	deadline := time.Now().Add(3 * time.Second)
	for fixture.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	other := rpc("BOB_TOKEN", "tasks/get", map[string]any{"taskId": id})
	if other["error"] == nil {
		t.Fatal("cross-principal task read", other)
	}
	state := rpc("ALICE_TOKEN", "tasks/get", map[string]any{"taskId": id})
	if fmt.Sprint(state) == "" || state["error"] != nil {
		t.Fatal(state)
	}
	fixture.fired.Store(true)
	completed := rpc("ALICE_TOKEN", "tasks/result", map[string]any{"taskId": id})
	b, _ := json.Marshal(completed)
	if !bytes.Contains(b, []byte("person_detected")) {
		t.Fatalf("wait lost request context or event: %s", b)
	}
	fixture.fired.Store(false)
	previousCalls := fixture.calls.Load()
	create = rpc("ALICE_TOKEN", "tools/call", map[string]any{"name": "wait_for_device_event", "arguments": map[string]any{"robot_id": "alpha", "trigger_id": "people", "timeout_seconds": 30}, "task": map[string]any{}})
	id = create["result"].(map[string]any)["task"].(map[string]any)["taskId"].(string)
	deadline = time.Now().Add(3 * time.Second)
	for fixture.calls.Load() == previousCalls && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	canceled := rpc("ALICE_TOKEN", "tasks/cancel", map[string]any{"taskId": id})
	if canceled["error"] != nil {
		t.Fatal(canceled)
	}
	deadline = time.Now().Add(3 * time.Second)
	for len(g.waitSlots) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(g.waitSlots) != 0 {
		t.Fatal("canceled task retained its device stream/wait slot")
	}
}
func TestGatewaySettingsAndResourcesAreScoped(t *testing.T) {
	cfg := gatewayTestConfig()
	cfg.StateDirectory = t.TempDir()
	g, err := NewRobotGateway(cfg, func(context.Context, string) (*grpcclient.AgentConnection, error) { return nil, fmt.Errorf("unused") })
	if err != nil {
		t.Fatal(err)
	}
	h := gatewayTestHTTP(t, g)
	alice := gatewayHTTPClient(t, h.URL, gatewayTestEnv("ALICE_TOKEN"))
	bob := gatewayHTTPClient(t, h.URL, gatewayTestEnv("BOB_TOKEN"))
	r, err := alice.CallTool(context.Background(), callToolReq("update_device_settings", map[string]any{"set": map[string]any{"show_3d": false}}))
	if err != nil || r.IsError {
		t.Fatal(r, err)
	}
	r, err = bob.CallTool(context.Background(), callToolReq("read_device_settings", nil))
	if err != nil || r.IsError {
		t.Fatal(r, err)
	}
	b, _ := json.Marshal(r.StructuredContent)
	if !bytes.Contains(b, []byte(`"show_3d":true`)) {
		t.Fatal("settings leaked", string(b))
	}
	var read mcpgo.ReadResourceRequest
	read.Params.URI = "wendy://devices/alpha"
	if _, err = bob.ReadResource(context.Background(), read); err == nil {
		t.Fatal("cross-account resource read")
	}
}

func TestGatewayAdvertisesSettingsOnlyWithDiscoverableTools(t *testing.T) {
	g, err := NewRobotGateway(gatewayTestConfig(), func(context.Context, string) (*grpcclient.AgentConnection, error) {
		return nil, fmt.Errorf("unused")
	})
	if err != nil {
		t.Fatal(err)
	}
	h := gatewayTestHTTP(t, g)
	// Repeat Alice after Bob to catch mutation of shared capability metadata.
	for _, token := range []string{"ALICE_TOKEN", "BOB_TOKEN", "ALICE_TOKEN"} {
		c := gatewayHTTPClient(t, h.URL, gatewayTestEnv(token))
		var req mcpgo.InitializeRequest
		req.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
		req.Params.ClientInfo = mcpgo.Implementation{Name: "scope-test", Version: "1"}
		init, err := c.Initialize(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		_, advertised := init.Capabilities.Experimental["openai/settings"]
		if advertised != (token == "ALICE_TOKEN") {
			t.Fatalf("%s settings advertised = %v", token, advertised)
		}
		catalog, err := c.ListTools(context.Background(), mcpgo.ListToolsRequest{})
		if err != nil {
			t.Fatal(err)
		}
		found := map[string]bool{}
		for _, tool := range catalog.Tools {
			found[tool.Name] = true
			if tool.Name == "read_device_logs" || tool.Name == "update_device_settings" {
				if tool.Meta == nil || tool.Meta.AdditionalFields["openai/widgetAccessible"] != true {
					t.Fatalf("%s not explicitly accessible to app", tool.Name)
				}
			}
		}
		if !found["read_device_logs"] || !found["read_device_settings"] || found["update_device_settings"] != advertised {
			t.Fatalf("%s capability/tool mismatch: %v", token, found)
		}
	}
}
