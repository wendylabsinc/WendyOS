// Package commands defines all Cobra commands for the Wendy CLI.
package commands

import (
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/analytics"
	"github.com/wendylabsinc/wendy/internal/cli/providers"
	"github.com/wendylabsinc/wendy/internal/shared/config"
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

			cfg, err := config.Load()
			if err != nil {
				return err
			}

			firstRun := analytics.Init(cfg)
			if firstRun {
				cmd.PrintErrln("Attention: The Wendy CLI collects anonymous analytics.")
				cmd.PrintErrln("They help us understand which commands are used most, identify common errors, and prioritize improvements.")
				cmd.PrintErrln("Analytics are enabled by default. If you'd like to opt-out, use the following command:")
				cmd.PrintErrln("  wendy analytics disable")
				cmd.PrintErrln("Or, set the following environment variable:")
				cmd.PrintErrln("  WENDY_ANALYTICS=false")

				cfg.Analytics = &config.AnalyticsConfig{Enabled: true}
				if err := config.Save(cfg); err != nil {
					return err
				}
			}

			return nil
		},
	}

	root.PersistentFlags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")
	root.PersistentFlags().StringVar(&deviceFlag, "device", "", "Target device hostname")

	root.AddGroup(
		&cobra.Group{ID: "project", Title: "Project Commands:"},
		&cobra.Group{ID: "cloud", Title: "Manage Your Cloud:"},
		&cobra.Group{ID: "devices", Title: "Manage Your Devices:"},
		&cobra.Group{ID: "misc", Title: "Misc.:"},
	)

	// Project Commands
	runCmd := newRunCmd()
	runCmd.GroupID = "project"
	buildCmd := newBuildCmd()
	buildCmd.GroupID = "project"
	initCmd := newInitCmd()
	initCmd.GroupID = "project"
	projectCmd := newProjectCmd()
	projectCmd.GroupID = "project"

	// Cloud Commands
	authCmd := newAuthCmd()
	authCmd.GroupID = "cloud"

	// Device Commands
	deviceCmd := newDeviceCmd()
	deviceCmd.GroupID = "devices"
	discoverCmd := newDiscoverCmd()
	discoverCmd.GroupID = "devices"
	osCmd := newOSCmd()
	osCmd.GroupID = "devices"
	appsCmd := newAppsCmd()
	appsCmd.GroupID = "devices"
	audioCmd := newAudioCmd()
	audioCmd.GroupID = "devices"
	bluetoothCmd := newBluetoothCmd()
	bluetoothCmd.GroupID = "devices"
	hardwareCmd := newHardwareCmd()
	hardwareCmd.GroupID = "devices"
	// Misc Commands
	cacheCmd := newCacheCmd()
	cacheCmd.GroupID = "misc"
	updateCmd := newUpdateCmd()
	updateCmd.GroupID = "misc"
	infoCmd := newInfoCmd()
	infoCmd.GroupID = "misc"
	analyticsCmd := newAnalyticsCmd()
	analyticsCmd.GroupID = "misc"

	root.AddCommand(
		runCmd,
		buildCmd,
		initCmd,
		projectCmd,
		authCmd,
		deviceCmd,
		discoverCmd,
		osCmd,
		appsCmd,
		audioCmd,
		bluetoothCmd,
		hardwareCmd,
		cacheCmd,
		updateCmd,
		infoCmd,
		analyticsCmd,
	)

	root.SetHelpCommandGroupID("misc")
	root.SetCompletionCommandGroupID("misc")

	root.Version = version.Version
	return root
}
