package commands

import (
	"context"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func TestMeshSelectorDefaultBothPathsMCPAndOrdinaryDeviceInfo(t *testing.T) {
	for _, device := range []string{"22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"} {
		t.Run(device, func(t *testing.T) {
			meshSelectorOrdinaryDeviceInfo(t, "mesh://11111111-1111-4111-8111-111111111111/"+device)
		})
	}
}
func meshSelectorOrdinaryDeviceInfo(t *testing.T, selector string) {
	setTempConfig(t, &config.Config{DefaultDevice: selector})
	t.Setenv("WENDY_AGENT_SOCKET", "")
	oldFlag, oldJSON, oldConnect, oldDescribe := deviceFlag, jsonOutput, connectOperatorMeshSelector, describeOperatorMeshTarget
	t.Cleanup(func() {
		deviceFlag, jsonOutput, connectOperatorMeshSelector, describeOperatorMeshTarget = oldFlag, oldJSON, oldConnect, oldDescribe
	})
	deviceFlag, jsonOutput = "", true
	describeOperatorMeshTarget = func(context.Context, string) (sessionbroker.OperatorTargetInfo, error) {
		return sessionbroker.OperatorTargetInfo{Platform: "WendyOS"}, nil
	}
	calls := 0
	connectOperatorMeshSelector = func(_ context.Context, device string) (*grpcclient.AgentConnection, error) {
		calls++
		if device != selector {
			t.Fatal("mesh selector identity changed", device)
		}
		transport, err := grpc.NewClient("passthrough:///mesh-selector-command-fixture", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithUnaryInterceptor(func(context.Context, string, any, any, *grpc.ClientConn, grpc.UnaryInvoker, ...grpc.CallOption) error {
			return status.Error(codes.Unimplemented, "optional fixture method")
		}))
		if err != nil {
			t.Fatal(err)
		}
		client := grpcclient.NewFromConn(transport)
		client.Host = selector
		client.Addr = selector
		client.IsSessionProxy = true
		client.IsMTLS = true
		client.AgentService = &deviceInfoClient{updateStatus: func(context.Context, *agentpb.GetOSUpdateStatusRequest) (*agentpb.GetOSUpdateStatusResponse, error) {
			return &agentpb.GetOSUpdateStatusResponse{}, nil
		}}
		client.CacheAgentVersion(&agentpb.GetAgentVersionResponse{Version: "retained-mesh-agent", Hostname: "adjacent-router"})
		return client, nil
	}
	ctx := context.Background()
	client, err := connectToAgent(ctx, NonInteractive())
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
	target, err := resolveTargetInner(ctx, NonInteractive())
	if err != nil {
		t.Fatal(err)
	}
	if stored, err := defaultDeviceNameFor(target); err != nil || stored != selector {
		t.Fatal("default replaced full mesh identity", stored, err)
	}
	target.Close()
	client, err = connectMCPDevice(ctx, mcpStartupAddress(selector))
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
	cmd := newDeviceInfoCmd()
	cmd.SetContext(ctx)
	output, err := captureCommandStdout(t, func() error { return cmd.RunE(cmd, nil) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "retained-mesh-agent") {
		t.Fatal("ordinary device info did not consume selected retained agent", output)
	}
	if calls != 4 {
		t.Fatalf("expected four normal selection paths, got%d", calls)
	}
	root := newDeviceLocalMeshCmd()
	if command, _, err := root.Find([]string{"connect"}); err != nil || command.Name() != "connect" {
		t.Fatal("connect entrypoint missing", err)
	}
}

func TestMeshLiteSelectorChoosesNativeProvider(t *testing.T) {
	const selector = "mesh://11111111-1111-4111-8111-111111111111/33333333-3333-4333-8333-333333333333"
	setTempConfig(t, &config.Config{DefaultDevice: selector})
	oldFlag, oldDescribe, oldConnect := deviceFlag, describeOperatorMeshTarget, connectOperatorMeshSelector
	t.Cleanup(func() {
		deviceFlag, describeOperatorMeshTarget, connectOperatorMeshSelector = oldFlag, oldDescribe, oldConnect
	})
	deviceFlag = ""
	describeOperatorMeshTarget = func(context.Context, string) (sessionbroker.OperatorTargetInfo, error) {
		return sessionbroker.OperatorTargetInfo{Selector: selector, Tenant: "11111111-1111-4111-8111-111111111111", Device: "33333333-3333-4333-8333-333333333333", Principal: "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/33333333-3333-4333-8333-333333333333", Name: "C5", Platform: "Lite", Transport: "mtls-v1", Caps: []string{"sensors", "future-optional"}}, nil
	}
	connectOperatorMeshSelector = func(context.Context, string) (*grpcclient.AgentConnection, error) {
		t.Fatal("Lite target must not receive a gRPC agent probe")
		return nil, nil
	}
	for _, explicit := range []bool{false, true} {
		deviceFlag = ""
		if explicit {
			deviceFlag = selector
		}
		picked, err := resolveTargetInner(context.Background(), NonInteractive())
		if err != nil {
			t.Fatal(err)
		}
		if picked.Agent != nil || picked.External == nil || picked.Provider == nil || picked.Provider.Key() != "wendy-lite" || picked.External.ConnectionType() != "Mesh" || picked.External.ConnectionInfo["caps"] != "sensors,future-optional" {
			t.Fatalf("Lite selection %#v", picked)
		}
		if stored, err := defaultDeviceNameFor(picked); err != nil || stored != selector {
			t.Fatal(stored, err)
		}
		picked.Close()
	}
	// Existing agent-only commands keep their clear protocol/capability error.
	_, err := connectToAgent(context.Background(), NonInteractive())
	if err == nil || !strings.Contains(err.Error(), "WendyOS") {
		t.Fatal("agent-only command", err)
	}
}
