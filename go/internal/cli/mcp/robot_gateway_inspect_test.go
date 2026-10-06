package mcp

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func gatewayInspectionTestConnection(t *testing.T, before func(context.Context, string) error) *grpcclient.AgentConnection {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(
		grpc.UnaryInterceptor(func(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			if err := before(ctx, info.FullMethod); err != nil {
				return nil, err
			}
			return handler(ctx, request)
		}),
		grpc.StreamInterceptor(func(service any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			if err := before(stream.Context(), info.FullMethod); err != nil {
				return err
			}
			return handler(service, stream)
		}),
	)
	fixture := &gatewayFixtureAgent{name: "inspection-fixture"}
	agentpb.RegisterWendyAgentServiceServer(server, &gatewayFixtureInfo{fixture: fixture})
	agentpb.RegisterWendyContainerServiceServer(server, fixture)
	agentpb.RegisterWendyVideoServiceServer(server, &gatewayFixtureVideo{})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	client, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	connection := grpcclient.NewFromConn(client)
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}

func TestRobotGatewayInspectReadsInventoriesConcurrently(t *testing.T) {
	for _, cameraAllowed := range []bool{true, false} {
		name := "with camera access"
		if !cameraAllowed {
			name = "without camera access"
		}
		t.Run(name, func(t *testing.T) {
			cfg := gatewayTestConfig()
			if !cameraAllowed {
				cfg.Grants[0].Scopes = gatewayAccessScopesWithout(RobotCameraScope)
			}
			g := &RobotGateway{cfg: cfg}
			var entered atomic.Int32
			allStarted := make(chan struct{})
			want := int32(2)
			if cameraAllowed {
				want++
			}
			conn := gatewayInspectionTestConnection(t, func(ctx context.Context, method string) error {
				limit := 15 * time.Second
				if strings.HasSuffix(method, "/GetAgentVersion") {
					limit = 10 * time.Second
				} else if strings.HasSuffix(method, "/ListVideoDevices") {
					limit = 5 * time.Second
					if !cameraAllowed {
						t.Error("inspection read cameras without camera scope")
					}
				}
				if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > limit {
					t.Errorf("%s does not have its bounded RPC deadline", method)
				}
				if entered.Add(1) == want {
					close(allStarted)
				}
				select {
				case <-allStarted:
					return nil
				case <-time.After(time.Second):
					return status.Error(codes.DeadlineExceeded, "independent inventory RPCs did not start together")
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			result, err := g.inspect(gatewayAccessContext(robotGatewayScopes, true), &cfg.Robots[0], &mcpServer{conn: conn}, callToolReq("inspect_robot", nil))
			if err != nil || result.IsError || entered.Load() != want {
				t.Fatalf("parallel inspection failed: %v %v, RPCs=%d", result, err, entered.Load())
			}
			data := result.StructuredContent.(map[string]any)
			if data["connected"] != true || len(data["apps"].([]map[string]any)) != 1 || len(data["warnings"].([]string)) != 0 {
				t.Fatalf("parallel inspection lost authorized data: %v", data)
			}
		})
	}
}

func TestRobotGatewayInspectOptionalInventoryFailurePreservesIdentity(t *testing.T) {
	cfg := gatewayTestConfig()
	g := &RobotGateway{cfg: cfg}
	conn := gatewayInspectionTestConnection(t, func(_ context.Context, method string) error {
		if strings.HasSuffix(method, "/GetAgentVersion") {
			return nil
		}
		return status.Error(codes.Unavailable, "fixture unavailable")
	})
	result, err := g.inspect(gatewayAccessContext(robotGatewayScopes, true), &cfg.Robots[0], &mcpServer{conn: conn}, callToolReq("inspect_robot", nil))
	if err != nil || result.IsError {
		t.Fatalf("optional inventory failure hid agent identity: %v %v", result, err)
	}
	data := result.StructuredContent.(map[string]any)
	if data["connected"] != true || data["agent_version"] != "inspection-fixture" || data["apps"] != nil || len(data["cameras"].([]map[string]any)) != 0 {
		t.Fatalf("incorrect partial inspection: %v", data)
	}
	warnings := data["warnings"].([]string)
	if len(warnings) != 2 || warnings[0] != "App state unavailable." || warnings[1] != "Camera inventory unavailable." {
		t.Fatalf("missing inventory warnings: %v", warnings)
	}
}

func TestRobotGatewayInspectCancelsInventoriesWhenIdentityFails(t *testing.T) {
	cfg := gatewayTestConfig()
	g := &RobotGateway{cfg: cfg}
	var started atomic.Int32
	inventoriesStarted := make(chan struct{})
	canceled := make(chan struct{}, 2)
	conn := gatewayInspectionTestConnection(t, func(ctx context.Context, method string) error {
		if strings.HasSuffix(method, "/GetAgentVersion") {
			select {
			case <-inventoriesStarted:
			case <-ctx.Done():
				return ctx.Err()
			}
			return status.Error(codes.Unavailable, "fixture identity failed")
		}
		if started.Add(1) == 2 {
			close(inventoriesStarted)
		}
		<-ctx.Done()
		canceled <- struct{}{}
		return ctx.Err()
	})
	result, err := g.inspect(gatewayAccessContext(robotGatewayScopes, true), &cfg.Robots[0], &mcpServer{conn: conn}, callToolReq("inspect_robot", nil))
	if err != nil || !result.IsError {
		t.Fatalf("failed identity reported a connected device: %v %v", result, err)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-canceled:
		case <-time.After(time.Second):
			t.Fatal("failed identity left an inventory RPC running")
		}
	}
}
