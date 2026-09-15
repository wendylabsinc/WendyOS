package commands

import (
	"context"
	"os"

	"github.com/spf13/cobra"
)

func newCloudRunCmd() *cobra.Command {
	var opts runOptions
	var cloudGRPC string
	var deviceName string
	var brokerURL string

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Deprecated: use 'wendy run' instead",
		// Hidden keeps it off the help menu, which also means the Short above is
		// never read. Deprecated makes cobra warn at the point of use, so a
		// script still calling this finds out.
		Deprecated: "use 'wendy run' instead",
		Hidden:     true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.WithValue(cmd.Context(), cloudDeviceContextKey{}, cloudDeviceConfig{
				CloudGRPC:  cloudGRPC,
				DeviceName: effectiveDeviceName(deviceName),
				BrokerURL:  brokerURL,
			})
			return runCommand(ctx, opts)
		},
	}

	cmd.Flags().StringVar(&opts.buildType, "build-type", "", "Build type: docker, swift, or python")
	cmd.Flags().StringVar(&opts.builder, "builder", "", "Image builder to force for Dockerfile/Containerfile builds: docker, apple-container, or buildkit")
	cmd.Flags().StringVar(&opts.stagefileBackend, "stagefile-backend", "", "Stagefile compiler backend: dockerfile (default) or llb")
	cmd.Flags().StringVar(&opts.buildHost, "build-host", "", "WendyOS device to build the image on instead of this machine")
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
	cmd.Flags().StringVar(&cloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint (optional when a default session is set via 'wendy auth use')")
	cmd.Flags().StringVar(&deviceName, "device", "", "Device name (skips interactive picker)")
	cmd.Flags().StringVar(&brokerURL, "broker-url", os.Getenv("WENDY_BROKER_URL"), "Tunnel broker host:port")

	return cmd
}
