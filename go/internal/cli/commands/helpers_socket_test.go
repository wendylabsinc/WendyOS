package commands

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
)

type versionOnlyAgent struct {
	agentpb.UnimplementedWendyAgentServiceServer
}

func (versionOnlyAgent) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	return &agentpb.GetAgentVersionResponse{Version: "sock"}, nil
}

// startUDSAgent serves a minimal agent on a unix socket and sets
// WENDY_AGENT_SOCKET to its path for the duration of the test.
func startUDSAgent(t *testing.T, register ...func(*grpc.Server)) {
	t.Helper()
	// Short temp dir to stay under the unix-socket sun_path limit on macOS
	// (see localsocket_test.go for the convention).
	dir, err := os.MkdirTemp("/tmp", "wsk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "agent.sock")

	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	agentpb.RegisterWendyAgentServiceServer(srv, versionOnlyAgent{})
	for _, registerService := range register {
		registerService(srv)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	t.Setenv("WENDY_AGENT_SOCKET", sock)
}

// connectWithAutoTLSDiagnostics is the deep chokepoint; it must honor the env var.
func TestConnectWithAutoTLS_UsesAgentSocketEnv(t *testing.T) {
	startUDSAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// plaintextAddr is deliberately bogus: if the socket is honored, the
	// unroutable address is never dialed.
	conn, _, err := connectWithAutoTLSDiagnostics(ctx, "203.0.113.1:50051")
	if err != nil {
		t.Fatalf("connectWithAutoTLSDiagnostics: %v", err)
	}
	defer conn.Close()
	if resp, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{}); err != nil || resp.GetVersion() != "sock" {
		t.Fatalf("did not route through the socket: resp=%v err=%v", resp, err)
	}
}

// resolveTarget is the front door real commands use (e.g. `wendy device info`).
// With no configured/default device — as inside a fresh container — it must
// still use the socket instead of falling through to mDNS discovery.
func TestResolveTarget_UsesAgentSocketEnv(t *testing.T) {
	startUDSAgent(t)
	// Ensure no --device flag leaks in from another test.
	deviceFlag = ""
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	target, err := resolveTarget(ctx)
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	defer target.Close()
	if target.Agent == nil {
		t.Fatal("resolveTarget returned no agent — fell through to discovery instead of the socket")
	}
	if resp, err := target.Agent.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{}); err != nil || resp.GetVersion() != "sock" {
		t.Fatalf("did not route through the socket: resp=%v err=%v", resp, err)
	}
}
