package commands

import (
	"context"
	"encoding/json"
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

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
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

// fakeMacProvisioningServer reports "not provisioned" so requireRegistryAuth
// (used on the docker/compose/python push path) doesn't require CLI mTLS
// certs in tests. The gate's provisioned branch — the actual security control —
// is exercised separately in TestRequireRegistryAuth_* below.
type fakeMacProvisioningServer struct {
	agentpb.UnimplementedWendyProvisioningServiceServer
}

func (s *fakeMacProvisioningServer) IsProvisioned(context.Context, *agentpb.IsProvisionedRequest) (*agentpb.IsProvisionedResponse, error) {
	return &agentpb.IsProvisionedResponse{
		Response: &agentpb.IsProvisionedResponse_NotProvisioned{NotProvisioned: &agentpb.NotProvisionedResponse{}},
	}, nil
}

func (s *fakeMacContainerServer) CreateContainer(_ context.Context, req *agentpb.CreateContainerRequest) (*agentpb.CreateContainerResponse, error) {
	s.state.createReqs = append(s.state.createReqs, proto.Clone(req).(*agentpb.CreateContainerRequest))
	return &agentpb.CreateContainerResponse{}, nil
}

func (s *fakeMacContainerServer) ListContainers(_ *agentpb.ListContainersRequest, stream grpc.ServerStreamingServer[agentpb.ListContainersResponse]) error {
	for _, req := range s.state.createReqs {
		if err := stream.Send(&agentpb.ListContainersResponse{Container: &agentpb.AppContainer{AppName: req.AppName, RunningState: agentpb.AppRunningState_RUNNING}}); err != nil {
			return err
		}
	}
	return nil
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
	agentpb.RegisterWendyProvisioningServiceServer(s, &fakeMacProvisioningServer{})
	go func() { _ = s.Serve(ln) }()

	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		s.Stop()
		ln.Close()
		t.Fatalf("grpc.NewClient: %v", err)
	}

	ac := &grpcclient.AgentConnection{
		Conn:                conn,
		AgentService:        agentpb.NewWendyAgentServiceClient(conn),
		ContainerService:    agentpb.NewWendyContainerServiceClient(conn),
		FileSyncService:     agentpb.NewWendyFileSyncServiceClient(conn),
		ProvisioningService: agentpb.NewWendyProvisioningServiceClient(conn),
	}

	cleanup := func() {
		_ = conn.Close()
		s.Stop()
		_ = ln.Close()
	}
	return ac, cleanup
}

func TestRunWithAgent_AllowsNativeDarwinXcodeAndUsesRunArgsFromAppConfig(t *testing.T) {
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
		AppID:    "sh.wendy.MyXcodeApp",
		Platform: appconfig.PlatformDarwin,
		Xcode:    &appconfig.XcodeConfig{Scheme: "MyScheme"},
		Run:      &appconfig.RunConfig{Args: []string{"--from-config", "hello world"}},
	}

	err := runWithAgent(context.Background(), conn, dir, appCfg, runOptions{
		deploy:   true,
		userArgs: []string{"--ignored-cli"},
	})
	if err != nil {
		t.Fatalf("runWithAgent: %v", err)
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
	if err := os.WriteFile(filepath.Join(dir, "Brewfile.wendy"), []byte("brew \"jq\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile Brewfile.wendy: %v", err)
	}

	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("MkdirAll bin: %v", err)
	}

	swiftPath := filepath.Join(binDir, "swift")
	if err := os.WriteFile(swiftPath, []byte("#!/bin/sh\nif [ \"$1\" = \"package\" ] && [ \"$2\" = \"dump-package\" ]; then\n  echo '{\"products\":[{\"name\":\"MySwiftApp\",\"type\":{\"executable\":null}}]}'\n  exit 0\nfi\nif [ \"$1\" = \"build\" ] && [ \"$2\" = \"-c\" ] && [ \"$3\" = \"release\" ] && [ \"$4\" = \"--show-bin-path\" ]; then\n  echo \"$PWD/.build/release\"\n  exit 0\nfi\nif [ \"$1\" = \"build\" ] && [ \"$2\" = \"-c\" ] && [ \"$3\" = \"release\" ]; then\n  mkdir -p \"$PWD/.build/release/MySwiftApp.bundle\" \"$PWD/.build/release/MySwiftApp.resources\"\n  printf '#!/bin/sh\\n' > \"$PWD/.build/release/MySwiftApp\"\n  printf '<plist/>' > \"$PWD/.build/release/MySwiftApp.bundle/Info.plist\"\n  printf '{}' > \"$PWD/.build/release/MySwiftApp.resources/config.json\"\n  chmod +x \"$PWD/.build/release/MySwiftApp\"\n  exit 0\nfi\necho \"unexpected args: $@\" >&2\nexit 1\n"), 0o755); err != nil {
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
	var sentConfig appconfig.AppConfig
	if err := json.Unmarshal(got.AppConfig, &sentConfig); err != nil {
		t.Fatalf("unmarshal AppConfig: %v", err)
	}
	if sentConfig.Brewfile != "Brewfile.wendy" {
		t.Fatalf("AppConfig Brewfile = %q, want Brewfile.wendy", sentConfig.Brewfile)
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
	if !acked["Brewfile.wendy"] {
		t.Fatalf("missing ack for Brewfile.wendy; got %v", state.ackedPaths)
	}
}

func TestResolveNativeBrewfileSyncEntry_IgnoresProjectRootBrewfile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Brewfile"), []byte("brew \"jq\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile Brewfile: %v", err)
	}

	cfg := &appconfig.AppConfig{AppID: "sh.wendy.MySwiftApp"}
	entry, err := resolveNativeBrewfileSyncEntry(dir, cfg)
	if err != nil {
		t.Fatalf("resolveNativeBrewfileSyncEntry: %v", err)
	}
	if entry != nil {
		t.Fatalf("entry = %+v, want nil for project-root Brewfile", entry)
	}
	if cfg.Brewfile != "" {
		t.Fatalf("cfg.Brewfile = %q, want empty", cfg.Brewfile)
	}
}

func TestResolveNativeBrewfileSyncEntry_RejectsSymlinkBrewfile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "RealBrewfile")
	link := filepath.Join(dir, "Brewfile.wendy")
	if err := os.WriteFile(target, []byte("brew \"jq\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile RealBrewfile: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink Brewfile.wendy: %v", err)
	}

	cfg := &appconfig.AppConfig{AppID: "sh.wendy.MySwiftApp"}
	_, err := resolveNativeBrewfileSyncEntry(dir, cfg)
	if err == nil {
		t.Fatal("resolveNativeBrewfileSyncEntry succeeded; want symlink rejection")
	}
	if !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("error = %v, want regular file", err)
	}
}

func TestAppendNativeBrewfileSyncEntry_DeduplicatesSameSource(t *testing.T) {
	dir := t.TempDir()
	brewfilePath := filepath.Join(dir, "ops", "Brewfile")
	if err := os.MkdirAll(filepath.Dir(brewfilePath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(brewfilePath, []byte("brew \"jq\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile Brewfile: %v", err)
	}

	cfg := &appconfig.AppConfig{AppID: "sh.wendy.MySwiftApp", Brewfile: "ops/Brewfile"}
	entries := []fileSyncEntry{{localPath: brewfilePath, remotePath: "ops/Brewfile"}}
	got, err := appendNativeBrewfileSyncEntry(entries, dir, cfg)
	if err != nil {
		t.Fatalf("appendNativeBrewfileSyncEntry: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("entries count = %d, want 1", len(got))
	}
}

func TestAppendNativeBrewfileSyncEntry_RejectsConflictingFilesMapping(t *testing.T) {
	dir := t.TempDir()
	appBrewfilePath := filepath.Join(dir, "ops", "Brewfile")
	devBrewfilePath := filepath.Join(dir, "dev", "Brewfile")
	for _, path := range []string{appBrewfilePath, devBrewfilePath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	if err := os.WriteFile(appBrewfilePath, []byte("brew \"jq\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile app Brewfile: %v", err)
	}
	if err := os.WriteFile(devBrewfilePath, []byte("brew \"mas\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile dev Brewfile: %v", err)
	}

	cfg := &appconfig.AppConfig{AppID: "sh.wendy.MySwiftApp", Brewfile: "ops/Brewfile"}
	entries := []fileSyncEntry{{localPath: devBrewfilePath, remotePath: "ops/Brewfile"}}
	_, err := appendNativeBrewfileSyncEntry(entries, dir, cfg)
	if err == nil {
		t.Fatal("expected conflict error")
	}
	if !strings.Contains(err.Error(), "conflicts with another synced file") {
		t.Fatalf("error = %q, want conflict message", err.Error())
	}
}

func TestAppendNativeBrewfileSyncEntry_DeduplicatesDirectoryCoveringSameSource(t *testing.T) {
	dir := t.TempDir()
	brewfilePath := filepath.Join(dir, "ops", "Brewfile")
	if err := os.MkdirAll(filepath.Dir(brewfilePath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(brewfilePath, []byte("brew \"jq\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile Brewfile: %v", err)
	}

	cfg := &appconfig.AppConfig{AppID: "sh.wendy.MySwiftApp", Brewfile: "ops/Brewfile"}
	entries := []fileSyncEntry{{localPath: filepath.Join(dir, "ops"), remotePath: "ops"}}
	got, err := appendNativeBrewfileSyncEntry(entries, dir, cfg)
	if err != nil {
		t.Fatalf("appendNativeBrewfileSyncEntry: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("entries count = %d, want 1", len(got))
	}
}

func TestAppendNativeBrewfileSyncEntry_AppendsWhenDirectoryDoesNotContainBrewfile(t *testing.T) {
	dir := t.TempDir()
	brewfilePath := filepath.Join(dir, "ops", "Brewfile")
	assetsDir := filepath.Join(dir, "assets")
	if err := os.MkdirAll(filepath.Dir(brewfilePath), 0o755); err != nil {
		t.Fatalf("MkdirAll ops: %v", err)
	}
	if err := os.MkdirAll(assetsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll assets: %v", err)
	}
	if err := os.WriteFile(brewfilePath, []byte("brew \"jq\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile Brewfile: %v", err)
	}

	cfg := &appconfig.AppConfig{AppID: "sh.wendy.MySwiftApp", Brewfile: "ops/Brewfile"}
	entries := []fileSyncEntry{{localPath: assetsDir, remotePath: "ops"}}
	got, err := appendNativeBrewfileSyncEntry(entries, dir, cfg)
	if err != nil {
		t.Fatalf("appendNativeBrewfileSyncEntry: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("entries count = %d, want 2", len(got))
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

// preflightMissingAppConfigForMacTarget runs before a wendy.json exists, so it
// always resolves the effective platform as "linux/<arch>" (the same default
// resolveAgentPlatform uses for an empty cfgPlatform). Now that Linux/WendyOS
// containers are supported on the Mac agent, that default is no longer a
// mismatch for any of these project types, so this early gate should let them
// through and defer to the normal ensureAppConfig/runWithAgent path.
func TestPreflightMissingAppConfigForMacTarget_AllowsContainerAndSwiftProjectsBeforeConfigPrompt(t *testing.T) {
	for _, pt := range []string{"docker", "python", "compose", "multi-service", "swift", "xcode"} {
		t.Run(pt, func(t *testing.T) {
			state := &fakeMacRunState{}
			conn, cleanup := startFakeMacRunServer(t, state)
			defer cleanup()

			target := &SelectedDevice{Agent: conn}
			if err := preflightMissingAppConfigForMacTarget(context.Background(), target, pt); err != nil {
				t.Fatalf("preflightMissingAppConfigForMacTarget(%q) = %v, want nil", pt, err)
			}
		})
	}
}

func TestRejectUnsupportedMacRunProject_AllowsLinuxContainers(t *testing.T) {
	// Linux container projects are now supported on the Mac agent.
	for _, pt := range []string{"docker", "python", "compose", "multi-service"} {
		if err := rejectUnsupportedMacRunProject(pt, "linux/arm64"); err != nil {
			t.Fatalf("projectType %q on linux/arm64 should be allowed, got: %v", pt, err)
		}
	}
	// Native darwin still fine.
	if err := rejectUnsupportedMacRunProject("swift", "darwin"); err != nil {
		t.Fatalf("swift/darwin should be allowed, got: %v", err)
	}
	// A genuinely unsupported platform still rejected.
	if err := rejectUnsupportedMacRunProject("docker", "windows/amd64"); err == nil {
		t.Fatalf("windows target should still be rejected")
	}
}

// Linux/WendyOS-platform docker, python, compose (via companion wendy.json),
// and multi-service projects are now allowed to target a Mac agent (they run
// as Linux containers via the Mac agent's container runtime). Only a
// genuinely foreign platform (e.g. windows) is still a real project/target
// mismatch. This is exercised at the runWithAgent level (not just the
// isolated rejectUnsupportedMacRunProject gate) so the platform check
// continues to fire before any build/push is attempted.
func TestRunWithAgent_RejectsGenuinelyUnsupportedPlatform(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("WriteFile Dockerfile: %v", err)
	}

	state := &fakeMacRunState{}
	conn, cleanup := startFakeMacRunServer(t, state)
	defer cleanup()

	appCfg := &appconfig.AppConfig{AppID: "sh.wendy.MacWindowsContainer", Platform: "windows/amd64"}
	err := runWithAgent(context.Background(), conn, dir, appCfg, runOptions{})
	if err == nil {
		t.Fatal("runWithAgent error = nil, want unsupported platform error")
	}
	got := err.Error()
	if !strings.Contains(got, "Project/target mismatch") || !strings.Contains(got, "platform: \"darwin\"") {
		t.Fatalf("runWithAgent error = %q, want Mac project guidance", got)
	}
	if strings.Contains(got, "agent version") || strings.Contains(got, "updating") {
		t.Fatalf("runWithAgent error = %q, should not suggest updating the agent", got)
	}
	if len(state.createReqs) != 0 {
		t.Fatalf("CreateContainer calls = %d, want 0", len(state.createReqs))
	}
	if len(state.startReqs) != 0 {
		t.Fatalf("StartContainer calls = %d, want 0", len(state.startReqs))
	}
}

// runComposeWithAgent always resolves the effective platform via
// resolveAgentPlatform("", agentOS, architecture) (compose has no per-run
// wendy.json platform override), which defaults to "linux/<arch>" regardless
// of the agent's actual OS. That means a compose run against a Mac agent is
// now unconditionally allowed by rejectUnsupportedMacRunProject (linux is a
// supported platform), so the old "reject before registry setup" behavior no
// longer exists to test at this call site: the gate simply passes through.
// Confirm end-to-end (using a pre-built image so no real docker build runs)
// that the old native-only rejection message is gone and the container is
// created.
func TestRunComposeWithAgent_AllowsMacAgentWithPrebuiltImage(t *testing.T) {
	dir := t.TempDir()
	compose := []byte("services:\n  web:\n    image: docker.io/library/nginx:latest\n")
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), compose, 0o644); err != nil {
		t.Fatalf("WriteFile compose.yaml: %v", err)
	}

	state := &fakeMacRunState{}
	conn, cleanup := startFakeMacRunServer(t, state)
	defer cleanup()

	err := runComposeWithAgent(context.Background(), conn, dir, runOptions{deploy: true})
	if err != nil {
		t.Fatalf("runComposeWithAgent: %v", err)
	}
	if len(state.createReqs) != 1 {
		t.Fatalf("CreateContainer calls = %d, want 1", len(state.createReqs))
	}
}

func TestRunMacOSNativeContainer_OverwritesPrepopulatedAppConfig(t *testing.T) {
	state := &fakeMacRunState{}
	conn, cleanup := startFakeMacRunServer(t, state)
	defer cleanup()

	appCfg := &appconfig.AppConfig{
		AppID:    "sh.wendy.MySwiftApp",
		Platform: appconfig.PlatformDarwin,
		Brewfile: "Brewfile.wendy",
	}
	createReq := &agentpb.CreateContainerRequest{
		AppName:   appCfg.AppID,
		Cmd:       "MySwiftApp",
		AppConfig: []byte(`{"appId":"sh.wendy.MySwiftApp","brewfile":"stale/Brewfile"}`),
	}

	if err := runMacOSNativeContainer(context.Background(), conn, appCfg, createReq, runOptions{deploy: true}); err != nil {
		t.Fatalf("runMacOSNativeContainer: %v", err)
	}

	if len(state.createReqs) != 1 {
		t.Fatalf("CreateContainer calls = %d, want 1", len(state.createReqs))
	}
	var sentConfig appconfig.AppConfig
	if err := json.Unmarshal(state.createReqs[0].AppConfig, &sentConfig); err != nil {
		t.Fatalf("unmarshal AppConfig: %v", err)
	}
	if sentConfig.Brewfile != "Brewfile.wendy" {
		t.Fatalf("AppConfig Brewfile = %q, want Brewfile.wendy", sentConfig.Brewfile)
	}
}

// fakeProvisionedServer reports the device as provisioned so that
// requireRegistryAuth must enforce the CLI-cert (mTLS) requirement.
type fakeProvisionedServer struct {
	agentpb.UnimplementedWendyProvisioningServiceServer
}

func (s *fakeProvisionedServer) IsProvisioned(context.Context, *agentpb.IsProvisionedRequest) (*agentpb.IsProvisionedResponse, error) {
	return &agentpb.IsProvisionedResponse{
		Response: &agentpb.IsProvisionedResponse_Provisioned{Provisioned: &agentpb.ProvisionedResponse{}},
	}, nil
}

// startFakeProvisioningServer stands up a gRPC server exposing only the
// provisioning service backed by prov, returning a connection to it.
func startFakeProvisioningServer(t *testing.T, prov agentpb.WendyProvisioningServiceServer) (*grpcclient.AgentConnection, func()) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	s := grpc.NewServer()
	agentpb.RegisterWendyProvisioningServiceServer(s, prov)
	go func() { _ = s.Serve(ln) }()

	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		s.Stop()
		_ = ln.Close()
		t.Fatalf("grpc.NewClient: %v", err)
	}

	ac := &grpcclient.AgentConnection{
		Conn:                conn,
		ProvisioningService: agentpb.NewWendyProvisioningServiceClient(conn),
	}
	cleanup := func() {
		_ = conn.Close()
		s.Stop()
		_ = ln.Close()
	}
	return ac, cleanup
}

// A provisioned device's registry requires mTLS. When the CLI has no client
// certificate loaded, requireRegistryAuth must fail with actionable guidance
// rather than letting the push proceed unauthenticated. This is the security
// gate that the fakeMacProvisioningServer ("not provisioned") deliberately
// bypasses in the other Mac run tests.
func TestRequireRegistryAuth_EnforcesMTLSWhenProvisionedWithoutCert(t *testing.T) {
	// Point config resolution (loadCLICert -> config.Load -> os.UserHomeDir) at
	// an empty home so no CLI certificate is present regardless of the machine.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	conn, cleanup := startFakeProvisioningServer(t, &fakeProvisionedServer{})
	defer cleanup()

	err := requireRegistryAuth(context.Background(), conn)
	if err == nil {
		t.Fatal("requireRegistryAuth = nil; want mTLS-required error for a provisioned device with no CLI cert")
	}
	if !strings.Contains(err.Error(), "mTLS authentication") {
		t.Fatalf("error = %q, want mTLS authentication guidance", err.Error())
	}
}

// A device that reports "not provisioned" has an unauthenticated registry, so
// requireRegistryAuth passes through even with no CLI cert. This documents the
// intended bypass that fakeMacProvisioningServer relies on.
func TestRequireRegistryAuth_AllowsWhenNotProvisioned(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	conn, cleanup := startFakeProvisioningServer(t, &fakeMacProvisioningServer{})
	defer cleanup()

	if err := requireRegistryAuth(context.Background(), conn); err != nil {
		t.Fatalf("requireRegistryAuth = %v; want nil for an unprovisioned device", err)
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
