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

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
	"golang.org/x/term"
)

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
	appCfg, err := appconfig.LoadFromFile(cfgPath)
	if err != nil {
		return fmt.Errorf("loading wendy.json: %w", err)
	}

	if err := appCfg.Validate(); err != nil {
		return fmt.Errorf("invalid wendy.json: %w", err)
	}

	imageName := appCfg.AppID + ":latest"

	// Step 2: Detect project type and ensure a Dockerfile exists.
	projectType := detectProjectType(cwd)
	switch projectType {
	case "docker":
		// Dockerfile already exists, nothing to do.
	case "python":
		if _, err := os.Stat(filepath.Join(cwd, "Dockerfile")); os.IsNotExist(err) {
			fmt.Println("No Dockerfile found. Generating one for Python project...")
			if _, genErr := generatePythonDockerfile(cwd); genErr != nil {
				return fmt.Errorf("generating Dockerfile: %w", genErr)
			}
			fmt.Println("Generated Dockerfile.")
		}
	case "swift":
		if _, err := os.Stat(filepath.Join(cwd, "Dockerfile")); os.IsNotExist(err) {
			return fmt.Errorf("Swift projects require a Dockerfile for cross-compilation to linux/arm64")
		}
	default:
		return fmt.Errorf("unable to detect project type; ensure a Dockerfile, requirements.txt, or Package.swift is present")
	}

	// Step 3: Build the Docker image for linux/arm64.
	fmt.Println("Building Docker image for linux/arm64...")
	if err := buildDockerImage(ctx, cwd, imageName, "linux/arm64", os.Stdout); err != nil {
		return fmt.Errorf("building Docker image: %w", err)
	}
	fmt.Println("Build completed.")

	// Step 4: Export the image as a tar.
	tarPath := filepath.Join(os.TempDir(), appCfg.AppID+"-image.tar")
	defer os.Remove(tarPath)

	fmt.Println("Exporting image...")
	if err := saveDockerImage(ctx, imageName, tarPath); err != nil {
		return fmt.Errorf("exporting Docker image: %w", err)
	}

	// Step 5: Extract OCI layers from the tar.
	ociImage, err := extractOCIImage(tarPath)
	if err != nil {
		return fmt.Errorf("extracting OCI image: %w", err)
	}

	fmt.Printf("Image has %d layers.\n", len(ociImage.Layers))

	// Step 6: Connect to agent gRPC.
	conn, err := connectToAgent(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Step 7: Serialize app config for the agent.
	appConfigData, err := json.Marshal(appCfg)
	if err != nil {
		return fmt.Errorf("marshaling app config: %w", err)
	}

	// Step 8: Upload missing layers and create container.
	restartPolicy := resolveRestartPolicy(opts)

	return uploadAndDeploy(ctx, conn, appCfg, ociImage, tarPath, appConfigData, restartPolicy, opts)
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

// uploadAndDeploy handles querying existing layers, uploading missing ones,
// and creating/starting the container on the agent.
func uploadAndDeploy(
	ctx context.Context,
	conn *grpcclient.AgentConnection,
	appCfg *appconfig.AppConfig,
	ociImage *OCIImage,
	tarPath string,
	appConfigData []byte,
	restartPolicy *agentpb.RestartPolicy,
	opts runOptions,
) error {
	// Query existing layers on the agent to skip re-uploads.
	existingDigests := make(map[string]bool)
	layerStream, err := conn.ContainerService.ListLayers(ctx, &agentpb.ListLayersRequest{})
	if err == nil {
		for {
			header, recvErr := layerStream.Recv()
			if recvErr == io.EOF {
				break
			}
			if recvErr != nil {
				break
			}
			existingDigests[header.GetDigest()] = true
		}
	}

	// Determine which layers need uploading.
	var missingLayers []OCILayer
	var totalUploadSize int64
	for _, layer := range ociImage.Layers {
		if !existingDigests[layer.Digest] {
			missingLayers = append(missingLayers, layer)
			totalUploadSize += layer.Size
		}
	}

	fmt.Printf("Uploading %d of %d layers (%d bytes)...\n", len(missingLayers), len(ociImage.Layers), totalUploadSize)

	if len(missingLayers) > 0 {
		if err := uploadLayers(ctx, conn, ociImage, missingLayers, totalUploadSize, tarPath); err != nil {
			return err
		}
	}

	fmt.Println("Upload complete.")

	// Build layer headers for the RunContainer / CreateContainer request.
	var layerHeaders []*agentpb.RunContainerLayerHeader
	for _, layer := range ociImage.Layers {
		layerHeaders = append(layerHeaders, &agentpb.RunContainerLayerHeader{
			Digest: layer.Digest,
			Size:   layer.Size,
			DiffId: layer.DiffID,
			Gzip:   layer.GZip,
		})
	}

	// Build the container command from the image's Entrypoint + Cmd.
	var cmdParts []string
	cmdParts = append(cmdParts, ociImage.Entrypoint...)
	cmdParts = append(cmdParts, ociImage.Cmd...)
	containerCmd := strings.Join(cmdParts, " ")

	// Use image's WorkingDir if available.
	workingDir := ociImage.WorkingDir

	if opts.deploy {
		// Create container without starting.
		_, err := conn.ContainerService.CreateContainer(ctx, &agentpb.CreateContainerRequest{
			ImageName:     appCfg.AppID + ":latest",
			AppName:       appCfg.AppID,
			Cmd:           containerCmd,
			WorkingDir:    workingDir,
			AppConfig:     appConfigData,
			RestartPolicy: restartPolicy,
			UserArgs:      opts.userArgs,
		})
		if err != nil {
			return fmt.Errorf("creating container: %w", err)
		}
		return nil
	}

	// Use RunContainer on the container service to create and start.
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	stream, err := conn.ContainerService.RunContainer(runCtx, &agentpb.RunContainerLayersRequest{
		ImageName:     appCfg.AppID + ":latest",
		AppName:       appCfg.AppID,
		Cmd:           containerCmd,
		WorkingDir:    workingDir,
		AppConfig:     appConfigData,
		Layers:        layerHeaders,
		RestartPolicy: restartPolicy,
		UserArgs:      opts.userArgs,
	})
	if err != nil {
		return fmt.Errorf("running container: %w", err)
	}

	// Wait for the Started response.
	for {
		resp, recvErr := stream.Recv()
		if recvErr == io.EOF {
			return nil
		}
		if recvErr != nil {
			return fmt.Errorf("receiving run response: %w", recvErr)
		}
		if resp.GetStarted() != nil {
			fmt.Printf("Container %s started.\n", appCfg.AppID)
			break
		}
	}

	if opts.detach {
		fmt.Printf("Application %s running in detached mode.\n", appCfg.AppID)
		return nil
	}

	// Stream stdout/stderr from the RunContainer stream directly.
	// Set up Ctrl+C handler to stop the container.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)

	go func() {
		<-sigCh
		fmt.Println("\nStopping container...")
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

	fmt.Printf("\nApplication %s stopped.\n", appCfg.AppID)
	return nil
}

// uploadLayers uploads missing layers, config, and manifest to the agent.
// Uses a TUI progress bar when a terminal is available, plain text otherwise.
func uploadLayers(ctx context.Context, conn *grpcclient.AgentConnection, ociImage *OCIImage, missingLayers []OCILayer, totalUploadSize int64, tarPath string) error {
	isTTY := term.IsTerminal(int(os.Stdout.Fd()))

	doUpload := func(onProgress func(layerIdx int, layerDigest string, uploaded int64)) error {
		var uploadedSoFar int64
		for i, layer := range missingLayers {
			layerData, err := readLayerData(tarPath, layer.FilePath)
			if err != nil {
				return err
			}
			if err := uploadLayer(ctx, conn, layer.Digest, layerData); err != nil {
				return err
			}
			uploadedSoFar += layer.Size
			if onProgress != nil {
				onProgress(i, layer.Digest, uploadedSoFar)
			}
		}

		return nil
	}

	if !isTTY {
		// Plain text progress.
		err := doUpload(func(i int, digest string, uploaded int64) {
			pct := 0
			if totalUploadSize > 0 {
				pct = int(100 * uploaded / totalUploadSize)
			}
			short := digest
			if len(short) > 19 {
				short = short[:19]
			}
			fmt.Printf("  [%d/%d] %s... %d%% (%d / %d bytes)\n", i+1, len(missingLayers), short, pct, uploaded, totalUploadSize)
		})
		if err != nil {
			return fmt.Errorf("upload failed: %w", err)
		}
		fmt.Println("Upload complete.")
		return nil
	}

	// TUI progress bar.
	progModel := tui.NewProgress("Uploading layers...")
	p := tea.NewProgram(progModel)

	go func() {
		err := doUpload(func(_ int, _ string, uploaded int64) {
			if totalUploadSize > 0 {
				p.Send(tui.ProgressUpdateMsg{Percent: float64(uploaded) / float64(totalUploadSize)})
			}
		})
		p.Send(tui.ProgressDoneMsg{Err: err})
	}()

	finalModel, runErr := p.Run()
	if runErr != nil {
		return fmt.Errorf("TUI error: %w", runErr)
	}
	pm := finalModel.(tui.ProgressModel)
	if pm.Err() != nil {
		return fmt.Errorf("upload failed: %w", pm.Err())
	}
	fmt.Println("Upload complete.")
	return nil
}

// uploadLayer sends a single layer to the agent via WriteLayer streaming RPC.
func uploadLayer(ctx context.Context, conn *grpcclient.AgentConnection, digest string, data []byte) error {
	stream, err := conn.ContainerService.WriteLayer(ctx)
	if err != nil {
		return fmt.Errorf("starting WriteLayer stream: %w", err)
	}

	const chunkSize = 64 * 1024 // 64KB
	for offset := 0; offset < len(data); offset += chunkSize {
		end := offset + chunkSize
		if end > len(data) {
			end = len(data)
		}

		if err := stream.Send(&agentpb.WriteLayerRequest{
			Digest: digest,
			Data:   data[offset:end],
		}); err != nil {
			return fmt.Errorf("sending layer chunk: %w", err)
		}
	}

	if err := stream.CloseSend(); err != nil {
		return fmt.Errorf("closing WriteLayer stream: %w", err)
	}

	// Wait for acknowledgement.
	if _, err := stream.Recv(); err != nil && err != io.EOF {
		return fmt.Errorf("WriteLayer response: %w", err)
	}

	return nil
}

