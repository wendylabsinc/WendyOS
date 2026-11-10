package meshsession

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperatorlink"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// No shell or socket backend is installed. These counters prove authorization
// happens before privileged handlers, even with an authenticated outer device.
type nativeIngressRPCProbe struct {
	agentpb.UnimplementedWendyContainerServiceServer
	agentpb.UnimplementedWendyShellServiceServer
	agentpbv2.UnimplementedWendyTunnelServiceServer
	allowed, privileged atomic.Int32
}

func (p *nativeIngressRPCProbe) QueryChunks(context.Context, *agentpb.QueryChunksRequest) (*agentpb.QueryChunksResponse, error) {
	p.allowed.Add(1)
	return &agentpb.QueryChunksResponse{}, nil
}
func (p *nativeIngressRPCProbe) HostShell(grpc.BidiStreamingServer[agentpb.HostShellRequest, agentpb.HostShellResponse]) error {
	p.privileged.Add(1)
	return status.Error(codes.Internal, "privileged shell handler reached")
}
func (p *nativeIngressRPCProbe) Tunnel(grpc.BidiStreamingServer[agentpbv2.DeviceTunnelRequest, agentpbv2.DeviceTunnelData]) error {
	p.privileged.Add(1)
	return status.Error(codes.Internal, "privileged tunnel handler reached")
}
func (p *nativeIngressRPCProbe) DatagramTunnel(grpc.BidiStreamingServer[agentpbv2.DeviceDatagramFrame, agentpbv2.DeviceDatagramFrame]) error {
	p.privileged.Add(1)
	return status.Error(codes.Internal, "privileged datagram handler reached")
}

func TestMeshNativeBootGraceDoesNotGrantDeviceAdminRPCs(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	stale := now.Add(-598 * time.Second)
	policy := certs.CertificateTimePolicy{Now: func() time.Time { return stale }, Floor: func() time.Time { return now.Add(-time.Hour) }, Uncertain: func() bool { return true }}
	a, b, _ := nativeIngressIdentities(t, policy, now)
	listener := meshoperatorlink.NewMeshTLSListener()
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, address := startTLSAppTest(t, b.credentials, WithAgentServiceDial(nil, 50052, func() (net.Conn, error) { return listener.DialContext(ctx) }))
	client, err := NewClient(a.credentials)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := mtls.NewServerWithTimePolicy(b.cert, b.chain, b.key, nil, policy, certs.Scope{TenantUUID: fixtureTenant}, interceptor.OrgModeStrict)
	if err != nil {
		t.Fatal(err)
	}
	probe := &nativeIngressRPCProbe{}
	agentpb.RegisterWendyContainerServiceServer(server, probe)
	agentpb.RegisterWendyShellServiceServer(server, probe)
	agentpbv2.RegisterWendyTunnelServiceServer(server, probe)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() { server.Stop(); <-done }()
	conn, err := grpc.NewClient("passthrough:///private-mesh-agent", grpc.WithTransportCredentials(credentials.NewTLS(nativeIngressClientConfig(t, a, b.credentials.Device))), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return client.DialTLS(ctx, b.credentials.Device, address, 50052)
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// A's leaf is not yet valid at stale UTC. Successful ordinary device RPC
	// proves both TLS layers and the boot policy worked before checking denial.
	if _, err := agentpb.NewWendyContainerServiceClient(conn).QueryChunks(ctx, &agentpb.QueryChunksRequest{}); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{agentpb.WendyShellService_HostShell_FullMethodName, agentpbv2.WendyTunnelService_Tunnel_FullMethodName, agentpbv2.WendyTunnelService_DatagramTunnel_FullMethodName} {
		stream, err := conn.NewStream(ctx, &grpc.StreamDesc{ClientStreams: true, ServerStreams: true}, method)
		if err == nil {
			_ = stream.CloseSend()
			err = stream.RecvMsg(&emptypb.Empty{})
		}
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s: expected device-role refusal, got %v", method, err)
		}
	}
	if probe.allowed.Load() != 1 || probe.privileged.Load() != 0 {
		t.Fatalf("allowed=%d privileged=%d", probe.allowed.Load(), probe.privileged.Load())
	}
}
