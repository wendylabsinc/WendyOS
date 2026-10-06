package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestRobotGatewayCloudDiscoveryRefreshAndIsolation(t *testing.T) {
	cfg := gatewayTestConfig()
	cfg.CloudSources = []GatewayCloudSource{{ID: "fleet-a", Endpoint: "cloud.example:443", OrganizationID: 1}, {ID: "fleet-b", Endpoint: "cloud.example:443", OrganizationID: 2}}
	cfg.Grants[0].CloudSources = []string{"fleet-a"}
	cfg.Grants[1].CloudSources = []string{"fleet-b"}
	const device = "cloud://cloud.example:443/org/1/asset/42"
	const other = "cloud://cloud.example:443/org/2/asset/42"
	var mu sync.Mutex
	available, fail := true, false
	discover := func(_ context.Context, source GatewayCloudSource, _ bool) ([]GatewayCloudDevice, error) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return nil, fmt.Errorf("private Cloud error")
		}
		if source.ID == "fleet-b" {
			return []GatewayCloudDevice{{Device: other, Name: "Other account robot"}}, nil
		}
		if !available {
			return nil, nil
		}
		// Existing alias/policy must survive its appearance in Cloud inventory.
		return []GatewayCloudDevice{{Device: cfg.Robots[0].Device, Name: "Cloud alias"}, {Device: device, Name: "New robot"}}, nil
	}
	addr, _ := gatewayFixture(t, "new-robot-version")
	var connects atomic.Int32
	g, err := NewRobotGateway(cfg, func(_ context.Context, target string) (*grpcclient.AgentConnection, error) {
		connects.Add(1)
		if target != device {
			return nil, fmt.Errorf("wrong device %s", target)
		}
		cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return nil, err
		}
		return grpcclient.NewFromConn(cc), nil
	}, WithRobotCloudDiscovery(discover))
	if err != nil {
		t.Fatal(err)
	}
	s := gatewayTestHTTP(t, g)
	alice := gatewayHTTPClient(t, s.URL, gatewayTestEnv("ALICE_TOKEN"))
	bob := gatewayHTTPClient(t, s.URL, gatewayTestEnv("BOB_TOKEN"))
	list, err := alice.CallTool(context.Background(), callToolReq("list_robots", map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	data := structuredMap(t, list)
	raw, _ := json.Marshal(data)
	if data["total_count"] != float64(2) || !strings.Contains(string(raw), "New robot") || strings.Contains(string(raw), "Other account") || strings.Contains(string(raw), "Cloud alias") || strings.Contains(string(raw), "cloud.example") {
		t.Fatalf("incorrect inventory: %s", raw)
	}
	newID := discoveredRobotID(device)
	for _, name := range []string{"start_robot_app", "capture_robot_image"} {
		args := map[string]any{"robot_id": newID}
		if name == "start_robot_app" {
			args["app_name"] = "companion"
		} else {
			args["camera_id"] = 0
		}
		result, err := alice.CallTool(context.Background(), callToolReq(name, args))
		if err != nil || !result.IsError {
			t.Fatalf("discovery granted a write: %v %v", result, err)
		}
	}
	result, err := bob.CallTool(context.Background(), callToolReq("inspect_robot", map[string]any{"robot_id": newID}))
	if err != nil || !result.IsError || connects.Load() != 0 {
		t.Fatal("discovery crossed account or action boundaries")
	}
	result, err = alice.CallTool(context.Background(), callToolReq("inspect_robot", map[string]any{"robot_id": newID}))
	if err != nil || result.IsError {
		t.Fatalf("new robot cannot be inspected: %v %v", result, err)
	}
	raw, _ = json.Marshal(result.StructuredContent)
	if !strings.Contains(string(raw), "new-robot-version") || !strings.Contains(string(raw), "private-app") {
		t.Fatalf("missing read-only app inventory: %s", raw)
	}
	mu.Lock()
	available = false
	mu.Unlock()
	result, err = alice.CallTool(context.Background(), callToolReq("inspect_robot", map[string]any{"robot_id": newID}))
	if err != nil || !result.IsError || connects.Load() != 1 {
		t.Fatal("removed discovery retained access")
	}
	mu.Lock()
	fail = true
	mu.Unlock()
	list, err = alice.CallTool(context.Background(), callToolReq("list_robots", map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	data = structuredMap(t, list)
	if data["discovery_complete"] != false || len(data["warnings"].([]any)) == 0 {
		t.Fatal("discovery failure presented a complete inventory")
	}
}

func TestRobotGatewayCloudDiscoveryPaginationAndOffline(t *testing.T) {
	cfg := gatewayTestConfig()
	cfg.Robots[0].Apps = nil
	cfg.CloudSources = []GatewayCloudSource{{ID: "fleet", Endpoint: "cloud.example:443", OrganizationID: 1}}
	cfg.Grants[0].CloudSources = []string{"fleet"}
	var onlineOnly bool
	g, err := NewRobotGateway(cfg, func(context.Context, string) (*grpcclient.AgentConnection, error) {
		return nil, fmt.Errorf("no device probes")
	}, WithRobotCloudDiscovery(func(_ context.Context, _ GatewayCloudSource, online bool) ([]GatewayCloudDevice, error) {
		onlineOnly = online
		rows := []GatewayCloudDevice{{Device: "one", Name: "Duplicate"}, {Device: "two", Name: "Duplicate"}}
		if !online {
			rows = append(rows, GatewayCloudDevice{Device: "three", Name: "Offline"})
		}
		return rows, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), gatewayPrincipalKey{}, gatewayPrincipal{"alice", robotGatewayScopes})
	list, _ := g.listRobots(ctx, callToolReq("list_robots", map[string]any{"limit": 1, "query": "Duplicate"}))
	data := list.StructuredContent.(map[string]any)
	if !onlineOnly || data["total_count"] != 2 || data["next_offset"] != 1 {
		t.Fatalf("bad first page: %v", data)
	}
	first := data["robots"].([]map[string]any)[0]["id"]
	list, _ = g.listRobots(ctx, callToolReq("list_robots", map[string]any{"limit": 1, "offset": 1, "query": "duplicate", "include_offline": true}))
	data = list.StructuredContent.(map[string]any)
	if onlineOnly || data["next_offset"] != nil || data["robots"].([]map[string]any)[0]["id"] == first {
		t.Fatalf("bad second page: %v", data)
	}
	list, _ = g.listRobots(ctx, callToolReq("list_robots", map[string]any{"query": "Alpha"}))
	if list.StructuredContent.(map[string]any)["robots"].([]map[string]any)[0]["can_control_apps"] != false {
		t.Fatal("robot with no permitted apps advertised app control")
	}
	// Opening an offline robot absent from the default list must keep it selected.
	open, err := g.protocol.ListTools()["open_robot"].Handler(ctx, callToolReq("open_robot", map[string]any{"robot_id": discoveredRobotID("three")}))
	if err != nil || open.IsError || open.StructuredContent.(map[string]any)["selected_robot_id"] != discoveredRobotID("three") {
		t.Fatalf("open discovered robot: %v %v", open, err)
	}
}

func TestRobotGatewayCloudSourceValidation(t *testing.T) {
	for _, source := range []GatewayCloudSource{
		{ID: "fleet", Endpoint: "https://cloud.example:443", OrganizationID: 1},
		{ID: "fleet", Endpoint: "cloud.example:443"},
		{ID: "fleet", Endpoint: "cloud.example:443", OrganizationID: 1, TenantUUID: "x"},
		{ID: "fleet", Endpoint: "cloud.example:443", TenantUUID: "invalid"},
	} {
		cfg := gatewayTestConfig()
		cfg.CloudSources = []GatewayCloudSource{source}
		if err := cfg.validate(); err == nil {
			t.Fatalf("accepted invalid source %+v", source)
		}
	}
	cfg := gatewayTestConfig()
	cfg.Grants[0].CloudSources = []string{"missing"}
	if err := cfg.validate(); err == nil {
		t.Fatal("unknown source grant accepted")
	}
	cfg = gatewayTestConfig()
	cfg.CloudSources = []GatewayCloudSource{{ID: "fleet", Endpoint: "cloud.example:443", OrganizationID: 1}}
	if _, err := NewRobotGateway(cfg, func(context.Context, string) (*grpcclient.AgentConnection, error) { return nil, nil }); err == nil {
		t.Fatal("missing discovery provider accepted")
	}
}
