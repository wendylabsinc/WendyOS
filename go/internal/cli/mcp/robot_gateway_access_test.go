package mcp

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/onboarding"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func gatewayAccessContext(scopes []string, local bool) context.Context {
	ctx := context.WithValue(context.Background(), gatewayPrincipalKey{}, gatewayPrincipal{"alice", scopes})
	return context.WithValue(ctx, gatewayLocalContextKey{}, local)
}

func gatewayAccessScopesWithout(scope string) []string {
	return slices.DeleteFunc(slices.Clone(robotGatewayScopes), func(s string) bool { return s == scope })
}

func TestRobotGatewayCloudAllAppsOptInAndScopeIntersection(t *testing.T) {
	const device = "cloud://cloud.example:443/org/1/asset/42"
	for _, tc := range []struct {
		name        string
		allow       bool
		missing     string
		wantControl bool
	}{
		{name: "read only default"},
		{name: "opted in", allow: true, wantControl: true},
		{name: "grant lacks control", allow: true, missing: "grant"},
		{name: "token lacks control", allow: true, missing: "token"},
		{name: "source not granted", allow: true, missing: "source"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := gatewayTestConfig()
			cfg.CloudSources = []GatewayCloudSource{{ID: "fleet", Endpoint: "cloud.example:443", OrganizationID: 1, AllowAllApps: tc.allow}}
			cfg.Grants[0].CloudSources = []string{"fleet"}
			scopes := robotGatewayScopes
			switch tc.missing {
			case "grant":
				cfg.Grants[0].Scopes = gatewayAccessScopesWithout(RobotControlScope)
			case "token":
				scopes = gatewayAccessScopesWithout(RobotControlScope)
			case "source":
				cfg.Grants[0].CloudSources = nil
			}
			addr, fixture := gatewayFixture(t, "cloud-apps")
			g, err := NewRobotGateway(cfg, func(_ context.Context, target string) (*grpcclient.AgentConnection, error) {
				if target != device {
					return nil, fmt.Errorf("unexpected target %s", target)
				}
				cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
				if err != nil {
					return nil, err
				}
				return grpcclient.NewFromConn(cc), nil
			}, WithRobotCloudDiscovery(func(context.Context, GatewayCloudSource, bool) ([]GatewayCloudDevice, error) {
				return []GatewayCloudDevice{{Device: device, Name: "Cloud robot"}}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			ctx := gatewayAccessContext(scopes, false)
			id := discoveredRobotID(device)
			list, _ := g.listRobots(ctx, callToolReq("list_robots", map[string]any{}))
			found := false
			for _, row := range list.StructuredContent.(map[string]any)["robots"].([]map[string]any) {
				if row["id"] == id {
					found = true
					if row["can_control_apps"] != tc.wantControl {
						t.Fatalf("incorrect catalog control flag: %v", row)
					}
				}
			}
			if found != (tc.missing != "source") {
				t.Fatal("catalog did not honor the source grant")
			}
			inspect, err := g.protocol.ListTools()["inspect_robot"].Handler(ctx, callToolReq("inspect_robot", map[string]any{"robot_id": id}))
			if err != nil || inspect.IsError != (tc.missing == "source") {
				t.Fatalf("inspect: %v %v", inspect, err)
			}
			if !inspect.IsError {
				apps := inspect.StructuredContent.(map[string]any)["apps"].([]map[string]any)
				if len(apps) != 2 {
					t.Fatalf("missing installed app inventory: %v", apps)
				}
				for _, app := range apps {
					if app["can_control"] != tc.wantControl {
						t.Fatalf("incorrect per-app control flag: %v", app)
					}
				}
			}
			result, err := g.protocol.ListTools()["start_robot_app"].Handler(ctx, callToolReq("start_robot_app", map[string]any{"robot_id": id, "app_name": "private-app"}))
			if err != nil || result.IsError == tc.wantControl || fixture.running.Load() != tc.wantControl {
				t.Fatalf("control policy: %v %v, starts=%d", result, err, fixture.starts.Load())
			}
		})
	}
}

func TestRobotGatewayAllAppsRequiresInstalledApp(t *testing.T) {
	cfg := gatewayTestConfig()
	cfg.Robots[0].Apps = nil
	cfg.Robots[0].AllowAllApps = true
	addr, fixture := gatewayFixture(t, "installed-apps")
	g, err := NewRobotGateway(cfg, func(context.Context, string) (*grpcclient.AgentConnection, error) {
		cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return nil, err
		}
		return grpcclient.NewFromConn(cc), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	server := gatewayTestHTTP(t, g)
	client := gatewayHTTPClient(t, server.URL, gatewayTestEnv("ALICE_TOKEN"))
	for _, action := range []string{"start", "stop"} {
		// A rejected stop must also leave an already-running app untouched.
		fixture.running.Store(action == "stop")
		result, err := client.CallTool(context.Background(), callToolReq(action+"_robot_app", map[string]any{"robot_id": "alpha", "app_name": "not-installed"}))
		if err != nil || !result.IsError || fixture.starts.Load() != 0 || fixture.running.Load() != (action == "stop") {
			t.Fatalf("uninstalled app %s reached device control: %v %v", action, result, err)
		}
	}
	for _, action := range []string{"start", "stop"} {
		result, err := client.CallTool(context.Background(), callToolReq(action+"_robot_app", map[string]any{"robot_id": "alpha", "app_name": "private-app"}))
		if err != nil || result.IsError || structuredMap(t, result)["state_verified"] != true || fixture.running.Load() != (action == "start") {
			t.Fatalf("installed app %s failed: %v %v", action, result, err)
		}
	}
}

type gatewayUnavailableAppInventory struct {
	// Any attempt to start or stop an app would panic on this nil embedded
	// client, so the test also verifies that no control call follows failure.
	agentpb.WendyContainerServiceClient
}

func (gatewayUnavailableAppInventory) ListContainers(context.Context, *agentpb.ListContainersRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[agentpb.ListContainersResponse], error) {
	return nil, fmt.Errorf("inventory unavailable")
}

func TestRobotGatewayAllAppsInventoryFailureDoesNotControl(t *testing.T) {
	g := &RobotGateway{}
	r := &GatewayRobot{ID: "alpha", AllowAllApps: true}
	s := &mcpServer{conn: &grpcclient.AgentConnection{ContainerService: gatewayUnavailableAppInventory{}}}
	for _, action := range []string{"start", "stop"} {
		result, err := g.controlApp(context.Background(), r, s, callToolReq(action+"_robot_app", map[string]any{"app_name": "private-app"}), action)
		if err != nil || !result.IsError {
			t.Fatalf("%s did not reject unavailable inventory: %v %v", action, result, err)
		}
	}
}

func TestRobotGatewayCloudAllAppsPreservesExplicitPolicy(t *testing.T) {
	for _, granted := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit grant %t", granted), func(t *testing.T) {
			cfg := gatewayTestConfig()
			cfg.CloudSources = []GatewayCloudSource{{ID: "fleet", Endpoint: "cloud.example:443", OrganizationID: 1, AllowAllApps: true}}
			cfg.Grants[0].CloudSources = []string{"fleet"}
			if !granted {
				cfg.Grants[0].Robots = nil
			}
			addr, fixture := gatewayFixture(t, "explicit-policy")
			g, err := NewRobotGateway(cfg, func(context.Context, string) (*grpcclient.AgentConnection, error) {
				cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
				if err != nil {
					return nil, err
				}
				return grpcclient.NewFromConn(cc), nil
			}, WithRobotCloudDiscovery(func(context.Context, GatewayCloudSource, bool) ([]GatewayCloudDevice, error) {
				return []GatewayCloudDevice{{Device: cfg.Robots[0].Device, Name: "Cloud alias"}}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			ctx := gatewayAccessContext(robotGatewayScopes, false)
			for _, id := range []string{"alpha", discoveredRobotID(cfg.Robots[0].Device)} {
				result, err := g.protocol.ListTools()["start_robot_app"].Handler(ctx, callToolReq("start_robot_app", map[string]any{"robot_id": id, "app_name": "private-app"}))
				if err != nil || !result.IsError || fixture.starts.Load() != 0 {
					t.Fatalf("source opt-in widened explicit robot policy: %v %v", result, err)
				}
			}
		})
	}
}

func TestRobotGatewaySimulatorDeviceAccessRequiresLocalOptInAndScopes(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		local, allow, manage bool
		missingGrant         string
		missingToken         string
		wantRead             bool
		wantControl          bool
		wantCamera           bool
	}{
		{name: "read only default", local: true, manage: true, wantRead: true},
		{name: "local opt-in", local: true, allow: true, manage: true, wantRead: true, wantControl: true, wantCamera: true},
		{name: "HTTP cannot access local simulator", allow: true, manage: true},
		{name: "simulators disabled", local: true, allow: true},
		{name: "no simulator grant", local: true, allow: true, manage: true, missingGrant: RobotSimulatorScope},
		{name: "no simulator token scope", local: true, allow: true, manage: true, missingToken: RobotSimulatorScope},
		{name: "no control grant", local: true, allow: true, manage: true, missingGrant: RobotControlScope, wantRead: true, wantCamera: true},
		{name: "no control token scope", local: true, allow: true, manage: true, missingToken: RobotControlScope, wantRead: true, wantCamera: true},
		{name: "no camera grant", local: true, allow: true, manage: true, missingGrant: RobotCameraScope, wantRead: true, wantControl: true},
		{name: "no camera token scope", local: true, allow: true, manage: true, missingToken: RobotCameraScope, wantRead: true, wantControl: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := gatewayTestConfig()
			cfg.AllowSimulators = tc.manage
			cfg.AllowSimulatorDeviceAccess = tc.allow
			cfg.Grants[0].Scopes = gatewayAccessScopesWithout(tc.missingGrant)
			ctx := gatewayAccessContext(gatewayAccessScopesWithout(tc.missingToken), tc.local)
			g, err := NewRobotGateway(cfg, func(context.Context, string) (*grpcclient.AgentConnection, error) {
				t.Fatal("authorization should not connect to the simulator")
				return nil, nil
			}, WithGatewayLifecycle(onboarding.Backend{}, ProjectBackend{}, SimulatorBackend{List: func(context.Context) ([]SimulatorInfo, error) {
				return []SimulatorInfo{{Name: "future-go2", Device: "vm:future-go2", State: "running", Profile: "go2"}}, nil
			}}))
			if err != nil {
				t.Fatal(err)
			}
			for scope, want := range map[string]bool{RobotReadScope: tc.wantRead, RobotControlScope: tc.wantControl, RobotCameraScope: tc.wantCamera} {
				robot, err := g.authorize(ctx, "sim-future-go2", scope)
				if (err == nil) != want || (want && robot.Device != "vm:future-go2") {
					t.Fatalf("scope %s authorized=%t, want=%t: %v", scope, err == nil, want, err)
				}
			}
			list, _ := g.listRobots(ctx, callToolReq("list_robots", map[string]any{}))
			found := false
			for _, row := range list.StructuredContent.(map[string]any)["robots"].([]map[string]any) {
				if row["id"] == "sim-future-go2" {
					found = true
					if row["can_control_apps"] != tc.wantControl || row["can_capture"] != tc.wantCamera {
						t.Fatalf("catalog permissions do not match caller scopes: %v", row)
					}
				}
			}
			if found != tc.wantRead {
				t.Fatal("simulator catalog did not honor local access policy")
			}
		})
	}
}
