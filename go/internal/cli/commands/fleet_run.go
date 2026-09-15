package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/gpu"
)

func newFleetRunCmd() *cobra.Command {
	var opts runOptions
	var group string
	var cloudGRPC, brokerURL string
	var lan bool
	var central string
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "run --group <group>",
		Short: "Build and deploy the current project to every device in a group",
		Long: "Deploys the current project (the wendy.json in the working directory) to every\n" +
			"device in a named group, one invocation instead of a per-device loop.\n\n" +
			"With --lan the group is resolved over the local network via mDNS (a glob over\n" +
			"device names, e.g. 'camera-*'); no cloud session required.\n\n" +
			"If the wendy.json is a fleet manifest (a 'components' map), each component is\n" +
			"placed by its own target: group components fan out to the matching devices and\n" +
			"the central component is deployed once (use --central <device>, or omit it to\n" +
			"print the peer snapshot for running central yourself).\n\n" +
			"Runs detached (it does not stream logs — there are many devices). With\n" +
			"--keep-going a device that fails to deploy does not abort the rest.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// A fleet deploy targets many devices: never prompt, never stream logs.
			opts.yes = true
			opts.detach = true
			return runFleetRun(cmd.Context(), opts, group, cloudGRPC, brokerURL, lan, central, timeout)
		},
	}

	cmd.Flags().StringVar(&group, "group", "", "Device group to deploy to (required for a single-project deploy)")
	cmd.Flags().BoolVar(&lan, "lan", false, "Resolve the group over the LAN (mDNS) instead of the cloud")
	cmd.Flags().StringVar(&central, "central", "", "Device to deploy a fleet manifest's central component to (LAN device name)")
	cmd.Flags().DurationVar(&timeout, "discover-timeout", fleetLANDiscoverTimeout, "How long to browse for LAN devices (with --lan)")
	cmd.Flags().StringVar(&cloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint (optional when a default session is set via 'wendy auth use')")
	cmd.Flags().StringVar(&brokerURL, "broker-url", os.Getenv("WENDY_BROKER_URL"), "Tunnel broker host:port")
	cmd.Flags().BoolVar(&opts.keepGoing, "keep-going", false, "Deploy to the remaining devices even if one fails, instead of stopping at the first error")

	// A focused subset of `wendy run` build flags (logs/streaming/picker flags
	// don't apply to a fan-out deploy).
	cmd.Flags().StringVar(&opts.buildType, "build-type", "", "Build type when ambiguous: docker, swift, or python")
	cmd.Flags().StringVar(&opts.dockerfile, "dockerfile", "", "Build file to build from: a Dockerfile, Containerfile, or Stagefile")
	cmd.Flags().StringVar(&opts.builder, "builder", "", "Image builder to force: docker or apple-container")
	cmd.Flags().StringVar(&opts.stagefileBackend, "stagefile-backend", "", "Stagefile compiler backend: dockerfile (default) or llb")
	cmd.Flags().StringVar(&opts.gpuArch, "gpu-arch", "", fmt.Sprintf("GPU architecture a Stagefile cuda: stage targets (%s); read from the device when one is selected", strings.Join(gpu.KnownArches(), ", ")))
	cmd.Flags().BoolVar(&opts.debug, "debug", false, "Enable debug logging + host networking")
	cmd.Flags().StringVar(&opts.service, "service", "", "Build and deploy only the named service and its dependencies")
	cmd.Flags().StringSliceVar(&opts.userArgs, "user-args", nil, "Extra arguments to pass to the container")
	cmd.Flags().BoolVar(&opts.restartUnlessStopped, "restart-unless-stopped", false, "Restart unless manually stopped")
	cmd.Flags().BoolVar(&opts.restartOnFailure, "restart-on-failure", false, "Restart on failure")
	cmd.Flags().BoolVar(&opts.noRestart, "no-restart", false, "Do not restart on exit")
	cmd.Flags().StringVar(&opts.prefix, "prefix", "", "Project directory to deploy from instead of the current working directory")
	cmd.Flags().StringVar(&opts.chunking, "chunking", chunkingAuto, "Deploy path: auto, force (chunk-diff only), or off (registry push only)")

	return cmd
}

// fleetRunResult is the per-device outcome of a fan-out deploy, for --json.
type fleetRunResult struct {
	Device string `json:"device"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
}

func runFleetRun(ctx context.Context, opts runOptions, group, cloudGRPC, brokerURL string, lan bool, central string, timeout time.Duration) error {
	normalizedBuilder, err := normalizeImageBuilder(opts.builder)
	if err != nil {
		return err
	}
	if _, err := stagefileBackendLLB(opts.stagefileBackend, normalizedBuilder); err != nil {
		return err
	}
	ctx = withStagefileBackend(ctx, opts.stagefileBackend)
	if err := validateChunkingMode(opts.chunking); err != nil {
		return err
	}

	// Load + validate the project once, before connecting to any device, so a
	// bad project fails fast rather than once per device.
	cwd, err := resolveRunWorkingDir(opts)
	if err != nil {
		return fmt.Errorf("resolving working directory: %w", err)
	}

	// A wendy-fleet.json drives a placement-aware fleet deploy: each component
	// is built from its own context and placed by its own target. It's a
	// separate file from any single-app wendy.json.
	fleetPath := filepath.Join(cwd, appconfig.FleetManifestFileName)
	if _, statErr := os.Stat(fleetPath); statErr == nil {
		manifest, mErr := appconfig.LoadFleetManifest(fleetPath)
		if mErr != nil {
			return fmt.Errorf("loading %s: %w", appconfig.FleetManifestFileName, mErr)
		}
		return runFleetManifest(ctx, opts, cwd, manifest, lan, central, cloudGRPC, brokerURL, timeout)
	}

	// Single-project fan-out: deploy this project's wendy.json to a --group.
	cfgPath := filepath.Join(cwd, "wendy.json")
	missing, err := appConfigFileMissing(cfgPath)
	if err != nil {
		return fmt.Errorf("checking wendy.json: %w", err)
	}
	if missing {
		return fmt.Errorf("no wendy.json or %s in %s — 'wendy fleet run' deploys a project to a group, or a fleet manifest across components", appconfig.FleetManifestFileName, cwd)
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

	if opts.debug {
		applyDebugHostNetworking(appCfg)
	}

	projectType, err := resolveRunProjectType(cwd, opts.buildType)
	if err != nil {
		return err
	}
	if projectType == "compose" {
		return fmt.Errorf("compose projects are not supported by 'wendy fleet run' yet")
	}
	if projectType == "docker" && opts.dockerfile == "" {
		// A fleet deploys one image to many devices, which may not share a GPU
		// architecture, so there is no device here to read it from — a cuda:
		// stage has to be told which board the image is for.
		resolved, err := resolveDockerfile(cwd, opts.dockerfile, !opts.yes && isInteractiveTerminal(), opts.gpuArch,
			debugStagefileOptions(opts.debug)...)
		if err != nil {
			return err
		}
		opts.dockerfile = resolved
	}

	if group == "" {
		return fmt.Errorf("--group is required (the device group to deploy to)")
	}
	targets, err := resolveFleetTargets(ctx, group, lan, cloudGRPC, brokerURL, timeout)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return fmt.Errorf("group %q has no devices; on the LAN check the name pattern, or add cloud devices with 'wendy fleet group add %s <device>...'", group, group)
	}

	fmt.Printf("Deploying %s to %d device(s) in group %q\n", appCfg.AppID, len(targets), group)

	results, failures := deployToTargets(ctx, targets, cwd, appCfg, opts)

	if jsonOutput {
		if err := printJSON(results); err != nil {
			return err
		}
	} else {
		fmt.Printf("\nDeployed to %d/%d device(s) in group %q\n", len(targets)-failures, len(targets), group)
	}
	if failures > 0 {
		return fmt.Errorf("%d of %d device(s) failed", failures, len(targets))
	}
	return nil
}

// deployToTargets runs the standard single-device deploy pipeline against each
// target in turn (sequential keeps build output legible and avoids concurrent
// builder contention). It honors opts.keepGoing: when false it stops at the
// first failure. Returns per-device results and a failure count.
func deployToTargets(ctx context.Context, targets []fleetTarget, cwd string, appCfg *appconfig.AppConfig, opts runOptions) (results []fleetRunResult, failures int) {
	results = make([]fleetRunResult, 0, len(targets))
	for _, target := range targets {
		res := fleetRunResult{Device: target.Name}
		fmt.Printf("\n── %s ──\n", target.Name)

		if derr := deployToTarget(ctx, target, cwd, appCfg, opts); derr != nil {
			res.Error = derr.Error()
			failures++
			results = append(results, res)
			fmt.Fprintf(os.Stderr, "  ✗ %s: %v\n", target.Name, derr)
			if !opts.keepGoing {
				// Surface the failure but return what we have so callers can report.
				return results, failures
			}
			continue
		}
		res.OK = true
		results = append(results, res)
	}
	return results, failures
}

// deployToTarget connects to one target and runs the standard agent deploy
// pipeline against it.
func deployToTarget(ctx context.Context, target fleetTarget, cwd string, appCfg *appconfig.AppConfig, opts runOptions) error {
	conn, err := target.connect(ctx)
	if err != nil {
		return fmt.Errorf("connecting: %w", err)
	}
	defer conn.Conn.Close()
	return runWithAgent(ctx, conn, cwd, appCfg, opts)
}

// applyDebugHostNetworking mirrors runCommand's --debug handling: enable debug
// and ensure a host-mode network entitlement for remote debugger access.
func applyDebugHostNetworking(appCfg *appconfig.AppConfig) {
	appCfg.Debug = true
	for i, e := range appCfg.Entitlements {
		if e.Type == appconfig.EntitlementNetwork {
			appCfg.Entitlements[i].Mode = "host"
			return
		}
	}
	appCfg.Entitlements = append(appCfg.Entitlements, appconfig.Entitlement{
		Type: appconfig.EntitlementNetwork,
		Mode: "host",
	})
}
