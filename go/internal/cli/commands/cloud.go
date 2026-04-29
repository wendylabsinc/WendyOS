package commands

import "github.com/spf13/cobra"

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
