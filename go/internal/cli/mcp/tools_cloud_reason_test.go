package mcp

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/clouddefaults"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type refusingBroker struct {
	cloudpb.UnimplementedTunnelBrokerServiceServer
	verdict error
}

func (b *refusingBroker) ClientTunnel(stream cloudpb.TunnelBrokerService_ClientTunnelServer) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	return b.verdict
}

func TestMCPOpenBrokerTunnelSurfacesBrokerVerdict(t *testing.T) {
	verdict := status.Error(codes.PermissionDenied, "user is not a current member of this organization")
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	cloudpb.RegisterTunnelBrokerServiceServer(srv, &refusingBroker{verdict: verdict})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	brokerConn, err := grpc.NewClient("passthrough:///broker",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer brokerConn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := mcpOpenBrokerTunnel(ctx, brokerConn, &config.AuthConfig{}, 42, 50052)
	if err != nil {
		t.Fatalf("mcpOpenBrokerTunnel: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = conn.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("Read succeeded on a tunnel the broker refused")
	}
	var closed *clouddefaults.TunnelClosedError
	if !errors.As(err, &closed) || status.Code(closed.Reason) != codes.PermissionDenied {
		t.Fatalf("Read returned %v, want a TunnelClosedError carrying the broker's PermissionDenied verdict", err)
	}
}
