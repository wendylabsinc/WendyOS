package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/internal/cli/providers"
	"github.com/wendylabsinc/wendy/internal/cli/swifttoolchain"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/internal/shared/browseropen"
	"github.com/wendylabsinc/wendy/internal/shared/config"
	"github.com/wendylabsinc/wendy/internal/shared/models"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

var cliStyle = lipgloss.NewStyle().Foreground(tui.ColorDim)
var cliNoticeStyle = lipgloss.NewStyle().Foreground(tui.ColorNotice)
var execCommandContext = exec.CommandContext

const linuxContainersOnMacsUnsupportedMessage = "Linux containers aren't supported on Macs yet. Support is planned for a future release. For now, deploy a native macOS app (platform: darwin) or target a Linux/WendyOS device."

// dimWriter writes each line rendered through cliStyle (dim/background).
// Incomplete lines are buffered until a newline or Flush is called.
type dimWriter struct {
	buf strings.Builder
}

func (w *dimWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			w.buf.Write(p)
			break
		}
		w.buf.Write(p[:i])
		fmt.Println(cliStyle.Render(w.buf.String()))
		w.buf.Reset()
		p = p[i+1:]
	}
	return total, nil
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
func openContainerStream(ctx context.Context, svc agentpb.WendyContainerServiceClient, appName string, appCfg *appconfig.AppConfig) (containerOutputStream, bool, error) {
	startCtx := contextWithPostStartAgentHook(ctx, appCfg)
	attachStream, attachErr := svc.AttachContainer(startCtx)
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
		startStream, startErr := svc.StartContainer(startCtx, &agentpb.StartContainerRequest{
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

func postStartAgentHook(appCfg *appconfig.AppConfig) string {
	if appCfg == nil || appCfg.Hooks == nil || appCfg.Hooks.PostStart == nil {
		return ""
	}
	return appCfg.Hooks.PostStart.Agent
}

func contextWithPostStartAgentHook(ctx context.Context, appCfg *appconfig.AppConfig) context.Context {
	hook := postStartAgentHook(appCfg)
	if hook == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, appconfig.PostStartAgentHookMetadataKey, hook)
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

var cliSuccessStyle = lipgloss.NewStyle().Foreground(tui.ColorPrimary)

func cliSuccess(format string, args ...any) {
	fmt.Println(cliSuccessStyle.Render(fmt.Sprintf(format, args...)))
}

func unpackProgressTitle(progress *agentpb.CreateContainerProgress) string {
	total := progress.GetTotalLayers()
	if total <= 0 {
		return "Pulling image on device..."
	}

	completed := progress.GetLayerIndex()
	if progress.GetPhase() == agentpb.CreateContainerProgress_APPLYING_LAYER {
		completed++
	}
	if completed > total {
		completed = total
	}

	title := fmt.Sprintf("Unpacking image on device... (%d/%d layers", completed, total)
	if progress.GetPhase() == agentpb.CreateContainerProgress_APPLYING_LAYER && progress.GetReusedSnapshot() {
		title += ", reused snapshot"
	}
	return title + ")"
}

func unpackProgressPercent(progress *agentpb.CreateContainerProgress) float64 {
	total := progress.GetTotalLayers()
	if total <= 0 {
		return 0
	}

	completed := progress.GetLayerIndex()
	if progress.GetPhase() == agentpb.CreateContainerProgress_APPLYING_LAYER {
		completed++
	}
	if completed < 0 {
		completed = 0
	}
	if completed > total {
		completed = total
	}

	return float64(completed) / float64(total)
}

func createContainerWithProgressPlain(stream agentpb.WendyContainerService_CreateContainerWithProgressClient) error {
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
				cliLogln("%s", unpackProgressTitle(r.Progress))
			case agentpb.CreateContainerProgress_APPLYING_LAYER:
				cliLogln("%s", unpackProgressTitle(r.Progress))
			case agentpb.CreateContainerProgress_CREATING_CONTAINER:
				cliLogln("Creating container...")
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

func isUnimplementedRPCError(err error) bool {
	for current := err; current != nil; current = errors.Unwrap(current) {
		if status.Code(current) == codes.Unimplemented {
			return true
		}
	}
	return false
}

func createContainerWithoutProgress(ctx context.Context, svc agentpb.WendyContainerServiceClient, req *agentpb.CreateContainerRequest) error {
	if _, err := svc.CreateContainer(ctx, req); err != nil {
		return fmt.Errorf("creating container: %w", err)
	}
	return nil
}

func fallbackCreateContainerWithoutProgress(ctx context.Context, svc agentpb.WendyContainerServiceClient, req *agentpb.CreateContainerRequest) error {
	cliLogln("Info: progress reporting is currently not available on this agent; continuing without progress")
	return createContainerWithoutProgress(ctx, svc, req)
}

func progressModelUserCancelled(model tea.Model) bool {
	pm, ok := model.(tui.ProgressModel)
	return ok && pm.Err() == context.Canceled
}

func createContainerWithProgressTUI(cancel context.CancelFunc, stream agentpb.WendyContainerService_CreateContainerWithProgressClient) error {
	prog := tea.NewProgram(tui.NewProgress("Pulling image on device...").WithoutErrorView())

	var (
		createErr error
		done      = make(chan struct{})
		creating  = make(chan struct{}, 1)
		completed bool
	)

	go func() {
		defer close(done)
		progressDone := false
		for {
			resp, recvErr := stream.Recv()
			if recvErr == io.EOF {
				if !completed && createErr == nil {
					createErr = fmt.Errorf("creating container: progress stream ended without completion")
				}
				if !progressDone {
					prog.Send(tui.ProgressDoneMsg{Err: createErr})
				}
				return
			}
			if recvErr != nil {
				createErr = fmt.Errorf("creating container: %w", recvErr)
				if !progressDone {
					prog.Send(tui.ProgressDoneMsg{Err: createErr})
				}
				return
			}

			switch r := resp.GetResponseType().(type) {
			case *agentpb.CreateContainerProgressResponse_Progress:
				switch r.Progress.GetPhase() {
				case agentpb.CreateContainerProgress_UNPACKING, agentpb.CreateContainerProgress_APPLYING_LAYER:
					prog.Send(tui.ProgressUpdateMsg{
						Percent: unpackProgressPercent(r.Progress),
						Title:   unpackProgressTitle(r.Progress),
					})
				case agentpb.CreateContainerProgress_CREATING_CONTAINER:
					if !progressDone {
						progressDone = true
						select {
						case creating <- struct{}{}:
						default:
						}
						prog.Send(tui.ProgressDoneMsg{})
					}
				case agentpb.CreateContainerProgress_COMPLETE:
					completed = true
					if !progressDone {
						progressDone = true
						prog.Send(tui.ProgressDoneMsg{})
					}
				}
			case *agentpb.CreateContainerProgressResponse_Completed:
				completed = true
				if !progressDone {
					progressDone = true
					prog.Send(tui.ProgressDoneMsg{})
				}
				return
			}
		}
	}()

	finalModel, err := prog.Run()
	if err != nil {
		cancel()
		<-done
		return fmt.Errorf("progress TUI: %w", err)
	}

	if progressModelUserCancelled(finalModel) {
		cancel()
		<-done
		return ErrUserCancelled
	}

	select {
	case <-creating:
		cliLogln("Creating container...")
	default:
	}

	<-done
	return createErr
}

// createContainerWithProgress calls CreateContainerWithProgress and prints
// phase updates so the user sees feedback during long image pulls/unpacks.
// Older agents may not implement the streaming RPC yet, so fall back to the
// legacy unary CreateContainer call when the server reports Unimplemented.
func createContainerWithProgress(ctx context.Context, svc agentpb.WendyContainerServiceClient, req *agentpb.CreateContainerRequest) error {
	if !isInteractiveTerminal() {
		stream, err := svc.CreateContainerWithProgress(ctx, req)
		if err != nil {
			if isUnimplementedRPCError(err) {
				return fallbackCreateContainerWithoutProgress(ctx, svc, req)
			}
			return fmt.Errorf("creating container: %w", err)
		}
		err = createContainerWithProgressPlain(stream)
		if isUnimplementedRPCError(err) {
			return fallbackCreateContainerWithoutProgress(ctx, svc, req)
		}
		return err
	}

	progressCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := svc.CreateContainerWithProgress(progressCtx, req)
	if err != nil {
		if isUnimplementedRPCError(err) {
			return fallbackCreateContainerWithoutProgress(ctx, svc, req)
		}
		return fmt.Errorf("creating container: %w", err)
	}
	if err := createContainerWithProgressTUI(cancel, stream); err != nil {
		if isUnimplementedRPCError(err) {
			return fallbackCreateContainerWithoutProgress(ctx, svc, req)
		}
		return err
	}
	return nil
}

// runOptions holds the parsed flags for the run command.
type runOptions struct {
	buildType            string
	debug                bool
	deploy               bool
	detach               bool
	yes                  bool
	restartUnlessStopped bool
	restartOnFailure     bool
	noRestart            bool
	prefix               string
	product              string
	service              string
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

	cmd.Flags().StringVar(&opts.buildType, "build-type", "", "Build type to use when Dockerfile is present alongside Package.swift or Python project markers: docker, swift, or python")
	cmd.Flags().BoolVar(&opts.debug, "debug", false, "Enable debug logging")
	cmd.Flags().BoolVar(&opts.deploy, "deploy", false, "Create container but do not start it")
	cmd.Flags().BoolVar(&opts.detach, "detach", false, "Start container but do not stream logs")
	cmd.Flags().BoolVarP(&opts.yes, "yes", "y", false, "Automatically accept all interactive prompts")
	cmd.Flags().BoolVar(&opts.restartUnlessStopped, "restart-unless-stopped", false, "Restart unless manually stopped")
	cmd.Flags().BoolVar(&opts.restartOnFailure, "restart-on-failure", false, "Restart on failure")
	cmd.Flags().BoolVar(&opts.noRestart, "no-restart", false, "Do not restart on exit")
	cmd.Flags().StringVar(&opts.prefix, "prefix", "", "Project directory to run from instead of the current working directory")
	cmd.Flags().StringVar(&opts.product, "product", "", "Swift Package Manager product to build and run")
	cmd.Flags().StringVar(&opts.service, "service", "", "Build and run only the named service and its dependencies (multi-service projects)")
	cmd.Flags().StringSliceVar(&opts.userArgs, "user-args", nil, "Extra arguments to pass to the container")

	return cmd
}

// resolveRunTarget resolves the target device for the run command. It first
// tries resolveTarget (direct/picker). If that fails and cloud auth entries
// exist, it retries via the cloud tunnel using the device name from --device
// or the configured default.
func resolveRunTarget(ctx context.Context, opts ...resolveOption) (*SelectedDevice, error) {
	target, err := resolveTarget(ctx, opts...)
	if err == nil {
		return target, nil
	}
	if errors.Is(err, ErrUserCancelled) {
		return nil, err
	}

	cfg, loadErr := config.Load()
	if loadErr != nil || len(cfg.Auth) == 0 {
		return nil, err
	}

	deviceName := deviceFlag
	if deviceName == "" {
		deviceName = cfg.DefaultDevice
	}
	if deviceName == "" {
		return nil, err
	}

	cloudConn, cloudErr := connectToCloudAgent(ctx, "", deviceName, "")
	if cloudErr != nil {
		return nil, err
	}
	return &SelectedDevice{Agent: cloudConn}, nil
}

func runCommand(ctx context.Context, opts runOptions) error {
	// Step 1: Load and validate wendy.json.
	cwd, err := resolveRunWorkingDir(opts)
	if err != nil {
		return fmt.Errorf("resolving working directory: %w", err)
	}

	// Compose projects don't use wendy.json — each service carries its own config.
	// Detect this early so we don't prompt to create an unneeded file. Surfacing
	// resolveRunProjectType errors here also catches invalid --build-type values
	// before we try to load wendy.json.
	projectType, err := resolveRunProjectType(cwd, opts.buildType)
	if err != nil {
		return err
	}
	if projectType == "compose" {
		return runComposeCommand(ctx, cwd, opts)
	}

	cfgPath := filepath.Join(cwd, "wendy.json")
	appCfg, err := ensureAppConfig(cfgPath, opts.yes)
	if err != nil {
		return fmt.Errorf("loading wendy.json: %w", err)
	}

	if err := appCfg.Validate(); err != nil {
		return fmt.Errorf("invalid wendy.json: %w", err)
	}
	if err := warnAppConfigFile(cfgPath); err != nil {
		return fmt.Errorf("reading wendy.json warnings: %w", err)
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
	target, err := resolveRunTarget(ctx, resolveOpts...)
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

// runComposeCommand handles the full device-selection + execution flow for
// docker-compose projects, bypassing the wendy.json requirement.
func runComposeCommand(ctx context.Context, cwd string, opts runOptions) error {
	var resolveOpts []resolveOption
	if opts.yes {
		resolveOpts = append(resolveOpts, NonInteractive())
	}
	target, err := resolveRunTarget(ctx, resolveOpts...)
	if err != nil {
		return err
	}

	if target.External != nil && target.Provider != nil {
		// Docker Desktop provider: use docker compose directly.
		return runWithProvider(ctx, target.Provider, *target.External, cwd, filepath.Base(cwd), opts)
	}

	if target.Agent == nil {
		if target.Bluetooth != nil {
			if target.Bluetooth.IsWendyAgent() {
				return fmt.Errorf("selected device is currently reachable only over Bluetooth. Connect it to WiFi and retry 'wendy run'")
			}
			return fmt.Errorf("selected device is a Wendy Lite device, which does not support 'wendy run'")
		}
		return fmt.Errorf("selected device does not have a reachable WendyOS agent and cannot run 'wendy run'")
	}

	defer target.Agent.Close()
	return runComposeWithAgent(ctx, target.Agent, cwd, opts)
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
		stream, err := conn.ContainerService.StartContainer(contextWithPostStartAgentHook(ctx, appCfg), &agentpb.StartContainerRequest{
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

	stream, err := conn.ContainerService.StartContainer(contextWithPostStartAgentHook(runCtx, appCfg), &agentpb.StartContainerRequest{
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
func runSwiftWithAgent(ctx context.Context, conn *grpcclient.AgentConnection, cwd string, appCfg *appconfig.AppConfig, opts runOptions) error {
	// Verify auth certs are available if the device's registry requires mTLS.
	if err := requireRegistryAuth(ctx, conn); err != nil {
		return err
	}

	// Query the device OS and architecture.
	versionResp, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		return fmt.Errorf("querying device version: %w", err)
	}
	agentOS := versionResp.GetOs()
	architecture := versionResp.GetCpuArchitecture()
	if architecture == "" {
		architecture = "arm64"
	}

	regPort := registryPort(agentOS)

	if err := swifttoolchain.EnsureSwiftVersion(ctx, &dimWriter{}, os.Stderr); err != nil {
		return err
	}

	product, err := swifttoolchain.FindSwiftProductWithOptions(cwd, opts.product, !opts.yes && isInteractiveTerminal())
	if err != nil {
		if errors.Is(err, swifttoolchain.ErrUserCancelled) {
			return ErrUserCancelled
		}
		return err
	}

	registryAddr, swiftUseMTLS, proxyCleanup, err := resolveRegistryForSwiftAgent(ctx, conn, regPort)
	if err != nil {
		return err
	}
	defer proxyCleanup()

	cliLogln("Building Swift container image for %s (%s)...", product, architecture)
	if err := buildSwiftContainerImage(ctx, cwd, product, registryAddr, architecture, swiftUseMTLS, &dimWriter{}, os.Stderr); err != nil {
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

	product, err := swifttoolchain.FindSwiftProductWithOptions(cwd, opts.product, !opts.yes && isInteractiveTerminal())
	if err != nil {
		return err
	}

	buildConfig := "release"
	if opts.debug {
		buildConfig = "debug"
	}

	// Build locally.
	cliLogln("Building Swift project locally...")
	buildCmd := exec.CommandContext(ctx, "swift", "build", "-c", buildConfig)
	buildCmd.Dir = cwd
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		return fmt.Errorf("swift build failed: %w", err)
	}
	cliLogln("Build completed.")

	binDir, err := swiftBuildBinPath(ctx, cwd, buildConfig)
	if err != nil {
		return err
	}

	binaryPath := filepath.Join(binDir, product)
	if _, err := os.Stat(binaryPath); err != nil {
		return fmt.Errorf("binary not found at %s: %w", binaryPath, err)
	}

	syncEntries, err := assembleSwiftPMSyncEntries(binaryPath, cwd, appCfg)
	if err != nil {
		return err
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

func swiftBuildBinPath(ctx context.Context, cwd, buildConfig string) (string, error) {
	showBinCmd := exec.CommandContext(ctx, "swift", "build", "-c", buildConfig, "--show-bin-path")
	showBinCmd.Dir = cwd
	out, err := showBinCmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("swift build -c %s --show-bin-path: %w\n%s", buildConfig, err, string(out))
	}

	binDir := strings.TrimSpace(string(out))
	if binDir == "" {
		return "", fmt.Errorf("swift build --show-bin-path returned an empty path")
	}
	return binDir, nil
}

func assembleSwiftPMSyncEntries(binaryPath, cwd string, appCfg *appconfig.AppConfig) ([]fileSyncEntry, error) {
	entries := []fileSyncEntry{{
		localPath:  binaryPath,
		remotePath: filepath.Base(binaryPath),
	}}

	buildDir := filepath.Dir(binaryPath)
	siblings, err := os.ReadDir(buildDir)
	if err != nil {
		return nil, fmt.Errorf("reading Swift build products directory %s: %w", buildDir, err)
	}
	for _, e := range siblings {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".bundle") && !strings.HasSuffix(name, ".resources") {
			continue
		}
		entries = append(entries, fileSyncEntry{
			localPath:  filepath.Join(buildDir, name),
			remotePath: name,
		})
	}

	// Include sandbox.sb if present.
	sandboxPath := filepath.Join(cwd, "sandbox.sb")
	if _, err := os.Stat(sandboxPath); err == nil {
		entries = append(entries, fileSyncEntry{
			localPath:  sandboxPath,
			remotePath: "sandbox.sb",
		})
	}

	// Append user-declared files from wendy.json.
	for _, f := range appCfg.Files {
		localAbs := filepath.Join(cwd, f.Path)
		entries = append(entries, fileSyncEntry{
			localPath:  localAbs,
			remotePath: effectiveRemotePath(f.Path, f.To),
		})
	}

	return entries, nil
}

func resolveRunProjectType(dir, requestedType string) (string, error) {
	if strings.TrimSpace(requestedType) == "" {
		return detectProjectType(dir)
	}

	buildType := normalizeBuildType(requestedType)
	if buildType != "docker" && buildType != "swift" && buildType != "python" && buildType != "compose" {
		return "", fmt.Errorf("invalid value %q for --build-type: must be one of docker, swift, python, or compose", requestedType)
	}

	switch buildType {
	case "compose":
		for _, name := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"} {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				return "compose", nil
			}
		}
	case "docker":
		marker := filepath.Join(dir, "Dockerfile")
		if _, err := os.Stat(marker); err == nil {
			return "docker", nil
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("checking for %s: %w", marker, err)
		}
	case "swift":
		marker := filepath.Join(dir, "Package.swift")
		if _, err := os.Stat(marker); err == nil {
			return "swift", nil
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("checking for %s: %w", marker, err)
		}
	case "python":
		for _, marker := range []string{"requirements.txt", "pyproject.toml", "setup.py"} {
			path := filepath.Join(dir, marker)
			if _, err := os.Stat(path); err == nil {
				return "python", nil
			} else if !os.IsNotExist(err) {
				return "", fmt.Errorf("checking for %s: %w", path, err)
			}
		}
	}

	return "", fmt.Errorf("build type %q is not available in %s", requestedType, dir)
}

// runWithProvider builds and runs via an external device provider.
func runWithProvider(ctx context.Context, p providers.DeviceProvider, device models.ExternalDevice, projectPath, product string, opts runOptions) error {
	projectType, err := resolveRunProjectType(projectPath, opts.buildType)
	if err != nil {
		return err
	}

	// Resolve Swift product name from Package.swift.
	if projectType == "swift" {
		if err := swifttoolchain.EnsureSwiftVersion(ctx, &dimWriter{}, os.Stderr); err != nil {
			return err
		}
		swiftProduct, err := swifttoolchain.FindSwiftProductWithOptions(projectPath, opts.product, !opts.yes && isInteractiveTerminal())
		if err != nil {
			if errors.Is(err, swifttoolchain.ErrUserCancelled) {
				return ErrUserCancelled
			}
			return fmt.Errorf("could not determine Swift product: %w", err)
		}
		product = swiftProduct
	} else if p.CanBuild(projectPath) {
		// Dockerfile exists — try to use Swift product name if Package.swift is also present.
		if swiftProduct, err := swifttoolchain.FindSwiftProductWithOptions(projectPath, opts.product, false); err == nil {
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
			imageName, err := buildSwiftDockerImage(ctx, projectPath, product, runtime.GOARCH, &dimWriter{}, os.Stderr)
			if err != nil {
				return fmt.Errorf("building Swift Docker image: %w", err)
			}
			app = ib.BuildFromImage(device, product, imageName)
		}
	}

	if app == nil {
		cliLogln("Building with %s provider...", p.DisplayName())
		var err error
		// Pass the resolved project type to providers that can disambiguate
		// between buildable markers (e.g. Docker vs Compose).
		if tb, ok := p.(providers.TypedBuilder); ok {
			app, err = tb.BuildWithType(ctx, device, projectPath, product, projectType, opts.debug)
		} else {
			app, err = p.Build(ctx, device, projectPath, product, opts.debug)
		}
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
	// Multi-service path: when wendy.json has a services map, build all images
	// in parallel and manage the app group lifecycle.
	if len(appCfg.Services) > 0 {
		return runMultiServiceWithAgent(ctx, conn, cwd, appCfg, opts)
	}

	// Detect project type and ensure a Dockerfile exists.
	projectType, err := resolveRunProjectType(cwd, opts.buildType)
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
	if agentOS == "darwin" && platformOS(platform) == "linux" {
		return errors.New(linuxContainersOnMacsUnsupportedMessage)
	}

	// Xcode projects: always use the local-build + file-sync path (darwin only).
	if projectType == "xcode" {
		if platformOS(platform) == "darwin" {
			return runMacOSXcodeWithAgent(ctx, conn, cwd, appCfg, opts)
		}
		return fmt.Errorf("Xcode projects require a darwin target (got %s)", platform)
	}

	// Swift projects use a native darwin path for macOS targets and
	// swift-container-plugin for Linux targets when --build-type=swift
	// explicitly selects that path or when no Dockerfile is present.
	// Both paths shell out to a host Swift toolchain:
	//   - darwin target: `swift build` on the host. Requires a darwin host —
	//     Linux's swift toolchain cannot cross-compile to macOS.
	//   - linux target: swift-container-plugin via `swift package`. Requires
	//     a darwin or linux host — swift-container-plugin does not yet ship
	//     for Windows.
	// On a Windows host with a Dockerfile the docker buildx path below
	// handles the build, so the gates only trip when the host swift path
	// would actually be taken.
	if projectType == "swift" {
		targetIsDarwin := platformOS(platform) == "darwin"
		explicitSwift := normalizeBuildType(opts.buildType) == "swift"
		_, dockerfileStatErr := os.Stat(filepath.Join(cwd, "Dockerfile"))
		needsHostSwift := explicitSwift || os.IsNotExist(dockerfileStatErr)

		if needsHostSwift {
			if targetIsDarwin && runtime.GOOS != "darwin" {
				return fmt.Errorf("`wendy run` for Swift packages targeting darwin requires a darwin host (got %s); provide a Dockerfile to build a Linux image instead", runtime.GOOS)
			}
			if !targetIsDarwin && runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
				return fmt.Errorf("`wendy run` for Swift packages is not supported on %s; provide a Dockerfile", runtime.GOOS)
			}
			if targetIsDarwin {
				return runMacOSSwiftPMWithAgent(ctx, conn, cwd, appCfg, opts)
			}
			return runSwiftWithAgent(ctx, conn, cwd, appCfg, opts)
		}
	}

	switch projectType {
	case "docker":
		// Dockerfile already exists.
	case "compose":
		return runComposeWithAgent(ctx, conn, cwd, opts)
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

	deviceType := versionResp.GetDeviceType()
	buildArgs := map[string]string{
		"WENDY_PLATFORM": wendyPlatform(deviceType),
		"WENDY_DEBUG":    fmt.Sprintf("%t", opts.debug),
	}
	// Only set WENDY_DEVICE_TYPE / GPU args when the agent reports them so
	// Dockerfiles can apply their own defaults on older agents.
	if deviceType != "" {
		buildArgs["WENDY_DEVICE_TYPE"] = deviceType
	}
	// WENDY_HAS_GPU is only set when the optional field is present; omitting it
	// on older agents preserves any Dockerfile ARG default.
	if versionResp.HasGpu != nil {
		buildArgs["WENDY_HAS_GPU"] = fmt.Sprintf("%t", versionResp.GetHasGpu())
	}
	// Remaining GPU build args — only set when the agent reports them.
	if vendor := versionResp.GetGpuVendor(); vendor != "" {
		buildArgs["WENDY_GPU_VENDOR"] = vendor
	}
	if jv := versionResp.GetJetpackVersion(); jv != "" {
		buildArgs["WENDY_JETPACK_VERSION"] = jv
	}
	if cv := versionResp.GetCudaVersion(); cv != "" {
		buildArgs["WENDY_CUDA_VERSION"] = cv
	}

	// Verify auth certs are available if the device's registry requires mTLS.
	if err := requireRegistryAuth(ctx, conn); err != nil {
		return err
	}

	// Build and push the Docker image directly to the device's registry.
	regPort := registryPort(agentOS)
	// For link-local addresses (USB), a TCP proxy bridges the Docker VM
	// to the host so buildx can reach the device.
	registryAddr, proxyCleanup, err := resolveRegistryForAgent(ctx, conn, regPort)
	if err != nil {
		return err
	}
	defer proxyCleanup()

	repo := strings.ToLower(appCfg.AppID)
	registryImage := fmt.Sprintf("%s/%s:latest", registryAddr, repo)

	cliLogln("Building and pushing Docker image for %s...", platform)
	if err := buildAndPushImage(ctx, cwd, registryAddr, registryImage, platform, buildArgs, os.Stdout, conn.IsMTLS); err != nil {
		return fmt.Errorf("building and pushing Docker image: %w", err)
	}
	cliLogln("Build and push completed.")

	if len(appCfg.Entitlements) > 0 {
		hostAddr := cliRegistryAddr(registryAddr)
		if err := annotateManifestWithEntitlements(ctx, hostAddr, repo, "latest", appCfg.Entitlements, conn.IsMTLS); err != nil {
			cliLogln("Warning: could not annotate image manifest with entitlements: %v", err)
		}
	}

	// Inject debugpy for Python remote debugging.
	if opts.debug && appCfg.Language == "python" {
		cliLogln("Injecting debugpy for remote debugging...")
		if err := injectDebugpy(ctx, registryAddr, registryImage, platform, buildArgs, os.Stdout, conn.IsMTLS); err != nil {
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
		stream, err := conn.ContainerService.StartContainer(contextWithPostStartAgentHook(ctx, appCfg), &agentpb.StartContainerRequest{
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

	outStream, stdinAttempted, err := openContainerStream(runCtx, conn.ContainerService, appCfg.AppID, appCfg)
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
				startStream, startErr := conn.ContainerService.StartContainer(contextWithPostStartAgentHook(runCtx, appCfg), &agentpb.StartContainerRequest{
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

// shellCommand returns the shell binary and the argument prefix for running a
// command string. On Windows it returns cmd.exe with /S /C: /S makes quote
// stripping predictable when the command contains nested quotes (under the
// default /C rules cmd.exe behavior depends on the count of `"` characters).
// On Unix it returns sh -c.
func shellCommand() (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd.exe", []string{"/S", "/C"}
	}
	return "sh", []string{"-c"}
}

// expandHookEnv resolves Wendy's documented placeholders in s. Both Unix-style
// (${VAR}, $VAR) and Windows-style (%WENDY_*%) forms are accepted for the two
// Wendy-provided placeholders, so the same hook string parses identically in
// sh and cmd.exe. Other ${VAR} forms fall through to os.Getenv; raw %VAR%
// forms for non-Wendy variables are left for cmd.exe to expand natively.
func expandHookEnv(s, hostname, appID string) string {
	s = strings.ReplaceAll(s, "%WENDY_HOSTNAME%", hostname)
	s = strings.ReplaceAll(s, "%WENDY_APP_ID%", appID)
	return os.Expand(s, func(key string) string {
		switch key {
		case "WENDY_HOSTNAME":
			return hostname
		case "WENDY_APP_ID":
			return appID
		default:
			return os.Getenv(key)
		}
	})
}

// browserOpen is the cross-platform browser opener used by openURL hooks.
// Indirected through a var so tests can swap it out.
var browserOpen = browseropen.Open

// startPostStartHook fires the postStart hook actions for appCfg.
//
// If openURL is set, it is expanded and opened in the developer's default
// browser via the shared browseropen helper — no shell, no quoting. If cli
// is set, it runs after, expanded for env vars and dispatched through the
// platform shell; the returned *exec.Cmd is the cli child for the caller to
// wait on or kill. Returns nil when no cli command is configured (regardless
// of whether openURL was fired).
func startPostStartHook(ctx context.Context, appCfg *appconfig.AppConfig, hostname string) *exec.Cmd {
	if appCfg.Hooks == nil || appCfg.Hooks.PostStart == nil {
		return nil
	}
	hook := appCfg.Hooks.PostStart

	if hook.OpenURL != "" {
		url := expandHookEnv(hook.OpenURL, hostname, appCfg.AppID)
		if err := browserOpen(url); err != nil {
			cliLogln("Warning: postStart openURL failed: %v", err)
		} else {
			cliLogln("Hook postStart: opened %s", url)
		}
	}

	if hook.CLI == "" {
		return nil
	}

	expanded := expandHookEnv(hook.CLI, hostname, appCfg.AppID)
	shell, flags := shellCommand()
	cmd := execCommandContext(ctx, shell, append(flags, expanded)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	finalizeProcessGroup := configurePostStartProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		// Release any OS resources the configure step allocated; the finalizer
		// no-ops the attach step when cmd.Process is nil.
		finalizeProcessGroup()
		cliLogln("Warning: postStart hook failed to start: %v", err)
		return nil
	}
	finalizeProcessGroup()
	cliLogln("Hook postStart: %s", expanded)
	return cmd
}

// wendyPlatform maps a WendyOS device type to a platform tier used for
// Dockerfile base stage selection. Adding a new device only requires adding
// a case here; templates need no changes until a new platform tier is introduced.
// Unknown device types fall back to "generic" (CPU-only).
func wendyPlatform(deviceType string) string {
	switch deviceType {
	case "jetson-agx-orin", "jetson-orin-nano":
		return "nvidia-jetson"
	default:
		return "generic"
	}
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
