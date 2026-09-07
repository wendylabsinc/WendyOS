// Package commands defines all Cobra commands for the Wendy CLI.
package commands

import (
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/analytics"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/env"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
)

var (
	jsonOutput bool
	deviceFlag string
)

func NewRootCmd() *cobra.Command {
	// firstRun records whether this invocation showed the first-run analytics
	// notice in PreRunE, so PostRunE can avoid stacking another prompt on top.
	var firstRun bool

	root := &cobra.Command{
		Use:           "wendy",
		Short:         "Wendy CLI - Edge Computing Development Tool",
		Long:          "Wendy is a CLI for developing and deploying edge computing applications to WendyOS devices.",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// Skip heavy init for commands that don't need device/cloud setup.
			// __usb-setup and __t234-write run as root under sudo; skipping init
			// avoids config/analytics writes (and an update check) as root, and
			// keeps the first-run banner out of the helper's captured output.
			switch cmd.Name() {
			case "__ble-check", "__session-broker", "__usb-setup", "__t234-write", "open-browser":
				return nil
			}

			if !cmd.Root().PersistentFlags().Changed("json") && !isInteractiveTerminal() {
				jsonOutput = true
			}

			// Provider availability is probed lazily on first use (see
			// providers.ensureAvailable) rather than here: the probes shell out
			// to `docker`/`container` and most commands never consult a
			// provider at all.
			premark := phaseTimer()

			cfg, err := config.Load()
			if err != nil {
				return err
			}
			premark("  prerun: config.Load")

			firstRun = analytics.Init(cfg)
			premark("  prerun: analytics.Init")
			if firstRun {
				cmd.PrintErrln("Attention: The Wendy CLI collects anonymous analytics.")
				cmd.PrintErrln("They help us understand which commands are used most, identify common errors, and prioritize improvements.")
				cmd.PrintErrln("Analytics are enabled by default. If you'd like to opt-out, use the following command:")
				cmd.PrintErrln("  wendy analytics disable")
				cmd.PrintErrln("Or, set the following environment variable:")
				cmd.PrintErrln("  WENDY_ANALYTICS=false")

				cmd.PrintErrln("")
				cmd.PrintErrln("New to Wendy? Run `wendy tour` for a guided setup.")

				cfg.Analytics = &config.AnalyticsConfig{Enabled: true}
				if err := config.Save(cfg); err != nil {
					return err
				}
			}

			// Refresh MCP config and skills if the CLI was upgraded since the
			// user last ran `wendy mcp setup`. Runs synchronously here, before
			// the update-check goroutine below also mutates and saves cfg.
			maybeRefreshMCPSetup(cfg)
			premark("  prerun: maybeRefreshMCPSetup")

			// Reconcile credentials with the configured storage policy. Runs in
			// the synchronous zone: the update-check goroutine below saves cfg
			// too, and its Save must observe an already-migrated on-disk state.
			if config.MigrateSecretsIfNeeded(cfg) {
				cmd.PrintErrln("Moved wendy credentials into ~/.wendy/config.json.")
			}

			if dueCLIUpdateCheck(cfg) {
				scheduleCLIUpdateCheck(cfg)
			}
			premark("  prerun: dueCLIUpdateCheck")

			return nil
		},
		PersistentPostRunE: func(cmd *cobra.Command, args []string) error {
			// Surface a throttled tip about `wendy project optimize` after a
			// successful build/run (no-op for other commands and in CI).
			maybeShowOptimizeTip(cmd)
			maybeShowNextStep(cmd)

			// Surface any pending CLI-update notice first. If it showed a prompt,
			// don't stack the completion prompt on top of it this invocation.
			updateShown, err := notifyCLIUpdate(cmd)
			if err != nil {
				return err
			}

			maybePromptInstallCompletions(cmd, firstRun, updateShown)
			return nil
		},
	}

	root.PersistentFlags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")
	// Do not name the hidden --build-host flag here: this description shows in
	// every command's --help (persistent flag), and the E2E help specs guard
	// that the unreleased flag never leaks into help output.
	root.PersistentFlags().StringVar(&deviceFlag, "device", "", "Target device hostname; `wendy run` accepts a comma-separated list to deploy one build to several devices (needs a remote build host and --detach)")

	// Render the top-level command groups in the deliberate order below rather
	// than alphabetically, so e.g. "project" lists before "device".
	cobra.EnableCommandSorting = false

	root.AddGroup(
		&cobra.Group{ID: "develop", Title: "Develop & Deploy:"},
		&cobra.Group{ID: "manage", Title: "Manage:"},
		&cobra.Group{ID: "cloud", Title: "Cloud:"},
		&cobra.Group{ID: "settings", Title: "Settings:"},
	)

	// Develop & Deploy
	initCmd := newInitCmd()
	initCmd.GroupID = "develop"
	runCmd := newRunCmd()
	runCmd.GroupID = "develop"
	// `wendy install` is the surfaced alias for `wendy os install` (the `os`
	// group is hidden). A fresh command instance is used because a cobra
	// command can only be attached to one parent.
	installCmd := newOSInstallCmd()
	installCmd.GroupID = "develop"
	docsCmd := newDocsCmd()
	docsCmd.GroupID = "develop"

	// Manage
	projectCmd := newProjectCmd()
	projectCmd.GroupID = "manage"
	deviceCmd := newDeviceCmd()
	deviceCmd.GroupID = "manage"
	fleetCmd := newFleetCmd()
	fleetCmd.GroupID = "manage"

	// Cloud
	cloudCmd := newCloudCmd()
	cloudCmd.GroupID = "cloud"

	// Settings
	analyticsCmd := newAnalyticsCmd()
	analyticsCmd.GroupID = "settings"
	cacheCmd := newCacheCmd()
	cacheCmd.GroupID = "settings"

	// Hidden commands: still fully functional, just omitted from `wendy --help`
	// to keep the top-level surface focused on the common workflow. `auth`
	// remains a working command for back-compat ('wendy cloud login' is the
	// surfaced entry point); 'json' is already hidden in its constructor.
	buildCmd := newBuildCmd()
	buildCmd.Hidden = true
	watchCmd := newWatchCmd()
	watchCmd.Hidden = true
	jsonCmd := newJSONCmd()
	authCmd := newAuthCmd()
	authCmd.Hidden = true
	discoverCmd := newDiscoverCmd()
	discoverCmd.Hidden = true
	osCmd := newOSCmd()
	osCmd.Hidden = true
	infoCmd := newInfoCmd()
	infoCmd.Hidden = true
	utilsCmd := newUtilsCmd()
	utilsCmd.Hidden = true
	tourCmd := newTourCmd()
	tourCmd.GroupID = "develop"
	mcpCmd := newMCPCmd()
	mcpCmd.Hidden = true
	completionCmd := newCompletionCmd()
	completionCmd.Hidden = true
	// Keep a valid group on the (hidden) completion command so the help/
	// completion group wiring below stays consistent.
	completionCmd.GroupID = "settings"

	// Hidden command used by a subprocess to test CoreBluetooth access.
	// The main process spawns a child process that runs this command so
	// the child gets a fresh Obj-C runtime and can safely probe
	// CoreBluetooth without risking SIGABRT in the long-lived parent.
	bleCheckCmd := &cobra.Command{
		Use:    "__ble-check",
		Hidden: true,
		Run: func(cmd *cobra.Command, args []string) {
			os.Exit(discovery.RunBLECheck())
		},
	}

	var sessionSpec string
	var sessionParentLease bool
	sessionBrokerCmd := &cobra.Command{
		Use:    "__session-broker",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// --parent-lease means the launcher passed its lease pipe's read
			// end as fd 3 (exec.Cmd.ExtraFiles); its EOF is the exact "parent
			// invocation exited" signal.
			var parentLease *os.File
			if sessionParentLease {
				parentLease = os.NewFile(3, "parent-lease")
			}
			return sessionbroker.Run(cmd.Context(), sessionSpec, sessionbroker.DefaultIdleTTL, parentLease)
		},
	}
	sessionBrokerCmd.Flags().StringVar(&sessionSpec, "spec", "", "Encoded session connection recipe")
	sessionBrokerCmd.Flags().BoolVar(&sessionParentLease, "parent-lease", false, "Parent invocation's lease pipe was inherited as fd 3")
	_ = sessionBrokerCmd.MarkFlagRequired("spec")

	var bmapDevice, bmapFile, bmapSource string
	var bmapWriters int
	bmapWriteCmd := &cobra.Command{
		Use:    "__bmap-write",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if bmapSource != "" {
				return runBmapWriteSeekable(bmapDevice, bmapFile, bmapSource, bmapWriters, cmd.OutOrStdout())
			}
			return runBmapWrite(bmapDevice, bmapFile, cmd.InOrStdin())
		},
	}
	bmapWriteCmd.Flags().StringVar(&bmapDevice, "device", "", "Raw device path to write")
	bmapWriteCmd.Flags().StringVar(&bmapFile, "bmap", "", "Path to the .bmap file")
	bmapWriteCmd.Flags().StringVar(&bmapSource, "source", "", "Path to the seekable .img.zst source")
	bmapWriteCmd.Flags().IntVar(&bmapWriters, "writers", 0, "Concurrent writer goroutines (0 = sequential default)")

	// Visible commands are added in display order (command sorting is disabled
	// above); hidden commands follow and never appear in help.
	root.AddCommand(
		// Develop & Deploy
		initCmd,
		runCmd,
		installCmd,
		docsCmd,
		// Manage
		projectCmd,
		deviceCmd,
		newVMCmd(),
		fleetCmd,
		// Cloud
		cloudCmd,
		// Settings
		analyticsCmd,
		cacheCmd,
		// Hidden
		bleCheckCmd,
		sessionBrokerCmd,
		bmapWriteCmd,
		newT234WriteCmd(),
		newUSBSetupHiddenCmd(),
		watchCmd,
		buildCmd,
		jsonCmd,
		authCmd,
		discoverCmd,
		osCmd,
		infoCmd,
		utilsCmd,
		tourCmd,
		mcpCmd,
		completionCmd,
	)

	root.SetHelpCommandGroupID("settings")
	root.SetCompletionCommandGroupID("settings")

	rejectStrayArguments(root)

	root.Version = version.Version
	return root
}

// rejectStrayArguments gives every command that takes no positional arguments a
// NoArgs validator, so a stray word is reported instead of silently dropped.
//
// Without a validator cobra defaults to accepting anything, which means
// `wendy device wifi connect MyNetwork` discards "MyNetwork" and proceeds as if
// no network had been named -- the SSID is supplied with --ssid. Around ninety
// commands were in that state; none of them had opted into it deliberately.
//
// A command is only treated as argument-free when its Use string declares no
// placeholder. Anything documenting a positional, such as "logs [app]" or
// "record [topics...]", already states its own contract and is left alone, as is
// any command that already sets Args. Commands that disable Cobra's flag parser
// are also left alone: their remaining argv is an application-defined protocol,
// not a list of positional arguments for Cobra to reject.
func rejectStrayArguments(cmd *cobra.Command) {
	for _, child := range cmd.Commands() {
		rejectStrayArguments(child)
	}
	if !cmd.Runnable() || cmd.Args != nil || cmd.DisableFlagParsing {
		return
	}
	for _, token := range strings.Fields(cmd.Use)[1:] {
		if token == "[flags]" {
			continue
		}
		if strings.HasPrefix(token, "<") || strings.HasPrefix(token, "[") {
			return
		}
	}
	cmd.Args = cobra.NoArgs
}

// nextStepHint returns a one-line suggestion for the next command to run after
// commandPath succeeds, or "" when there is no suggestion. Keyed off the full
// cobra command path (e.g. "wendy device info").
func nextStepHint(commandPath string) string {
	switch commandPath {
	case "wendy discover":
		return "Next: run `wendy init` to create an app, then `wendy run` to deploy it."
	case "wendy device info", "wendy device top", "wendy device apps list":
		return "Next: run `wendy run` to build and deploy an app to this device."
	case "wendy run":
		return "Next: run `wendy device logs` to stream your app's logs."
	}
	return ""
}

// maybeShowNextStep prints a next-step hint after a successful command. cobra
// only runs PersistentPostRunE when RunE succeeded, so this is success-only. It
// is suppressed for JSON output, non-interactive terminals, and CI.
func maybeShowNextStep(cmd *cobra.Command) {
	if jsonOutput || !isInteractiveTerminal() || env.IsCI() {
		return
	}
	if hint := nextStepHint(cmd.CommandPath()); hint != "" {
		cmd.PrintErrln(hint)
	}
}

// newUSBSetupHiddenCmd builds the hidden "__usb-setup" subcommand. It is the
// privileged half of the USB-C auto-setup flow: maybeOfferUSBSetup re-execs the
// CLI as `sudo wendy __usb-setup --iface <iface>` so the NetworkManager + udev
// changes run as root. It is hidden because users never invoke it directly.
func newUSBSetupHiddenCmd() *cobra.Command {
	var iface string
	cmd := &cobra.Command{
		Use:    "__usb-setup",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUSBSetup(cmd.Context(), iface, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&iface, "iface", "", "USB gadget interface to configure")
	return cmd
}
