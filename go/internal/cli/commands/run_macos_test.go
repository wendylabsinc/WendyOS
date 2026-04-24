package commands

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
	startReqs  []*agentpb.StartContainerRequest
}

type fakeMacAgentServer struct {
	agentpb.UnimplementedWendyAgentServiceServer
}

func (s *fakeMacAgentServer) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	return &agentpb.GetAgentVersionResponse{Os: "darwin", CpuArchitecture: runtime.GOARCH}, nil
}

type fakeMacContainerServer struct {
	agentpb.UnimplementedWendyContainerServiceServer
	state *fakeMacRunState
}

func (s *fakeMacContainerServer) CreateContainer(_ context.Context, req *agentpb.CreateContainerRequest) (*agentpb.CreateContainerResponse, error) {
	s.state.createReqs = append(s.state.createReqs, proto.Clone(req).(*agentpb.CreateContainerRequest))
	return &agentpb.CreateContainerResponse{}, nil
}

func (s *fakeMacContainerServer) StartContainer(req *agentpb.StartContainerRequest, _ grpc.ServerStreamingServer[agentpb.RunContainerLayersResponse]) error {
	s.state.startReqs = append(s.state.startReqs, proto.Clone(req).(*agentpb.StartContainerRequest))
	return nil
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
	if err := os.WriteFile(swiftlyPath, []byte("#!/bin/sh\necho '{\"products\":[{\"name\":\"MySwiftApp\",\"type\":{\"executable\":null}}]}'\n"), 0o755); err != nil {
		t.Fatalf("WriteFile swiftly: %v", err)
	}
	if err := os.WriteFile(swiftPath, []byte("#!/bin/sh\nif [ \"$1\" = \"build\" ] && [ \"$2\" = \"--show-bin-path\" ]; then\n  echo \"$PWD/.build/debug\"\n  exit 0\nfi\nif [ \"$1\" = \"build\" ]; then\n  mkdir -p \"$PWD/.build/debug/MySwiftApp.bundle\" \"$PWD/.build/debug/MySwiftApp.resources\"\n  printf '#!/bin/sh\\n' > \"$PWD/.build/debug/MySwiftApp\"\n  printf '<plist/>' > \"$PWD/.build/debug/MySwiftApp.bundle/Info.plist\"\n  printf '{}' > \"$PWD/.build/debug/MySwiftApp.resources/config.json\"\n  chmod +x \"$PWD/.build/debug/MySwiftApp\"\n  exit 0\nfi\necho \"unexpected args: $@\" >&2\nexit 1\n"), 0o755); err != nil {
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

	acked := make(map[string]bool)
	for _, path := range state.ackedPaths {
		acked[path] = true
	}
	if !acked["MySwiftApp"] {
		t.Fatalf("missing ack for MySwiftApp; got %v", state.ackedPaths)
	}
	if !acked["MySwiftApp.bundle/Info.plist"] {
		t.Fatalf("missing ack for MySwiftApp.bundle/Info.plist; got %v", state.ackedPaths)
	}
	if !acked["MySwiftApp.resources/config.json"] {
		t.Fatalf("missing ack for MySwiftApp.resources/config.json; got %v", state.ackedPaths)
	}
}

func TestAssembleSwiftPMSyncEntries_IncludesSiblingResourceDirectories(t *testing.T) {
	binDir := t.TempDir()
	binaryPath := filepath.Join(binDir, "MySwiftApp")
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile binary: %v", err)
	}

	bundleDir := filepath.Join(binDir, "MySwiftApp.bundle")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatalf("MkdirAll bundle: %v", err)
	}

	resourcesDir := filepath.Join(binDir, "MySwiftApp.resources")
	if err := os.MkdirAll(resourcesDir, 0o755); err != nil {
		t.Fatalf("MkdirAll resources: %v", err)
	}

	cwd := t.TempDir()
	cfg := &appconfig.AppConfig{AppID: "sh.wendy.MySwiftApp"}

	entries, err := assembleSwiftPMSyncEntries(binaryPath, cwd, cfg)
	if err != nil {
		t.Fatalf("assembleSwiftPMSyncEntries: %v", err)
	}

	remotes := make(map[string]bool)
	for _, entry := range entries {
		remotes[entry.remotePath] = true
	}
	if !remotes["MySwiftApp"] {
		t.Fatalf("expected binary entry with remotePath MySwiftApp")
	}
	if !remotes["MySwiftApp.bundle"] {
		t.Fatalf("expected bundle entry with remotePath MySwiftApp.bundle")
	}
	if !remotes["MySwiftApp.resources"] {
		t.Fatalf("expected resources entry with remotePath MySwiftApp.resources")
	}
}

func TestRunWithAgent_RejectsLinuxContainersOnMacs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("WriteFile Dockerfile: %v", err)
	}

	state := &fakeMacRunState{}
	conn, cleanup := startFakeMacRunServer(t, state)
	defer cleanup()

	appCfg := &appconfig.AppConfig{
		AppID:    "sh.wendy.MacLinuxContainer",
		Platform: "linux/arm64",
	}

	err := runWithAgent(context.Background(), conn, dir, appCfg, runOptions{})
	if err == nil {
		t.Fatal("runWithAgent error = nil, want unsupported platform error")
	}
	if got := err.Error(); !strings.Contains(got, "Linux containers aren't supported on Macs yet") {
		t.Fatalf("runWithAgent error = %q, want unsupported Macs message", got)
	}
	if len(state.createReqs) != 0 {
		t.Fatalf("CreateContainer calls = %d, want 0", len(state.createReqs))
	}
	if len(state.startReqs) != 0 {
		t.Fatalf("StartContainer calls = %d, want 0", len(state.startReqs))
	}
}

func TestStartAndStreamContainer_FallsBackWhenCreateProgressIsUnimplemented(t *testing.T) {
	origInteractive := isInteractiveTerminalFn
	t.Cleanup(func() { isInteractiveTerminalFn = origInteractive })
	isInteractiveTerminalFn = func() bool { return false }

	state := &fakeMacRunState{}
	conn, cleanup := startFakeMacRunServer(t, state)
	defer cleanup()

	appCfg := &appconfig.AppConfig{AppID: "sh.wendy.LegacyLinuxApp"}
	createReq := &agentpb.CreateContainerRequest{
		AppName:   appCfg.AppID,
		ImageName: "localhost:5000/sh.wendy.legacylinuxapp:latest",
	}

	err := startAndStreamContainer(context.Background(), conn, appCfg, createReq, runOptions{detach: true})
	if err != nil {
		t.Fatalf("startAndStreamContainer: %v", err)
	}

	if len(state.createReqs) != 1 {
		t.Fatalf("CreateContainer calls = %d, want 1", len(state.createReqs))
	}
	if state.createReqs[0].GetAppName() != appCfg.AppID {
		t.Fatalf("CreateContainer AppName = %q, want %q", state.createReqs[0].GetAppName(), appCfg.AppID)
	}
	if len(state.startReqs) != 1 {
		t.Fatalf("StartContainer calls = %d, want 1", len(state.startReqs))
	}
	if state.startReqs[0].GetAppName() != appCfg.AppID {
		t.Fatalf("StartContainer AppName = %q, want %q", state.startReqs[0].GetAppName(), appCfg.AppID)
	}
}
