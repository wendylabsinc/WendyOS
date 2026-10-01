package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/onboarding"
)

func TestGatewaySimulatorAccessAndExplicitTarget(t *testing.T) {
	cfg := gatewayTestConfig()
	cfg.AllowSimulators = true
	var connected string
	var inspected string
	var updated string
	g, err := NewRobotGateway(cfg, func(_ context.Context, target string) (*grpcclient.AgentConnection, error) {
		connected = target
		return &grpcclient.AgentConnection{}, nil
	}, WithGatewayLifecycle(onboarding.Backend{}, ProjectBackend{}, SimulatorBackend{
		Viewer: func(_ context.Context, name string) (*SimulatorViewer, error) {
			inspected = name
			return &SimulatorViewer{Name: name, Profile: "go2", URL: "http://127.0.0.1:8890", Ready: true, Healthy: true}, nil
		},
		UpdateAgent: func(_ context.Context, name string) (*SimulatorAgentUpdate, error) {
			updated = name
			return &SimulatorAgentUpdate{Name: name, CapabilityVerified: true}, nil
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	principal := context.WithValue(context.Background(), gatewayPrincipalKey{}, gatewayPrincipal{"alice", robotGatewayScopes})
	local := context.WithValue(principal, gatewayLocalContextKey{}, true)
	for _, name := range []string{"simulator_start", "simulator_update_agent", "simulator_viewer"} {
		handler := g.protocol.ListTools()[name].Handler
		// A remote session cannot turn simulator permission into local host access.
		denied, err := handler(principal, callToolReq(name, map[string]any{"name": "chatgpt-go2"}))
		if err != nil || !denied.IsError || connected != "" || inspected != "" || updated != "" {
			t.Fatalf("%s remote access: %v, %v", name, denied, err)
		}
		invalid, err := handler(local, callToolReq(name, map[string]any{"name": "../physical-robot"}))
		if err != nil || !invalid.IsError || connected != "" || inspected != "" || updated != "" {
			t.Fatalf("%s invalid target: %v, %v", name, invalid, err)
		}
	}
	update, err := g.protocol.ListTools()["simulator_update_agent"].Handler(local, callToolReq("simulator_update_agent", map[string]any{"name": "chatgpt-go2"}))
	if err != nil || update.IsError || updated != "chatgpt-go2" {
		t.Fatalf("update target = %q, result = %v, err = %v", updated, update, err)
	}
	started, err := g.protocol.ListTools()["simulator_start"].Handler(local, callToolReq("simulator_start", map[string]any{"name": "chatgpt-go2"}))
	if err != nil || started.IsError || connected != "vm:chatgpt-go2" {
		t.Fatalf("start target = %q, result = %v, err = %v", connected, started, err)
	}
	viewed, err := g.protocol.ListTools()["simulator_viewer"].Handler(local, callToolReq("simulator_viewer", map[string]any{"name": "chatgpt-go2"}))
	if err != nil || viewed.IsError || inspected != "chatgpt-go2" {
		t.Fatalf("viewer target = %q, result = %v, err = %v", inspected, viewed, err)
	}
}

func TestGatewaySimulatorAgentUpdateNameCannotBeShadowed(t *testing.T) {
	cfg := gatewayTestConfig()
	cfg.Robots[0].Exports = []GatewayExport{{Name: "simulator_update_agent", App: "companion", Tool: gatewayTool("app_status", "App status", readOnly())}}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "invalid app tool export") {
		t.Fatalf("reserved simulator tool name accepted: %v", err)
	}
	cfg.Robots[0].Exports[0].Name = "allowed_app_status"
	if err := cfg.validate(); err != nil {
		t.Fatalf("ordinary app tool export rejected: %v", err)
	}
}

func TestGatewaySimulatorStartFailureDoesNotReportReady(t *testing.T) {
	cfg := gatewayTestConfig()
	cfg.AllowSimulators = true
	g, err := NewRobotGateway(cfg, func(context.Context, string) (*grpcclient.AgentConnection, error) {
		return nil, fmt.Errorf("agent lacks go2-virtual-robot support")
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), gatewayPrincipalKey{}, gatewayPrincipal{"alice", robotGatewayScopes})
	ctx = context.WithValue(ctx, gatewayLocalContextKey{}, true)
	result, err := g.protocol.ListTools()["simulator_start"].Handler(ctx, callToolReq("simulator_start", map[string]any{"name": "chatgpt-go2"}))
	if err != nil || !result.IsError || result.StructuredContent != nil {
		t.Fatalf("failed provision reported success: %v, %v", result, err)
	}
}

func TestGatewayCatalogSimulatorCountAcrossPages(t *testing.T) {
	cfg := gatewayTestConfig()
	cfg.AllowSimulators = true
	cfg.Robots[1].Device = "vm:configured-sim"
	cfg.Grants[0].Robots = append(cfg.Grants[0].Robots, "beta")
	g, err := NewRobotGateway(cfg, func(context.Context, string) (*grpcclient.AgentConnection, error) {
		t.Fatal("listing must not connect to a device")
		return nil, nil
	}, WithGatewayLifecycle(onboarding.Backend{}, ProjectBackend{}, SimulatorBackend{
		List: func(context.Context) ([]SimulatorInfo, error) {
			return []SimulatorInfo{
				{Name: "configured-sim", Device: "vm:configured-sim", State: "running"},
				{Name: "discovered-sim", Device: "vm:discovered-sim", State: "running"},
			}, nil
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), gatewayPrincipalKey{}, gatewayPrincipal{"alice", robotGatewayScopes})
	ctx = context.WithValue(ctx, gatewayLocalContextKey{}, true)
	for page := 0; page < 3; page++ {
		result, err := g.listRobots(ctx, callToolReq("list_robots", map[string]any{"limit": 1, "offset": page}))
		if err != nil || result.IsError {
			t.Fatal("catalog failed", err)
		}
		data := result.StructuredContent.(map[string]any)
		if data["total_count"] != 3 || data["simulator_count"] != 2 {
			t.Fatal("device count must exclude simulators outside the current page", data)
		}
		rows := data["robots"].([]map[string]any)
		if len(rows) != 1 || (page > 0 && rows[0]["source"] != "simulator") {
			t.Fatal("configured and discovered VMs must both be marked as simulators", rows)
		}
	}
	result, _ := g.listRobots(ctx, callToolReq("list_robots", map[string]any{"query": "Alpha"}))
	data := result.StructuredContent.(map[string]any)
	if data["total_count"] != 1 || data["simulator_count"] != 0 {
		t.Fatal("simulator count must respect the device search", data)
	}
}
