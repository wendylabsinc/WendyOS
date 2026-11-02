// Package commands - the CLI update reminder.
package commands

import (
	"context"
	"errors"
	"runtime"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/analytics"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/cliupdate"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
)

var confirmCLIUpdate = func(cmd *cobra.Command, latest string) (bool, error) {
	return tui.PromptCLIUpdate(version.Version, latest, tea.WithContext(cmd.Context()), tea.WithInput(cmd.InOrStdin()), tea.WithOutput(cmd.ErrOrStderr()))
}
var applyCLIUpdate = performCLIUpdate
var detectCLIUpdate = cliupdate.Detect

type cliUpdateNoticeEligible struct{}
type postCommandFirstRun struct{}

// FinishInvocation offers optional guidance after the command's outcome has
// been recorded. Call it before closing analytics so update events still flush.
// Its prompts and updates never change the original command's result.
func FinishInvocation(cmd *cobra.Command, err error) {
	if cmd == nil {
		return
	}
	if err != nil {
		var reported *ReportedExitError
		if !errors.As(err, &reported) {
			NotifyCLIUpdateAfterFailure(cmd)
		}
		return
	}
	ctx := cmd.Context()
	if ctx == nil || runsAsForeignUser() {
		return
	}
	// Presence means the successful command ran the normal post-run hook.
	// Help, validation failures, and commands with their own hook stay exempt.
	firstRun, eligible := ctx.Value(postCommandFirstRun{}).(bool)
	if !eligible {
		return
	}
	var updateShown bool
	if ctx.Value(cliUpdateNoticeEligible{}) == true {
		waitCLIUpdateCheck(ctx)
		var updateErr error
		updateShown, updateErr = notifyCLIUpdate(cmd)
		if updateErr != nil {
			cmd.PrintErrf("CLI update did not complete: %v\nYour command completed successfully. Retry with `wendy update`.\n", updateErr)
		}
	}
	maybePromptInstallCompletions(cmd, firstRun, updateShown)
}

func backgroundCLIUpdatesDisabled() bool {
	return cliupdate.BackgroundChecksDisabled()
}

// notifyCLIUpdate is best effort. The caller must preserve the outcome of the
// command that finished, even when a separately accepted update fails.
func notifyCLIUpdate(cmd *cobra.Command) (bool, error) {
	return showCLIUpdateNotice(cmd, true)
}

func showCLIUpdateNotice(cmd *cobra.Command, allowPrompt bool) (bool, error) {
	if backgroundCLIUpdatesDisabled() || runsAsForeignUser() || version.IsDev(version.Version) {
		return false, nil
	}
	cfg, err := config.Load()
	if err != nil {
		return false, nil
	}
	latest := cfg.LatestCLIRelease
	if latest == "" {
		latest = cfg.AvailableCLIUpdate
	}
	if !validCLIReleaseTag(latest) || version.CompareVersions(latest, version.Version) <= 0 {
		return false, nil
	}

	interactive := allowPrompt && !jsonOutput && isInteractiveTerminal()
	if interactive && timestampWithin(cfg.CLIUpdatePromptedAt, cliUpdateCheckInterval, time.Now()) {
		return false, nil
	}
	if !interactive && cfg.CLIUpdateNoticeShown == latest {
		return false, nil
	}
	method := "unknown"
	if interactive {
		detectCtx, cancelDetect := context.WithTimeout(cmd.Context(), time.Second)
		plan, detectErr := detectCLIUpdate(detectCtx)
		cancelDetect()
		if detectErr != nil {
			interactive = false
		}
		// Stable-release metadata cannot tell a nightly installation to change channels.
		if detectErr == nil && plan.Channel == "nightly" {
			return false, nil
		}
		if detectErr == nil {
			method = plan.Method
		}
		if !plan.Supported {
			interactive = false
		}
	}
	if !interactive && cfg.CLIUpdateNoticeShown == latest {
		return false, nil
	}
	cmd.PrintErrf("\nA new version of the Wendy CLI is available: %s (you have %s)\nUpdate with: %s\n", latest, version.Version, cliupdate.Command(runtime.GOOS))
	trackCLIUpdate(cmd, "cli_update_offered", "completed", "", method)
	if !interactive {
		_ = updateCLIState(cmd.Context(), func(current *config.Config) {
			current.CLIUpdateNoticeShown = latest
		})
		return true, nil
	}
	confirmed, promptErr := confirmCLIUpdate(cmd, latest)
	if promptDismissed(promptErr) {
		// Closing or interrupting the dialog is not an answer. Offer it again
		// next time rather than snoozing for a day.
		trackCLIUpdate(cmd, "cli_update_dismissed", "cancelled", "user_cancelled", method)
		return true, nil
	}
	if promptErr != nil || !confirmed {
		// A dialog that cannot open would fail again after every command, so
		// snooze it like a deferral: the notice above already showed the command.
		_ = updateCLIState(cmd.Context(), func(current *config.Config) {
			current.CLIUpdatePromptVersion = latest
			current.CLIUpdatePromptedAt = time.Now().UTC().Format(time.RFC3339)
		})
		if promptErr != nil {
			trackCLIUpdate(cmd, "cli_update_dismissed", "failed", "other", method)
			return true, nil
		}
		trackCLIUpdate(cmd, "cli_update_deferred", "cancelled", "user_cancelled", method)
		cmd.PrintErrln("Remind you tomorrow. Run `wendy update` whenever you are ready.")
		return true, nil
	}
	return true, applyCLIUpdate(cmd, latest)
}

// promptDismissed reports a user who closed or interrupted the update dialog,
// as opposed to a dialog that could not run.
func promptDismissed(err error) bool {
	return errors.Is(err, tui.ErrCancelled) || errors.Is(err, tea.ErrInterrupted) || errors.Is(err, context.Canceled)
}

// Failed commands do not run Cobra's post-run hook. Give them the same cached
// guidance without opening a prompt over the original error or cancellation.
func NotifyCLIUpdateAfterFailure(cmd *cobra.Command) {
	if cmd == nil || cmd.Context() == nil || cmd.Context().Value(cliUpdateNoticeEligible{}) != true {
		return
	}
	waitCLIUpdateCheck(cmd.Context())
	_, _ = showCLIUpdateNotice(cmd, false)
}

func updateCLIState(ctx context.Context, change func(*config.Config)) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	unlock, err := acquireAuthRefreshLock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	deadline, _ := ctx.Deadline()
	return config.UpdateWithTimeout(time.Until(deadline), func(cfg *config.Config) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		change(cfg)
		return true, nil
	})
}

func recordVerifiedCLIUpdate(verifiedVersion string) error {
	if runsAsForeignUser() {
		return nil
	}
	return updateCLIState(context.Background(), func(cfg *config.Config) {
		// This is verified on-disk evidence, not merely a successful installer
		// exit. Older MCP processes still need to compare against it.
		if validCLIReleaseTag(verifiedVersion) && version.CompareVersions(verifiedVersion, cfg.LatestCLIRelease) > 0 {
			cfg.LatestCLIRelease = verifiedVersion
		}
		if version.CompareVersions(cfg.AvailableCLIUpdate, verifiedVersion) <= 0 {
			cfg.AvailableCLIUpdate = ""
		}
	})
}

func trackCLIUpdate(cmd *cobra.Command, event, outcome, errorClass, method string) {
	switch method {
	case "apt", "rpm", "pacman":
		method = "package_manager"
	}
	analytics.TrackStage(cmd.Context(), event, "wendy update", outcome, errorClass, map[string]string{
		"stage": "cli_update", "install_method": method,
	})
}
