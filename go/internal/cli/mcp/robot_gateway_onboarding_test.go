package mcp

import (
	"context"
	"fmt"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
)

func TestRobotGatewayEmptyOnboardingHasNoOperationalAccess(t *testing.T) {
	cfg := RobotGatewayConfig{AllowEmptyInventory: true, LocalSubject: "owner", Grants: []GatewayGrant{{Subject: "owner", Scopes: []string{RobotReadScope, RobotSettingsScope}}}}
	connections := 0
	g, err := NewRobotGateway(cfg, func(context.Context, string) (*grpcclient.AgentConnection, error) {
		connections++
		return nil, fmt.Errorf("should not connect")
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), gatewayPrincipalKey{}, gatewayPrincipal{Subject: "owner", Scopes: cfg.Grants[0].Scopes})
	result, err := g.listRobots(ctx, callToolReq("list_robots", map[string]any{}))
	if err != nil || result.IsError {
		t.Fatal("empty inventory could not be opened", result, err)
	}
	data := result.StructuredContent.(map[string]any)
	if data["total_count"] != 0 || data["discovery_complete"] != true || data["can_manage_simulators"] != false || connections != 0 {
		t.Fatal("onboarding required credentials or accessed a device", data, connections)
	}
	if _, err := g.authorize(ctx, "invented-device", RobotReadScope); err == nil {
		t.Fatal("empty inventory authorized a target")
	}
	for _, scope := range []string{RobotHostScope, RobotSimulatorScope, RobotProjectWriteScope, RobotDeployScope, RobotControlScope, RobotCameraScope} {
		if g.hasScope(ctx, scope) {
			t.Fatalf("onboarding granted %s", scope)
		}
	}
	cfg.HTTP = &GatewayHTTPConfig{}
	if err := cfg.validate(); err == nil {
		t.Fatal("local onboarding policy accepted HTTP")
	}
	cfg.HTTP, cfg.LocalSubject = nil, ""
	if err := cfg.validate(); err == nil {
		t.Fatal("empty inventory accepted without a local identity")
	}
}
