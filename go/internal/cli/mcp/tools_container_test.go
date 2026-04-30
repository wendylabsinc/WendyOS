package mcp

import (
	"context"
	"encoding/json"
	"net"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/internal/shared/config"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// fakeContainerServer implements WendyContainerServiceServer for container tests.
type fakeContainerServer struct {
	agentpb.UnimplementedWendyContainerServiceServer
	containers []*agentpb.AppContainer
	stats      []*agentpb.ContainerStats
	stopErr    error
	deleteErr  error
}

func (s *fakeContainerServer) ListContainers(_ *agentpb.ListContainersRequest, stream agentpb.WendyContainerService_ListContainersServer) error {
	for _, c := range s.containers {
		if err := stream.Send(&agentpb.ListContainersResponse{Container: c}); err != nil {
			return err
		}
	}
	return nil
}

func (s *fakeContainerServer) StopContainer(_ context.Context, req *agentpb.StopContainerRequest) (*agentpb.StopContainerResponse, error) {
	return &agentpb.StopContainerResponse{}, s.stopErr
}

func (s *fakeContainerServer) DeleteContainer(_ context.Context, req *agentpb.DeleteContainerRequest) (*agentpb.DeleteContainerResponse, error) {
	return &agentpb.DeleteContainerResponse{}, s.deleteErr
}

func (s *fakeContainerServer) ListContainerStats(_ context.Context, _ *agentpb.ListContainerStatsRequest) (*agentpb.ListContainerStatsResponse, error) {
	return &agentpb.ListContainerStatsResponse{Stats: s.stats}, nil
}

func (s *fakeContainerServer) StartContainer(req *agentpb.StartContainerRequest, stream agentpb.WendyContainerService_StartContainerServer) error {
	_ = stream.Send(&agentpb.RunContainerLayersResponse{
		ResponseType: &agentpb.RunContainerLayersResponse_StdoutOutput{
			StdoutOutput: &agentpb.RunContainerLayersResponse_ConsoleOutput{Data: []byte("started\n")},
		},
	})
	return nil
}

func (s *fakeContainerServer) AttachContainer(stream agentpb.WendyContainerService_AttachContainerServer) error {
	_, err := stream.Recv()
	if err != nil {
		return err
	}
	_ = stream.Send(&agentpb.RunContainerLayersResponse{
		ResponseType: &agentpb.RunContainerLayersResponse_StdoutOutput{
			StdoutOutput: &agentpb.RunContainerLayersResponse_ConsoleOutput{Data: []byte("hello from container\n")},
		},
	})
	return nil
}

func startFakeContainerServer(t *testing.T, fake *fakeContainerServer) *grpcclient.AgentConnection {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	g := grpc.NewServer()
	agentpb.RegisterWendyContainerServiceServer(g, fake)
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(func() { g.Stop() })

	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return &grpcclient.AgentConnection{
		Conn:             conn,
		ContainerService: agentpb.NewWendyContainerServiceClient(conn),
	}
}

func TestContainerList_NotConnected(t *testing.T) {
	srv := New(&config.Config{}, nil)
	result, err := srv.callTool(context.Background(), "container_list", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError=true when not connected")
	}
}

func TestContainerList_ReturnsJSON(t *testing.T) {
	fake := &fakeContainerServer{
		containers: []*agentpb.AppContainer{
			{AppName: "myapp", AppVersion: "1.0.0", RunningState: agentpb.AppRunningState_RUNNING},
		},
	}
	conn := startFakeContainerServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "container_list", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	var containers []map[string]any
	if err := json.Unmarshal([]byte(text), &containers); err != nil {
		t.Fatalf("invalid JSON: %v\ntext: %s", err, text)
	}
	if len(containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(containers))
	}
	if containers[0]["app_name"] != "myapp" {
		t.Errorf("app_name = %v, want myapp", containers[0]["app_name"])
	}
}

func TestContainerStop_NotConnected(t *testing.T) {
	srv := New(&config.Config{}, nil)
	result, err := srv.callTool(context.Background(), "container_stop", map[string]any{"app_name": "myapp"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError=true when not connected")
	}
}

func TestContainerStop_Success(t *testing.T) {
	fake := &fakeContainerServer{}
	conn := startFakeContainerServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "container_stop", map[string]any{"app_name": "myapp"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	if text != "container myapp stopped" {
		t.Errorf("text = %q, want %q", text, "container myapp stopped")
	}
}

func TestContainerDelete_Success(t *testing.T) {
	fake := &fakeContainerServer{}
	conn := startFakeContainerServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "container_delete", map[string]any{"app_name": "myapp"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	if text != "container myapp deleted" {
		t.Errorf("text = %q, want %q", text, "container myapp deleted")
	}
}

func TestContainerStats_ReturnsJSON(t *testing.T) {
	fake := &fakeContainerServer{
		stats: []*agentpb.ContainerStats{
			{AppName: "myapp", MemoryBytes: 1024 * 1024, StorageBytes: 50 * 1024 * 1024},
		},
	}
	conn := startFakeContainerServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "container_stats", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	var stats []map[string]any
	if err := json.Unmarshal([]byte(text), &stats); err != nil {
		t.Fatalf("invalid JSON: %v\ntext: %s", err, text)
	}
	if len(stats) != 1 {
		t.Fatalf("expected 1 stat, got %d", len(stats))
	}
	if stats[0]["app_name"] != "myapp" {
		t.Errorf("app_name = %v, want myapp", stats[0]["app_name"])
	}
}

func TestContainerStart_ReturnsOutput(t *testing.T) {
	fake := &fakeContainerServer{}
	conn := startFakeContainerServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "container_start", map[string]any{"app_name": "myapp"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	if text != "started\n" {
		t.Errorf("text = %q, want %q", text, "started\n")
	}
}

func TestContainerAttach_ReturnsOutput(t *testing.T) {
	fake := &fakeContainerServer{}
	conn := startFakeContainerServer(t, fake)
	srv := New(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "container_attach", map[string]any{"app_name": "myapp"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	if text != "hello from container\n" {
		t.Errorf("text = %q, want %q", text, "hello from container\n")
	}
}
