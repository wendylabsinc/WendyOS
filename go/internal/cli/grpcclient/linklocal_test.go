package grpcclient

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

func serveFakeAgent(t *testing.T, lis net.Listener) {
	t.Helper()
	srv := grpc.NewServer()
	agentpb.RegisterWendyAgentServiceServer(srv, fakeAgentServer{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
}

func stubLinkLocalDial(t *testing.T, fn func(context.Context, string) (net.Conn, error)) {
	t.Helper()
	orig := linkLocalDialFn
	t.Cleanup(func() { linkLocalDialFn = orig })
	linkLocalDialFn = fn
}

// With several USB devices attached, 169.254.0.0/16 routes out of only one of
// their links; the connection must go through the link-aware dialer.
func TestConnectDialsLinkLocalAgentsThroughTheLinkAwareDialer(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	serveFakeAgent(t, lis)
	var dialed string
	stubLinkLocalDial(t, func(ctx context.Context, addr string) (net.Conn, error) {
		dialed = addr
		return lis.DialContext(ctx)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := Connect(ctx, "169.254.198.132:50051")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()
	if _, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{}); err != nil {
		t.Fatalf("GetAgentVersion: %v", err)
	}
	if dialed != "169.254.198.132:50051" {
		t.Fatalf("link-aware dialer got %q, want 169.254.198.132:50051", dialed)
	}
}

func TestConnectLeavesRoutableAgentsOnTheDefaultDialer(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveFakeAgent(t, lis)
	stubLinkLocalDial(t, func(context.Context, string) (net.Conn, error) {
		t.Error("link-aware dialer used for a routable address")
		return nil, net.ErrClosed
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := Connect(ctx, lis.Addr().String())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()
	if _, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{}); err != nil {
		t.Fatalf("GetAgentVersion: %v", err)
	}
}
