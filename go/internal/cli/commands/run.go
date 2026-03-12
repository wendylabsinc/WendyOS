package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/internal/cli/providers"
	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/internal/shared/models"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

var cliStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

func cliLog(format string, args ...any) {
	fmt.Print(cliStyle.Render(fmt.Sprintf(format, args...)))
}

func cliLogln(format string, args ...any) {
	fmt.Println(cliStyle.Render(fmt.Sprintf(format, args...)))
}

// runOptions holds the parsed flags for the run command.
type runOptions struct {
	debug                bool
	deploy               bool
	detach               bool
	restartUnlessStopped bool
	restartOnFailure     bool
	noRestart            bool
	userArgs             []string
}

func newRunCmd() *cobra.Command {
	var opts runOptions

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Build and run application on a WendyOS device",
		Long:  "Reads wendy.json from the current directory, builds a container image, and deploys it to the target device.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCommand(cmd.Context(), opts)
		},
	}

	cmd.Flags().BoolVar(&opts.debug, "debug", false, "Enable debug logging")
	cmd.Flags().BoolVar(&opts.deploy, "deploy", false, "Create container but do not start it")
	cmd.Flags().BoolVar(&opts.detach, "detach", false, "Start container but do not stream logs")
	cmd.Flags().BoolVar(&opts.restartUnlessStopped, "restart-unless-stopped", false, "Restart unless manually stopped")
	cmd.Flags().BoolVar(&opts.restartOnFailure, "restart-on-failure", false, "Restart on failure")
	cmd.Flags().BoolVar(&opts.noRestart, "no-restart", false, "Do not restart on exit")
	cmd.Flags().StringSliceVar(&opts.userArgs, "user-args", nil, "Extra arguments to pass to the container")

	return cmd
}

func runCommand(ctx context.Context, opts runOptions) error {
	// Step 1: Load and validate wendy.json.
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}

	cfgPath := filepath.Join(cwd, "wendy.json")
	appCfg, err := ensureAppConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("loading wendy.json: %w", err)
	}

	if err := appCfg.Validate(); err != nil {
		return fmt.Errorf("invalid wendy.json: %w", err)
	}

	// Debug mode requires host networking for remote debugger access (gdb/lldb).
	// Python apps also need host networking for debugpy.
	if opts.debug {
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
	target, err := resolveTarget(ctx)
	if err != nil {
		return err
	}

	// Provider-based run path.
	if target.External != nil && target.Provider != nil {
		return runWithProvider(ctx, target.Provider, *target.External, cwd, appCfg.AppID, opts)
	}

	// Wendy Lite devices don't run the WendyOS agent — they can't execute containers.
	if target.Agent == nil {
		return fmt.Errorf("selected device is a Wendy Lite device and does not support 'wendy run'; use 'wendy wifi' for provisioning")
	}

	// Agent-based run path (existing gRPC pipeline).
	defer target.Agent.Close()
	return runWithAgent(ctx, target.Agent, cwd, appCfg, opts)
}

// runSwiftWithAgent builds a Swift package using swift-container-plugin, which
// pushes the image directly to the device's registry. Then it creates and
// starts the container on the agent.
func runSwiftWithAgent(ctx context.Context, conn *grpcclient.AgentConnection, cwd string, appCfg *appconfig.AppConfig, opts runOptions) error {
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

	product := findSwiftProduct(cwd)

	cliLogln("Building Swift container image for %s (%s)...", product, architecture)
	if err := buildSwiftContainerImage(ctx, cwd, product, conn.Host, architecture); err != nil {
		return fmt.Errorf("building Swift container image: %w", err)
	}
	cliLogln("Build and push completed.")

	// The image is now in the device's registry. The agent will pull it
	// from localhost:5000 when creating the container.
	deviceImage := fmt.Sprintf("localhost:5000/%s:latest", strings.ToLower(product))

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

	if opts.deploy {
		_, err := conn.ContainerService.CreateContainer(ctx, createReq)
		if err != nil {
			return fmt.Errorf("creating container: %w", err)
		}
		cliLogln("Container %s created (not started).", appCfg.AppID)
		return nil
	}

	// Create the container.
	_, err = conn.ContainerService.CreateContainer(ctx, createReq)
	if err != nil {
		return fmt.Errorf("creating container: %w", err)
	}
	cliLogln("Container %s created.", appCfg.AppID)

	if opts.detach {
		// Start but don't stream output.
		stream, err := conn.ContainerService.StartContainer(ctx, &agentpb.StartContainerRequest{
			AppName: appCfg.AppID,
		})
		if err != nil {
			return fmt.Errorf("starting container: %w", err)
		}
		// Wait for the started confirmation then return.
		if _, err := stream.Recv(); err != nil && err != io.EOF {
			return fmt.Errorf("waiting for container start: %w", err)
		}
		cliLogln("Application %s running in detached mode.", appCfg.AppID)
		return nil
	}

	// Start and stream output.
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

// runWithProvider builds and runs via an external device provider.
func runWithProvider(ctx context.Context, p providers.DeviceProvider, device models.ExternalDevice, projectPath, product string, opts runOptions) error {
	// For Swift projects, resolve the actual executable product name from
	// Package.swift rather than using the wendy.json app ID.
	if p.CanBuild(projectPath) {
		if swiftProduct := findSwiftProduct(projectPath); swiftProduct != "" {
			product = swiftProduct
		}
	}

	cliLogln("Building with %s provider...", p.DisplayName())
	app, err := p.Build(ctx, device, projectPath, product, opts.debug)
	if err != nil {
		return fmt.Errorf("provider build: %w", err)
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
	projectType := detectProjectType(cwd)

	// Swift projects without a Dockerfile use swift-container-plugin to push
	// directly to the device's registry, bypassing the Docker build pipeline.
	if projectType == "swift" {
		if _, err := os.Stat(filepath.Join(cwd, "Dockerfile")); os.IsNotExist(err) {
			return runSwiftWithAgent(ctx, conn, cwd, appCfg, opts)
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

	// Query the device architecture.
	versionResp, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		return fmt.Errorf("querying device version: %w", err)
	}
	architecture := versionResp.GetCpuArchitecture()
	if architecture == "" {
		architecture = "arm64"
	}
	platform := "linux/" + architecture

	// Verify auth certs are available if the device's registry requires mTLS.
	if err := requireRegistryAuth(ctx, conn); err != nil {
		return err
	}

	// Build and push the Docker image directly to the device's registry.
	registryAddr := registryHost(conn.Host, 5000)
	repo := strings.ToLower(appCfg.AppID)
	registryImage := fmt.Sprintf("%s/%s:latest", registryAddr, repo)

	cliLogln("Building and pushing Docker image for %s...", platform)
	if err := buildAndPushImage(ctx, cwd, registryAddr, registryImage, platform, os.Stdout); err != nil {
		return fmt.Errorf("building and pushing Docker image: %w", err)
	}
	cliLogln("Build and push completed.")

	// Inject debugpy for Python remote debugging.
	if opts.debug && appCfg.Language == "python" {
		cliLogln("Injecting debugpy for remote debugging...")
		if err := injectDebugpy(ctx, registryAddr, registryImage, platform, os.Stdout); err != nil {
			return fmt.Errorf("injecting debugpy: %w", err)
		}
	}

	// The agent pulls from localhost:5000.
	deviceImage := fmt.Sprintf("localhost:5000/%s:latest", repo)

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

	if opts.deploy {
		_, err := conn.ContainerService.CreateContainer(ctx, createReq)
		if err != nil {
			return fmt.Errorf("creating container: %w", err)
		}
		cliLogln("Container %s created (not started).", appCfg.AppID)
		return nil
	}

	// Create the container.
	_, err = conn.ContainerService.CreateContainer(ctx, createReq)
	if err != nil {
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

	// Start and stream output.
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
