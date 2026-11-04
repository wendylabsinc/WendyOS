package mcp

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// rebootingOSAgent behaves like an agent that reboots after its last frame:
// it either holds the stream open or cuts the connection, never ending the RPC.
type rebootingOSAgent struct {
	agentpb.UnimplementedWendyAgentServiceServer
	frames   []*agentpb.UpdateOSResponse
	cut      func()
	released chan struct{}
}

func (a *rebootingOSAgent) UpdateOS(_ *agentpb.UpdateOSRequest, stream grpc.ServerStreamingServer[agentpb.UpdateOSResponse]) error {
	for _, f := range a.frames {
		if err := stream.Send(f); err != nil {
			return err
		}
	}
	if a.cut != nil {
		time.Sleep(200 * time.Millisecond)
		a.cut()
		return nil
	}
	select {
	case <-stream.Context().Done():
		close(a.released)
	case <-time.After(10 * time.Second):
	}
	return nil
}

type cuttingListener struct {
	net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func (l *cuttingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.mu.Lock()
		l.conns = append(l.conns, c)
		l.mu.Unlock()
	}
	return c, err
}

func (l *cuttingListener) cutAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.conns {
		c.Close()
	}
}

func startRebootingOSAgent(t *testing.T, frames []*agentpb.UpdateOSResponse, cut bool) (*grpcclient.AgentConnection, *rebootingOSAgent) {
	t.Helper()
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	lis := &cuttingListener{Listener: inner}
	agent := &rebootingOSAgent{frames: frames, released: make(chan struct{})}
	if cut {
		agent.cut = lis.cutAll
	}
	g := grpc.NewServer()
	agentpb.RegisterWendyAgentServiceServer(g, agent)
	go func() { _ = g.Serve(lis) }()
	conn, err := grpc.NewClient(inner.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn.Close()
		g.Stop()
	})
	return &grpcclient.AgentConnection{Conn: conn, AgentService: agentpb.NewWendyAgentServiceClient(conn)}, agent
}

func osProgress(phase string, percent int32) *agentpb.UpdateOSResponse {
	return &agentpb.UpdateOSResponse{ResponseType: &agentpb.UpdateOSResponse_Progress_{
		Progress: &agentpb.UpdateOSResponse_Progress{Phase: phase, Percent: percent},
	}}
}

func callOSUpdate(t *testing.T, conn *grpcclient.AgentConnection) *mcpgo.CallToolResult {
	t.Helper()
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)
	type res struct {
		r   *mcpgo.CallToolResult
		err error
	}
	got := make(chan res, 1)
	go func() {
		r, err := srv.callTool(context.Background(), "os_update", nil)
		got <- res{r, err}
	}()
	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("callTool: %v", r.err)
		}
		return r.r
	case <-time.After(5 * time.Second):
		t.Fatal("os_update did not return; it waited for a stream end that a rebooting agent never sends")
		return nil
	}
}

func TestOSUpdate_ReturnsAtCompletedAndReleasesAgent(t *testing.T) {
	conn, agent := startRebootingOSAgent(t, []*agentpb.UpdateOSResponse{
		osProgress("done", 100),
		{ResponseType: &agentpb.UpdateOSResponse_Completed_{Completed: &agentpb.UpdateOSResponse_Completed{RebootRequired: true}}},
	}, false)
	result := callOSUpdate(t, conn)
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	if text := result.Content[0].(mcpgo.TextContent).Text; !strings.Contains(text, "reboot required") {
		t.Fatalf("result = %q", text)
	}
	select {
	case <-agent.released:
	case <-time.After(5 * time.Second):
		t.Fatal("agent never saw the stream cancelled after Completed")
	}
}

func TestOSUpdate_ConnectionLostAfterInstallIsNotAnError(t *testing.T) {
	conn, _ := startRebootingOSAgent(t, []*agentpb.UpdateOSResponse{osProgress("write", 100), osProgress("done", 100)}, true)
	result := callOSUpdate(t, conn)
	if result.IsError {
		t.Fatalf("loss after done reported as error: %v", result.Content)
	}
	if text := result.Content[0].(mcpgo.TextContent).Text; !strings.Contains(text, "os_update_status") {
		t.Fatalf("result = %q, want a hint to check os_update_status", text)
	}
}

func TestOSUpdate_ConnectionLostBeforeInstallFinishedIsAnError(t *testing.T) {
	conn, _ := startRebootingOSAgent(t, []*agentpb.UpdateOSResponse{osProgress("write", 100)}, true)
	result := callOSUpdate(t, conn)
	if !result.IsError {
		t.Fatalf("loss during write reported as success: %v", result.Content)
	}
}
