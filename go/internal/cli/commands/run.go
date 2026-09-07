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
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"gopkg.in/yaml.v3"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/cli/swifttoolchain"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/browseropen"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/internal/shared/seriallock"
	"github.com/wendylabsinc/wendy/go/internal/stagefile"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/gpu"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

var cliStyle = lipgloss.NewStyle().Foreground(tui.ColorDim)
var cliNoticeStyle = lipgloss.NewStyle().Foreground(tui.ColorNotice)
var execCommandContext = exec.CommandContext

func macPlatformMismatchMessage(platform string) string {
	return fmt.Sprintf("Project/target mismatch: selected target is Wendy Agent for Mac, but wendy.json resolves to platform %q. Wendy Agent for Mac currently runs native macOS apps only. To fix this, set `platform: \"darwin\"` and use a Mac-compatible native SwiftPM or Xcode template, or target a Linux/WendyOS device.", platform)
}

func rejectUnsupportedMacRunProject(projectType, platform string) error {
	osName := platformOS(platform)
	// Native darwin apps and Linux/WendyOS containers (via the Mac agent's
	// container runtime) are both supported. Anything else is a real mismatch.
	if !strings.EqualFold(osName, appconfig.PlatformDarwin) &&
		!strings.EqualFold(osName, "linux") &&
		!strings.EqualFold(osName, "wendyos") {
		return errors.New(macPlatformMismatchMessage(platform))
	}
	switch projectType {
	case "swift", "xcode", "docker", "python", "compose", "multi-service":
		return nil
	default:
		return fmt.Errorf("unable to detect project type for a Mac target: %q", projectType)
	}
}

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

// containerDisplayName returns the container identity for CLI lifecycle
// messages (created/started/stopped), styled for terminal output. It is the
// real container name — "{appID}_{serviceName}" when appCfg describes a single
// service of a multi-service app — because printing the bare appID obscures
// which service container a deploy just acted on (WDY-1828).
func containerDisplayName(appCfg *appconfig.AppConfig) string {
	return tui.App(appCfg.ContainerName())
}

// cliLogln prints a human status line. It writes to stderr: --json is a global
// persistent flag (root.go) and is auto-enabled when the terminal is
// non-interactive, so any status line on stdout can corrupt machine-read output.
// Real payloads (JSON, listings) print via fmt.Println/encoders and stay on stdout.
func cliLogln(format string, args ...any) {
	fmt.Fprintln(os.Stderr, cliStyle.Render(fmt.Sprintf(format, args...)))
}

func cliNotice(format string, args ...any) {
	fmt.Fprintln(os.Stderr, cliNoticeStyle.Render(fmt.Sprintf(format, args...)))
}

var cliSuccessStyle = lipgloss.NewStyle().Foreground(tui.ColorPrimary)

// cliSuccess prints a styled success status line. Writes to stderr for the
// same reason as cliLogln above — it is a status line, not a payload.
func cliSuccess(format string, args ...any) {
	fmt.Fprintln(os.Stderr, cliSuccessStyle.Render(fmt.Sprintf(format, args...)))
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

func unpackProgressDetail(progress *agentpb.CreateContainerProgress) string {
	total := progress.GetTotalLayers()
	if total <= 0 {
		return ""
	}

	switch progress.GetPhase() {
	case agentpb.CreateContainerProgress_UNPACKING:
		if progress.GetLayerSize() > 0 {
			return fmt.Sprintf("Layer %d/%d applying%s", unpackLayerNumber(progress, total), total, unpackLayerSizeSuffix(progress))
		}
		return fmt.Sprintf("Unpack plan: %d %s", total, pluralize(total, "layer", "layers"))
	case agentpb.CreateContainerProgress_APPLYING_LAYER:
		status := "unpacked"
		if progress.GetReusedSnapshot() {
			status = "reused snapshot"
		}
		return fmt.Sprintf("Layer %d/%d %s%s", unpackLayerNumber(progress, total), total, status, unpackLayerSizeSuffix(progress))
	default:
		return ""
	}
}

func unpackLayerNumber(progress *agentpb.CreateContainerProgress, total int32) int32 {
	if total <= 0 {
		return 0
	}
	index := progress.GetLayerIndex()
	if index < 0 {
		index = 0
	}
	if index >= total {
		index = total - 1
	}
	return index + 1
}

func unpackLayerSizeSuffix(progress *agentpb.CreateContainerProgress) string {
	size := progress.GetLayerSize()
	if size <= 0 {
		return ""
	}
	return fmt.Sprintf(" (%s)", tui.FormatBytes(size))
}

func pluralize(n int32, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
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
			case agentpb.CreateContainerProgress_UNPACKING, agentpb.CreateContainerProgress_APPLYING_LAYER:
				if detail := unpackProgressDetail(r.Progress); detail != "" {
					cliLogln("%s", detail)
				} else {
					cliLogln("%s", unpackProgressTitle(r.Progress))
				}
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
	prog := tui.NewProgressProgram(tui.NewProgress("Pulling image on device...").WithoutErrorView())

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
						Detail:  unpackProgressDetail(r.Progress),
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
func useInteractiveCreateProgress(ctx context.Context) bool {
	return isInteractiveTerminal() && !watchUsesPlainProgress(ctx)
}

func createContainerWithProgress(ctx context.Context, svc agentpb.WendyContainerServiceClient, req *agentpb.CreateContainerRequest) error {
	if !useInteractiveCreateProgress(ctx) {
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

// resolveStagefileGPUTarget resolves a target only when GPU-aware Stagefile
// selection actually needs one. Watch sessions pass their already-selected
// target so every cycle stays pinned to the same device and connection.
func resolveStagefileGPUTarget(ctx context.Context, cwd string, target *SelectedDevice, opts runOptions) (*SelectedDevice, error) {
	if target != nil || opts.gpuArch != "" || !stagefile.NeedsGPUTarget(cwd) {
		return target, nil
	}
	return resolveRunTarget(ctx, runResolveOptions(opts)...)
}

type runOptions struct {
	buildType  string
	dockerfile string
	builder    string
	// buildHost names a WendyOS device that builds the image instead of this
	// machine. Empty means build locally, and every existing local path must be
	// unaffected when it is empty.
	buildHost string
	// Devices beyond the primary --device that this build is also delivered to.
	// Empty for every ordinary run.
	fleetDevices         []string
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
	keepGoing            bool
	// maxConcurrency bounds simultaneous service build-and-push jobs for both
	// standalone multi-service manifests and Compose projects.
	maxConcurrency int
	userArgs       []string
	// env are extra KEY=VALUE environment variables injected into the container
	// at create time (CreateContainerRequest.Env). Set by --env, and by fleet
	// deploys to wire cross-component discovery (e.g. WENDY_FLEET_PEERS) into a
	// component. They override wendy.json env of the same key.
	env []string
	// quietBuild suppresses image-build output unless the build fails. Watch
	// mode enables it unless --verbose is set.
	quietBuild bool
	// watchState tracks the effective per-service state successfully deployed by
	// this watch session. Multi-service deploys use it to leave unchanged,
	// already-running containers completely untouched on later cycles. It is
	// non-nil only under `wendy run --watch`, so the deploy paths also read it
	// as "this run belongs to a watch session".
	watchState *watchDeployState
	// watchTarget is resolved once by the watch session and shared by its
	// serialized deploy cycles. Ordinary runs leave it nil and own the target
	// they resolve inside runCommand.
	watchTarget *SelectedDevice
	// gpuArch overrides the GPU architecture a Stagefile cuda: stage compiles
	// against. Normally the selected device answers this; the flag exists for
	// building against a board that isn't the one in front of you.
	gpuArch string
	// chunking controls the content-defined chunking (CBC) deploy path:
	// chunkingAuto (default/empty) tries chunk-diff and falls back to a registry
	// push on failure, chunkingForce uses chunk-diff with no fallback, and
	// chunkingOff skips chunk-diff entirely (registry push only).
	chunking string
}

// runResolveOptions builds the resolveTarget options shared by every `wendy run`
// device-selection path. The interactive picker hides local run targets (the
// local machine, Docker/OrbStack, Apple Container) unless
// WENDY_SHOW_LOCAL_DEVICES is set; --yes suppresses the picker entirely.
func runResolveOptions(opts runOptions) []resolveOption {
	var resolveOpts []resolveOption
	if opts.isWatch() {
		resolveOpts = append(resolveOpts, DisableSessionBroker())
	}
	if opts.yes {
		resolveOpts = append(resolveOpts, NonInteractive())
	}
	return resolveOpts
}

// Valid values for runOptions.chunking. An empty value is treated as
// chunkingAuto so callers that build runOptions directly (e.g. wendy watch)
// keep the default behavior.
const (
	chunkingAuto  = "auto"
	chunkingForce = "force"
	chunkingOff   = "off"
)

// validateChunkingMode rejects unknown --chunking values. Empty is allowed and
// means chunkingAuto.
func validateChunkingMode(mode string) error {
	switch mode {
	case "", chunkingAuto, chunkingForce, chunkingOff:
		return nil
	default:
		return fmt.Errorf("invalid --chunking value %q: must be auto, force, or off", mode)
	}
}

func newRunCmd() *cobra.Command {
	var opts runOptions
	var watch bool
	var debounceMS int
	var verbose bool

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Build and run application on a WendyOS device",
		Long:  "Reads wendy.json from the current directory or --prefix directory, builds a container image, and deploys it to the target device.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithInterruptContext(cmd.Context(), func(runCtx context.Context) error {
				if err := validateEnvFlag(opts.env); err != nil {
					return err
				}
				if watch {
					// In watch mode, hide build output unless a build fails (unless
					// --verbose); watchCommand enforces non-interactive behavior.
					opts.quietBuild = !verbose
					return watchCommand(runCtx, opts, time.Duration(debounceMS)*time.Millisecond)
				}
				return runCommand(runCtx, opts)
			})
		},
	}

	cmd.Flags().StringVar(&opts.buildType, "build-type", "", "Build type to use when Dockerfile/Containerfile is present alongside Package.swift or Python project markers: docker, swift, or python")
	cmd.Flags().StringVar(&opts.dockerfile, "dockerfile", "", "Build file to build from: a Dockerfile, Containerfile, or Stagefile (e.g. Dockerfile.prod, Containerfile, prod.stagefile.yaml); shows a selection menu when multiple build files exist")
	cmd.Flags().StringVar(&opts.builder, "builder", "", "Image builder to force for Dockerfile/Containerfile builds: docker, apple-container, or buildkit")
	cmd.Flags().StringVar(&opts.gpuArch, "gpu-arch", "", fmt.Sprintf("GPU architecture a Stagefile cuda: stage targets (%s); read from the device when one is selected", strings.Join(gpu.KnownArches(), ", ")))
	cmd.Flags().StringVar(&opts.buildHost, "build-host", "", "WendyOS device to build the image on instead of this machine (e.g. a DGX Spark); the built image is pushed straight to the target device")
	cmd.Flags().BoolVar(&opts.debug, "debug", false, "Enable debug logging")
	cmd.Flags().BoolVar(&opts.deploy, "deploy", false, "Create container but do not start it")
	cmd.Flags().BoolVar(&opts.detach, "detach", false, "Start container and return without streaming logs, waiting for readiness, or opening the app URL")
	cmd.Flags().BoolVarP(&opts.yes, "yes", "y", false, "Automatically accept all interactive prompts")
	cmd.Flags().BoolVar(&opts.restartUnlessStopped, "restart-unless-stopped", false, "Restart unless manually stopped")
	cmd.Flags().BoolVar(&opts.restartOnFailure, "restart-on-failure", false, "Restart on failure")
	cmd.Flags().BoolVar(&opts.noRestart, "no-restart", false, "Do not restart on exit")
	cmd.Flags().StringVar(&opts.prefix, "prefix", "", "Project directory to run from instead of the current working directory")
	cmd.Flags().StringVar(&opts.product, "product", "", "Swift Package Manager product to build and run")
	cmd.Flags().StringVar(&opts.service, "service", "", "Build and run only the named service and its dependencies (multi-service projects)")
	cmd.Flags().BoolVar(&opts.keepGoing, "keep-going", false, "Multi-service: deploy services that build successfully instead of aborting the whole group on the first build/push failure")
	cmd.Flags().IntVar(&opts.maxConcurrency, "max-concurrency", 0, "Multi-service/Compose: max service images to build+push at once (0 = default limit of 4)")
	cmd.Flags().StringSliceVar(&opts.userArgs, "user-args", nil, "Extra arguments to pass to the container")
	cmd.Flags().StringArrayVar(&opts.env, "env", nil, "Set an environment variable in the container as KEY=VALUE; repeatable, and overrides wendy.json env of the same key")
	cmd.Flags().StringVar(&opts.chunking, "chunking", chunkingAuto, "Content-defined chunking (CBC) deploy path: auto (try chunk-diff, fall back to registry push), force (chunk-diff only, no fallback), or off (registry push only)")
	cmd.Flags().BoolVar(&watch, "watch", false, "Watch the project directory and redeploy on every change, streaming logs between deploys (same as 'wendy watch')")
	cmd.Flags().IntVar(&debounceMS, "debounce", 400, "Watch mode (--watch): quiet period in milliseconds after the last change before redeploying")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "Watch mode (--watch): always show build output (default: hidden unless the build fails)")

	return cmd
}

// runWithInterruptContext gives every phase of `wendy run` the same
// cancellation signal, including provider builds that do not use Wendy's build
// progress UI. Previously only the individual Bubble Tea program observed
// Ctrl-C during a build; its docker/OrbStack/Apple Container subprocess kept a
// live parent context, so parallel services and fallback builders each surfaced
// another cancellation of their own.
func runWithInterruptContext(parent context.Context, run func(context.Context) error) error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	return runWithInterruptChannel(parent, sigCh, run)
}

func runWithInterruptChannel(parent context.Context, sigCh <-chan os.Signal, run func(context.Context) error) error {
	ctx, cancel := context.WithCancelCause(parent)
	done := make(chan struct{})
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		select {
		case <-sigCh:
			cancel(ErrUserCancelled)
		case <-done:
		}
	}()

	err := run(ctx)
	// If the operation returned from the subprocess's copy of SIGINT before
	// the goroutine above was scheduled, consume the already-buffered signal
	// here so cancellation is still classified consistently.
	select {
	case <-sigCh:
		cancel(ErrUserCancelled)
	default:
	}
	close(done)
	<-handlerDone
	cancel(nil)
	if errors.Is(context.Cause(ctx), ErrUserCancelled) && err != nil {
		return ErrUserCancelled
	}
	return err
}

// resolveRunTarget resolves the target device for the run command. It first
// tries resolveTarget (direct/picker). If that fails and cloud auth entries
// exist, it retries via the cloud tunnel using the device name from --device
// or the configured default.
func resolveRunTarget(ctx context.Context, opts ...resolveOption) (*SelectedDevice, error) {
	return resolveWithCloudFallback(ctx, "", opts...)
}

// cloudFallbackDeviceName picks which device the cloud tunnel should dial.
//
// An explicit name always wins, and must never be silently replaced by
// deviceFlag: those name two different machines during `wendy run --build-host`,
// and preferring the flag would build on the deploy target.
func cloudFallbackDeviceName(explicit, flagValue, configDefault string) string {
	if explicit != "" {
		return explicit
	}
	if flagValue != "" {
		return flagValue
	}
	return configDefault
}

// resolveWithCloudFallback is resolveRunTarget with the cloud-tunnel device name
// stated explicitly rather than read from the --device flag.
//
// cloudName must be set by any caller connecting to a device that is NOT the
// deploy target. `wendy run --build-host` has two devices in flight, and the
// fallback name is not interchangeable between them: with cloudName empty this
// falls back to deviceFlag, so a build-host caller would tunnel to the TARGET
// and build on the machine it meant to deploy to — landing the image on the
// wrong device while reporting success, which is the exact failure mode the
// two-explicit-flags design exists to prevent.
//
// An empty cloudName preserves the original behaviour for the deploy target,
// where --device IS the device being resolved.
func resolveWithCloudFallback(ctx context.Context, cloudName string, opts ...resolveOption) (*SelectedDevice, error) {
	target, err := resolveTarget(ctx, opts...)
	if err == nil {
		return target, nil
	}
	if errors.Is(err, ErrUserCancelled) {
		return nil, err
	}
	// The user picked a local VM. Falling back to a cloud device here would
	// deploy their app to an entirely different machine and report success.
	if errors.Is(err, errSimulatorUnavailable) {
		return nil, err
	}

	cfg, loadErr := config.Load()
	if loadErr != nil || len(cfg.Auth) == 0 {
		return nil, err
	}

	deviceName := cloudFallbackDeviceName(cloudName, deviceFlag, cfg.DefaultDevice)
	if deviceName == "" {
		return nil, err
	}

	cloudConn, cloudErr := connectToCloudAgent(ctx, "", deviceName, "")
	if cloudErr != nil {
		return nil, err
	}
	maybeFixClock(ctx, cloudConn)
	return &SelectedDevice{Agent: cloudConn}, nil
}

// debugRequiresDebugpy fails fast when a --debug deploy would crash-loop for
// want of debugpy. The agent unconditionally rewrites a Python entrypoint to
// run under debugpy when debug mode is requested (wrapWithDebugpy), but
// nothing injects debugpy into the image itself: 70493f702 ("Remove debugpy
// injection") deliberately deleted that step from the registry-push path,
// so an image that doesn't already bundle debugpy fails immediately on
// device with "No module named debugpy".
//
// This check only has enough information to catch that failure mode for
// Stagefile projects, where the CLI knows which pip requirements file(s)
// feed the build. It is a no-op (returns nil) for anything else: non-python
// apps, and non-Stagefile projects (Dockerfile-authored Python images are
// covered by the printed note below instead, since the CLI cannot inspect
// an opaque Dockerfile's installed packages).
//
// Every Stagefile in the project contributes its requirements files, not just
// the canonical one: this runs before the build file has been chosen, and
// warning about a requirements file that a sibling variant would have installed
// debugpy from is far better than silently skipping the check.
func debugRequiresDebugpy(cwd string, appCfg *appconfig.AppConfig) error {
	if appCfg == nil || appCfg.Language != "python" {
		return nil
	}

	var reqFiles []string
	seen := map[string]bool{}
	for _, source := range stagefile.SourceNames(cwd) {
		data, err := os.ReadFile(filepath.Join(cwd, source))
		if err != nil {
			continue
		}

		var parsed struct {
			Stages []struct {
				Install struct {
					Pip struct {
						Requirements string `yaml:"requirements"`
					} `yaml:"pip"`
				} `yaml:"install"`
			} `yaml:"stages"`
		}
		if err := yaml.Unmarshal(data, &parsed); err != nil {
			// Malformed Stagefile: let the normal build path surface the real
			// parse error instead of failing here on an unrelated check.
			continue
		}

		for _, stage := range parsed.Stages {
			req := strings.TrimSpace(stage.Install.Pip.Requirements)
			if req == "" || seen[req] {
				continue
			}
			seen[req] = true
			reqFiles = append(reqFiles, req)
		}
	}
	if len(reqFiles) == 0 {
		return nil
	}

	for _, req := range reqFiles {
		content, err := os.ReadFile(filepath.Join(cwd, req))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(content), "\n") {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "debugpy") {
				return nil
			}
		}
	}

	return fmt.Errorf(`--debug requires debugpy in the image: add "debugpy" to requirements.txt, or run without --debug`)
}

func runCommand(ctx context.Context, opts runOptions) error {
	mark := phaseTimer()
	// Step 1: Load and validate wendy.json.
	cwd, err := resolveRunWorkingDir(opts)
	if err != nil {
		return fmt.Errorf("resolving working directory: %w", err)
	}
	if _, err := normalizeImageBuilder(opts.builder); err != nil {
		return err
	}
	if opts.maxConcurrency < 0 {
		return fmt.Errorf("--max-concurrency must be >= 0 (0 = default limit of 4)")
	}
	if err := validateChunkingMode(opts.chunking); err != nil {
		return err
	}
	buildHost, err := resolveAndValidateRunBuildHost(opts.buildHost, opts.builder)
	if err != nil {
		return err
	}
	opts.buildHost = buildHost

	// A comma-separated --device names a fleet. Split it HERE, before anything
	// resolves a device: deviceFlag is what target resolution and the cloud
	// tunnel fallback look up, and "ccr1,ccr2" is not a device name. Leaving the
	// split any later means the first lookup fails with a confusing "no device
	// named ccr1,ccr2".
	//
	// deviceFlag is narrowed to the primary so every existing decision in this
	// function -- GPU architecture, agent OS, build-arg hints -- keeps being made
	// against exactly one device, as it always has been.
	if strings.Contains(deviceFlag, ",") {
		primary, extras, splitErr := splitFleetDevices(deviceFlag)
		if splitErr != nil {
			return splitErr
		}
		if err := validateFleetRun(extras, opts.buildHost, opts.detach); err != nil {
			return err
		}
		deviceFlag = primary
		opts.fleetDevices = extras
	}

	// --dockerfile implies a docker build; validate the file exists and ensure
	// --build-type is compatible.
	if opts.dockerfile != "" {
		if opts.buildType != "" && normalizeBuildType(opts.buildType) != "docker" {
			return fmt.Errorf("--dockerfile cannot be used with --build-type=%s", opts.buildType)
		}
		if err := validateDockerfileName(opts.dockerfile); err != nil {
			return fmt.Errorf("--dockerfile: %w", err)
		}
		if _, err := confinedDockerfilePath(cwd, opts.dockerfile); err != nil {
			return fmt.Errorf("--dockerfile: %w", err)
		}
		if opts.buildType == "" {
			opts.buildType = "docker"
		}
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
		if err := rejectUnsupportedBuildHostProject(opts.buildHost, "Compose projects"); err != nil {
			return err
		}
		return runComposeCommand(ctx, cwd, opts)
	}

	// The CLI owns the selected connection lifetime for both the preflight and
	// normal run paths; lower-level run helpers do not close it. Declared here
	// because a GPU project resolves its target earlier than the rest do — see
	// below.
	target := opts.watchTarget
	ownedTarget := target == nil
	defer func() {
		if ownedTarget && target != nil && target.Agent != nil {
			target.Agent.Close()
		}
	}()

	// For docker-type projects, resolve which build file to use before
	// connecting to the target — so the picker shows regardless of whether
	// we end up on the agent path or a provider path (Docker, etc.).
	if projectType == "docker" && opts.dockerfile == "" {
		// Exception: a Stagefile with a cuda: stage compiles against the GPU
		// architecture of the device it is being deployed to, so for those
		// projects the device has to come first. Only they pay for it, and
		// only when --gpu-arch didn't already answer the question. Step 2
		// below reuses whatever is resolved here.
		target, err = resolveStagefileGPUTarget(ctx, cwd, target, opts)
		if err != nil {
			return err
		}
		resolved, err := resolveDockerfile(cwd, opts.dockerfile, !opts.yes && isInteractiveTerminal(),
			resolveGPUArch(ctx, cwd, opts.gpuArch, agentConn(target)),
			debugStagefileOptions(opts.debug)...)
		if err != nil {
			return err
		}
		opts.dockerfile = resolved
	}

	cfgPath := filepath.Join(cwd, "wendy.json")
	cfgMissing, err := appConfigFileMissing(cfgPath)
	if err != nil {
		return fmt.Errorf("checking wendy.json: %w", err)
	}

	// If wendy.json is missing, resolve the target before prompting to create
	// one. That lets Mac beta targets reject container-only project shapes with
	// the real project/target mismatch instead of first asking about config.
	if cfgMissing {
		if target == nil {
			target, err = resolveRunTarget(ctx, runResolveOptions(opts)...)
			if err != nil {
				return err
			}
		}
		if err := preflightMissingAppConfigForMacTarget(ctx, target, projectType); err != nil {
			return err
		}
	}

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

	// Debug mode requires host networking for remote debugger access.
	if opts.debug {
		// Fail fast, before any build/deploy work starts, when this is a
		// Stagefile Python project whose pip requirements don't include
		// debugpy. See debugRequiresDebugpy.
		if err := debugRequiresDebugpy(cwd, appCfg); err != nil {
			return err
		}
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

	mark("cli setup (project/dockerfile/config)")

	// Step 2: Resolve the target device.
	if target == nil {
		target, err = resolveRunTarget(ctx, runResolveOptions(opts)...)
		if err != nil {
			return err
		}
	}
	mark("resolve + connect device")

	// Build-file selection happens before wendy.json is loaded so device
	// discovery and picker behavior stay cheap. Once both config and target are
	// known, recompile a selected Stagefile with framework-derived runtime
	// packages. This is idempotent and touches no project source; it only updates
	// Wendy's generated Dockerfile/lock artifacts.
	if sfOpts := ros2StagefileOptions(appCfg.ResolveROS2ConfigForService(opts.service)); len(sfOpts) > 0 {
		if source, ok := stagefileSourceForGenerated(opts.dockerfile); ok {
			if _, statErr := os.Stat(filepath.Join(cwd, source)); statErr == nil {
				sfOpts = append(debugStagefileOptions(opts.debug), sfOpts...)
				resolved, compileErr := prepareDockerBuildFile(cwd, source,
					resolveGPUArch(ctx, cwd, opts.gpuArch, agentConn(target)), sfOpts...)
				if compileErr != nil {
					return compileErr
				}
				opts.dockerfile = resolved
			}
		}
	}

	// Provider-based run path.
	if target.External != nil && target.Provider != nil {
		if err := rejectUnsupportedBuildHostProject(opts.buildHost, "provider targets"); err != nil {
			return err
		}
		return runWithProvider(ctx, target.Provider, *target.External, cwd, appCfg.AppID, appCfg.Entitlements, opts)
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
	return runWithAgent(ctx, target.Agent, cwd, appCfg, opts)
}

func appConfigFileMissing(cfgPath string) (bool, error) {
	if _, err := os.Stat(cfgPath); err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

// agentVersionForRun reuses the liveness/version probe already performed while
// establishing a direct-agent connection. Cloud and test connections that do
// not probe during dial still perform the RPC here, then make the result
// available to later run phases on this connection.
func agentVersionForRun(ctx context.Context, conn *grpcclient.AgentConnection) (*agentpb.GetAgentVersionResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if resp, ok := conn.CachedAgentVersion(); ok {
		return resp, nil
	}
	resp, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		return nil, err
	}
	conn.CacheAgentVersion(resp)
	return resp, nil
}

func preflightMissingAppConfigForMacTarget(ctx context.Context, target *SelectedDevice, projectType string) error {
	if target == nil || target.Agent == nil {
		return nil
	}
	versionResp, err := agentVersionForRun(ctx, target.Agent)
	if err != nil {
		return fmt.Errorf("querying device version for Mac target preflight: %w", err)
	}
	agentOS := versionResp.GetOs()
	architecture := versionResp.GetCpuArchitecture()
	if architecture == "" {
		architecture = "arm64"
	}
	platform := resolveAgentPlatform("", agentOS, architecture)
	if strings.EqualFold(agentOS, appconfig.PlatformDarwin) {
		return rejectUnsupportedMacRunProject(projectType, platform)
	}
	return nil
}

// runComposeCommand handles the full device-selection + execution flow for
// docker-compose projects, bypassing the wendy.json requirement.
func runComposeCommand(ctx context.Context, cwd string, opts runOptions) error {
	target := opts.watchTarget
	ownedTarget := target == nil
	if target == nil {
		var err error
		target, err = resolveRunTarget(ctx, runResolveOptions(opts)...)
		if err != nil {
			return err
		}
	}

	if target.External != nil && target.Provider != nil {
		if opts.builder != "" {
			return fmt.Errorf("--builder is only used when --device selects a WendyOS device; use --device docker for local Compose runs")
		}
		// External providers handle local compose support themselves.
		// Compose projects have no wendy.json, so entitlements are nil.
		return runWithProvider(ctx, target.Provider, *target.External, cwd, filepath.Base(cwd), nil, opts)
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

	if ownedTarget {
		defer target.Agent.Close()
	}
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
	appConfigData, err := json.Marshal(appCfg)
	if err != nil {
		return fmt.Errorf("marshaling app config: %w", err)
	}
	createReq.AppConfig = appConfigData

	if appCfg.Brewfile != "" {
		cliLogln("Will apply Brewfile on target Mac.")
	}

	if opts.deploy {
		if _, err := conn.ContainerService.CreateContainer(ctx, createReq); err != nil {
			return macOSNativeCreateContainerError(err, appCfg)
		}
		if appCfg.Brewfile != "" {
			cliLogln("Brewfile applied.")
		}
		cliLogln("Container %s created (not started).", containerDisplayName(appCfg))
		return nil
	}

	if _, err := conn.ContainerService.CreateContainer(ctx, createReq); err != nil {
		return macOSNativeCreateContainerError(err, appCfg)
	}
	if appCfg.Brewfile != "" {
		cliLogln("Brewfile applied.")
	}
	cliLogln("Container %s created.", containerDisplayName(appCfg))

	if opts.detach {
		stream, err := conn.ContainerService.StartContainer(contextWithPostStartAgentHook(ctx, appCfg), &agentpb.StartContainerRequest{
			AppName: appCfg.ContainerName(),
		})
		if err != nil {
			return fmt.Errorf("starting container: %w", err)
		}
		if _, err := stream.Recv(); err != nil && err != io.EOF {
			return fmt.Errorf("waiting for container start: %w", err)
		}
		cliLogln("Application %s running in detached mode.", containerDisplayName(appCfg))
		return nil
	}
	if opts.isWatch() {
		// Watch tails the app through its session-level telemetry subscription.
		// This RPC is used only as the authoritative start acknowledgement, so
		// closing it afterwards leaves the native task running on the device.
		startCtx, startCancel := context.WithCancel(ctx)
		defer startCancel()
		stream, err := conn.ContainerService.StartContainer(contextWithPostStartAgentHook(startCtx, appCfg), &agentpb.StartContainerRequest{
			AppName: appCfg.ContainerName(),
		})
		if err != nil {
			return fmt.Errorf("starting container: %w", err)
		}
		if err := awaitStarted(stream); err != nil {
			return fmt.Errorf("waiting for container start: %w", err)
		}
		startCancel()
		cliLogln("Application %s started.", containerDisplayName(appCfg))
		cmd := runPostStartIfReady(ctx, opts.watchState.hookContext(ctx), conn, appCfg, opts)
		opts.watchState.reapCommand(cmd)
		return nil
	}

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	logSub := startRunLogSubscription(runCtx, conn, appCfg.AppID, os.Stdout, runLogStreamWarning)
	defer logSub.stop()

	stream, err := conn.ContainerService.StartContainer(contextWithPostStartAgentHook(runCtx, appCfg), &agentpb.StartContainerRequest{
		AppName: appCfg.ContainerName(),
	})
	if err != nil {
		return fmt.Errorf("starting container: %w", err)
	}

	cliLogln("Application %s started.", containerDisplayName(appCfg))

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		cliLogln("\nStopping container...")
		_, _ = conn.ContainerService.StopContainer(context.Background(), &agentpb.StopContainerRequest{
			AppName: appCfg.ContainerName(),
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

	cliLogln("\nApplication %s stopped.", containerDisplayName(appCfg))
	return nil
}

func macOSNativeCreateContainerError(err error, appCfg *appconfig.AppConfig) error {
	if appCfg != nil && appCfg.Brewfile != "" {
		return fmt.Errorf("creating container (including brew bundle): %w", err)
	}
	return fmt.Errorf("creating container: %w", err)
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
	versionResp, err := agentVersionForRun(ctx, conn)
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

	registryAddr, swiftUseMTLS, proxyCleanup, proxyDialErr, err := resolveRegistryForSwiftAgent(ctx, conn, regPort)
	if err != nil {
		return err
	}
	defer proxyCleanup()

	cliLogln("Building Swift container image for %s (%s)...", tui.App(product), tui.Value(architecture))
	if err := buildSwiftContainerImage(ctx, cwd, product, registryAddr, architecture, swiftUseMTLS, opts.debug, &dimWriter{}, os.Stderr); err != nil {
		// A Mac agent only runs a container registry when it found a Linux
		// container backend (Docker, OrbStack, or Apple `container`) at startup;
		// otherwise the registry proxy above never reaches anything and the push
		// dies with a bare "connection refused" that reads like a CLI bug. A real
		// WendyOS/Linux device always ships its container runtime as part of the
		// OS, so a refused connection there is an actual fault, not a missing
		// optional dependency — only reinterpret the error for darwin agents.
		if strings.EqualFold(agentOS, appconfig.PlatformDarwin) {
			if dialErr := proxyDialErr(); isDialRefused(dialErr) {
				return fmt.Errorf(
					"the Mac agent at %s isn't running a container registry (%v); "+
						"install Docker, OrbStack, or Apple's `container` CLI on the agent to run "+
						"Linux/WendyOS apps there, or set \"platform\": \"darwin\" in wendy.json to run "+
						"this app natively on the Mac instead",
					conn.Host, dialErr,
				)
			}
		}
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

	// wendy.json run.args are the default arguments; explicit `wendy run -- ...`
	// args take precedence. Current agents append UserArgs to the image's own
	// entrypoint, but agents older than this change replace it, so keep passing
	// the product binary as Cmd — swift-container-plugin images use /<product>
	// as their entrypoint, so both agent versions produce the same argv.
	userArgs := opts.userArgs
	if len(userArgs) == 0 && appCfg.Run != nil {
		userArgs = appCfg.Run.Args
	}
	var cmd string
	if len(userArgs) > 0 {
		cmd = "/" + product
	}

	createReq := &agentpb.CreateContainerRequest{
		ImageName:     deviceImage,
		AppName:       appCfg.AppID,
		AppConfig:     appConfigData,
		RestartPolicy: restartPolicy,
		Cmd:           cmd,
		UserArgs:      userArgs,
		// Service env from wendy.json (mesh: MESH_PEERS etc.) plus any fleet-injected
		// env (discovery peers). Fleet env is appended last so it wins on key clash.
		Env: append(resolveServiceEnv(appCfg), opts.env...),
	}

	return startAndStreamContainer(ctx, conn, appCfg, createReq, opts)
}

// runMacOSSwiftPMWithAgent builds a Swift package locally via `swift build`,
// syncs the binary (and optional sandbox.sb / wendy.json files) to the device
// via SyncFiles gRPC, and creates/starts the container.
func runMacOSSwiftPMWithAgent(ctx context.Context, conn *grpcclient.AgentConnection, cwd string, appCfg *appconfig.AppConfig, opts runOptions) error {
	// Verify CPU architecture matches.
	versionResp, err := agentVersionForRun(ctx, conn)
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

	product, err := swifttoolchain.FindSwiftProductWithActiveSwiftOptions(cwd, opts.product, !opts.yes && isInteractiveTerminal())
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

	return appendNativeBrewfileSyncEntry(entries, cwd, appCfg)
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
		if len(stagefile.SourceNames(dir)) > 0 {
			return "docker", nil
		}
		// Accept Dockerfile/Containerfile and dot/hyphen variants.
		entries, readErr := os.ReadDir(dir)
		if readErr != nil {
			for _, base := range []string{"Dockerfile", "Containerfile"} {
				marker := filepath.Join(dir, base)
				if _, err := os.Stat(marker); err == nil {
					return "docker", nil
				} else if !os.IsNotExist(err) {
					return "", fmt.Errorf("checking for %s: %w", marker, err)
				}
			}
		} else {
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				name := e.Name()
				if isContainerBuildFileName(name) {
					return "docker", nil
				}
			}
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
func runWithProvider(ctx context.Context, p providers.DeviceProvider, device models.ExternalDevice, projectPath, product string, entitlements []appconfig.Entitlement, opts runOptions) error {
	if opts.builder != "" {
		return fmt.Errorf("--builder is only used when --device selects a WendyOS device; use --device docker or --device apple-container for local provider runs")
	}
	projectType, err := resolveRunProjectType(projectPath, opts.buildType)
	if err != nil {
		return err
	}
	if err := ensureProviderSupportsProjectType(p, projectType, projectPath); err != nil {
		return err
	}

	// Resolve Swift product name from Package.swift.
	if projectType == "swift" {
		if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
			return fmt.Errorf("`wendy run` for Swift packages is not supported on %s; provide a Dockerfile or Containerfile", runtime.GOOS)
		}
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
		// A container build file exists — try to use Swift product name if Package.swift is also present.
		if swiftProduct, err := swifttoolchain.FindSwiftProductWithOptions(projectPath, opts.product, false); err == nil {
			product = swiftProduct
		}
	}

	var app *providers.BuiltApp

	// Xcode projects cannot be deployed via provider (requires darwin + file sync).
	if projectType == "xcode" {
		return fmt.Errorf("Xcode projects are not supported by the %s provider; use 'wendy run' with a macOS target instead", p.DisplayName())
	}

	// Swift projects without a container build file: cross-compile on the host and
	// build a Docker image, bypassing the provider's normal Build method.
	if projectType == "swift" {
		if ib, ok := p.(providers.ImageBuilder); ok {
			cliLogln("Building Swift project for %s...", p.DisplayName())
			imageName, err := buildSwiftDockerImage(ctx, projectPath, product, runtime.GOARCH, swiftBuildConfig(opts.debug), &dimWriter{}, os.Stderr)
			if err != nil {
				return fmt.Errorf("building Swift Docker image: %w", err)
			}
			app = ib.BuildFromImage(device, product, imageName)
		}
	}

	if app == nil {
		cliLogln("Building with %s provider...", p.DisplayName())
		var err error
		app, err = providerBuild(ctx, p, device, projectPath, projectType, product, opts)
		if err != nil {
			app, err = offerLiteReinstallAndRebuild(ctx, p, device, projectPath, projectType, product, opts, err)
			if err != nil {
				return err
			}
		}
	}

	app.Entitlements = entitlements
	cliLogln("Build completed.")

	if opts.deploy {
		cliLogln("Application %s built but not started (--deploy).", tui.App(product))
		return nil
	}

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	output := make(chan providers.RunOutput, 64)

	// The watch session owns Ctrl-C and leaves the application running. Provider
	// watch is detached, so each deploy cycle must not install a signal handler.
	if !opts.isWatch() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt)
		defer signal.Stop(sigCh)
		go func() {
			select {
			case <-sigCh:
			case <-runCtx.Done():
				return
			}
			cliLogln("\nStopping application...")
			p.Stop(context.Background(), app)
			runCancel()
		}()
	}

	// Start the application in a goroutine.
	errCh := make(chan error, 1)
	go func() {
		errCh <- p.Run(runCtx, app, opts.detach, output)
	}()

	// Consume output.
	for out := range output {
		switch out.Type {
		case providers.RunOutputStarted:
			cliLogln("Application %s started.", tui.App(product))
			if opts.detach {
				cliLogln("Application %s running in detached mode.", tui.App(product))
				return nil
			}
		case providers.RunOutputStdout:
			os.Stdout.Write(out.Data)
		case providers.RunOutputStderr:
			os.Stderr.Write(out.Data)
		}
	}

	runErr := <-errCh
	cliLogln("\nApplication %s stopped.", tui.App(product))
	if runCtx.Err() != nil {
		return nil // cancelled by signal
	}
	return runErr
}

// providerBuild dispatches a build to the most specific builder interface the
// provider implements.
func providerBuild(ctx context.Context, p providers.DeviceProvider, device models.ExternalDevice, projectPath, projectType, product string, opts runOptions) (*providers.BuiltApp, error) {
	if db, ok := p.(providers.DockerfileBuilder); ok && opts.dockerfile != "" {
		return db.BuildWithDockerfile(ctx, device, projectPath, product, projectType, opts.dockerfile, opts.debug)
	}
	if tb, ok := p.(providers.TypedBuilder); ok {
		return tb.BuildWithType(ctx, device, projectPath, product, projectType, opts.debug)
	}
	return p.Build(ctx, device, projectPath, projectType, product, opts.debug)
}

// shouldOfferLiteReinstall reports whether a failed provider build should turn
// into an interactive offer to install (or reinstall) Wendy Lite, and returns
// the unsupported-requirements error when it should. This fires both when
// existing firmware rejected the app's requirements and when the device has
// no Wendy Lite firmware at all yet (see MicroWendyProvider.GetDeviceInfo's
// needsInstall short-circuit) — either way GetDeviceInfo returns the same
// error type. The offer only makes sense when reflashing can happen over the
// same USB cable, and only in a session where a human can answer.
func shouldOfferLiteReinstall(buildErr error, device models.ExternalDevice, interactive bool) (*providers.AppRequirementsUnsupportedError, bool) {
	var unsupported *providers.AppRequirementsUnsupportedError
	if !errors.As(buildErr, &unsupported) {
		return nil, false
	}
	if device.ConnectionType() != "USB" || !interactive {
		return nil, false
	}
	return unsupported, true
}

// deviceNeedsInstall reports whether device is a serial ESP32 board with no
// Wendy Lite firmware installed yet, as opposed to one whose existing
// firmware simply doesn't support what the app requires.
func deviceNeedsInstall(device models.ExternalDevice) bool {
	return device.ConnectionInfo["needsInstall"] == "true"
}

var (
	serialPortFreeBudget = 6 * time.Second // probe worst case: 3s handshake + 3s identity
	serialPortFreePoll   = 200 * time.Millisecond
)

// waitForSerialPortFree reports whether port is free of an advisory lock, waiting
// up to serialPortFreeBudget for a holder to release it. Only ErrLocked is waited
// on; other Acquire failures aren't contention and the caller's own open reports
// them better. Always true on Windows, where Acquire is a no-op.
func waitForSerialPortFree(ctx context.Context, port string) bool {
	deadline := time.Now().Add(serialPortFreeBudget)
	for {
		lock, err := seriallock.Acquire(port)
		if err == nil {
			lock.Release()
			return true
		}
		if !errors.Is(err, seriallock.ErrLocked) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-time.After(serialPortFreePoll):
		case <-ctx.Done():
			return false
		}
	}
}

// offerLiteReinstallAndRebuild handles a provider build that failed because
// the device cannot host the app: either its existing firmware does not
// support the app's requirements (native or WASM apps), or it has no Wendy
// Lite firmware installed at all. Either way, it offers to (re)install Wendy
// Lite (the classic `wendy os install` flow) and, once the device is back,
// builds again. In every other case — including --yes, which must not
// silently accept a destructive reinstall — it returns the build error
// unchanged.
func offerLiteReinstallAndRebuild(ctx context.Context, p providers.DeviceProvider, device models.ExternalDevice, projectPath, projectType, product string, opts runOptions, buildErr error) (*providers.BuiltApp, error) {
	wrapped := fmt.Errorf("provider build: %w", buildErr)
	unsupported, ok := shouldOfferLiteReinstall(buildErr, device, !opts.yes && isInteractiveTerminal())
	if !ok {
		return nil, wrapped
	}

	freshInstall := deviceNeedsInstall(device)
	var confirmPrompt string
	if freshInstall {
		cliLogln("Device %s has no Wendy Lite firmware installed.", tui.Value(device.DisplayName))
		confirmPrompt = "Would you like to install Wendy Lite on it now?"
	} else {
		cliLogln("Device %s does not support %s.", tui.Value(device.DisplayName), unsupported.Missing)
		confirmPrompt = "Would you like to reinstall it with a different version of Wendy Lite? This will erase all data on the device."
	}
	confirmed, err := tui.Confirm(confirmPrompt)
	if err != nil {
		if errors.Is(err, tui.ErrCancelled) {
			return nil, ErrUserCancelled
		}
		return nil, err
	}
	if !confirmed {
		return nil, wrapped
	}

	board, err := pickWendyLiteBoard("", false)
	if err != nil {
		if errors.Is(err, ErrUserCancelled) {
			return nil, wrapped
		}
		return nil, err
	}

	if freshInstall {
		// Improbable, but the picker's scanner may still be probing this port,
		// so wait until the port is free. The lock is released immediately:
		// installESP32Firmware takes its own, and flock is per-descriptor, so
		// holding ours would block it.
		port := device.ConnectionInfo["serialPort"]
		if !waitForSerialPortFree(ctx, port) {
			cliNotice("Serial port %s is still in use; attempting the install anyway.", port)
		}
	}

	serialDevice := discovery.SerialPortInfo{
		Port:      device.ConnectionInfo["serialPort"],
		Transport: discovery.SerialTransportNativeUSB,
	}
	if err := installESP32Firmware(ctx, false, board, serialDevice, wifiCLIOptions{}, "", preEnrollOptions{mode: preEnrollAuto}); err != nil {
		return nil, fmt.Errorf("reinstalling Wendy Lite: %w", err)
	}

	// The device now has firmware where it had none (or different firmware),
	// so the needsInstall short-circuit in GetDeviceInfo must not fire again —
	// clear it before probing/building against the freshly flashed board.
	if freshInstall {
		delete(device.ConnectionInfo, "needsInstall")
	}

	cliLogln("Waiting for the device to restart...")

	// Give the device time to start booting and be discovered by the OS before
	// probing it.
	select {
	case <-time.After(5 * time.Second):
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	if err := waitForDeviceReady(ctx, p, device, 30*time.Second); err != nil {
		return nil, fmt.Errorf("device did not come back after reinstall: %w", err)
	}

	cliLogln("Building with %s provider...", p.DisplayName())
	app, err := providerBuild(ctx, p, device, projectPath, projectType, product, opts)
	if err != nil {
		return nil, fmt.Errorf("provider build: %w", err)
	}
	return app, nil
}

// waitForDeviceReady polls the provider until the device answers again after
// a reboot, or the timeout elapses.
func waitForDeviceReady(ctx context.Context, p providers.DeviceProvider, device models.ExternalDevice, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if _, err := p.GetDeviceInfo(ctx, device); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return lastErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// runWithAgent is the existing gRPC agent pipeline.
func runWithAgent(ctx context.Context, conn *grpcclient.AgentConnection, cwd string, appCfg *appconfig.AppConfig, opts runOptions) error {
	mark := phaseTimer()
	if opts.isWatch() && !opts.detach {
		if err := opts.watchState.ensureLogStream(conn, appCfg.AppID); err != nil {
			return err
		}
	}
	// Multi-service path: when wendy.json has a services map, build all images
	// in parallel and manage the app group lifecycle.
	if len(appCfg.Services) > 0 {
		if err := rejectUnsupportedBuildHostProject(opts.buildHost, "multi-service projects"); err != nil {
			return err
		}
		return runMultiServiceWithAgent(ctx, conn, cwd, appCfg, opts)
	}

	if err := prepareVMAppPorts(ctx, conn, appCfg); err != nil {
		return err
	}
	// Detect project type and ensure a build file exists when needed.
	projectType, err := resolveRunProjectType(cwd, opts.buildType)
	if err != nil {
		return err
	}

	// Resolve the target platform. Query the agent for its OS and architecture,
	// then determine the effective platform from wendy.json or defaults.
	versionResp, err := agentVersionForRun(ctx, conn)
	if err != nil {
		return fmt.Errorf("querying device version: %w", err)
	}
	printRunDiskUsageWarning(versionResp)
	mark("agent version metadata (in runWithAgent)")
	agentOS := versionResp.GetOs()
	architecture := versionResp.GetCpuArchitecture()
	if architecture == "" {
		architecture = "arm64"
	}

	platform := resolveAgentPlatform(appCfg.Platform, agentOS, architecture)
	if strings.EqualFold(agentOS, appconfig.PlatformDarwin) {
		if err := rejectUnsupportedMacRunProject(projectType, platform); err != nil {
			return err
		}
	}

	// Xcode projects: always use the local-build + file-sync path (darwin only).
	if projectType == "xcode" {
		if err := rejectUnsupportedBuildHostProject(opts.buildHost, "Xcode projects"); err != nil {
			return err
		}
		if platformOS(platform) == "darwin" {
			return runMacOSXcodeWithAgent(ctx, conn, cwd, appCfg, opts)
		}
		return fmt.Errorf("Xcode projects require a darwin target (got %s)", platform)
	}

	// Swift projects use a native darwin path for macOS targets and
	// swift-container-plugin for Linux targets when --build-type=swift
	// explicitly selects that path or when no Dockerfile/Containerfile is present.
	// Both paths shell out to a host Swift toolchain:
	//   - darwin target: `swift build` on the host. Requires a darwin host —
	//     Linux's swift toolchain cannot cross-compile to macOS.
	//   - linux target: swift-container-plugin via `swift package`. Requires
	//     a darwin or linux host — swift-container-plugin does not yet ship
	//     for Windows.
	// On a Windows host with a Dockerfile/Containerfile the docker buildx path below
	// handles the build, so the gates only trip when the host swift path
	// would actually be taken.
	if projectType == "swift" {
		targetIsDarwin := platformOS(platform) == "darwin"
		explicitSwift := normalizeBuildType(opts.buildType) == "swift"
		// Read-only existence probe: resolveDockerfile would compile a
		// Stagefile (registry resolution, lockfile write) or write an
		// auto-fixed Dockerfile.generated just to answer a yes/no question
		// whose result is discarded on the host-swift path.
		needsHostSwift := explicitSwift || !hasContainerBuildFile(cwd)

		if needsHostSwift {
			if err := rejectUnsupportedBuildHostProject(opts.buildHost, "native Swift projects"); err != nil {
				return err
			}
			if targetIsDarwin && runtime.GOOS != "darwin" {
				return fmt.Errorf("`wendy run` for Swift packages targeting darwin requires a darwin host (got %s); provide a Dockerfile or Containerfile to build a Linux image instead", runtime.GOOS)
			}
			if !targetIsDarwin && runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
				return fmt.Errorf("`wendy run` for Swift packages is not supported on %s; provide a Dockerfile or Containerfile", runtime.GOOS)
			}
			if targetIsDarwin {
				return runMacOSSwiftPMWithAgent(ctx, conn, cwd, appCfg, opts)
			}
			return runSwiftWithAgent(ctx, conn, cwd, appCfg, opts)
		}
	}

	switch projectType {
	case "docker":
		// Dockerfile/Containerfile already exists.
	case "compose":
		if err := rejectUnsupportedBuildHostProject(opts.buildHost, "Compose projects"); err != nil {
			return err
		}
		return runComposeWithAgent(ctx, conn, cwd, opts)
	case "python":
		if _, err := os.Stat(filepath.Join(cwd, "Dockerfile")); os.IsNotExist(err) {
			cliLogln("No Dockerfile found. Generating one for Python project...")
			if _, genErr := generatePythonDockerfile(cwd, opts.debug); genErr != nil {
				return fmt.Errorf("generating Dockerfile: %w", genErr)
			}
			cliLogln("Generated Dockerfile.")
		} else if opts.debug {
			cliLogln("Note: --debug requires debugpy in the container image. Ensure your Dockerfile installs debugpy (e.g. RUN pip install debugpy).")
		}
	case "swift":
		if normalized, _ := normalizeImageBuilder(opts.builder); normalized == imageBuilderAppleContainer {
			return fmt.Errorf("Apple Container builder is only supported for Dockerfile/Containerfile builds; provide a build file or omit --builder")
		}
		// A container build file exists; use the image build path.
	default:
		return fmt.Errorf("unable to detect project type; ensure a Dockerfile/Containerfile, requirements.txt, or Package.swift is present")
	}

	deviceType := versionResp.GetDeviceType()
	buildArgs := map[string]string{
		"WENDY_PLATFORM": wendyPlatform(deviceType),
		"WENDY_DEBUG":    fmt.Sprintf("%t", opts.debug),
	}
	// Only set WENDY_DEVICE_TYPE / GPU args when the agent reports them so
	// Dockerfiles can apply their own defaults on older agents; device-reported
	// values that fail build-arg validation are skipped rather than fatal.
	applyDeviceBuildArgHints(buildArgs, versionResp)

	// wendy.json env plus --env and fleet-injected env, appended last so they win
	// on key clash. Feeds the remote-build path below, the fingerprint, and
	// whichever local deploy path runs.
	deployEnv := append(resolveServiceEnv(appCfg), opts.env...)

	// Remote build: hand the build to another WendyOS device, which pushes the
	// finished image straight into this device's registry over the mesh. Placed
	// ahead of every local path because those exist to optimise a local build
	// that is not going to happen.
	if opts.buildHost != "" {
		return runRemoteBuild(ctx, conn, opts.buildHost, cwd, appCfg, platform, opts.dockerfile, buildArgs, deployEnv, opts)
	}

	// The Mac agent runs Linux containers via a CLI runtime with no chunk-diff
	// (CDC) support, so every fast-deploy attempt just probes, fails, and falls
	// back to a registry push — wasted round trips. Skip both fast-deploy paths
	// entirely for darwin agents and go straight to the registry push below.
	isDarwinAgent := strings.EqualFold(agentOS, appconfig.PlatformDarwin)

	// Detached fast path: when nothing that affects the image has changed since
	// the last successful deploy to this device, skip the build entirely and
	// just ensure the existing container is running. Best-effort — a missing or
	// mismatched fingerprint, a missing app, or any RPC error falls through to
	// the normal deploy below, so it can never deploy stale code.
	deviceKey := deviceFingerprintKey(versionResp)
	inputHash, hashErr := computeBuildInputHash(cwd, opts.dockerfile, platform, buildArgs, deployEnv)
	if hashErr == nil {
		var basesPinned bool
		basesPinned, hashErr = dockerfileBasesContentPinned(cwd, opts.dockerfile)
		if hashErr == nil && !basesPinned {
			hashErr = fmt.Errorf("persistent build skip requires digest-pinned base images")
		}
	}
	desiredHash := ""
	if hashErr == nil {
		desiredHash, hashErr = computeDeployDesiredHash(inputHash, appCfg, opts.userArgs, deployEnv, resolveRestartPolicy(opts))
	}
	if !isDarwinAgent && opts.detach && !opts.deploy && hashErr == nil {
		if done, _ := tryDeployFastPath(ctx, conn, appCfg, deviceKey, desiredHash, opts); done {
			mark("fast-path (skipped build)")
			return nil
		}
	}

	// A build will run below (the no-build fast path returned above), so make
	// sure the Apple Container system is up when --builder apple-container is
	// explicit. This covers both the chunk-diff and the registry-push build.
	if err := ensureAppleContainerSystemForBuilder(ctx, opts.builder, opts.yes); err != nil {
		return err
	}

	chunkDiffWillRun := !isDarwinAgent && !opts.deploy && opts.chunking != chunkingOff

	// The daemon check prompts on macOS. Run it here: once the build progress UI
	// owns the terminal it repaints over any prompt, so the CLI waits on input the
	// user cannot see.
	// An unknown builder is left for the build below to report.
	if chunkDiffWillRun {
		if b, err := resolveOCIExportBuilder(opts.builder); err == nil && b == imageBuilderDocker {
			if err := ensureDockerDaemon(ctx); err != nil {
				return err
			}
		}
	}

	// The fast chunk-diff (CDC) deploy path handles attached (default) and
	// detached (--detach) runs. Deploy-only (--deploy) is excluded because it
	// must create the container WITHOUT starting it, whereas RunContainer always
	// starts; that mode stays on the registry path via startAndStreamContainer.
	//
	// --chunking gates this path: "off" skips it entirely (registry push only),
	// while "force" uses it with no registry-push fallback on failure.
	var ociHint *ociReuseHint
	if chunkDiffWillRun {
		// stats is filled by deployByChunkDiff as soon as it has read the built
		// image's layers, even on a later failure, so the fallback branch below
		// can size the registry push it's about to fall back to (WDY-2432).
		var stats chunkDeployStats
		diffIDs, hint, err := deployByChunkDiff(ctx, conn, cwd, appCfg, platform, opts.dockerfile, buildArgs, deployEnv, opts, &stats)
		ociHint = hint
		if err == nil {
			if hashErr == nil {
				// Record the layer diff IDs we deployed so the next run's fast path
				// can verify the device still holds this content before skipping the
				// build (WDY-1824).
				saveDeployFingerprint(appCfg.AppID, deviceKey, deployFingerprint{InputHash: desiredHash, AppVersion: appCfg.Version, LayerDiffIDs: diffIDs})
			}
			return nil
		} else if isChunkDeployCancellation(ctx, err) {
			// The deploy was cancelled — either the context (e.g. `wendy watch`
			// superseded it with a newer change) or the user backing out of the
			// interactive chunk-push progress bar (ErrUserCancelled; ctx itself
			// is NOT cancelled there). Don't fall back to a full registry push,
			// which is often a BIGGER upload than the one just cancelled — just
			// surface the cancellation.
			return err
		} else if opts.chunking == chunkingForce {
			// --chunking=force opts out of the registry-push fallback so the
			// failure is surfaced instead of silently masked by a slower path.
			return fmt.Errorf("chunk-diff deploy failed and --chunking=force disables the registry-push fallback: %w", err)
		} else if isImageBuildFailure(err) {
			// The image build itself failed (e.g. a Dockerfile/build-command
			// error). The registry-push fallback rebuilds the same image from the
			// same Dockerfile, so it would fail identically — and can even mask the
			// real error behind an unrelated builder-setup failure. Surface the
			// actionable build error directly instead of falling back. (#1166)
			return err
		} else if shouldUseBuildkitOnDevice() {
			// On-device (inside the agent container: WENDY_AGENT_SOCKET set, no
			// Docker), the registry-push fallback below cannot run — it shells out
			// to the Docker CLI, which is absent. Chunk-diff over the agent socket
			// is the only supported on-device deploy path, so surface ITS real
			// error instead of masking it behind a guaranteed "docker CLI is not on
			// PATH" failure from the fallback.
			return fmt.Errorf("on-device deploy failed and no Docker fallback is possible inside the container; the chunk-diff error was: %w", err)
		} else {
			// Surface the chunk-diff error instead of dropping it — it used to be
			// silently discarded here, leaving no trail for why a deploy suddenly
			// fell back to the slower path.
			cliNotice("%s", formatRegistryFallbackNotice(err, stats.imageBytes))
			if registryFallbackPlan(stats.imageBytes, isInteractiveTerminal(), opts.yes) == fallbackConfirm {
				if !confirmFn("Continue with the full registry push?") {
					return fmt.Errorf("registry-push fallback declined; pass --chunking=off to skip chunk-diff and go straight to a registry push next time, or resolve the chunk-diff failure: %w", err)
				}
			}
		}
	}

	// Verify auth certs are available if the device's registry requires mTLS.
	if err := requireRegistryAuth(ctx, conn); err != nil {
		return err
	}

	// Build and push the Docker image directly to the device's registry.
	regPort := registryPort(agentOS)
	repo := strings.ToLower(appCfg.AppID)

	// The chunk-diff attempt above may have already built this exact image
	// (same Dockerfile, build-args, and platform) to a local OCI layout
	// directory before failing for a reason unrelated to the build itself
	// (e.g. the device not supporting chunk-diff). Reuse that content — a
	// direct registry push, no buildx involved — instead of paying for a
	// second full build of identical content. This is what used to show up
	// as a second "Building and pushing image..." buildx run that re-did
	// everything the first build already did, including any non-cacheable
	// steps (e.g. a Swift compile) that BuildKit's own layer cache can't
	// carry across the two builds' separate builder instances. Purely an
	// optimization: any failure here (stale layout, registry unreachable,
	// unsupported builder combination, ...) falls straight through to the
	// normal rebuild below exactly as if this block were absent.
	pushed := false
	if ociHint != nil && registryPushWouldUseDocker(opts.builder) {
		if err := tryPushExistingOCILayout(ctx, conn, regPort, ociHint, repo); err == nil {
			cliSuccess("Reused already-built image for the registry push (skipped a redundant rebuild)")
			pushed = true
		} else if opts.debug {
			cliLogln("Reusing the already-built image failed (%v); rebuilding instead.", err)
		}
	}

	if !pushed {
		// Single-service build: no concurrency, so keep the shared local cache dir
		// (empty cache key) for cross-run cache reuse.
		buildTitle := fmt.Sprintf("Building and pushing image for %s...", tui.Value(platform))
		if err := runBuildWithProgress(ctx, buildTitle, dumpRawUnlessRegistryUnavailable, func(buildCtx context.Context, stream, logw io.Writer) error {
			return buildAndPushImageForAgent(buildCtx, conn, regPort, agentOS, opts.builder, cwd, repo, platform, opts.dockerfile, buildArgs, "", stream, logw)
		}); err != nil {
			if isRegistryUnavailable(err) {
				// Return the friendly error bare (matching the Swift path above) —
				// the "building and pushing image" prefix adds nothing to it.
				return err
			}
			return fmt.Errorf("building and pushing image: %w", err)
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
		Env:           deployEnv,
	}

	return startAndStreamContainer(ctx, conn, appCfg, createReq, opts)
}

// registryPushWouldUseDocker reports whether the registry-push fallback in
// runWithAgent would end up building with the Docker builder — mirroring,
// without any side effects, the precedence buildAndPushImageForAgent applies:
// an explicit --builder wins outright, otherwise the macOS Apple Container
// auto-attempt (shouldAutoAttemptAppleContainerBuilder) is tried before
// Docker. The chunk-diff deploy's reusable OCI layout (ociReuseHint) was only
// ever built with Docker/buildx (chunkExportPlan), so reusing it for the
// fallback is only valid when this also resolves to Docker — otherwise the
// fallback may legitimately want a different builder and reuse must not
// short-circuit that choice.
func registryPushWouldUseDocker(builder string) bool {
	if imageBuilderWasExplicit(builder) {
		normalized, err := normalizeImageBuilder(builder)
		return err == nil && normalized == imageBuilderDocker
	}
	return !shouldAutoAttemptAppleContainerBuilder()
}

// tryPushExistingOCILayout pushes the image hint already points at straight
// to the device's registry, without invoking docker/buildx again. It is the
// registry-push fallback's fast path when the preceding chunk-diff attempt
// already built this exact image (see ociReuseHint's doc comment for why the
// content is guaranteed identical). Any error here should be treated as
// "reuse didn't work" by the caller, which then falls back to the normal
// rebuild — this function never leaves the deploy worse off than skipping it
// entirely would have.
func tryPushExistingOCILayout(ctx context.Context, conn *grpcclient.AgentConnection, regPort int, hint *ociReuseHint, repo string) error {
	// The OCI pusher runs on the host, not inside BuildKit's VM. Resolve a
	// host-reachable address (and terminate device mTLS on a loopback proxy when
	// required) instead of using host.docker.internal, which is only meaningful
	// from inside the builder container.
	registryAddr, useMTLS, cleanup, dialErr, err := resolveRegistryForSwiftAgent(ctx, conn, regPort)
	if err != nil {
		return fmt.Errorf("resolving device registry: %w", err)
	}
	defer cleanup()

	if err := pushOCILayoutToRegistry(ctx, hint.layoutDir, hint.platform, registryAddr, repo, useMTLS); err != nil {
		if dialErr != nil {
			if de := dialErr(); isDialRefused(de) {
				return fmt.Errorf("device registry unreachable: %w", de)
			}
		}
		return err
	}
	return nil
}

// validateEnvFlag checks --env entries are KEY=VALUE with a POSIX-portable
// key, so a typo is reported before a build runs rather than by the agent at
// container create.
func validateEnvFlag(entries []string) error {
	for _, kv := range entries {
		key, _, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("--env %q must be KEY=VALUE", kv)
		}
		if err := appconfig.ValidateEnvKey("--env", key); err != nil {
			return err
		}
	}
	return nil
}

// expandEnvMap resolves one `env` map from wendy.json into expanded key/value
// pairs. Values may reference host environment variables via ${VAR} (or $VAR);
// they are expanded here, on the deploy host, since the agent has no access to
// this shell's environment. An entry whose value expands to empty is dropped so
// the container falls back to its own built-in default rather than receiving an
// empty override.
func expandEnvMap(env map[string]string, into map[string]string) {
	for k, v := range env {
		if expanded := os.Expand(v, os.Getenv); expanded != "" {
			into[k] = expanded
		}
	}
}

// sortedEnvEntries renders expanded env as the sorted "KEY=VALUE" list carried
// by CreateContainerRequest.Env. Sorting keeps requests deterministic; the
// agent re-validates every entry (POSIX key, blocked prefixes) before applying
// it.
func sortedEnvEntries(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// expandServiceEnv resolves the env for one service of a multi-service app:
// the app-level env is the default and the service's own env overrides it key
// by key, matching how a service's resources override the app's.
func expandServiceEnv(appCfg *appconfig.AppConfig, svc *appconfig.ServiceConfig) []string {
	merged := map[string]string{}
	if appCfg != nil {
		expandEnvMap(appCfg.Env, merged)
	}
	if svc != nil {
		expandEnvMap(svc.Env, merged)
	}
	return sortedEnvEntries(merged)
}

// resolveServiceEnv is the whole-app env for deploy paths that build one
// CreateContainerRequest rather than one per service: the app-level env plus
// every service's env merged over it (see multibuild.go for the per-service
// path, which calls expandServiceEnv directly).
func resolveServiceEnv(appCfg *appconfig.AppConfig) []string {
	if appCfg == nil {
		return nil
	}
	merged := map[string]string{}
	expandEnvMap(appCfg.Env, merged)

	// Sort service names so cross-service key collisions resolve deterministically.
	svcNames := make([]string, 0, len(appCfg.Services))
	for name := range appCfg.Services {
		svcNames = append(svcNames, name)
	}
	sort.Strings(svcNames)
	for _, name := range svcNames {
		if svc := appCfg.Services[name]; svc != nil {
			expandEnvMap(svc.Env, merged)
		}
	}
	return sortedEnvEntries(merged)
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
		cliLogln("Container %s created (not started).", containerDisplayName(appCfg))
		return nil
	}

	// Create the container with progress streaming.
	if err := createContainerWithProgress(ctx, conn.ContainerService, createReq); err != nil {
		return err
	}
	cliLogln("Container %s created.", containerDisplayName(appCfg))

	if opts.detach {
		stream, err := conn.ContainerService.StartContainer(contextWithPostStartAgentHook(ctx, appCfg), &agentpb.StartContainerRequest{
			AppName: appCfg.ContainerName(),
		})
		if err != nil {
			return fmt.Errorf("starting container: %w", err)
		}
		if _, err := stream.Recv(); err != nil && err != io.EOF {
			return fmt.Errorf("waiting for container start: %w", err)
		}
		cliLogln("Application %s running in detached mode.", containerDisplayName(appCfg))
		// Detached returns as soon as the container is started — see
		// runPostStartIfReady's doc comment.
		return nil
	}

	if opts.isWatch() {
		// Logs belong to the session-level telemetry follower. This stream exists
		// only to obtain an authoritative Started acknowledgement; canceling it
		// afterwards does not stop the task (the agent deliberately detaches task
		// lifetime from the requesting RPC).
		startCtx, startCancel := context.WithCancel(ctx)
		defer startCancel()
		stream, err := conn.ContainerService.StartContainer(contextWithPostStartAgentHook(startCtx, appCfg), &agentpb.StartContainerRequest{
			AppName: appCfg.ContainerName(),
		})
		if err != nil {
			return fmt.Errorf("starting container: %w", err)
		}
		if err := awaitStarted(stream); err != nil {
			return fmt.Errorf("waiting for container start: %w", err)
		}
		startCancel()
		cliLogln("Application %s started.", containerDisplayName(appCfg))
		cmd := runPostStartIfReady(ctx, opts.watchState.hookContext(ctx), conn, appCfg, opts)
		opts.watchState.reapCommand(cmd)
		return nil
	}

	// Start and stream output using AttachContainer so stdin is forwarded.
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	logSub := startRunLogSubscription(runCtx, conn, appCfg.AppID, os.Stdout, runLogStreamWarning)
	defer logSub.stop()

	outStream, stdinAttempted, err := openContainerStream(runCtx, conn.ContainerService, appCfg.ContainerName(), appCfg)
	if err != nil {
		return err
	}

	cliLogln("Application %s started.", containerDisplayName(appCfg))

	// Set up Ctrl+C handler first so readiness polling is cancellable.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
		case <-runCtx.Done():
			return
		}
		cliLogln("\nStopping container...")
		_, _ = conn.ContainerService.StopContainer(context.Background(), &agentpb.StopContainerRequest{
			AppName: appCfg.ContainerName(),
		})
		runCancel()
	}()

	// Announce + post-start hook, gated on readiness; the hook is tied to runCtx
	// so Ctrl+C kills it.
	var postStartCmd *exec.Cmd
	postStartCmd = runPostStartIfReady(runCtx, runCtx, conn, appCfg, runOptions{})

	gotFirstResponse := false
	// Set when the stream ends on a genuine failure (as opposed to a clean
	// container exit, which arrives as io.EOF, or a user Ctrl+C, which cancels
	// runCtx). Held so the normal stop/cleanup below still runs before we
	// surface the failure and exit non-zero.
	var runErr error
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
					AppName: appCfg.ContainerName(),
				})
				if startErr != nil {
					return fmt.Errorf("starting container: %w", startErr)
				}
				outStream = startStream
				stdinAttempted = false
				continue
			}
			// Any other stream error is a real failure: the container failed to
			// start or exited abnormally (the agent wraps the real cause in the
			// status message), or the stream itself broke (agent crash, network
			// drop, auth). A clean container exit arrives as io.EOF above, so
			// reaching here always means the run did not succeed. Record it, run
			// the normal cleanup, then return it so `wendy run` exits non-zero
			// instead of reporting a false success. Use the status message so the
			// output reads as a container failure, not a CLI crash.
			runErr = fmt.Errorf("container run failed: %s", status.Convert(recvErr).Message())
			break
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
	if runErr != nil {
		return runErr
	}
	cliLogln("\nApplication %s stopped.", containerDisplayName(appCfg))
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
	cliLogln("Waiting for %s to be ready...", tui.Value(addr))

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

func shellCommand() (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd.exe", []string{"/S", "/C"}
	}
	return "sh", []string{"-c"}
}

// expandHookEnv resolves Wendy's documented placeholders in s. Both Unix-style
// (${VAR}, $VAR) and Windows-style (%WENDY_*%) forms are accepted for the
// Wendy-provided placeholders, so the same hook string parses identically in
// sh and cmd.exe. Other ${VAR} forms fall through to os.Getenv; raw %VAR%
// forms for non-Wendy variables are left for cmd.exe to expand natively.
//
// serviceName is "" for single-container apps (and the app-level fallback
// hook), which expands WENDY_SERVICE_NAME to the empty string rather than
// leaving it verbatim — the placeholder simply isn't meaningful there.
func expandHookEnv(s, hostname, appID, serviceName string) string {
	s = strings.ReplaceAll(s, "%WENDY_HOSTNAME%", hostname)
	s = strings.ReplaceAll(s, "%WENDY_APP_ID%", appID)
	s = strings.ReplaceAll(s, "%WENDY_SERVICE_NAME%", serviceName)
	return os.Expand(s, func(key string) string {
		switch key {
		case "WENDY_HOSTNAME":
			return hostname
		case "WENDY_APP_ID":
			return appID
		case "WENDY_SERVICE_NAME":
			return serviceName
		default:
			return os.Getenv(key)
		}
	})
}

// browserOpen is the cross-platform browser opener used by openURL hooks.
// Indirected through a var so tests can swap it out.
var browserOpen = browseropen.Open

// announceReachableURL prints an IP-based URL the developer can open to reach a
// freshly started app. `wendy run` otherwise only surfaces the device's .local
// hostname, which frequently fails to resolve in a browser (see issue #1301);
// this asks the agent for the device's routable IPs and prints a URL built
// from one of them. It is best-effort: it only queries the device when there is
// something to show (a postStart openURL or a readiness TCP port) and stays
// silent on any error or when no reachable address can be determined.
// Returns the device IP the printed URL uses, or "" when nothing was announced.
func announceReachableURL(ctx context.Context, conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig) string {
	var hookURL string
	if appCfg.Hooks != nil && appCfg.Hooks.PostStart != nil {
		hookURL = appCfg.Hooks.PostStart.OpenURL
	}
	readiness := effectiveReadiness(appCfg)
	httpPort, hasHTTPPort := httpEntitlementPort(appCfg.Entitlements)
	hasPort := hasHTTPPort || (readiness != nil && readiness.TCPSocket != nil && readiness.TCPSocket.Port != 0)
	if hookURL == "" && !hasPort {
		return ""
	}

	resp, err := agentVersionForRun(ctx, conn)
	if err != nil {
		return ""
	}
	ip := bestReachableIP(resp.GetNetworkInterfaces())
	if name, err := userVMForConnection(conn); err == nil && name != "" {
		ip = "127.0.0.1"
	}
	url := reachableAppURL(hookURL, appCfg.AppID, appCfg.ServiceName, ip, httpPort, readiness)
	if url == "" {
		return ""
	}
	cliLogln("App reachable at %s", tui.Value(url))
	return ip
}

// resolveHookHost returns the host the developer-side readiness probe and
// postStart hook should target. conn.Host is perfect for LAN connections, but
// a cloud tunnel sets it to the ASSET NAME (cloud_tunnel.go: agentConn.Host =
// asset.GetName()), which does not resolve from this machine — and an IPv6
// literal needs the agent-reported IP too, since it is often an RFC 4941
// temporary (privacy) address that rotates away. In both cases prefer the
// routable IP the agent reports via GetAgentVersion (announceReachableURL).
//
// conn.Reconnect != nil is the cloud marker: it is the sole assignment
// (cloud_tunnel.go, on the connection cloud_tunnel.go builds) for a
// transport where the connection identity can't be re-derived from Host
// alone. conn.Addr can't be used instead — it is empty for NewFromConn
// conns, cloud tunnels included.
//
// ok=false means no usable host exists (a cloud conn with no reported IP):
// the caller must skip host-side probes/hooks with guidance instead of
// dialing a dead asset name.
func resolveHookHost(ctx context.Context, conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig) (host string, ok bool) {
	ip := announceReachableURL(ctx, conn, appCfg)
	isCloud := conn.Reconnect != nil
	if ip != "" && (isCloud || isIPv6Literal(conn.Host)) {
		return ip, true
	}
	if isCloud && ip == "" {
		return "", false
	}
	return conn.Host, true
}

// synthesizedOpenURLHook returns appCfg.Hooks unchanged when the app already
// configures an explicit openURL. Otherwise, when the app declares an `http`
// entitlement, it returns a copied HooksConfig whose postStart opens that port
// automatically while preserving any explicit cli/agent actions.
func synthesizedOpenURLHook(appCfg *appconfig.AppConfig) *appconfig.HooksConfig {
	if appCfg.Hooks != nil && appCfg.Hooks.PostStart != nil && appCfg.Hooks.PostStart.OpenURL != "" {
		return appCfg.Hooks
	}
	port, ok := httpEntitlementPort(appCfg.Entitlements)
	if !ok {
		return appCfg.Hooks
	}
	hooks := &appconfig.HooksConfig{}
	if appCfg.Hooks != nil {
		*hooks = *appCfg.Hooks
	}
	postStart := &appconfig.HookCommand{}
	if hooks.PostStart != nil {
		*postStart = *hooks.PostStart
	}
	postStart.OpenURL = fmt.Sprintf("http://${WENDY_HOSTNAME}:%d", port)
	hooks.PostStart = postStart
	return hooks
}

// runPostStartIfReady waits for readiness, announces the reachable URL, and
// launches host-side postStart actions. It is used by attached runs; detached
// paths return after Started without waiting for readiness. In watch mode the
// actions run once per container after the first successful readiness check.
// A canceled or failed attempt releases that claim for a later deploy.
//
// hookCtx controls the lifetime of a CLI hook child process. The returned
// command must be reaped by the caller; nil means no CLI hook was launched.
//
// ATTACHED RUNS ONLY. Detached deploys never call this. The readiness probe
// waits out the app's own boot, which an attached run can afford because it
// remains to stream logs. An attached watch may call this after multiple
// deploys, but opts' watch lifecycle lease allows the actions to complete only
// once per container in the session. The agent-side hook is unaffected: it
// rides on the RunContainer/StartContainer RPC context and runs on the device.
func runPostStartIfReady(ctx, hookCtx context.Context, conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig, opts runOptions) *exec.Cmd {
	containerName := appCfg.ContainerName()
	if !opts.beginHostLifecycle(containerName) {
		return nil
	}
	completed := false
	defer func() {
		if !completed {
			opts.abandonHostLifecycle(containerName)
		}
	}()

	rp := phaseTimer()
	readiness := effectiveReadiness(appCfg)
	hooks := synthesizedOpenURLHook(appCfg)

	// Nothing to probe or fire: an app with no TCP readiness (explicit or
	// http-entitlement-synthesized) and no postStart hook (explicit or
	// http-entitlement-synthesized) has nothing for this function to do.
	// Returning before resolveHookHost matters specifically for cloud
	// connections: resolveHookHost's isCloud branch would otherwise still run
	// and, since announceReachableURL short-circuits to "" without ever
	// querying the agent when there's no hookURL/port to build a URL from,
	// report "no reported IP" and print a "Skipping postStart hook" notice
	// for a hook that was never configured. Mirrors
	// service_lifecycle.go's serviceHookRunner.runOne guard.
	if readiness == nil && hooks == nil {
		return nil
	}

	// Resolve the host BEFORE probing readiness: for a cloud connection,
	// conn.Host is the tunnel's asset name, which does not resolve from this
	// machine — dialing it always fails, so the postStart hook logic below
	// would never even be reached unless the probe target is swapped too.
	// This also means the "App reachable at ..." line (printed inside
	// resolveHookHost/announceReachableURL) now prints before readiness is
	// confirmed rather than after — acceptable since it is the same URL the
	// user watches for regardless of when the probe finishes.
	hookHost, hostOK := resolveHookHost(ctx, conn, appCfg)
	if !hostOK {
		cliNotice("Skipping postStart hook: no routable device address reported; open the app manually once the device IP is known.")
		return nil
	}

	err := waitForReadiness(ctx, readiness, hookHost)
	rp("  ↳ runcontainer: readiness wait")
	if err != nil {
		if ctx.Err() == nil {
			warnReadiness(ctx, conn, appCfg.AppID, err)
			if appCfg.Hooks != nil && appCfg.Hooks.PostStart != nil &&
				(appCfg.Hooks.PostStart.OpenURL != "" || appCfg.Hooks.PostStart.CLI != "") {
				cliLogln("Skipping postStart hook: %s is not ready.", containerDisplayName(appCfg))
			}
		}
		return nil
	}
	if ctx.Err() != nil {
		return nil
	}
	effectiveCfg := appCfg
	if hooks != appCfg.Hooks {
		clone := *appCfg
		clone.Hooks = hooks
		effectiveCfg = &clone
	}
	if ctx.Err() != nil {
		return nil
	}
	opts.completeHostLifecycle(containerName)
	completed = true
	cmd := startPostStartHook(hookCtx, effectiveCfg, hookHost, appCfg.ServiceName)
	rp("  ↳ runcontainer: announce + postStart hook")
	return cmd
}

// startPostStartHook fires the postStart hook actions for appCfg. serviceName
// is threaded through separately from appCfg (rather than read off
// appCfg.ServiceName internally) so callers building a synthetic/app-level
// config can control it explicitly; existing single-container callers pass
// appCfg.ServiceName, which is "" on true single-container paths.
//
// If openURL is set, it is expanded and opened in the developer's default
// browser via the shared browseropen helper — no shell, no quoting. If cli
// is set, it runs after, expanded for env vars and dispatched through the
// platform shell; the returned *exec.Cmd is the cli child for the caller to
// wait on or kill. Returns nil when no cli command is configured (regardless
// of whether openURL was fired).
func startPostStartHook(ctx context.Context, appCfg *appconfig.AppConfig, hostname, serviceName string) *exec.Cmd {
	if appCfg.Hooks == nil || appCfg.Hooks.PostStart == nil {
		return nil
	}
	hook := appCfg.Hooks.PostStart

	if hook.OpenURL != "" {
		// openURL is a URL by definition, so an IPv6 hostname must be
		// bracketed; the CLI hook below stays raw for shell contexts.
		url := expandHookEnv(hook.OpenURL, urlSafeHost(hostname), appCfg.AppID, serviceName)
		if err := browserOpen(url); err != nil {
			cliLogln("Warning: postStart openURL failed: %v", err)
		} else {
			cliLogln("Hook postStart: opened %s", tui.Path(url))
		}
	}

	if hook.CLI == "" {
		return nil
	}

	expanded := expandHookEnv(hook.CLI, hostname, appCfg.AppID, serviceName)
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
	cliLogln("Hook postStart: %s", tui.Command(expanded))
	return cmd
}

// wendyPlatform maps a WendyOS device type to a platform tier used for
// Dockerfile base stage selection. Adding a new device only requires adding
// a case here; templates need no changes until a new platform tier is introduced.
// Unknown device types fall back to "generic" (CPU-only).
//
// jetson-agx-thor (tegra264 / JetPack 7 / CUDA 13) shares the "nvidia-jetson"
// tier with the Orin boards (tegra234 / JetPack 6 / CUDA 12). The tier only says
// "NVIDIA Jetson"; templates that ship a JetPack-pinned base image should branch
// on the WENDY_JETPACK_MAJOR build arg (a coarse "6"/"7", also injected by
// `wendy run`) — or WENDY_JETPACK_VERSION / WENDY_CUDA_VERSION for finer pins —
// to pick a Thor-compatible image where the JetPack 6 image differs.
func wendyPlatform(deviceType string) string {
	switch deviceType {
	case "jetson-agx-orin", "jetson-orin-nano", "jetson-agx-thor":
		return "nvidia-jetson"
	default:
		return "generic"
	}
}

// isWatch reports whether this run is one cycle of a `wendy run --watch`
// session.
func (o runOptions) isWatch() bool { return o.watchState != nil }

func (o runOptions) beginHostLifecycle(containerName string) bool {
	return o.watchState.beginHostLifecycle(containerName)
}

func (o runOptions) completeHostLifecycle(containerName string) {
	o.watchState.completeHostLifecycle(containerName)
}

func (o runOptions) abandonHostLifecycle(containerName string) {
	o.watchState.abandonHostLifecycle(containerName)
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

// streamRunContainer drains a RunContainer server stream, writing stdout/stderr
// to the corresponding OS streams. When opts.deploy or opts.detach is set the
// function returns as soon as the Started message is received (mirroring the
// behaviour of startAndStreamContainer for those flags). In attached mode the
// Started message triggers readiness + the host-side postStart hook (again
// mirroring startAndStreamContainer), then log streaming continues.
func streamRunContainer(ctx context.Context, conn *grpcclient.AgentConnection, stream grpc.ServerStreamingClient[agentpb.RunContainerLayersResponse], appCfg *appconfig.AppConfig, opts runOptions) error {
	// The attached-mode postStart hook is tied to hookCtx so it is terminated
	// when the stream ends (matching startAndStreamContainer's runCtx handling).
	// Cleanup runs in a defer so the hook is killed and reaped on every exit
	// path, including stream errors.
	// Splits the runcontainer phase into its device-side and host-side halves:
	// everything up to Started is the agent creating and starting the
	// container, everything after is the CLI waiting on the app and firing
	// hooks. They have completely different causes when one is slow.
	rc := phaseTimer()
	hookCtx, hookCancel := context.WithCancel(ctx)
	var postStartCmd *exec.Cmd
	defer func() {
		hookCancel()
		if postStartCmd != nil {
			_ = postStartCmd.Wait()
		}
	}()
	hookFired := false
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("receiving container output: %w", err)
		}
		if resp.GetStarted() != nil {
			rc("  ↳ runcontainer: device create+start")
			if opts.deploy {
				cliLogln("Container %s created (not started).", containerDisplayName(appCfg))
				return nil
			}
			if opts.detach {
				// Mirror startAndStreamContainer's detach branch: the container
				// is started, so return without tailing logs or waiting on
				// readiness (see runPostStartIfReady's doc comment). The container keeps
				// running independently of this (now-abandoned) output stream.
				cliLogln("Application %s running in detached mode.", containerDisplayName(appCfg))
				return nil
			}
			// Attached runs wait for readiness, announce the URL, and fire the
			// host-side postStart hook before continuing to stream logs.
			// hookFired guards against a malformed stream sending Started twice.
			if !hookFired {
				hookFired = true
				if opts.isWatch() {
					cmd := runPostStartIfReady(ctx, opts.watchState.hookContext(ctx), conn, appCfg, opts)
					opts.watchState.reapCommand(cmd)
					return nil
				}
				postStartCmd = runPostStartIfReady(ctx, hookCtx, conn, appCfg, runOptions{})
			}
			continue
		}
		if out := resp.GetStdoutOutput(); out != nil {
			_, _ = os.Stdout.Write(out.GetData())
		}
		if out := resp.GetStderrOutput(); out != nil {
			_, _ = os.Stderr.Write(out.GetData())
		}
	}
	cliLogln("\nApplication %s stopped.", containerDisplayName(appCfg))
	return nil
}

// phaseTimer returns a closure that logs the elapsed time since the previous
// call to stderr, but only when WENDY_TIMING is set. It is a lightweight
// diagnostic for finding where wall-clock time goes in the deploy path.
func phaseTimer() func(label string) {
	if os.Getenv("WENDY_TIMING") == "" {
		return func(string) {}
	}
	last := time.Now()
	return func(label string) {
		now := time.Now()
		fmt.Fprintf(os.Stderr, "[timing] %-26s %s\n", label, now.Sub(last).Round(time.Millisecond))
		last = now
	}
}

// shouldDumpChunkDiffBuildLog decides whether the chunk-diff build replays its
// captured build log when the build fails. The log must be shown whenever the
// error is surfaced to the user directly: always under --chunking=force (no
// fallback), and for image-build failures under auto chunking, which skip the
// registry-push fallback (#1166). Only builder-setup failures under auto
// chunking stay quiet — those fall back to a registry push whose own build
// output supersedes the discarded log.
func shouldDumpChunkDiffBuildLog(chunking string) func(error) bool {
	return func(err error) bool {
		return chunking == chunkingForce || isImageBuildFailure(err)
	}
}

// imageSignaturePathEnv optionally points at a detached signature file over the
// SHA256 digest of the OCI image config (e.g. an ML-DSA65 signature). No signer
// exists yet, so this is unset in normal operation and RunContainerLayersRequest
// carries an empty ImageSignature — the agent's verifier tolerates that until a
// pinned key is embedded (see internal/shared/sigverify).
// TODO(H2): once a signed-release pipeline ships, replace this env var with a
// real sidecar/manifest convention resolved automatically alongside the build,
// instead of requiring the caller to set it by hand.
const imageSignaturePathEnv = "WENDY_IMAGE_SIGNATURE_PATH"

// chunkDeployStats is an out-param sink so the caller of deployByChunkDiff
// knows what a registry-push fallback would cost even though the chunk-diff
// deploy failed. Filled as soon as the built image's layers are read; stays
// zero when the failure preceded (or prevented) a successful layer read.
type chunkDeployStats struct {
	imageBytes int64
}

// isChunkDeployCancellation reports whether a deployByChunkDiff failure was a
// cancellation rather than a genuine deploy failure — either the context was
// cancelled (e.g. `wendy watch` superseded the deploy with a newer change, or
// the user hit Ctrl-C before the interactive chunk-push progress bar started)
// or the user backed out of that progress bar itself. Bubble Tea captures
// Ctrl-C as a key event there (see pushLayersWithProgress), so the parent ctx
// is NOT cancelled in that case — err is the only signal, via ErrUserCancelled.
// Either way, the fallback ladder must never proceed to a registry push (often
// a bigger upload than the one just cancelled) after the user said stop.
func isChunkDeployCancellation(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, ErrUserCancelled)
}

// largeRegistryFallbackBytes is the (decimal) image-size threshold above which
// a registry-push fallback is treated as "large" — big enough that silently
// re-uploading every layer is worth interrupting an interactive user to
// confirm, rather than just logging a line they might miss.
const largeRegistryFallbackBytes = 500 * 1000 * 1000

// registryFallbackAction is the decision registryFallbackPlan hands back: log
// loudly and proceed, or stop and confirm first.
type registryFallbackAction int

const (
	fallbackProceedLoud registryFallbackAction = iota
	fallbackConfirm
)

// registryFallbackPlan decides whether a chunk-diff failure can fall back to a
// full registry push with just a loud notice, or must be confirmed first.
// Confirmation is gated on all three of: the fallback being large, a human
// being present to answer (interactive), and the caller not having already
// pre-approved every prompt (--yes). Non-interactive runs (CI, `wendy watch`)
// and --yes always proceed on the notice alone — this path must never hard-fail
// an unattended deploy.
func registryFallbackPlan(imageBytes int64, interactive, assumeYes bool) registryFallbackAction {
	if imageBytes >= largeRegistryFallbackBytes && interactive && !assumeYes {
		return fallbackConfirm
	}
	return fallbackProceedLoud
}

// formatRegistryFallbackNotice explains why the chunk-diff (fast) deploy path
// failed and what the registry-push fallback is about to do next — the
// chunk-diff error used to be dropped silently here, leaving no trail for why
// a deploy suddenly got slower. Large fallbacks (imageBytes unknown/zero counts
// as small) get a louder, more specific message: re-uploading gigabytes of
// otherwise-unchanged layers is expensive enough that "using registry push"
// alone undersells what is about to happen.
func formatRegistryFallbackNotice(chunkErr error, imageBytes int64) string {
	if imageBytes >= largeRegistryFallbackBytes {
		size := tui.ByteProgress{Current: imageBytes}.String()
		return fmt.Sprintf("Fast deploy unavailable (%v). Falling back to a FULL registry push — ~%s will be re-uploaded.", chunkErr, size)
	}
	return fmt.Sprintf("Fast deploy unavailable (%v); falling back to registry push.", chunkErr)
}

// ociReuseHint carries the on-disk location of the OCI-layout image the
// chunk-diff deploy just built, so a registry-push fallback triggered by a
// LATER failure (e.g. the device rejecting chunk-diff entirely) can push that
// exact content straight to the registry via pushOCILayoutToRegistry instead
// of re-running buildx from scratch — the two builds are otherwise given the
// same Dockerfile, build-args, and platform, so the content would be
// identical anyway. Non-nil only when the "dir" export plan was used (see
// chunkExportPlan) and the build succeeded and was read back successfully:
// nil for the "tar" export plan (whose temp directory is removed before the
// caller could reuse it) and whenever the build/read itself failed.
type ociReuseHint struct {
	layoutDir string
	platform  string
}

// deployByChunkDiff builds the image to a local OCI layout tar, diffs the
// layers against what the device already has via content-defined chunking, and
// calls RunContainer with the resulting layer headers. On success it returns the
// uncompressed layer diff IDs it deployed, so the caller can record them in the
// deploy fingerprint and later verify the device still holds this content before
// skipping a rebuild (WDY-1824). It also returns an *ociReuseHint (see its doc
// comment) so a caller that has to fall back to a registry push after a
// failure here can reuse the image already built rather than rebuilding it.
//
// stats, when non-nil, is filled with the built image's size/layer count as
// soon as a layer read succeeds — including on failure paths below that point
// — so a caller whose overall deploy still fails can decide how to handle a
// registry-push fallback without re-reading the layers itself.
func deployByChunkDiff(ctx context.Context, conn *grpcclient.AgentConnection, cwd string, appCfg *appconfig.AppConfig, platform, dockerfile string, buildArgs map[string]string, deployEnv []string, opts runOptions, stats *chunkDeployStats) ([]string, *ociReuseHint, error) {
	mark := phaseTimer()
	var hint *ociReuseHint

	buildTitle := fmt.Sprintf("Building image (OCI layout) for %s...", tui.Value(platform))
	runBuild := func(build func(context.Context, io.Writer, io.Writer) error) error {
		if opts.quietBuild {
			// wendy watch: keep the legacy quiet behavior (buffer, surface only on
			// genuine failure) rather than rendering a live UI under the watcher.
			var buildLog bytes.Buffer
			if err := build(ctx, &buildLog, &buildLog); err != nil {
				if ctx.Err() == nil {
					renderBuildFailure(os.Stderr, "", buildLog.String(), err)
				}
				return err
			}
			return nil
		}
		return runBuildWithProgress(ctx, buildTitle, shouldDumpChunkDiffBuildLog(opts.chunking), build)
	}

	// The docker backend exports into a persistent per-app OCI layout DIRECTORY:
	// BuildKit skips blobs already present there, so a warm rebuild writes only
	// the changed layers instead of re-serializing the whole image (which costs
	// seconds per GB of image on every iteration). Tar-only backends and the
	// WENDY_CHUNK_EXPORT=tar escape hatch keep the legacy temp tar.
	exportMode := chunkExportPlan(opts.builder)
	var layoutDir string
	if exportMode == "dir" {
		if userCache, cacheErr := os.UserCacheDir(); cacheErr == nil {
			layoutDir = chunkLayoutDir(userCache, appCfg.AppID, platform)
		} else {
			exportMode = "tar"
		}
	}

	var layers []localLayer
	var imageConfig []byte
	// fillStats snapshots the current layers into the caller's out-param
	// (A7/WDY-2432). Called after every point below where a layer read just
	// succeeded, so stats reflects the most recent successful read even if a
	// later step (another rebuild, the chunk push, RunContainer) fails.
	fillStats := func() {
		if stats != nil {
			stats.imageBytes = totalCompressedLayerBytes(layers)
		}
	}
	if exportMode == "dir" {
		releaseLayout, err := lockOCILayoutDir(ctx, layoutDir)
		if err != nil {
			return nil, hint, err
		}
		defer releaseLayout()
		build := func(buildCtx context.Context, stream, logw io.Writer) error {
			return buildImageToOCILayoutDirWithDocker(buildCtx, cwd, dockerfile, platform, buildArgs, layoutDir, stream, logw)
		}

		// Native fast path: for a Stagefile project whose deps inputs are
		// unchanged since the layout dir's last build, rebuild only the app COPY
		// layer(s) in-process and splice them into the layout — no Docker at
		// all. Every guard failure silently falls through to buildx.
		sf, nativeEligible := nativeBuildEligibility(cwd, dockerfile)
		var depsHash string
		if nativeEligible {
			if h, hashErr := nativeDepsHash(cwd, dockerfile, platform, buildArgs, sf); hashErr == nil {
				depsHash = h
			} else {
				nativeEligible = false
			}
		}
		nativeDone := false
		if nativeEligible {
			if st, ok := loadNativeState(layoutDir); ok && st.DepsHash == depsHash {
				if done, rebuildErr := tryNativeRebuild(layoutDir, platform, cwd, sf, st); rebuildErr == nil && done {
					nativeDone = true
					cliLogln("App layer(s) rebuilt natively (deps unchanged; buildx skipped)")
				}
			}
		}

		if nativeDone {
			mark("build (native layers)")
		} else {
			if err := runBuild(build); err != nil {
				return nil, hint, err
			}
			mark("build (oci export)")
		}
		layers, imageConfig, err = readOCILayoutDirLayers(layoutDir, platform)
		if err != nil {
			// Self-heal exactly once: a corrupt or partially-written layout dir is
			// wiped and rebuilt from scratch, which is precisely the legacy cold
			// behavior. A second failure is a real bug and surfaces. The wipe also
			// removes state.json, so the native path stays off until re-adoption.
			cliLogln("OCI layout cache unreadable (%v); rebuilding it from scratch", err)
			if rmErr := os.RemoveAll(layoutDir); rmErr != nil {
				return nil, hint, fmt.Errorf("resetting OCI layout cache %s: %w", layoutDir, rmErr)
			}
			nativeDone = false
			if err := runBuild(build); err != nil {
				return nil, hint, err
			}
			if layers, imageConfig, err = readOCILayoutDirLayers(layoutDir, platform); err != nil {
				return nil, hint, err
			}
		}
		fillStats()
		if nativeEligible && !nativeDone {
			// After a buildx build, take ownership of the app layers: replace them
			// with deterministic native rebuilds (verified against the buildx
			// layers' file sets) so every following iteration can skip buildx.
			if adopted, adoptErr := adoptNativeLayers(layoutDir, platform, cwd, sf, depsHash); adoptErr == nil && adopted {
				if layers, imageConfig, err = readOCILayoutDirLayers(layoutDir, platform); err != nil {
					return nil, hint, err
				}
				fillStats()
			}
		}
		// The layout directory now holds a known-good, freshly built image —
		// record it so a chunk-diff failure below can fall back to reusing it
		// (see ociReuseHint) instead of a redundant second buildx build.
		hint = &ociReuseHint{layoutDir: layoutDir, platform: platform}
		// Once the deploy is done with the layout: GC blobs superseded by this
		// build, THEN dedup identical blobs across app layout dirs, evict
		// least-recently-used caches over the size cap, and bound the daemon
		// store. Defers run LIFO, so maintenance is registered first and GC
		// last — otherwise the size cap would measure this build's soon-to-be-
		// pruned orphans as live usage and evict other apps for nothing. Both
		// are best-effort; this build's own layout is protected (keep) so
		// maintenance never yanks it, and a failed GC only leaves garbage for
		// the next run to collect.
		if userCache, cacheErr := os.UserCacheDir(); cacheErr == nil {
			keep := map[string]bool{layoutDir: true}
			defer func() { _, _ = maintainBuildCaches(ctx, userCache, buildCacheMaxBytes(), keep) }()
		}
		defer func() { _ = gcOCILayoutDir(layoutDir) }()
	} else {
		tmp, err := os.MkdirTemp("", "wendy-oci-*")
		if err != nil {
			return nil, hint, err
		}
		defer os.RemoveAll(tmp)
		ociTar := filepath.Join(tmp, "image.tar")
		if err := runBuild(func(buildCtx context.Context, stream, logw io.Writer) error {
			return buildImageToOCILayout(buildCtx, cwd, dockerfile, platform, buildArgs, opts.builder, ociTar, stream, logw)
		}); err != nil {
			return nil, hint, err
		}
		mark("build (oci export)")
		layers, imageConfig, err = readOCILayoutLayers(ociTar, platform)
		if err != nil {
			return nil, hint, err
		}
		fillStats()
	}
	mark("read+decompress layers")

	sizeClause := ""
	if compressedTotal := totalCompressedLayerBytes(layers); compressedTotal > 0 {
		sizeClause = fmt.Sprintf(" (%s compressed)", tui.Value(tui.ByteProgress{Current: compressedTotal}.String()))
	}
	cliLogln("Diffing %s layer(s)%s against device...", tui.Value(fmt.Sprintf("%d", len(layers))), sizeClause)
	appConfigData, err := json.Marshal(appCfg)
	if err != nil {
		return nil, hint, err
	}
	imageSignature, err := readOptionalSignature(os.Getenv(imageSignaturePathEnv))
	if err != nil {
		return nil, hint, fmt.Errorf("reading image signature from %s: %w", imageSignaturePathEnv, err)
	}
	imageName := strings.ToLower(appCfg.AppID) + ":latest"
	// prepareFor builds the device-side preparation func against a specific
	// client: pushLayersResumingTunnelDrops re-derives it per attempt so a
	// post-drop retry's PrepareImage rides the reconnected tunnel, not the
	// dead one (WDY-2433).
	prepareFor := func(cs agentpb.WendyContainerServiceClient) imagePrepareFunc {
		return func(prepareCtx context.Context, headers []*agentpb.RunContainerLayerHeader) error {
			_, prepareErr := cs.PrepareImage(prepareCtx, &agentpb.RunContainerLayersRequest{
				ImageName:      imageName,
				Layers:         headers,
				ImageConfig:    imageConfig,
				ImageSignature: imageSignature,
			})
			return prepareErr
		}
	}

	// pushConn may differ from conn: a tunnel drop mid-transfer reconnects to a
	// fresh connection (WDY-2433), and everything from here on — RunContainer,
	// its response stream, and the post-start hook — must ride that live
	// connection rather than the one that just dropped.
	pushConn, headers, err := pushLayersResumingTunnelDrops(ctx, conn, layers, prepareFor)
	if pushConn != conn {
		defer pushConn.Close()
	}
	if err != nil {
		return nil, hint, err
	}
	mark("chunk+query+write+prepare")
	// Carry the post-start agent-hook metadata so the agent runs the device-host
	// hook on start, matching the registry path's StartContainer call.
	rpcCtx := ctx
	var rpcCancel context.CancelFunc
	if opts.isWatch() {
		rpcCtx, rpcCancel = context.WithCancel(ctx)
		defer rpcCancel()
	}
	runCtx := contextWithPostStartAgentHook(rpcCtx, appCfg)
	// The log subscription rides pushConn for the same reason RunContainer
	// does: after a mid-transfer tunnel drop, conn is dead and pushConn is the
	// live reconnected connection (WDY-2433). Watch owns one session-wide
	// subscription, so it must not also open this per-deploy subscription.
	var logSub *runLogSubscription
	if runCycleOwnsLogSubscription(opts) {
		logSub = startRunLogSubscription(ctx, pushConn, appCfg.AppID, os.Stdout, runLogStreamWarning)
		defer logSub.stop()
	}
	stream, err := pushConn.ContainerService.RunContainer(runCtx, &agentpb.RunContainerLayersRequest{
		ImageName:      imageName,
		AppName:        appCfg.AppID,
		Layers:         headers,
		AppConfig:      appConfigData,
		ImageConfig:    imageConfig,
		RestartPolicy:  resolveRestartPolicy(opts),
		UserArgs:       opts.userArgs,
		ImageSignature: imageSignature,
		Env:            deployEnv,
	})
	if err != nil {
		return nil, hint, err
	}
	if err := streamRunContainer(rpcCtx, pushConn, stream, appCfg, opts); err != nil {
		mark("runcontainer (assemble+create+start[+readiness])")
		return nil, hint, err
	}
	mark("runcontainer (assemble+create+start[+readiness])")
	return layerDiffIDs(headers), hint, nil
}

// layerDiffIDs extracts the ordered uncompressed diff IDs from the reassembly
// headers that were deployed, for recording in the deploy fingerprint. Each
// header's DiffId is the same content identity QueryLayers reports, so the next
// run can verify the device still holds every layer before skipping (WDY-1824).
func layerDiffIDs(headers []*agentpb.RunContainerLayerHeader) []string {
	ids := make([]string, 0, len(headers))
	for _, h := range headers {
		if id := h.GetDiffId(); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}
