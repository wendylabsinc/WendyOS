package commands

import (
	"context"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
)

type completionProbeServer struct {
	agentpbv2.UnimplementedWendyProvisioningServiceServer
}

func (completionProbeServer) IsProvisioned(context.Context, *agentpbv2.IsProvisionedRequest) (*agentpbv2.IsProvisionedResponse, error) {
	return &agentpbv2.IsProvisionedResponse{ResponseType: &agentpbv2.IsProvisionedResponse_NotProvisioned{NotProvisioned: &agentpbv2.NotProvisionedResponse{UnenrollmentCompletion: []byte("forged receipt")}}}, nil
}

func TestCompletionProbeCannotAcceptPlaintextIdentityOrChangePins(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	pin := config.DevicePin{Principal: testUnenrollProgress().Principal, CloudGRPC: testUnenrollProgress().Cloud}
	cfg := &config.Config{DevicePins: map[string]config.DevicePin{"127.0.0.1": pin}}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		calls.Add(1)
		if info.FullMethod != agentpbv2.WendyProvisioningService_IsProvisioned_FullMethodName {
			t.Errorf("unexpected RPC: %s", info.FullMethod)
		}
		return handler(ctx, req)
	}))
	agentpbv2.RegisterWendyProvisioningServiceServer(server, completionProbeServer{})
	go server.Serve(listener)
	defer server.Stop()
	old := deviceFlag
	deviceFlag = listener.Addr().String()
	defer func() { deviceFlag = old }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := recoverCompletedV2Unenroll(ctx, "", ""); err == nil || !strings.Contains(err.Error(), "authenticating prior reset completion") {
		t.Fatalf("forged completion accepted: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected only one public status read: %d", calls.Load())
	}
	after, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.DevicePins, after.DevicePins) {
		t.Fatal("public completion probe changed pins")
	}
}
