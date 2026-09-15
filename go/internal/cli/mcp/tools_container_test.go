package mcp

import (
	"context"
	"io"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func TestContainerStart_DescriptionMentionsEntitlements(t *testing.T) {
	srv := server.NewMCPServer("t", "0")
	s := New(&config.Config{}, nil)
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
	s := New(&config.Config{}, nil)
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
	exec          func(agentpb.WendyContainerService_ExecContainerServer) error
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

func (s *fakeContainerServer) ExecContainer(stream agentpb.WendyContainerService_ExecContainerServer) error {
	if s.exec == nil {
		return status.Error(codes.Unimplemented, "container exec unavailable")
	}
	return s.exec(stream)
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
	containers := listPayload(t, result, "containers")
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

func TestContainerAttach_MaxLinesAlias_LimitsChunks(t *testing.T) {
	fake := &fakeContainerServer{
		attachOutputs: [][]byte{[]byte("a"), []byte("b"), []byte("c"), []byte("d"), []byte("e")},
	}
	conn := startFakeContainerServer(t, fake)
	srv := New(&config.Config{}, nil)
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
	srv := New(&config.Config{}, nil)
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
	srv := New(&config.Config{}, nil)
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
	srv := New(&config.Config{}, nil)
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
	srv := New(&config.Config{}, nil)
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

func TestContainerAttachAndExecRequireApprovalAndExplainTheirBehavior(t *testing.T) {
	srv := server.NewMCPServer("t", "0")
	s := New(&config.Config{}, nil)
	s.registerContainerTools(srv)
	for _, name := range []string{"container_attach", "container_exec"} {
		tool, exists := srv.ListTools()[name]
		if !exists || tool.Tool.Annotations.ReadOnlyHint == nil || *tool.Tool.Annotations.ReadOnlyHint || tool.Tool.Annotations.DestructiveHint == nil || !*tool.Tool.Annotations.DestructiveHint {
			t.Fatalf("%s must advertise that it can disrupt the device and requires approval", name)
		}
	}
	description := srv.ListTools()["container_attach"].Tool.Description
	for _, phrase := range []string{"restart", "interrupts", "telemetry_logs", "container_exec"} {
		if !strings.Contains(description, phrase) {
			t.Fatalf("attach description must explain %q: %s", phrase, description)
		}
	}
}

func callContainerExec(t *testing.T, s *mcpServer, ctx context.Context, args map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	srv := server.NewMCPServer("t", "0")
	s.registerContainerTools(srv)
	result, err := srv.ListTools()["container_exec"].Handler(ctx, callToolReq("container_exec", args))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestContainerExecUsesActiveDirectOrCloudConnectionAndExactArgv(t *testing.T) {
	for _, connectionType := range []string{"direct", "cloud"} {
		t.Run(connectionType, func(t *testing.T) {
			command := []string{"printf", "$(touch should-not-run)", "argument with spaces", ""}
			started := make(chan *agentpb.ExecContainerRequest_ExecStart, 1)
			fake := &fakeContainerServer{exec: func(stream agentpb.WendyContainerService_ExecContainerServer) error {
				request, err := stream.Recv()
				if err != nil {
					return err
				}
				started <- request.GetStart()
				if _, err := stream.Recv(); err != io.EOF {
					t.Errorf("noninteractive execution must close stdin, got %v", err)
				}
				if err := stream.Send(&agentpb.ExecContainerResponse{ResponseType: &agentpb.ExecContainerResponse_StdoutData{StdoutData: []byte("hello")}}); err != nil {
					return err
				}
				if err := stream.Send(&agentpb.ExecContainerResponse{ResponseType: &agentpb.ExecContainerResponse_StderrData{StderrData: []byte("diagnostic")}}); err != nil {
					return err
				}
				return stream.Send(&agentpb.ExecContainerResponse{ResponseType: &agentpb.ExecContainerResponse_ExitCode{ExitCode: 0}})
			}}
			s := New(&config.Config{}, nil)
			s.SetConn(startFakeContainerServer(t, fake))
			s.SetConnType(connectionType)
			result := callContainerExec(t, s, context.Background(), map[string]any{"app_name": "go2-remote-control", "command": command})
			if result.IsError {
				t.Fatalf("exec failed: %s", toolResultText(t, result))
			}
			request := <-started
			if request.GetAppName() != "go2-remote-control" || request.GetTty() || !reflect.DeepEqual(request.GetCommand(), command) {
				t.Fatalf("exec changed its target, argv, or noninteractive behavior: %+v", request)
			}
			out := structuredMap(t, result)
			if out["stdout"] != "hello" || out["stderr"] != "diagnostic" || out["exit_code"] != int32(0) || out["truncated"] != false {
				t.Fatalf("exec result lost output or status: %#v", out)
			}
		})
	}
}

func TestContainerExecPreservesFailureAndExitCodeAfterOutputTruncation(t *testing.T) {
	for _, ending := range []string{"nonzero", "missing exit", "rpc error"} {
		t.Run(ending, func(t *testing.T) {
			fake := &fakeContainerServer{exec: func(stream agentpb.WendyContainerService_ExecContainerServer) error {
				if _, err := stream.Recv(); err != nil {
					return err
				}
				if err := stream.Send(&agentpb.ExecContainerResponse{ResponseType: &agentpb.ExecContainerResponse_StdoutData{StdoutData: []byte("abcd硬件")}}); err != nil {
					return err
				}
				if err := stream.Send(&agentpb.ExecContainerResponse{ResponseType: &agentpb.ExecContainerResponse_StderrData{StderrData: []byte("later output")}}); err != nil {
					return err
				}
				switch ending {
				case "nonzero":
					return stream.Send(&agentpb.ExecContainerResponse{ResponseType: &agentpb.ExecContainerResponse_ExitCode{ExitCode: 7}})
				case "rpc error":
					return status.Error(codes.NotFound, "container vanished")
				default:
					return nil
				}
			}}
			s := New(&config.Config{}, nil)
			s.SetConn(startFakeContainerServer(t, fake))
			result := callContainerExec(t, s, context.Background(), map[string]any{"app_name": "app", "command": []string{"test"}, "max_bytes": 5})
			out := structuredMap(t, result)
			if !result.IsError || out["stdout"] != "abcd" || out["stderr"] != "" || out["truncated"] != true || !utf8.ValidString(toolResultText(t, result)) {
				t.Fatalf("failed command output must stay bounded and retain error status: %#v", out)
			}
			if ending == "nonzero" && (out["exit_code"] != int32(7) || out["error_code"] != "COMMAND_FAILED") {
				t.Fatalf("output truncation lost final exit status: %#v", out)
			}
			if ending == "rpc error" && (out["error_code"] != "NOT_FOUND" || !strings.Contains(out["message"].(string), "container vanished")) {
				t.Fatalf("RPC error details were lost: %#v", out)
			}
			if ending == "missing exit" && !strings.Contains(out["message"].(string), "without an exit code") {
				t.Fatalf("missing exit code should report unknown completion: %#v", out)
			}
		})
	}
}

func TestContainerExecTimeoutCancelsRPCAndPreservesOutput(t *testing.T) {
	canceled := make(chan struct{})
	fake := &fakeContainerServer{exec: func(stream agentpb.WendyContainerService_ExecContainerServer) error {
		if _, err := stream.Recv(); err != nil {
			return err
		}
		if err := stream.Send(&agentpb.ExecContainerResponse{ResponseType: &agentpb.ExecContainerResponse_StdoutData{StdoutData: []byte("working")}}); err != nil {
			return err
		}
		<-stream.Context().Done()
		close(canceled)
		return stream.Context().Err()
	}}
	s := New(&config.Config{}, nil)
	s.SetConn(startFakeContainerServer(t, fake))
	result := callContainerExec(t, s, context.Background(), map[string]any{"app_name": "app", "command": []string{"wait"}, "timeout_seconds": 1})
	out := structuredMap(t, result)
	if !result.IsError || out["error_code"] != "TIMEOUT" || out["stdout"] != "working" {
		t.Fatalf("timeout must retain partial output and report failure: %#v", out)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("exec timeout did not release the remote RPC")
	}
}

func TestContainerExecErrorSummaryPrecedesLargeOutput(t *testing.T) {
	fake := &fakeContainerServer{exec: func(stream agentpb.WendyContainerService_ExecContainerServer) error {
		if _, err := stream.Recv(); err != nil {
			return err
		}
		if err := stream.Send(&agentpb.ExecContainerResponse{ResponseType: &agentpb.ExecContainerResponse_StdoutData{StdoutData: []byte(strings.Repeat("x", 40000))}}); err != nil {
			return err
		}
		return stream.Send(&agentpb.ExecContainerResponse{ResponseType: &agentpb.ExecContainerResponse_ExitCode{ExitCode: 2}})
	}}
	s := New(&config.Config{}, nil)
	s.SetConn(startFakeContainerServer(t, fake))
	result := callContainerExec(t, s, context.Background(), map[string]any{"app_name": "app", "command": []string{"test"}})
	if !result.IsError || toolResultText(t, result) != "[COMMAND_FAILED] command exited with code 2" || len(result.Content) != 2 {
		t.Fatalf("failure summary must precede a potentially truncated JSON result: %v", result.Content)
	}
	if len(structuredMap(t, result)["stdout"].(string)) != 40000 {
		t.Fatal("the short failure summary must not replace captured output")
	}
}

func TestContainerExecValidatesArgumentsBeforeRPC(t *testing.T) {
	var calls atomic.Int32
	fake := &fakeContainerServer{exec: func(agentpb.WendyContainerService_ExecContainerServer) error {
		calls.Add(1)
		return nil
	}}
	s := New(&config.Config{}, nil)
	s.SetConn(startFakeContainerServer(t, fake))
	for _, invalid := range []map[string]any{
		{"app_name": ""}, {"command": "echo hello"}, {"command": []string{}}, {"command": []string{""}},
		{"command": []any{"echo", nil}}, {"command": []string{"echo", "a\x00b"}},
		{"command": []string{strings.Repeat("x", 16385)}},
		{"timeout_seconds": 0}, {"timeout_seconds": 301}, {"timeout_seconds": 1.5}, {"max_bytes": 0}, {"max_bytes": 1000001},
	} {
		args := map[string]any{"app_name": "app", "command": []string{"true"}}
		for key, value := range invalid {
			args[key] = value
		}
		result := callContainerExec(t, s, context.Background(), args)
		if !result.IsError || structuredMap(t, result)["error_code"] != "INVALID_ARGUMENT" {
			t.Fatalf("invalid arguments were accepted: %#v", invalid)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid arguments reached the device")
	}
	s.SetConn(nil)
	result := callContainerExec(t, s, context.Background(), map[string]any{"app_name": "app", "command": []string{"true"}})
	if !result.IsError || structuredMap(t, result)["error_code"] != "NOT_CONNECTED" {
		t.Fatal("exec must require an active connection")
	}
}
