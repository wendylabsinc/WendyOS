package commands

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

type fakeMacRunState struct {
	fakeSyncServer
	createReqs []*agentpb.CreateContainerRequest
}

type fakeMacAgentServer struct {
	agentpb.UnimplementedWendyAgentServiceServer
}

func (s *fakeMacAgentServer) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	return &agentpb.GetAgentVersionResponse{CpuArchitecture: runtime.GOARCH}, nil
}

type fakeMacContainerServer struct {
	agentpb.UnimplementedWendyContainerServiceServer
	state *fakeMacRunState
}

func (s *fakeMacContainerServer) CreateContainer(_ context.Context, req *agentpb.CreateContainerRequest) (*agentpb.CreateContainerResponse, error) {
	s.state.createReqs = append(s.state.createReqs, proto.Clone(req).(*agentpb.CreateContainerRequest))
	return &agentpb.CreateContainerResponse{}, nil
}

func startFakeMacRunServer(t *testing.T, state *fakeMacRunState) (*grpcclient.AgentConnection, func()) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	s := grpc.NewServer()
	agentpb.RegisterWendyAgentServiceServer(s, &fakeMacAgentServer{})
	agentpb.RegisterWendyContainerServiceServer(s, &fakeMacContainerServer{state: state})
	agentpb.RegisterWendyFileSyncServiceServer(s, &state.fakeSyncServer)
	go func() { _ = s.Serve(ln) }()

	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		s.Stop()
		ln.Close()
		t.Fatalf("grpc.NewClient: %v", err)
	}

	ac := &grpcclient.AgentConnection{
		Conn:             conn,
		AgentService:     agentpb.NewWendyAgentServiceClient(conn),
		ContainerService: agentpb.NewWendyContainerServiceClient(conn),
		FileSyncService:  agentpb.NewWendyFileSyncServiceClient(conn),
	}

	cleanup := func() {
		_ = conn.Close()
		s.Stop()
		_ = ln.Close()
	}
	return ac, cleanup
}

func TestRunMacOSXcodeWithAgent_UsesRunArgsFromAppConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "MyApp.xcodeproj"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	original := execCommandContext
	t.Cleanup(func() { execCommandContext = original })
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		scheme := "MyScheme"
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-scheme" {
				scheme = args[i+1]
				break
			}
		}
		productPath := filepath.Join(dir, ".xcode", "Build", "Products", "Release", scheme)
		if err := os.MkdirAll(filepath.Dir(productPath), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(productPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		return exec.CommandContext(ctx, "true")
	}

	state := &fakeMacRunState{}
	conn, cleanup := startFakeMacRunServer(t, state)
	defer cleanup()

	appCfg := &appconfig.AppConfig{
		AppID: "sh.wendy.MyXcodeApp",
		Xcode: &appconfig.XcodeConfig{Scheme: "MyScheme"},
		Run:   &appconfig.RunConfig{Args: []string{"--from-config", "hello world"}},
	}

	err := runMacOSXcodeWithAgent(context.Background(), conn, dir, appCfg, runOptions{
		deploy:   true,
		userArgs: []string{"--ignored-cli"},
	})
	if err != nil {
		t.Fatalf("runMacOSXcodeWithAgent: %v", err)
	}

	if len(state.createReqs) != 1 {
		t.Fatalf("CreateContainer calls = %d, want 1", len(state.createReqs))
	}
	got := state.createReqs[0]
	if got.AppName != appCfg.AppID {
		t.Fatalf("AppName = %q, want %q", got.AppName, appCfg.AppID)
	}
	if got.Cmd != "MyScheme" {
		t.Fatalf("Cmd = %q, want %q", got.Cmd, "MyScheme")
	}
	if len(got.UserArgs) != 2 || got.UserArgs[0] != "--from-config" || got.UserArgs[1] != "hello world" {
		t.Fatalf("UserArgs = %v, want %v", got.UserArgs, appCfg.Run.Args)
	}
}

func TestRunMacOSXcodeWithAgent_SendsAppBundleRootForAgentSideResolution(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "MyApp.xcodeproj"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	original := execCommandContext
	t.Cleanup(func() { execCommandContext = original })
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		productPath := filepath.Join(dir, ".xcode", "Build", "Products", "Release", "MyBundle.app", "Contents", "MacOS")
		if err := os.MkdirAll(productPath, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(productPath, "ActualExecutable"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("WriteFile executable: %v", err)
		}
		return exec.CommandContext(ctx, "true")
	}

	state := &fakeMacRunState{}
	conn, cleanup := startFakeMacRunServer(t, state)
	defer cleanup()

	appCfg := &appconfig.AppConfig{
		AppID: "sh.wendy.MyBundleApp",
		Xcode: &appconfig.XcodeConfig{Scheme: "MyBundle"},
	}

	err := runMacOSXcodeWithAgent(context.Background(), conn, dir, appCfg, runOptions{deploy: true})
	if err != nil {
		t.Fatalf("runMacOSXcodeWithAgent: %v", err)
	}

	if len(state.createReqs) != 1 {
		t.Fatalf("CreateContainer calls = %d, want 1", len(state.createReqs))
	}
	if got := state.createReqs[0].Cmd; got != "MyBundle.app" {
		t.Fatalf("Cmd = %q, want %q", got, "MyBundle.app")
	}
}

func TestRunMacOSSwiftPMWithAgent_UsesRunArgsFromAppConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Package.swift"), []byte("// test package\n"), 0o644); err != nil {
		t.Fatalf("WriteFile Package.swift: %v", err)
	}

	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("MkdirAll bin: %v", err)
	}

	swiftlyPath := filepath.Join(binDir, "swiftly")
	swiftPath := filepath.Join(binDir, "swift")
	if err := os.WriteFile(swiftlyPath, []byte("#!/bin/sh\necho '{\"products\":[{\"name\":\"MySwiftApp\"}]}'\n"), 0o755); err != nil {
		t.Fatalf("WriteFile swiftly: %v", err)
	}
	if err := os.WriteFile(swiftPath, []byte("#!/bin/sh\nif [ \"$1\" = \"build\" ]; then\n  mkdir -p \"$PWD/.build/debug\"\n  printf '#!/bin/sh\\n' > \"$PWD/.build/debug/MySwiftApp\"\n  chmod +x \"$PWD/.build/debug/MySwiftApp\"\n  exit 0\nfi\necho \"unexpected args: $@\" >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("WriteFile swift: %v", err)
	}

	originalPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", binDir+string(os.PathListSeparator)+originalPath); err != nil {
		t.Fatalf("Setenv PATH: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Setenv("PATH", originalPath)
	})

	state := &fakeMacRunState{}
	conn, cleanup := startFakeMacRunServer(t, state)
	defer cleanup()

	appCfg := &appconfig.AppConfig{
		AppID: "sh.wendy.MySwiftApp",
		Run:   &appconfig.RunConfig{Args: []string{"--port", "8080"}},
	}

	err := runMacOSSwiftPMWithAgent(context.Background(), conn, dir, appCfg, runOptions{
		deploy:   true,
		userArgs: []string{"--ignored-cli"},
	})
	if err != nil {
		t.Fatalf("runMacOSSwiftPMWithAgent: %v", err)
	}

	if len(state.createReqs) != 1 {
		t.Fatalf("CreateContainer calls = %d, want 1", len(state.createReqs))
	}
	got := state.createReqs[0]
	if got.AppName != appCfg.AppID {
		t.Fatalf("AppName = %q, want %q", got.AppName, appCfg.AppID)
	}
	if got.Cmd != "MySwiftApp" {
		t.Fatalf("Cmd = %q, want %q", got.Cmd, "MySwiftApp")
	}
	if len(got.UserArgs) != 2 || got.UserArgs[0] != "--port" || got.UserArgs[1] != "8080" {
		t.Fatalf("UserArgs = %v, want %v", got.UserArgs, appCfg.Run.Args)
	}
}
