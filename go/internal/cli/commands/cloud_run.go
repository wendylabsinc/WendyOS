package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
)

func newCloudRunCmd() *cobra.Command {
	var opts runOptions
	var cloudGRPC string
	var deviceName string
	var brokerURL string

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Build and run application on a cloud-enrolled device",
		Long:  "Same as 'wendy run' but connects to the device through the Wendy Cloud tunnel broker instead of a direct network path.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cloudRunCommand(cmd.Context(), opts, cloudGRPC, deviceName, brokerURL)
		},
	}

	cmd.Flags().StringVar(&opts.buildType, "build-type", "", "Build type: docker, swift, or python")
	cmd.Flags().BoolVar(&opts.debug, "debug", false, "Enable debug logging")
	cmd.Flags().BoolVar(&opts.deploy, "deploy", false, "Create container but do not start it")
	cmd.Flags().BoolVar(&opts.detach, "detach", false, "Start container but do not stream logs")
	cmd.Flags().BoolVarP(&opts.yes, "yes", "y", false, "Automatically accept all interactive prompts")
	cmd.Flags().BoolVar(&opts.restartUnlessStopped, "restart-unless-stopped", false, "Restart unless manually stopped")
	cmd.Flags().BoolVar(&opts.restartOnFailure, "restart-on-failure", false, "Restart on failure")
	cmd.Flags().BoolVar(&opts.noRestart, "no-restart", false, "Do not restart on exit")
	cmd.Flags().StringVar(&opts.prefix, "prefix", "", "Project directory instead of current working directory")
	cmd.Flags().StringVar(&opts.product, "product", "", "Swift Package Manager product to build and run")
	cmd.Flags().StringSliceVar(&opts.userArgs, "user-args", nil, "Extra arguments to pass to the container")
	cmd.Flags().StringVar(&cloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint (required when multiple auth sessions exist)")
	cmd.Flags().StringVar(&deviceName, "device", "", "Device name (skips interactive picker)")
	cmd.Flags().StringVar(&brokerURL, "broker-url", os.Getenv("WENDY_BROKER_URL"), "Tunnel broker host:port (default: <cloud-host>:50052)")

	return cmd
}

func cloudRunCommand(ctx context.Context, opts runOptions, cloudGRPC, deviceName, brokerURL string) error {
	cwd, err := resolveRunWorkingDir(opts)
	if err != nil {
		return fmt.Errorf("resolving working directory: %w", err)
	}

	appCfg, err := ensureAppConfig(cwd+"/wendy.json", opts.yes)
	if err != nil {
		return fmt.Errorf("loading wendy.json: %w", err)
	}
	if err := appCfg.Validate(); err != nil {
		return fmt.Errorf("invalid wendy.json: %w", err)
	}

	agentConn, err := connectToCloudAgent(ctx, cloudGRPC, deviceName, brokerURL)
	if err != nil {
		return err
	}
	defer agentConn.Close()

	return runWithAgent(ctx, agentConn, cwd, appCfg, opts)
}
