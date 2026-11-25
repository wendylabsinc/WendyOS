package commands

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Exercise gRPC's actual dial deadline: its default budget otherwise cuts off
// the combined Cloud admission, end-to-end TLS, and HTTP/2 startup sequence.
func TestCloudConnectBudgetPreservesRPCCancellation(t *testing.T) {
	operator, _, _, _, expected := cloudIntermediatePKI(t)
	deadline := make(chan time.Time, 1)
	stopped := make(chan struct{})
	conn, err := connectCloudAgentTransport(context.Background(), operator, expected,
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			until, ok := ctx.Deadline()
			if !ok {
				until = time.Time{}
			}
			deadline <- until
			<-ctx.Done()
			close(stopped)
			return nil, ctx.Err()
		}))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Conn.Connect()
	select {
	case until := <-deadline:
		if remaining := time.Until(until); remaining < 55*time.Second || remaining > 65*time.Second {
			t.Fatalf("Cloud connection budget = %v, want approximately one minute", remaining)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("gRPC did not start the Cloud dial")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("short RPC deadline = %v, want DeadlineExceeded", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("closing the connection did not cancel its outstanding dial")
	}
}
