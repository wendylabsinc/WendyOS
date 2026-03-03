// Package commands defines all Cobra commands for the Wendy CLI.
package commands

import (
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/providers"
	"github.com/wendylabsinc/wendy/internal/shared/version"
)

var (
	jsonOutput bool
	deviceFlag string
)

// NewRootCmd creates the root Cobra command with all subcommands.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "wendy",
		Short:         "Wendy CLI - Edge Computing Development Tool",
		Long:          "Wendy is a CLI for developing and deploying edge computing applications to WendyOS devices.",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			providers.Initialize(cmd.Context())
			return nil
		},
	}

	root.PersistentFlags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")
	root.PersistentFlags().StringVar(&deviceFlag, "device", "", "Target device hostname")

	root.AddCommand(
		newRunCmd(),
		newBuildCmd(),
		newInitCmd(),
		newDiscoverCmd(),
		newDeviceCmd(),
		newOSCmd(),
		newAppsCmd(),
		newWifiCmd(),
		newAudioCmd(),
		newHardwareCmd(),
		newBluetoothCmd(),
		newAuthCmd(),
		newTelemetryCmd(),
		newCacheCmd(),
		newUpdateCmd(),
		newInfoCmd(),
	)

	root.Version = version.Version
	return root
}
