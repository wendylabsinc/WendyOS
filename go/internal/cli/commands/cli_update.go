package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/cliupdate"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/env"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
)

type cliUpdaterDeps struct {
	check       func(context.Context) (string, error)
	detect      func(context.Context) (cliupdate.Plan, error)
	apply       func(context.Context, cliupdate.Plan, string, string, io.Reader, io.Writer, io.Writer) (cliupdate.Result, error)
	interactive func() bool
	isCI        func() bool
	initialize  func(*cobra.Command)
	confirm     func(*cobra.Command) (bool, error)
	track       func(*cobra.Command, string, string, string, string)
	record      func(string) error
}

func defaultCLIUpdaterDeps() cliUpdaterDeps {
	return cliUpdaterDeps{
		check: checkLatestReleaseContext, detect: pluginAwareDetect(cliupdate.Detect), apply: cliupdate.Apply,
		interactive: isInteractiveTerminal,
		isCI:        env.IsCI,
		initialize: func(cmd *cobra.Command) {
			if runsAsForeignUser() {
				return
			}
			cfg, err := config.Load()
			if err != nil {
				return
			}
			initializeCommandAnalytics(cmd, cfg, true)
		},
		confirm: func(cmd *cobra.Command) (bool, error) {
			return tui.ConfirmDefaultYes("Update now?", tea.WithInput(cmd.InOrStdin()), tea.WithOutput(cmd.ErrOrStderr()))
		},
		track: trackCLIUpdate, record: recordVerifiedCLIUpdate,
	}
}

// pluginAwareDetect wraps detect so that a CLI the Wendy plugin's launcher
// installed gets the plugin's advice. No updater that cliupdate.Detect
// recognizes owns that copy, so Detect would call it a direct install.
func pluginAwareDetect(detect func(context.Context) (cliupdate.Plan, error)) func(context.Context) (cliupdate.Plan, error) {
	return func(ctx context.Context) (cliupdate.Plan, error) {
		p, err := detect(ctx)
		if err != nil || !pluginManagedCLI() {
			return p, err
		}
		p.Method, p.Supported, p.Commands = "plugin", false, nil
		p.Guidance = cliupdate.PluginAdvice(runtime.GOOS)
		return p, nil
	}
}

type cliUpdateReport struct {
	CurrentVersion      string         `json:"current_version"`
	LatestStableVersion string         `json:"latest_stable_version,omitempty"`
	Status              string         `json:"status"`
	UpdateAvailable     bool           `json:"update_available"`
	Installation        cliupdate.Plan `json:"installation"`
	VerifiedVersion     string         `json:"verified_version,omitempty"`
	RestartMCP          bool           `json:"restart_mcp,omitempty"`
}

func newCLIUpdateCmd() *cobra.Command { return newCLIUpdateCmdWithDeps(defaultCLIUpdaterDeps()) }

func newCLIUpdateCmdWithDeps(d cliUpdaterDeps) *cobra.Command {
	var checkOnly, yes bool
	cmd := &cobra.Command{
		Use: "update", Short: "Check for a newer CLI release or update this installation",
		Long: "Check the latest stable release, identify the installation method, and update with its owning package manager when supported. Use --check for a fresh read-only check. Unsupported installations receive instructions; the updater never silently creates a second installation.",
		Args: cobra.NoArgs,
		// A check must not create configuration, refresh credentials, or launch
		// another updater. Actual updates emit their explicit lifecycle below.
		PersistentPreRunE:  func(*cobra.Command, []string) error { return nil },
		PersistentPostRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := d.detect(cmd.Context())
			if err != nil {
				return err
			}
			var latest string
			if checkOnly || p.Channel != "nightly" {
				latest, err = d.check(cmd.Context())
				if err != nil {
					return fmt.Errorf("could not check the latest CLI release: %w", err)
				}
				if strings.TrimSpace(latest) == "" {
					return errors.New("release service returned an empty version; update status is unknown")
				}
			}
			report := makeCLIUpdateReport(p, latest)
			if checkOnly {
				return printCLIUpdateReport(cmd, report)
			}
			if version.IsDev(version.Version) {
				return errors.New("this is a development CLI build; rebuild it from source or choose a release installation explicitly")
			}
			if p.Channel != "nightly" && !report.UpdateAvailable {
				return printCLIUpdateReport(cmd, report)
			}
			if !p.Supported {
				return errors.New(p.Guidance)
			}
			if !yes {
				if cliUpdateJSON(cmd) || d.isCI() || !d.interactive() {
					return errors.New("updating requires confirmation; run wendy update --yes to approve, or wendy update --check to inspect")
				}
				printCLIUpdatePlan(cmd, p)
				confirmed, err := d.confirm(cmd)
				if err != nil {
					return err
				}
				if !confirmed {
					fmt.Fprintln(cmd.ErrOrStderr(), "Update skipped. Run wendy update when you are ready.")
					return nil
				}
			}
			d.initialize(cmd)
			return performCLIUpdateWithPlan(cmd, d, p, latest)
		},
	}
	cmd.Flags().BoolVar(&checkOnly, "check", false, "Fetch the latest stable version and report update instructions without changing anything")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Approve the detected installation's update (required without an interactive terminal)")
	return cmd
}

func makeCLIUpdateReport(p cliupdate.Plan, latest string) cliUpdateReport {
	r := cliUpdateReport{CurrentVersion: version.Version, LatestStableVersion: latest, Status: "up_to_date", Installation: p}
	switch {
	case version.IsDev(version.Version):
		r.Status = "development_build"
	case p.Channel == "nightly":
		r.Status = "nightly_channel_not_checked"
	case version.CompareVersions(latest, version.Version) > 0:
		r.Status, r.UpdateAvailable = "update_available", true
	}
	return r
}

func cliUpdateJSON(cmd *cobra.Command) bool {
	value, _ := cmd.Flags().GetBool("json")
	return value
}

func printCLIUpdateReport(cmd *cobra.Command, r cliUpdateReport) error {
	if cliUpdateJSON(cmd) {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(r)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Wendy CLI %s; latest stable release: %s\n", r.CurrentVersion, r.LatestStableVersion)
	switch r.Status {
	case "development_build":
		fmt.Fprintln(cmd.OutOrStdout(), "This is a development build. Update its source checkout or select a release installation explicitly.")
	case "nightly_channel_not_checked":
		fmt.Fprintln(cmd.OutOrStdout(), "This installation follows nightly releases; the stable release check does not determine whether it is current. wendy update preserves the nightly channel.")
	case "update_available":
		fmt.Fprintln(cmd.OutOrStdout(), "A newer release is available. Run wendy update.")
	default:
		fmt.Fprintln(cmd.OutOrStdout(), "This CLI is at or newer than the latest stable release.")
	}
	if !r.Installation.Supported || r.Installation.Guidance != "" {
		fmt.Fprintln(cmd.OutOrStdout(), r.Installation.Guidance)
	}
	return nil
}

func printCLIUpdatePlan(cmd *cobra.Command, p cliupdate.Plan) {
	fmt.Fprintf(cmd.ErrOrStderr(), "Update this %s installation (%s):\n", p.Method, p.Executable)
	for _, command := range p.Commands {
		fmt.Fprintln(cmd.ErrOrStderr(), "  "+strings.Join(command, " "))
	}
}

// performCLIUpdate is also used after an already-confirmed ambient prompt.
// A target supplied by the notice is checked against the version on PATH after
// installation. It is never used to change a nightly installation to stable.
func performCLIUpdate(cmd *cobra.Command, expectedVersion string) error {
	d := defaultCLIUpdaterDeps()
	p, err := d.detect(cmd.Context())
	if err != nil {
		return err
	}
	if p.Channel == "nightly" && expectedVersion != "" {
		return errors.New("this installation follows nightly releases; run wendy update to preserve its channel")
	}
	if expectedVersion == "" && p.Channel != "nightly" {
		expectedVersion, err = d.check(cmd.Context())
		if err != nil {
			return err
		}
	}
	return performCLIUpdateWithPlan(cmd, d, p, expectedVersion)
}

func performCLIUpdateWithPlan(cmd *cobra.Command, d cliUpdaterDeps, p cliupdate.Plan, expectedVersion string) error {
	if !p.Supported {
		return errors.New(p.Guidance)
	}
	// Ambient updates already initialized telemetry in the root pre-run.
	// Reinitializing here could race with events the original command sent.
	d.track(cmd, "cli_update_started", "started", "", p.Method)
	result, err := d.apply(cmd.Context(), p, version.Version, expectedVersion, cmd.InOrStdin(), cmd.ErrOrStderr(), cmd.ErrOrStderr())
	if err != nil {
		if errors.Is(err, context.Canceled) {
			d.track(cmd, "cli_update_failed", "cancelled", "user_cancelled", p.Method)
		} else {
			d.track(cmd, "cli_update_failed", "failed", cliupdate.ErrorCode(err), p.Method)
		}
		return err
	}
	d.track(cmd, "cli_update_verified", "completed", "", p.Method)
	if p.Channel != "nightly" {
		if err := d.record(result.Version); err != nil {
			fmt.Fprintln(cmd.ErrOrStderr(), "The new CLI version is verified, but the update-notice cache could not be refreshed.")
		}
	}
	if cliUpdateJSON(cmd) {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(cliUpdateReport{CurrentVersion: version.Version, Status: "updated", Installation: p, VerifiedVersion: result.Version, RestartMCP: true})
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Updated and verified Wendy CLI %s (%s).\nRestart the Wendy MCP server or your AI app to load the new CLI and tools.\n", result.Version, result.Executable)
	return nil
}
