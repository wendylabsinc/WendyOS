package mcp

import (
	"context"
	"net"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestContainerStart_DescriptionMentionsEntitlements(t *testing.T) {
	srv := server.NewMCPServer("t", "0")
	s := newTestServer(&config.Config{}, nil)
	s.registerContainerTools(srv)
	tool, ok := srv.ListTools()["container_start"]
	if !ok {
		t.Fatal("container_start not registered")
	}
	if !strings.Contains(strings.ToLower(tool.Tool.Description), "entitlement") {
		t.Errorf("container_start description should mention entitlements; got: %s", tool.Tool.Description)
	}
}

func TestContainerAnnotations_ReadOnlyNotDestructive(t *testing.T) {
	srv := server.NewMCPServer("t", "0")
	s := newTestServer(&config.Config{}, nil)
	s.registerContainerTools(srv)
	tools := srv.ListTools()
	list, ok := tools["container_list"]
	if !ok {
		t.Fatal("container_list not registered")
	}
	if list.Tool.Annotations.DestructiveHint == nil || *list.Tool.Annotations.DestructiveHint {
		t.Error("container_list (readOnly) must have DestructiveHint=false")
	}
	if list.Tool.Annotations.OpenWorldHint == nil || *list.Tool.Annotations.OpenWorldHint {
		t.Error("container_list must have OpenWorldHint=false (localOnly)")
	}
	del, ok := tools["container_delete"]
	if !ok {
		t.Fatal("container_delete not registered")
	}
	if del.Tool.Annotations.DestructiveHint == nil || !*del.Tool.Annotations.DestructiveHint {
		t.Error("container_delete must have DestructiveHint=true")
	}
}

// fakeContainerServer implements WendyContainerServiceServer for container tests.
type fakeContainerServer struct {
	agentpb.UnimplementedWendyContainerServiceServer
	containers []*agentpb.AppContainer
	stats      []*agentpb.ContainerStats
	stopErr    error
	deleteErr  error
	// startOutputs/attachOutputs override the default single-chunk output for
	// StartContainer/AttachContainer, letting tests assert chunk-capping
	// (max_chunks / max_lines) behavior. Nil means "use the single default
	// chunk" (preserves pre-existing test expectations).
	startOutputs  [][]byte
	attachOutputs [][]byte
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
	outputs := s.startOutputs
	if outputs == nil {
		outputs = [][]byte{[]byte("started\n")}
	}
	for _, o := range outputs {
		if err := stream.Send(&agentpb.RunContainerLayersResponse{
			ResponseType: &agentpb.RunContainerLayersResponse_StdoutOutput{
				StdoutOutput: &agentpb.RunContainerLayersResponse_ConsoleOutput{Data: o},
			},
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *fakeContainerServer) AttachContainer(stream agentpb.WendyContainerService_AttachContainerServer) error {
	_, err := stream.Recv()
	if err != nil {
		return err
	}
	outputs := s.attachOutputs
	if outputs == nil {
		outputs = [][]byte{[]byte("hello from container\n")}
	}
	for _, o := range outputs {
		if err := stream.Send(&agentpb.RunContainerLayersResponse{
			ResponseType: &agentpb.RunContainerLayersResponse_StdoutOutput{
				StdoutOutput: &agentpb.RunContainerLayersResponse_ConsoleOutput{Data: o},
			},
		}); err != nil {
			return err
		}
	}
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
	srv := newTestServer(&config.Config{}, nil)
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
	srv := newTestServer(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "container_list", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	containers := listPayload(t, result, "containers")
	if len(containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(containers))
	}
	if containers[0]["app_name"] != "myapp" {
		t.Errorf("app_name = %v, want myapp", containers[0]["app_name"])
	}
}

func TestContainerStop_NotConnected(t *testing.T) {
	srv := newTestServer(&config.Config{}, nil)
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
	srv := newTestServer(&config.Config{}, nil)
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
	srv := newTestServer(&config.Config{}, nil)
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
	srv := newTestServer(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "container_stats", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	stats := listPayload(t, result, "stats")
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
	srv := newTestServer(&config.Config{}, nil)
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
	srv := newTestServer(&config.Config{}, nil)
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

func TestContainerAttach_MaxLinesAlias_LimitsChunks(t *testing.T) {
	fake := &fakeContainerServer{
		attachOutputs: [][]byte{[]byte("a"), []byte("b"), []byte("c"), []byte("d"), []byte("e")},
	}
	conn := startFakeContainerServer(t, fake)
	srv := newTestServer(&config.Config{}, nil)
	srv.SetConn(conn)

	// max_lines is the deprecated alias for max_chunks; it must keep working.
	result, err := srv.callTool(context.Background(), "container_attach", map[string]any{"app_name": "myapp", "max_lines": 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	if text != "ab" {
		t.Errorf("text = %q, want %q (max_lines alias should cap at 2 chunks)", text, "ab")
	}
}

func TestContainerAttach_MaxChunks_LimitsChunks(t *testing.T) {
	fake := &fakeContainerServer{
		attachOutputs: [][]byte{[]byte("a"), []byte("b"), []byte("c")},
	}
	conn := startFakeContainerServer(t, fake)
	srv := newTestServer(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "container_attach", map[string]any{"app_name": "myapp", "max_chunks": 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	if text != "a" {
		t.Errorf("text = %q, want %q (max_chunks should cap at 1 chunk)", text, "a")
	}
}

func TestContainerAttach_MaxChunksTakesPriorityOverMaxLines(t *testing.T) {
	fake := &fakeContainerServer{
		attachOutputs: [][]byte{[]byte("a"), []byte("b"), []byte("c")},
	}
	conn := startFakeContainerServer(t, fake)
	srv := newTestServer(&config.Config{}, nil)
	srv.SetConn(conn)

	// When both are passed, the new name wins per intParamAlias semantics.
	result, err := srv.callTool(context.Background(), "container_attach", map[string]any{"app_name": "myapp", "max_chunks": 1, "max_lines": 3})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	if text != "a" {
		t.Errorf("text = %q, want %q (max_chunks should take priority over max_lines)", text, "a")
	}
}

func TestContainerStart_MaxChunks_LimitsChunks(t *testing.T) {
	fake := &fakeContainerServer{
		startOutputs: [][]byte{[]byte("a"), []byte("b"), []byte("c")},
	}
	conn := startFakeContainerServer(t, fake)
	srv := newTestServer(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "container_start", map[string]any{"app_name": "myapp", "max_chunks": 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	if text != "ab" {
		t.Errorf("text = %q, want %q (max_chunks should cap at 2 chunks)", text, "ab")
	}
}

func TestContainerAttach_MaxBytes_TruncatesOversizeOutput(t *testing.T) {
	fake := &fakeContainerServer{
		attachOutputs: [][]byte{[]byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")},
	}
	conn := startFakeContainerServer(t, fake)
	srv := newTestServer(&config.Config{}, nil)
	srv.SetConn(conn)

	result, err := srv.callTool(context.Background(), "container_attach", map[string]any{"app_name": "myapp", "max_bytes": 10})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	text := result.Content[0].(mcpgo.TextContent).Text
	if len(text) <= 10 {
		t.Errorf("expected truncated text with an appended note (len > 10), got %q", text)
	}
	if text[:10] != "aaaaaaaaaa" {
		t.Errorf("expected truncated text to start with the original bytes, got %q", text)
	}
}
