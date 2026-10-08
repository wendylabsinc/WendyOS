package commands

import (
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// showFirstRunNotice prints the one-time analytics notice and records in cfg
// that it has been shown.
//
// Saving is best-effort: a read-only config dir (a sandbox, a locked-down CI
// home) must not fail every command before it even starts. The cost of a
// failed save is only that the notice repeats next run, which the warning says.
//
// The `wendy tour` hint is shown only on an interactive terminal, because
// tour refuses to run without one.
func showFirstRunNotice(cmd *cobra.Command, cfg *config.Config) {
	cmd.PrintErrln("Attention: The Wendy CLI collects anonymous analytics.")
	cmd.PrintErrln("They help us understand which commands are used most, identify common errors, and prioritize improvements.")
	cmd.PrintErrln("Analytics are enabled by default. If you'd like to opt-out, use the following command:")
	cmd.PrintErrln("  wendy analytics disable")
	cmd.PrintErrln("Or, set the following environment variable:")
	cmd.PrintErrln("  WENDY_ANALYTICS=false")

	if isInteractiveTerminal() {
		cmd.PrintErrln("")
		cmd.PrintErrln("New to Wendy? Run `wendy tour` for a guided setup.")
	}

	cfg.Analytics = &config.AnalyticsConfig{Enabled: true}
	if err := config.Save(cfg); err != nil {
		cmd.PrintErrf("Warning: could not save the Wendy CLI config (%v); continuing. This notice will show again until it can be saved.\n", err)
	}
}
