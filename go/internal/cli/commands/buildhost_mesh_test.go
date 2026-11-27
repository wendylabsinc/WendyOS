package commands

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type buildMeshCodec struct{}

func (buildMeshCodec) Name() string                  { return "build-mesh-fixture" }
func (buildMeshCodec) Marshal(v any) ([]byte, error) { return v.([]byte), nil }
func (buildMeshCodec) Unmarshal(data []byte, v any) error {
	*v.(*[]byte) = append([]byte(nil), data...)
	return nil
}

func TestConnectedTargetAgentPortUsesMeshAdvertisement(t *testing.T) {
	const selector = "mesh://11111111-1111-4111-8111-111111111111/22222222-2222-4222-8222-222222222222"
	for _, tc := range []struct {
		name, address, replySelector, platform string
		unavailable                            bool
		want                                   uint32
	}{
		{name: "standard port", address: "[fd00::1]:50052", want: 50052},
		{name: "custom port", address: "[fd00::1]:51002", want: 51002},
		{name: "missing endpoint"},
		{name: "invalid port", address: "[fd00::1]:70000"},
		{name: "zero port", address: "[fd00::1]:0"},
		{name: "different target", address: "[fd00::1]:50052", replySelector: "mesh://other"},
		{name: "unsupported platform", address: "[fd00::1]:5054", platform: "Lite"},
		{name: "unavailable route", unavailable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			listener := bufconn.Listen(1024 * 1024)
			defer listener.Close()
			server := grpc.NewServer(grpc.ForceServerCodec(buildMeshCodec{}), grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
				method, _ := grpc.MethodFromServerStream(stream)
				if method != "/wendy.sessionbroker.ManagementV1/Describe" {
					return status.Error(codes.Unimplemented, "unexpected method")
				}
				md, _ := metadata.FromIncomingContext(stream.Context())
				targets := md.Get("wendy-operator-target")
				if len(targets) != 1 || targets[0] != selector {
					return status.Error(codes.InvalidArgument, "missing selected mesh target")
				}
				var request []byte
				if err := stream.RecvMsg(&request); err != nil {
					return err
				}
				if tc.unavailable {
					return status.Error(codes.Unavailable, "route unavailable")
				}
				info := sessionbroker.OperatorTargetInfo{Selector: selector, Address: tc.address, Platform: "WendyOS", Transport: "mtls-v1"}
				if tc.replySelector != "" {
					info.Selector = tc.replySelector
				}
				if tc.platform != "" {
					info.Platform = tc.platform
				}
				data, err := json.Marshal(info)
				if err != nil {
					return err
				}
				return stream.SendMsg(data)
			}))
			defer server.Stop()
			go server.Serve(listener)
			conn, err := grpc.NewClient("passthrough:///mesh-broker-fixture", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			target := grpcclient.NewFromConn(conn)
			target.Addr, target.IsSessionProxy = selector, true
			got, err := connectedTargetAgentPort(ctx, target)
			if tc.want == 0 {
				if err == nil {
					t.Fatalf("unexpected port %d", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("port = %d, error = %v; want %d", got, err, tc.want)
			}
		})
	}
}

// This smoke performs only device reads. It exercises the remote-build target
// preparation against an authenticated mesh device without starting a build.
// Set WENDY_CONFIG_DIR to the signed-in profile directory because TestMain
// isolates the default home directory.
func TestBuildDeliveryMeshTargetSmoke(t *testing.T) {
	selector := os.Getenv("WENDY_BUILD_TARGET_SMOKE")
	if selector == "" {
		t.Skip("set WENDY_BUILD_TARGET_SMOKE to an authorized mesh device selector")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	conn, err := sessionbroker.ConnectOperator(ctx, selector)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	target, err := targetPushTargetBase(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if target.GetAssetUuid() == "" || target.GetAgentPort() == 0 || target.GetRegistryPort() == 0 {
		t.Fatal("incomplete build delivery target")
	}
	t.Logf("Authenticated mesh build target resolved: agent port %d, registry port %d", target.GetAgentPort(), target.GetRegistryPort())
}
