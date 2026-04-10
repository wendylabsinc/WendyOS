package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/internal/cli/providers"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/internal/shared/models"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

var cliStyle = lipgloss.NewStyle().Foreground(tui.ColorDim)
var cliNoticeStyle = lipgloss.NewStyle().Foreground(tui.ColorNotice)

// dimWriter writes each line rendered through cliStyle (dim/background).
// Incomplete lines are buffered until a newline or Flush is called.
type dimWriter struct {
	buf strings.Builder
}

func (w *dimWriter) Write(p []byte) (int, error) {
	for _, b := range p {
		if b == '\n' {
			fmt.Println(cliStyle.Render(w.buf.String()))
			w.buf.Reset()
		} else {
			w.buf.WriteByte(b)
		}
	}
	return len(p), nil
}

func (w *dimWriter) Flush() {
	if w.buf.Len() > 0 {
		fmt.Println(cliStyle.Render(w.buf.String()))
		w.buf.Reset()
	}
}

// containerOutputStream is satisfied by both the bidi AttachContainer stream
// and the server-streaming StartContainer stream.
type containerOutputStream interface {
	Recv() (*agentpb.RunContainerLayersResponse, error)
}

// openContainerStream opens an AttachContainer bidi stream and starts a
// goroutine that pumps local stdin to the remote process. If the stream cannot
// be opened (e.g. the agent is too old and returns Unimplemented), it logs a
// notice and falls back to a plain StartContainer stream. Returns the output
// stream and whether stdin is being forwarded.
func openContainerStream(ctx context.Context, svc agentpb.WendyContainerServiceClient, appName string) (containerOutputStream, bool, error) {
	attachStream, attachErr := svc.AttachContainer(ctx)
	if attachErr == nil {
		attachErr = attachStream.Send(&agentpb.AttachContainerRequest{
			RequestType: &agentpb.AttachContainerRequest_AppName{AppName: appName},
		})
		if attachErr != nil {
			_ = attachStream.CloseSend()
		}
	}
	if attachErr != nil {
		cliNotice("Notice: stdin not attached (%v)", attachErr)
		startStream, startErr := svc.StartContainer(ctx, &agentpb.StartContainerRequest{
			AppName: appName,
		})
		if startErr != nil {
			return nil, false, fmt.Errorf("starting container: %w", startErr)
		}
		return startStream, false, nil
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, readErr := os.Stdin.Read(buf)
			if n > 0 {
				if sendErr := attachStream.Send(&agentpb.AttachContainerRequest{
					RequestType: &agentpb.AttachContainerRequest_StdinData{StdinData: buf[:n]},
				}); sendErr != nil {
					cliNotice("Notice: stdin detached (%v)", sendErr)
					_ = attachStream.CloseSend()
					return
				}
			}
			if readErr != nil {
				_ = attachStream.CloseSend()
				return
			}
		}
	}()
	return attachStream, true, nil
}

func cliLog(format string, args ...any) {
	fmt.Print(cliStyle.Render(fmt.Sprintf(format, args...)))
}

func cliLogln(format string, args ...any) {
	fmt.Println(cliStyle.Render(fmt.Sprintf(format, args...)))
}

func cliNotice(format string, args ...any) {
	fmt.Fprintln(os.Stderr, cliNoticeStyle.Render(fmt.Sprintf(format, args...)))
}

// createContainerWithProgress calls CreateContainerWithProgress and prints
// phase updates so the user sees feedback during long image pulls/unpacks.
func createContainerWithProgress(ctx context.Context, svc agentpb.WendyContainerServiceClient, req *agentpb.CreateContainerRequest) error {
	stream, err := svc.CreateContainerWithProgress(ctx, req)
	if err != nil {
		return fmt.Errorf("creating container: %w", err)
	}

	isTTY := term.IsTerminal(int(os.Stdout.Fd()))
	clearLine := func() {
		if isTTY {
			fmt.Print("\033[2K\r")
		} else {
			fmt.Println()
		}
	}

	completed := false
	for {
		resp, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			return fmt.Errorf("creating container: %w", recvErr)
		}

		switch r := resp.GetResponseType().(type) {
		case *agentpb.CreateContainerProgressResponse_Progress:
			switch r.Progress.GetPhase() {
			case agentpb.CreateContainerProgress_UNPACKING:
				cliLog("Pulling and unpacking image on device...")
			case agentpb.CreateContainerProgress_CREATING_CONTAINER:
				clearLine()
				cliLog("Creating container...")
			case agentpb.CreateContainerProgress_COMPLETE:
				clearLine()
			}
		case *agentpb.CreateContainerProgressResponse_Completed:
			completed = true
		}

		if completed {
			break
		}
	}

	if !completed {
		return fmt.Errorf("creating container: progress stream ended without completion")
	}
	return nil
}

// runOptions holds the parsed flags for the run command.
type runOptions struct {
	debug                bool
	deploy               bool
	detach               bool
	yes                  bool
	restartUnlessStopped bool
	restartOnFailure     bool
	noRestart            bool
	prefix               string
	userArgs             []string
}

func newRunCmd() *cobra.Command {
	var opts runOptions

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Build and run application on a WendyOS device",
		Long:  "Reads wendy.json from the current directory or --prefix directory, builds a container image, and deploys it to the target device.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCommand(cmd.Context(), opts)
		},
	}

	cmd.Flags().BoolVar(&opts.debug, "debug", false, "Enable debug logging")
	cmd.Flags().BoolVar(&opts.deploy, "deploy", false, "Create container but do not start it")
	cmd.Flags().BoolVar(&opts.detach, "detach", false, "Start container but do not stream logs")
	cmd.Flags().BoolVarP(&opts.yes, "yes", "y", false, "Automatically accept all interactive prompts")
	cmd.Flags().BoolVar(&opts.restartUnlessStopped, "restart-unless-stopped", false, "Restart unless manually stopped")
	cmd.Flags().BoolVar(&opts.restartOnFailure, "restart-on-failure", false, "Restart on failure")
	cmd.Flags().BoolVar(&opts.noRestart, "no-restart", false, "Do not restart on exit")
	cmd.Flags().StringVar(&opts.prefix, "prefix", "", "Project directory to run from instead of the current working directory")
	cmd.Flags().StringSliceVar(&opts.userArgs, "user-args", nil, "Extra arguments to pass to the container")

	return cmd
}

func runCommand(ctx context.Context, opts runOptions) error {
	// Step 1: Load and validate wendy.json.
	cwd, err := resolveRunWorkingDir(opts)
	if err != nil {
		return fmt.Errorf("resolving working directory: %w", err)
	}

	cfgPath := filepath.Join(cwd, "wendy.json")
	appCfg, err := ensureAppConfig(cfgPath, opts.yes)
	if err != nil {
		return fmt.Errorf("loading wendy.json: %w", err)
	}

	if err := appCfg.Validate(); err != nil {
		return fmt.Errorf("invalid wendy.json: %w", err)
	}

	// Debug mode requires host networking for remote debugger access
	// (gdb/lldb for native apps, debugpy for Python apps).
	if opts.debug {
		appCfg.Debug = true
		foundNetwork := false
		for i, e := range appCfg.Entitlements {
			if e.Type == appconfig.EntitlementNetwork {
				appCfg.Entitlements[i].Mode = "host"
				foundNetwork = true
				break
			}
		}
		if !foundNetwork {
			appCfg.Entitlements = append(appCfg.Entitlements, appconfig.Entitlement{
				Type: appconfig.EntitlementNetwork,
				Mode: "host",
			})
		}
	}

	// Step 2: Resolve the target device.
	var resolveOpts []resolveOption
	if opts.yes {
		resolveOpts = append(resolveOpts, NonInteractive())
	}
	target, err := resolveTarget(ctx, resolveOpts...)
	if err != nil {
		return err
	}

	// Provider-based run path.
	if target.External != nil && target.Provider != nil {
		return runWithProvider(ctx, target.Provider, *target.External, cwd, appCfg.AppID, opts)
	}

	// Devices without a reachable WendyOS agent can't execute containers.
	if target.Agent == nil {
		// SelectedDevice sets exactly one of Agent/Bluetooth/External.
		// At this point we've already handled the External+Provider case above,
		// so a nil Agent here typically means we're talking to the device over BLE.
		if target.Bluetooth != nil {
			if target.Bluetooth.IsWendyAgent() {
				// Full WendyOS device reachable only over Bluetooth: instruct user
				// to get it onto WiFi / LAN so the agent can be reached.
				return fmt.Errorf("selected device is currently reachable only over Bluetooth. To run apps on it, first connect it to WiFi or ensure it has a LAN address, then retry 'wendy run'")
			}
			// BLE-only Wendy Lite device: these cannot run containers.
			return fmt.Errorf("selected device is a Wendy Lite device, which does not support 'wendy run'. To provision it, first connect it to WiFi using 'wendy device wifi connect'")
		}

		// Fallback: no agent and no Bluetooth/External path we can use.
		return fmt.Errorf("selected device does not have a reachable WendyOS agent and cannot run 'wendy run'")
	}

	// Agent-based run path (existing gRPC pipeline).
	defer target.Agent.Close()
	return runWithAgent(ctx, target.Agent, cwd, appCfg, opts)
}

func resolveRunWorkingDir(opts runOptions) (string, error) {
	prefix := strings.TrimSpace(opts.prefix)
	if prefix == "" {
		return os.Getwd()
	}

	abs, err := filepath.Abs(prefix)
	if err != nil {
		return "", fmt.Errorf("resolving %q: %w", prefix, err)
	}

	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%q does not exist", prefix)
		}
		return "", fmt.Errorf("checking %q: %w", prefix, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", prefix)
	}

	return abs, nil
}

// runMacOSNativeContainer creates, optionally starts, and optionally streams
// from a container that was deployed via file sync (not an OCI image pull).
// It is shared by both the SwiftPM and Xcode macOS run paths.
func runMacOSNativeContainer(ctx context.Context, conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig, createReq *agentpb.CreateContainerRequest, opts runOptions) error {
	if opts.deploy {
		if _, err := conn.ContainerService.CreateContainer(ctx, createReq); err != nil {
			return fmt.Errorf("creating container: %w", err)
		}
		cliLogln("Container %s created (not started).", appCfg.AppID)
		return nil
	}

	if _, err := conn.ContainerService.CreateContainer(ctx, createReq); err != nil {
		return fmt.Errorf("creating container: %w", err)
	}
	cliLogln("Container %s created.", appCfg.AppID)

	if opts.detach {
		stream, err := conn.ContainerService.StartContainer(ctx, &agentpb.StartContainerRequest{
			AppName: appCfg.AppID,
		})
		if err != nil {
			return fmt.Errorf("starting container: %w", err)
		}
		if _, err := stream.Recv(); err != nil && err != io.EOF {
			return fmt.Errorf("waiting for container start: %w", err)
		}
		cliLogln("Application %s running in detached mode.", appCfg.AppID)
		return nil
	}

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	stream, err := conn.ContainerService.StartContainer(runCtx, &agentpb.StartContainerRequest{
		AppName: appCfg.AppID,
	})
	if err != nil {
		return fmt.Errorf("starting container: %w", err)
	}

	cliLogln("Application %s started.", appCfg.AppID)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		cliLogln("\nStopping container...")
		_, _ = conn.ContainerService.StopContainer(context.Background(), &agentpb.StopContainerRequest{
			AppName: appCfg.AppID,
		})
		runCancel()
	}()

	for {
		resp, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			if runCtx.Err() != nil {
				break
			}
			return fmt.Errorf("receiving container output: %w", recvErr)
		}
		if out := resp.GetStdoutOutput(); out != nil {
			_, _ = os.Stdout.Write(out.GetData())
		}
		if out := resp.GetStderrOutput(); out != nil {
			_, _ = os.Stderr.Write(out.GetData())
		}
	}

	cliLogln("\nApplication %s stopped.", appCfg.AppID)
	return nil
}

// runSwiftWithAgent builds a Swift package using swift-container-plugin, which
// pushes the image directly to the device's registry. Then it creates and
// starts the container on the agent.
func runSwiftWithAgent(ctx context.Context, conn *grpcclient.AgentConnection, cwd string, appCfg *appconfig.AppConfig, opts runOptions, agentOS string) error {
	// Verify auth certs are available if the device's registry requires mTLS.
	if err := requireRegistryAuth(ctx, conn); err != nil {
		return err
	}

	// Query the device architecture.
	versionResp, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		return fmt.Errorf("querying device version: %w", err)
	}
	architecture := versionResp.GetCpuArchitecture()
	if architecture == "" {
		architecture = "arm64"
	}

	regPort := registryPort(agentOS)

	if err := ensureSwiftVersion(ctx); err != nil {
		return err
	}

	product, err := findSwiftProduct(cwd)
	if err != nil {
		return err
	}

	registryAddr, proxyCleanup, err := resolveRegistryForSwift(ctx, conn.Host, regPort)
	if err != nil {
		return err
	}
	defer proxyCleanup()

	cliLogln("Building Swift container image for %s (%s)...", product, architecture)
	if err := buildSwiftContainerImage(ctx, cwd, product, registryAddr, architecture, conn.IsMTLS); err != nil {
		return fmt.Errorf("building Swift container image: %w", err)
	}
	cliLogln("Build and push completed.")

	// The image is now in the device's registry. The agent will pull it
	// from localhost:<regPort> when creating the container.
	deviceImage := fmt.Sprintf("localhost:%d/%s:latest", regPort, strings.ToLower(product))

	appConfigData, err := json.Marshal(appCfg)
	if err != nil {
		return fmt.Errorf("marshaling app config: %w", err)
	}

	restartPolicy := resolveRestartPolicy(opts)

	createReq := &agentpb.CreateContainerRequest{
		ImageName:     deviceImage,
		AppName:       appCfg.AppID,
		AppConfig:     appConfigData,
		RestartPolicy: restartPolicy,
		UserArgs:      opts.userArgs,
	}

	return startAndStreamContainer(ctx, conn, appCfg, createReq, opts)
}

// runMacOSSwiftPMWithAgent builds a Swift package locally via `swift build`,
// syncs the binary (and optional sandbox.sb / wendy.json files) to the device
// via SyncFiles gRPC, and creates/starts the container.
func runMacOSSwiftPMWithAgent(ctx context.Context, conn *grpcclient.AgentConnection, cwd string, appCfg *appconfig.AppConfig, opts runOptions) error {
	// Verify CPU architecture matches.
	versionResp, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		return fmt.Errorf("querying device version: %w", err)
	}
	deviceArch := versionResp.GetCpuArchitecture()
	if deviceArch == "" {
		deviceArch = "arm64"
	}
	if deviceArch != runtime.GOARCH {
		return fmt.Errorf("architecture mismatch: device is %s but host is %s", deviceArch, runtime.GOARCH)
	}

	product, err := findSwiftProduct(cwd)
	if err != nil {
		return err
	}

	// Build locally.
	cliLogln("Building Swift project locally...")
	buildCmd := exec.CommandContext(ctx, "swift", "build")
	buildCmd.Dir = cwd
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		return fmt.Errorf("swift build failed: %w", err)
	}
	cliLogln("Build completed.")

	// Locate the binary.
	binaryPath := filepath.Join(cwd, ".build", "debug", product)
	if _, err := os.Stat(binaryPath); err != nil {
		return fmt.Errorf("binary not found at %s: %w", binaryPath, err)
	}

	// Assemble file sync entries.
	syncEntries := []fileSyncEntry{
		{localPath: binaryPath, remotePath: product},
	}

	// Include sandbox.sb if present.
	sandboxPath := filepath.Join(cwd, "sandbox.sb")
	if _, err := os.Stat(sandboxPath); err == nil {
		syncEntries = append(syncEntries, fileSyncEntry{
			localPath:  sandboxPath,
			remotePath: "sandbox.sb",
		})
	}

	// Append user-declared files from wendy.json.
	for _, f := range appCfg.Files {
		localAbs := filepath.Join(cwd, f.Path)
		syncEntries = append(syncEntries, fileSyncEntry{
			localPath:  localAbs,
			remotePath: effectiveRemotePath(f.Path, f.To),
		})
	}

	// Sync files to the device.
	if err := syncFiles(ctx, conn, appCfg.AppID, syncEntries); err != nil {
		return fmt.Errorf("syncing files: %w", err)
	}

	var runArgs []string
	if appCfg.Run != nil {
		runArgs = appCfg.Run.Args
	}
	createReq := &agentpb.CreateContainerRequest{
		AppName:  appCfg.AppID,
		Cmd:      product,
		UserArgs: runArgs,
	}
	return runMacOSNativeContainer(ctx, conn, appCfg, createReq, opts)
}

// runWithProvider builds and runs via an external device provider.
func runWithProvider(ctx context.Context, p providers.DeviceProvider, device models.ExternalDevice, projectPath, product string, opts runOptions) error {
	projectType, err := detectProjectType(projectPath)
	if err != nil {
		return err
	}

	// Resolve Swift product name from Package.swift.
	if projectType == "swift" {
		if err := ensureSwiftVersion(ctx); err != nil {
			return err
		}
		swiftProduct, err := findSwiftProduct(projectPath)
		if err != nil {
			return fmt.Errorf("could not determine Swift product: %w", err)
		}
		product = swiftProduct
	} else if p.CanBuild(projectPath) {
		// Dockerfile exists — try to use Swift product name if Package.swift is also present.
		if swiftProduct, err := findSwiftProduct(projectPath); err == nil {
			product = swiftProduct
		}
	}

	var app *providers.BuiltApp

	// Xcode projects cannot be deployed via provider (requires darwin + file sync).
	if projectType == "xcode" {
		return fmt.Errorf("Xcode projects are not supported by the %s provider; use 'wendy run' with a macOS target instead", p.DisplayName())
	}

	// Swift projects without a Dockerfile: cross-compile on the host and
	// build a Docker image, bypassing the provider's normal Build method.
	if projectType == "swift" {
		if ib, ok := p.(providers.ImageBuilder); ok {
			cliLogln("Building Swift project for %s...", p.DisplayName())
			imageName, err := buildSwiftDockerImage(ctx, projectPath, product)
			if err != nil {
				return fmt.Errorf("building Swift Docker image: %w", err)
			}
			app = ib.BuildFromImage(device, product, imageName)
		}
	}

	if app == nil {
		cliLogln("Building with %s provider...", p.DisplayName())
		var err error
		app, err = p.Build(ctx, device, projectPath, product, opts.debug)
		if err != nil {
			return fmt.Errorf("provider build: %w", err)
		}
	}

	cliLogln("Build completed.")

	if opts.deploy {
		cliLogln("Application %s built but not started (--deploy).", product)
		return nil
	}

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	output := make(chan providers.RunOutput, 64)

	// Ctrl+C handler.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		cliLogln("\nStopping application...")
		p.Stop(context.Background(), app)
		runCancel()
	}()

	// Start the application in a goroutine.
	errCh := make(chan error, 1)
	go func() {
		errCh <- p.Run(runCtx, app, opts.detach, output)
	}()

	// Consume output.
	for out := range output {
		switch out.Type {
		case providers.RunOutputStarted:
			cliLogln("Application %s started.", product)
			if opts.detach {
				cliLogln("Application %s running in detached mode.", product)
				return nil
			}
		case providers.RunOutputStdout:
			os.Stdout.Write(out.Data)
		case providers.RunOutputStderr:
			os.Stderr.Write(out.Data)
		}
	}

	runErr := <-errCh
	cliLogln("\nApplication %s stopped.", product)
	if runCtx.Err() != nil {
		return nil // cancelled by signal
	}
	return runErr
}

// runWithAgent is the existing gRPC agent pipeline.
func runWithAgent(ctx context.Context, conn *grpcclient.AgentConnection, cwd string, appCfg *appconfig.AppConfig, opts runOptions) error {
	// Detect project type and ensure a Dockerfile exists.
	projectType, err := detectProjectType(cwd)
	if err != nil {
		return err
	}

	// Resolve the target platform. Query the agent for its OS and architecture,
	// then determine the effective platform from wendy.json or defaults.
	versionResp, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		return fmt.Errorf("querying device version: %w", err)
	}
	agentOS := versionResp.GetOs()
	architecture := versionResp.GetCpuArchitecture()
	if architecture == "" {
		architecture = "arm64"
	}

	platform := resolveAgentPlatform(appCfg.Platform, agentOS, architecture)
	if platformOS(platform) == "darwin" {
		maybeWarnAboutRuntimePermissions(appCfg)
	}

	// Xcode projects: always use the local-build + file-sync path (darwin only).
	if projectType == "xcode" {
		if platformOS(platform) == "darwin" {
			return runMacOSXcodeWithAgent(ctx, conn, cwd, appCfg, opts)
		}
		return fmt.Errorf("Xcode projects require a darwin target (got %s)", platform)
	}

	// Swift projects without a Dockerfile: check if the target platform is
	// darwin (binary upload) or Linux (swift-container-plugin).
	if projectType == "swift" {
		if _, err := os.Stat(filepath.Join(cwd, "Dockerfile")); os.IsNotExist(err) {
			if platformOS(platform) == "darwin" {
				return runMacOSSwiftPMWithAgent(ctx, conn, cwd, appCfg, opts)
			}
			return runSwiftWithAgent(ctx, conn, cwd, appCfg, opts, agentOS)
		}
	}

	switch projectType {
	case "docker":
		// Dockerfile already exists.
	case "python":
		if _, err := os.Stat(filepath.Join(cwd, "Dockerfile")); os.IsNotExist(err) {
			cliLogln("No Dockerfile found. Generating one for Python project...")
			if _, genErr := generatePythonDockerfile(cwd); genErr != nil {
				return fmt.Errorf("generating Dockerfile: %w", genErr)
			}
			cliLogln("Generated Dockerfile.")
		}
	case "swift":
		// Dockerfile exists; use the Docker build path.
	default:
		return fmt.Errorf("unable to detect project type; ensure a Dockerfile, requirements.txt, or Package.swift is present")
	}

	// Verify auth certs are available if the device's registry requires mTLS.
	if err := requireRegistryAuth(ctx, conn); err != nil {
		return err
	}

	// Build and push the Docker image directly to the device's registry.
	regPort := registryPort(agentOS)
	// For link-local addresses (USB), a TCP proxy bridges the Docker VM
	// to the host so buildx can reach the device.
	registryAddr, proxyCleanup, err := resolveRegistry(ctx, conn.Host, regPort)
	if err != nil {
		return err
	}
	defer proxyCleanup()

	repo := strings.ToLower(appCfg.AppID)
	registryImage := fmt.Sprintf("%s/%s:latest", registryAddr, repo)

	cliLogln("Building and pushing Docker image for %s...", platform)
	if err := buildAndPushImage(ctx, cwd, registryAddr, registryImage, platform, os.Stdout, conn.IsMTLS); err != nil {
		return fmt.Errorf("building and pushing Docker image: %w", err)
	}
	cliLogln("Build and push completed.")

	// Inject debugpy for Python remote debugging.
	if opts.debug && appCfg.Language == "python" {
		cliLogln("Injecting debugpy for remote debugging...")
		if err := injectDebugpy(ctx, registryAddr, registryImage, platform, os.Stdout, conn.IsMTLS); err != nil {
			return fmt.Errorf("injecting debugpy: %w", err)
		}
	}

	// The agent pulls from localhost:<regPort>.
	deviceImage := fmt.Sprintf("localhost:%d/%s:latest", regPort, repo)

	appConfigData, err := json.Marshal(appCfg)
	if err != nil {
		return fmt.Errorf("marshaling app config: %w", err)
	}

	restartPolicy := resolveRestartPolicy(opts)

	createReq := &agentpb.CreateContainerRequest{
		ImageName:     deviceImage,
		AppName:       appCfg.AppID,
		AppConfig:     appConfigData,
		RestartPolicy: restartPolicy,
		UserArgs:      opts.userArgs,
	}

	return startAndStreamContainer(ctx, conn, appCfg, createReq, opts)
}

// startAndStreamContainer handles the deploy/detach/attached lifecycle that is
// shared between runSwiftWithAgent and runWithAgent. It creates the container,
// optionally starts it, streams output, and manages readiness + postStart hooks.
func startAndStreamContainer(ctx context.Context, conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig, createReq *agentpb.CreateContainerRequest, opts runOptions) error {
	if opts.deploy {
		_, err := conn.ContainerService.CreateContainer(ctx, createReq)
		if err != nil {
			return fmt.Errorf("creating container: %w", err)
		}
		cliLogln("Container %s created (not started).", appCfg.AppID)
		return nil
	}

	// Create the container with progress streaming.
	if err := createContainerWithProgress(ctx, conn.ContainerService, createReq); err != nil {
		return err
	}
	cliLogln("Container %s created.", appCfg.AppID)

	if opts.detach {
		stream, err := conn.ContainerService.StartContainer(ctx, &agentpb.StartContainerRequest{
			AppName: appCfg.AppID,
		})
		if err != nil {
			return fmt.Errorf("starting container: %w", err)
		}
		if _, err := stream.Recv(); err != nil && err != io.EOF {
			return fmt.Errorf("waiting for container start: %w", err)
		}
		cliLogln("Application %s running in detached mode.", appCfg.AppID)
		// Wait for readiness before firing hook.
		if err := waitForReadiness(ctx, appCfg.Readiness, conn.Host); err != nil {
			cliLogln("Warning: %v", err)
		}
		// Fire-and-forget: post-start hook outlives the CLI process.
		startPostStartHook(context.Background(), appCfg, conn.Host)
		return nil
	}

	// Start and stream output using AttachContainer so stdin is forwarded.
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	outStream, stdinAttempted, err := openContainerStream(runCtx, conn.ContainerService, appCfg.AppID)
	if err != nil {
		return err
	}

	cliLogln("Application %s started.", appCfg.AppID)

	// Set up Ctrl+C handler first so readiness polling is cancellable.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		cliLogln("\nStopping container...")
		_, _ = conn.ContainerService.StopContainer(context.Background(), &agentpb.StopContainerRequest{
			AppName: appCfg.AppID,
		})
		runCancel()
	}()

	// Wait for readiness before firing hook.
	if err := waitForReadiness(runCtx, appCfg.Readiness, conn.Host); err != nil {
		if runCtx.Err() == nil {
			cliLogln("Warning: %v", err)
		}
	}

	// Post-start hook tied to runCtx so Ctrl+C kills it.
	postStartCmd := startPostStartHook(runCtx, appCfg, conn.Host)

	gotFirstResponse := false
	for {
		resp, recvErr := outStream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			if runCtx.Err() != nil {
				break
			}
			// If the bidi stream returned Unimplemented before any response,
			// the container was never started — fall back silently to StartContainer.
			if stdinAttempted && !gotFirstResponse && status.Code(recvErr) == codes.Unimplemented {
				cliNotice("Notice: stdin not attached (not supported by agent)")
				startStream, startErr := conn.ContainerService.StartContainer(runCtx, &agentpb.StartContainerRequest{
					AppName: appCfg.AppID,
				})
				if startErr != nil {
					return fmt.Errorf("starting container: %w", startErr)
				}
				outStream = startStream
				stdinAttempted = false
				continue
			}
			return fmt.Errorf("receiving container output: %w", recvErr)
		}
		gotFirstResponse = true
		if out := resp.GetStdoutOutput(); out != nil {
			_, _ = os.Stdout.Write(out.GetData())
		}
		if out := resp.GetStderrOutput(); out != nil {
			_, _ = os.Stderr.Write(out.GetData())
		}
	}

	// Cancel runCtx to terminate the postStart hook if it's still running,
	// then wait for it to exit so we don't leave orphan processes.
	runCancel()
	if postStartCmd != nil {
		_ = postStartCmd.Wait()
	}
	cliLogln("\nApplication %s stopped.", appCfg.AppID)
	return nil
}

// waitForReadiness polls the readiness probe until it passes or the context is
// cancelled. Returns nil on success, the parent context error on cancellation,
// or a timeout error if the probe deadline expires.
func waitForReadiness(ctx context.Context, cfg *appconfig.ReadinessConfig, hostname string) error {
	if cfg == nil || cfg.TCPSocket == nil {
		return nil
	}

	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	addr := net.JoinHostPort(hostname, fmt.Sprintf("%d", cfg.TCPSocket.Port))
	cliLogln("Waiting for %s to be ready...", addr)

	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dialer := net.Dialer{Timeout: 2 * time.Second}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		conn, err := dialer.DialContext(probeCtx, "tcp", addr)
		if err == nil {
			conn.Close()
			cliLogln("Ready.")
			return nil
		}

		select {
		case <-probeCtx.Done():
			// Distinguish parent cancellation (Ctrl+C) from probe timeout.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("readiness probe timed out after %s waiting for %s", timeout, addr)
		case <-ticker.C:
		}
	}
}

// shellCommand returns the platform-appropriate shell and flag for running a
// command string. On Windows it uses cmd.exe /C; everywhere else sh -c.
func shellCommand() (string, string) {
	if runtime.GOOS == "windows" {
		return "cmd.exe", "/C"
	}
	return "sh", "-c"
}

// startPostStartHook expands environment variables in the postStart CLI hook
// and spawns it as a child process. The returned *exec.Cmd can be used to wait
// on or kill the process. Returns nil if there is no postStart CLI hook.
func startPostStartHook(ctx context.Context, appCfg *appconfig.AppConfig, hostname string) *exec.Cmd {
	if appCfg.Hooks == nil || appCfg.Hooks.PostStart == nil || appCfg.Hooks.PostStart.CLI == "" {
		return nil
	}

	expanded := os.Expand(appCfg.Hooks.PostStart.CLI, func(key string) string {
		switch key {
		case "WENDY_HOSTNAME":
			return hostname
		case "WENDY_APP_ID":
			return appCfg.AppID
		default:
			return os.Getenv(key)
		}
	})

	shell, flag := shellCommand()
	cmd := execCommandContext(ctx, shell, flag, expanded)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		cliLogln("Warning: postStart hook failed to start: %v", err)
		return nil
	}
	cliLogln("Hook postStart: %s", expanded)
	return cmd
}

// resolveRestartPolicy converts the flag options into a protobuf RestartPolicy.
func resolveRestartPolicy(opts runOptions) *agentpb.RestartPolicy {
	mode := agentpb.RestartPolicyMode_DEFAULT
	if opts.restartUnlessStopped {
		mode = agentpb.RestartPolicyMode_UNLESS_STOPPED
	} else if opts.restartOnFailure {
		mode = agentpb.RestartPolicyMode_ON_FAILURE
	} else if opts.noRestart {
		mode = agentpb.RestartPolicyMode_NO
	}
	return &agentpb.RestartPolicy{Mode: mode}
}
