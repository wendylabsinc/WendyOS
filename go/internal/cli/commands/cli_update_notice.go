// Package commands - the post-run CLI update notice.
package commands

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/cliupdate"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
)

// notifyCLIUpdate surfaces a pending CLI-update notice recorded by the
// background update check. It returns whether a notice was shown (so callers can
// avoid stacking other prompts on top) and any error from performing an
// interactive update.
func notifyCLIUpdate(cmd *cobra.Command) (shown bool, err error) {
	// Load fresh config so we see any value written by the background
	// goroutine (possibly from a previous invocation).
	cfg, err := config.Load()
	if err != nil || cfg.AvailableCLIUpdate == "" {
		return false, nil
	}
	// Double-check: user may have updated since the goroutine last ran.
	if version.CompareVersions(cfg.AvailableCLIUpdate, version.Version) <= 0 {
		return false, nil
	}
	newVersion := cfg.AvailableCLIUpdate

	updateShellCmd := cliupdate.Command(runtime.GOOS)

	if jsonOutput || !isInteractiveTerminal() {
		// No prompt here, so nothing else clears AvailableCLIUpdate: remember
		// which release was announced and stay quiet until a newer one appears.
		if cfg.CLIUpdateNoticeShown == newVersion {
			return false, nil
		}
		msg := "\nA new version of the Wendy CLI is available: %s (you have %s)\nUpdate with: %s\n"
		if runtime.GOOS == "darwin" {
			msg += "  (if the tap is untrusted: brew trust wendylabsinc/tap)\n"
		}
		cmd.PrintErrf(msg, newVersion, version.Version, updateShellCmd)
		cfg.CLIUpdateNoticeShown = newVersion
		_ = config.Save(cfg) // best-effort: if it can't be saved, the notice just repeats
		return true, nil
	}

	cmd.PrintErrf("\nA new version of the Wendy CLI is available: %s (you have %s)\n", newVersion, version.Version)
	confirmed, promptErr := tui.ConfirmDefaultYes("Update now?", tea.WithOutput(os.Stderr))

	// Clear the stored version so the prompt doesn't reappear on the next
	// command regardless of the user's choice; it'll re-surface after the
	// next 24 h update check if still relevant.
	cfg.AvailableCLIUpdate = ""
	_ = config.Save(cfg)

	if promptErr != nil || !confirmed {
		cmd.PrintErrf("Run %q to update manually.\n", updateShellCmd)
		return true, nil
	}

	var runErr error
	switch runtime.GOOS {
	case "windows":
		c := exec.Command("winget", "upgrade", "WendyLabs.Wendy")
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		runErr = c.Run()
	case "darwin":
		for _, brewArgs := range [][]string{{"update"}, {"install", "wendy"}} {
			c := exec.Command("brew", brewArgs...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			if runErr = c.Run(); runErr != nil {
				break
			}
		}
	default:
		// Pipe the installer script directly into bash without shell interpolation.
		curl := exec.Command("curl", "-fsSL", "https://install.wendy.dev/cli.sh")
		bash := exec.Command("bash")
		curl.Stderr = os.Stderr
		bash.Stdout, bash.Stderr = os.Stdout, os.Stderr
		if bash.Stdin, runErr = curl.StdoutPipe(); runErr == nil {
			if runErr = curl.Start(); runErr == nil {
				if runErr = bash.Start(); runErr == nil {
					_ = curl.Wait()
					runErr = bash.Wait()
				}
			}
		}
	}
	if runErr != nil {
		return true, fmt.Errorf("update failed: %w", runErr)
	}
	return true, nil
}
