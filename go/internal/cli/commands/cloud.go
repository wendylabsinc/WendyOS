package commands

import (
	"github.com/spf13/cobra"
)

func newCloudCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cloud",
		Short: "Manage Wendy Cloud resources",
	}

	cmd.AddCommand(newCloudEnrollDeviceCmd())
	cmd.AddCommand(newCloudRunCmd())
	cmd.AddCommand(newCloudTunnelCmd())
	return cmd
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
