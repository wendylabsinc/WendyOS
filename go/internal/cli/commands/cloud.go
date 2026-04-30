package commands

import (
	"context"
	"os"

	"github.com/spf13/cobra"
)

type cloudDeviceConfig struct {
	CloudGRPC  string
	DeviceName string
	BrokerURL  string
}

type cloudDeviceContextKey struct{}

func newCloudCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cloud",
		Short: "Manage Wendy Cloud resources",
	}

	cmd.AddCommand(newCloudEnrollDeviceCmd())
	cmd.AddCommand(newCloudDiscoverCmd())
	cmd.AddCommand(newCloudRunCmd())
	cmd.AddCommand(newCloudTunnelCmd())
	cmd.AddCommand(newCloudDeviceCmd())
	return cmd
}

func newCloudDeviceCmd() *cobra.Command {
	var cloudGRPC string
	var brokerURL string

	cmd := newDeviceCmd()
	cmd.Short = "Manage WendyOS devices through Wendy Cloud"
	cmd.Long = "Mirror of 'wendy device', but connects to the target device through the Wendy Cloud tunnel broker."
	cmd.PersistentFlags().StringVar(&cloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint (required when multiple auth sessions exist)")
	cmd.PersistentFlags().StringVar(&brokerURL, "broker-url", os.Getenv("WENDY_BROKER_URL"), "Tunnel broker host:port (default: cloud :443 endpoint, otherwise <cloud-host>:50052)")

	wrapCloudDeviceCommands(cmd, func() cloudDeviceConfig {
		return cloudDeviceConfig{
			CloudGRPC:  cloudGRPC,
			DeviceName: deviceFlag,
			BrokerURL:  brokerURL,
		}
	})
	return cmd
}

func wrapCloudDeviceCommands(cmd *cobra.Command, cfg func() cloudDeviceConfig) {
	if cmd.RunE != nil {
		runE := cmd.RunE
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			cmd.SetContext(context.WithValue(cmd.Context(), cloudDeviceContextKey{}, cfg()))
			return runE(cmd, args)
		}
	}
	if cmd.Run != nil {
		run := cmd.Run
		cmd.Run = func(cmd *cobra.Command, args []string) {
			cmd.SetContext(context.WithValue(cmd.Context(), cloudDeviceContextKey{}, cfg()))
			run(cmd, args)
		}
	}
	for _, child := range cmd.Commands() {
		wrapCloudDeviceCommands(child, cfg)
	}
}

func cloudDeviceConfigFromContext(ctx context.Context) (cloudDeviceConfig, bool) {
	cfg, ok := ctx.Value(cloudDeviceContextKey{}).(cloudDeviceConfig)
	return cfg, ok
}

func newCloudEnrollDeviceCmd() *cobra.Command {
	var name string
	var cloudGRPC string

	cmd := &cobra.Command{
		Use:   "enroll-device",
		Short: "Enroll the connected device with Wendy Cloud or a local pki-core",
		Long:  "Alias for 'wendy device enroll'. Creates an enrollment token using your stored auth session and provisions the connected device with mTLS certificates.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			conn, err := connectToAgent(ctx, SuppressProvisioningHint())
			if err != nil {
				return err
			}
			defer conn.Close()

			auth, err := pickAuthEntry(cloudGRPC)
			if err != nil {
				return err
			}

			return runEnrollDevice(ctx, conn, auth, name)
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Device name")
	cmd.Flags().StringVar(&cloudGRPC, "cloud-grpc", "", "Cloud/pki-core gRPC endpoint to use (required when multiple auth sessions exist)")
	return cmd
}
