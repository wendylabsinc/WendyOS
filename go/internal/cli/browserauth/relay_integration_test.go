package browserauth

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/cli/clouddefaults"
	relaypb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	health "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLiveCloudRelay(t *testing.T) {
	endpoint := os.Getenv("WENDY_TEST_CLOUD_RELAY")
	if endpoint == "" {
		t.Skip("set WENDY_TEST_CLOUD_RELAY for the running local relay")
	}
	target := os.Getenv("WENDY_TEST_CLOUD_AUTHORITY")
	if target == "" {
		target = "api.dev.wendy.sh:443"
	}
	conn, err := grpc.NewClient("passthrough:///"+target, grpc.WithTransportCredentials(insecure.NewCredentials()), clouddefaults.TunnelDialer(func(ctx context.Context) (net.Conn, error) {
		return dialTestRelay(ctx, endpoint)
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if os.Getenv("WENDY_TEST_BROKER_JOIN") == "1" {
		stream, e := relaypb.NewTunnelBrokerV2ServiceClient(conn).JoinSession(ctx)
		if e != nil {
			t.Fatal(e)
		}
		if e = stream.Send(&relaypb.JoinSessionRequest{Message: &relaypb.JoinSessionRequest_Open{Open: &relaypb.JoinOpen{Role: relaypb.JoinRole_JOIN_ROLE_CALLER}}}); e != nil {
			t.Fatal(e)
		}
		_, e = stream.Recv()
		switch status.Code(e) {
		case codes.Unauthenticated, codes.PermissionDenied, codes.InvalidArgument:
			t.Logf("Broker JoinSession reached admission and rejected the absent grant: %v", e)
		default:
			t.Fatalf("Expected broker admission response, got: %v", e)
		}
		return
	}
	_, err = health.NewHealthClient(conn).Check(ctx, &health.HealthCheckRequest{})
	if err != nil && strings.Contains(err.Error(), "unexpected HTTP status code") {
		t.Fatalf("Relay reached HTTP error rather than gRPC: %v", err)
	}
	switch status.Code(err) {
	case codes.OK, codes.Unauthenticated, codes.PermissionDenied, codes.Unimplemented:
		t.Logf("Cloud answered gRPC through relay: %s (%v)", status.Code(err), err)
	default:
		t.Fatalf("Cloud relay failed before a service response: %v", err)
	}
}
