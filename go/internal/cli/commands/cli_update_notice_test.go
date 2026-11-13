package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/analytics"
	"github.com/wendylabsinc/wendy/go/internal/cli/pluginmode"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/cliupdate"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
)

// runNotifyCLIUpdate calls notifyCLIUpdate on a throwaway command and returns
// whether it reported a notice plus what it printed to stderr.
func runNotifyCLIUpdate(t *testing.T) (bool, string) {
	t.Helper()
	enableCLIUpdateChecks(t)
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	shown, err := notifyCLIUpdate(cmd)
	if err != nil {
		t.Fatalf("notifyCLIUpdate: %v", err)
	}
	return shown, stderr.String()
}

func setAvailableCLIUpdate(t *testing.T, tag string) {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.AvailableCLIUpdate = tag
	if err := config.Save(cfg); err != nil {
		t.Fatalf("config.Save: %v", err)
	}
}

// In non-interactive/JSON mode the notice used to print after every single
// command, because only the interactive prompt cleared the stored version.
func TestNotifyCLIUpdate_NonInteractiveShowsOncePerRelease(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	t.Setenv("WENDY_SECRET_STORE", "file")
	stubNonInteractive(t)
	prevVersion := version.Version
	version.Version = "2026.01.01-000000"
	t.Cleanup(func() { version.Version = prevVersion })

	setAvailableCLIUpdate(t, "2026.02.01-000000")

	shown, out := runNotifyCLIUpdate(t)
	if !shown || !strings.Contains(out, "2026.02.01-000000") {
		t.Fatalf("first run: shown=%v out=%q, want the notice", shown, out)
	}
	shown, out = runNotifyCLIUpdate(t)
	if shown || out != "" {
		t.Fatalf("second run repeated the notice: shown=%v out=%q", shown, out)
	}

	// A newer release is announced again — once.
	setAvailableCLIUpdate(t, "2026.03.01-000000")
	shown, out = runNotifyCLIUpdate(t)
	if !shown || !strings.Contains(out, "2026.03.01-000000") {
		t.Fatalf("newer release: shown=%v out=%q, want the notice", shown, out)
	}
	if shown, out = runNotifyCLIUpdate(t); shown || out != "" {
		t.Fatalf("newer release repeated: shown=%v out=%q", shown, out)
	}
}

// A read-only config dir can't record that the notice was shown. The notice
// then repeats, but it must never turn into an error that fails the command.
func TestNotifyCLIUpdate_ReadOnlyConfigDirNeverFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod cannot make a directory read-only on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	t.Setenv("WENDY_CONFIG_DIR", dir)
	t.Setenv("WENDY_SECRET_STORE", "file")
	stubNonInteractive(t)
	prevVersion := version.Version
	version.Version = "2026.01.01-000000"
	t.Cleanup(func() { version.Version = prevVersion })

	setAvailableCLIUpdate(t, "2026.02.01-000000")
	// Both the file and its dir: a read-only dir alone still lets the
	// existing config.json be rewritten in place.
	cfgFile := filepath.Join(dir, "config.json")
	if err := os.Chmod(cfgFile, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(dir, 0o700)
		_ = os.Chmod(cfgFile, 0o600)
	})

	for i := range 2 {
		shown, out := runNotifyCLIUpdate(t) // fails the test on any error
		if !shown || !strings.Contains(out, "2026.02.01-000000") {
			t.Fatalf("run %d: shown=%v out=%q, want the notice", i+1, shown, out)
		}
	}
}

func setupInteractiveCLIUpdate(t *testing.T) {
	enableCLIUpdateChecks(t)
	t.Setenv("SUDO_UID", "")
	t.Setenv("WENDY_ANALYTICS", "false")
	stubInteractive(t)
	oldJSON, oldVersion := jsonOutput, version.Version
	jsonOutput, version.Version = false, "2026.09.29-120000"
	oldDetect := detectCLIUpdate
	detectCLIUpdate = func(context.Context) (cliupdate.Plan, error) {
		return cliupdate.Plan{Method: "brew", Channel: "stable", Supported: true}, nil
	}
	t.Cleanup(func() { jsonOutput, version.Version, detectCLIUpdate = oldJSON, oldVersion, oldDetect })
	setTempConfig(t, &config.Config{Analytics: &config.AnalyticsConfig{Enabled: false}, LatestCLIRelease: "2026.10.05-134249", LastCLIUpdateCheck: time.Now().UTC().Format(time.RFC3339)})
}

func TestCLIUpdateDeclineSnoozesWithoutLosingReleaseOrConfig(t *testing.T) {
	setupInteractiveCLIUpdate(t)
	previous := confirmCLIUpdate
	t.Cleanup(func() { confirmCLIUpdate = previous })
	prompts := 0
	confirmCLIUpdate = func(*cobra.Command, string) (bool, error) {
		prompts++
		err := config.Update(func(cfg *config.Config) (bool, error) {
			cfg.DefaultDevice = "chosen-while-prompt-open"
			return true, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return false, nil
	}
	if shown, _ := runNotifyCLIUpdate(t); !shown {
		t.Fatal("first prompt missing")
	}
	if shown, _ := runNotifyCLIUpdate(t); shown {
		t.Fatal("snoozed prompt repeated")
	}
	cfg, _ := config.Load()
	if cfg.LatestCLIRelease != "2026.10.05-134249" || cfg.DefaultDevice != "chosen-while-prompt-open" {
		t.Fatal("prompt lost release or concurrent config")
	}
	cfg.CLIUpdatePromptedAt = time.Now().Add(-25 * time.Hour).UTC().Format(time.RFC3339)
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if shown, _ := runNotifyCLIUpdate(t); !shown || prompts != 2 {
		t.Fatal("reminder did not return after snooze")
	}
}

func TestAcceptedCLIUpdateFailureDoesNotFailSuccessfulCommand(t *testing.T) {
	setupInteractiveCLIUpdate(t)
	previousConfirm, previousApply := confirmCLIUpdate, applyCLIUpdate
	t.Cleanup(func() { confirmCLIUpdate, applyCLIUpdate = previousConfirm, previousApply })
	prompts := 0
	confirmCLIUpdate = func(*cobra.Command, string) (bool, error) {
		prompts++
		return true, nil
	}
	applyCLIUpdate = func(*cobra.Command, string) error { return errors.New("installer unavailable") }
	root := NewRootCmd()
	root.AddCommand(&cobra.Command{Use: "update-test-success", RunE: func(*cobra.Command, []string) error { return nil }})
	root.SetArgs([]string{"update-test-success"})
	var stderr bytes.Buffer
	root.SetErr(&stderr)
	cmd, err := root.ExecuteC()
	if err != nil {
		t.Fatalf("updater replaced successful command result: %v", err)
	}
	if prompts != 0 || stderr.Len() != 0 {
		t.Fatalf("optional update ran before command outcome was recorded: prompts=%d stderr=%q", prompts, stderr.String())
	}
	FinishInvocation(cmd, err)
	if prompts != 1 {
		t.Fatalf("FinishInvocation offered %d updates, want 1", prompts)
	}
	if !strings.Contains(stderr.String(), "Your command completed successfully") {
		t.Fatalf("missing independent failure guidance: %s", stderr.String())
	}
}

func TestFailedCommandUpdateNoticeNeverPrompts(t *testing.T) {
	setupInteractiveCLIUpdate(t)
	previous := confirmCLIUpdate
	t.Cleanup(func() { confirmCLIUpdate = previous })
	confirmCLIUpdate = func(*cobra.Command, string) (bool, error) {
		t.Fatal("failed command must not prompt")
		return false, nil
	}
	cmd := &cobra.Command{}
	cmd.SetContext(context.WithValue(context.Background(), cliUpdateNoticeEligible{}, true))
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	FinishInvocation(cmd, errors.New("command failed"))
	if !strings.Contains(stderr.String(), "wendy update") {
		t.Fatal("failed command omitted update guidance")
	}
}

func TestFinishInvocationSkipsUnexecutedAndReportedFailures(t *testing.T) {
	setupInteractiveCLIUpdate(t)
	previous := confirmCLIUpdate
	t.Cleanup(func() { confirmCLIUpdate = previous })
	confirmCLIUpdate = func(*cobra.Command, string) (bool, error) {
		t.Fatal("unexecuted or already reported command must not prompt")
		return false, nil
	}
	for _, tc := range []struct {
		name string
		cmd  *cobra.Command
		err  error
	}{
		{name: "nil command"},
		{name: "no context", cmd: &cobra.Command{}, err: errors.New("unknown subcommand")},
		{name: "no post-run", cmd: &cobra.Command{}},
		{name: "reported failure", cmd: &cobra.Command{}, err: &ReportedExitError{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if tc.cmd != nil {
				tc.cmd.SetErr(&stderr)
				if tc.name != "no context" {
					tc.cmd.SetContext(context.WithValue(context.Background(), cliUpdateNoticeEligible{}, true))
				}
			}
			FinishInvocation(tc.cmd, tc.err)
			if stderr.Len() != 0 {
				t.Fatalf("unexpected guidance: %q", stderr.String())
			}
		})
	}
}

func TestCLIUpdateCIAndOptOutStayQuiet(t *testing.T) {
	setupInteractiveCLIUpdate(t)
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	for _, key := range []string{"CI", "WENDY_NO_UPDATE_CHECK"} {
		t.Setenv(key, "1")
		if shown, err := notifyCLIUpdate(cmd); shown || err != nil || stderr.Len() != 0 {
			t.Fatalf("%s was not quiet", key)
		}
		t.Setenv(key, "")
	}
}

func TestCLIUpdateNightlyIsNotOfferedStable(t *testing.T) {
	setupInteractiveCLIUpdate(t)
	detectCLIUpdate = func(context.Context) (cliupdate.Plan, error) {
		return cliupdate.Plan{Method: "brew", Channel: "nightly", Supported: true}, nil
	}
	if shown, _ := runNotifyCLIUpdate(t); shown {
		t.Fatal("offered stable target to nightly installation")
	}
}

func TestVerifiedCLIUpdateKeepsNewerConcurrentRelease(t *testing.T) {
	setupInteractiveCLIUpdate(t)
	cfg, _ := config.Load()
	cfg.AvailableCLIUpdate = "2026.10.06-120000"
	cfg.LatestCLIRelease = cfg.AvailableCLIUpdate
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if err := recordVerifiedCLIUpdate("2026.10.05-134249"); err != nil {
		t.Fatal(err)
	}
	saved, _ := config.Load()
	if saved.AvailableCLIUpdate != cfg.AvailableCLIUpdate || saved.LatestCLIRelease != cfg.LatestCLIRelease {
		t.Fatal("verification erased a newer release")
	}
}

func TestCLIUpdateNoticeDoesNotWaitForAuthRefresh(t *testing.T) {
	setupInteractiveCLIUpdate(t)
	previous := confirmCLIUpdate
	confirmCLIUpdate = func(*cobra.Command, string) (bool, error) { return false, nil }
	t.Cleanup(func() { confirmCLIUpdate = previous })
	unlock, err := acquireAuthRefreshLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	started := time.Now()
	shown, _ := runNotifyCLIUpdate(t)
	if !shown || time.Since(started) > time.Second {
		t.Fatal("update reminder blocked on authentication refresh")
	}
}

func TestShellCompletionDoesNotCheckOrAnnounceCLIUpdates(t *testing.T) {
	setupInteractiveCLIUpdate(t)
	previous := scheduleCLIUpdateCheck
	calls := 0
	scheduleCLIUpdateCheck = func() { calls++ }
	t.Cleanup(func() { scheduleCLIUpdateCheck = previous })
	for _, name := range []string{"__complete", "__completeNoDesc"} {
		root := NewRootCmd()
		root.SetArgs([]string{name, ""})
		var out, stderr bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&stderr)
		cmd, err := root.ExecuteC()
		if err != nil {
			t.Fatal(err)
		}
		FinishInvocation(cmd, err)
		if calls != 0 || strings.Contains(stderr.String(), "new version") {
			t.Fatalf("completion triggered update: %s", stderr.String())
		}
	}
}

func TestVerifiedCLIUpdateInformsAnOlderMCPProcess(t *testing.T) {
	setupInteractiveCLIUpdate(t)
	if err := config.Save(&config.Config{}); err != nil {
		t.Fatal(err)
	}
	if err := recordVerifiedCLIUpdate("2026.10.05-134249"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load()
	if cfg.LatestCLIRelease != "2026.10.05-134249" || cfg.AvailableCLIUpdate != "" {
		t.Fatal("verified version was not retained for an older process")
	}
}

// runUpdateNotice poses as the binary at exe, records the release
// 2026.02.01-000000 over a 2026.01.01-000000 CLI, and returns whether
// notifyCLIUpdate showed a notice and what it printed. Detection reports a
// supported Homebrew install, and an update popup fails the test.
func runUpdateNotice(t *testing.T, exe string, interactive bool) (bool, string) {
	t.Helper()
	t.Setenv("WENDY_SECRET_STORE", "file")
	t.Setenv("WENDY_ANALYTICS", "false")
	t.Setenv("SUDO_UID", "")
	oldJSON, oldVersion := jsonOutput, version.Version
	jsonOutput, version.Version = false, "2026.01.01-000000"
	oldExe, oldDetect, oldConfirm := cliExecutable, detectCLIUpdate, confirmCLIUpdate
	cliExecutable = func() (string, error) { return exe, nil }
	detectCLIUpdate = func(context.Context) (cliupdate.Plan, error) {
		return cliupdate.Plan{Method: "brew", Channel: "stable", Supported: true}, nil
	}
	confirmCLIUpdate = func(*cobra.Command, string) (bool, error) {
		t.Error("the notice opened an update popup")
		return false, nil
	}
	t.Cleanup(func() {
		jsonOutput, version.Version = oldJSON, oldVersion
		cliExecutable, detectCLIUpdate, confirmCLIUpdate = oldExe, oldDetect, oldConfirm
	})
	if interactive {
		stubInteractive(t)
	} else {
		stubNonInteractive(t)
	}
	setAvailableCLIUpdate(t, "2026.02.01-000000")
	return runNotifyCLIUpdate(t)
}

func TestNotifyCLIUpdate_PluginManagedCLI(t *testing.T) {
	home := isolateAIToolHome(t)
	managed := filepath.Join(home, ".wendy", "cli", "2026.01.01-000000", "wendy")
	if err := os.MkdirAll(filepath.Dir(managed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(managed, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// wendy update recognizes no updater for the launcher's copy, so even an
	// interactive terminal gets advice rather than a prompt. The plugin pins
	// its CLI version, which can trail the latest release, and its launcher
	// runs an installed wendy ahead of its own copy: the notice names both
	// ways to update and promises no version from the plugin.
	shown, out := runUpdateNotice(t, managed, true)
	if !shown || !strings.Contains(out, cliupdate.PluginAdvice(runtime.GOOS)) {
		t.Errorf("notice lacks the plugin advice: shown=%v\n%s", shown, out)
	}
	if strings.Contains(out, "Update with:") || strings.Contains(out, "to get 2026.02.01-000000") {
		t.Errorf("notice points at wendy update or promises the new version from the plugin:\n%s", out)
	}

	// Like the non-interactive notice, it shows once per release.
	if shown, again := runUpdateNotice(t, managed, true); shown || again != "" {
		t.Errorf("second run repeated the plugin notice: shown=%v out=%q", shown, again)
	}
}

// WENDY_PLUGIN alone does not make a CLI plugin-managed: a user whose PATH
// wendy (Homebrew) is new enough runs that binary under the plugin, and
// wendy update still updates it.
func TestNotifyCLIUpdate_HomebrewCLIUnderPluginKeepsUpdateAdvice(t *testing.T) {
	home := isolateAIToolHome(t)
	t.Setenv(pluginmode.EnvVar, "claude")
	brew := filepath.Join(home, "homebrew", "bin", "wendy")

	_, out := runUpdateNotice(t, brew, false)
	if strings.Contains(out, "came with the Wendy plugin") {
		t.Errorf("a Homebrew CLI was reported as plugin-managed:\n%s", out)
	}
	if !strings.Contains(out, "Update with: wendy update") {
		t.Errorf("notice lost its wendy update advice:\n%s", out)
	}
}

func TestCLIUpdateSnoozeIncludesNewReleases(t *testing.T) {
	setupInteractiveCLIUpdate(t)
	previous := confirmCLIUpdate
	t.Cleanup(func() { confirmCLIUpdate = previous })
	confirmCLIUpdate = func(*cobra.Command, string) (bool, error) { return false, nil }
	if shown, _ := runNotifyCLIUpdate(t); !shown {
		t.Fatal("first prompt missing")
	}
	if err := config.Update(func(cfg *config.Config) (bool, error) {
		cfg.LatestCLIRelease = "2026.10.06-120000"
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if shown, _ := runNotifyCLIUpdate(t); shown {
		t.Fatal("new release interrupted the promised 24-hour snooze")
	}
}

func TestCLIUpdateDismissalOrFailedDownloadDoesNotSnooze(t *testing.T) {
	for _, tc := range []struct {
		name      string
		accepted  bool
		promptErr error
		event     string
	}{
		{name: "escape", promptErr: tui.ErrCancelled, event: "cli_update_dismissed"},
		{name: "interrupted", promptErr: fmt.Errorf("CLI update prompt: %w", tea.ErrInterrupted), event: "cli_update_dismissed"},
		{name: "download failed", accepted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupInteractiveCLIUpdate(t)
			previous, previousApply := confirmCLIUpdate, applyCLIUpdate
			t.Cleanup(func() { confirmCLIUpdate, applyCLIUpdate = previous, previousApply })
			confirmCLIUpdate = func(*cobra.Command, string) (bool, error) { return tc.accepted, tc.promptErr }
			applyCLIUpdate = func(_ *cobra.Command, latest string) error {
				if latest != "2026.10.05-134249" {
					t.Fatalf("wrong download target: %s", latest)
				}
				return errors.New("download failed")
			}
			events := map[string]int{}
			analytics.SetTrackHookForTesting(func(event string, _ map[string]string) { events[event]++ })
			t.Cleanup(func() { analytics.SetTrackHookForTesting(nil) })
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			cmd.SetErr(&bytes.Buffer{})
			for range 2 {
				if shown, _ := notifyCLIUpdate(cmd); !shown {
					t.Fatal("unanswered or failed update was snoozed")
				}
			}
			cfg, _ := config.Load()
			if cfg.CLIUpdatePromptedAt != "" {
				t.Fatal("recorded a snooze without explicit deferral")
			}
			if tc.event != "" && events[tc.event] != 2 {
				t.Fatalf("events=%v, want %s twice", events, tc.event)
			}
		})
	}
}

// A dialog that cannot open must not repeat the notice after every command.
func TestCLIUpdatePromptFailureSnoozes(t *testing.T) {
	setupInteractiveCLIUpdate(t)
	previous := confirmCLIUpdate
	t.Cleanup(func() { confirmCLIUpdate = previous })
	prompts := 0
	confirmCLIUpdate = func(*cobra.Command, string) (bool, error) {
		prompts++
		return false, errors.New("CLI update prompt: error entering raw mode")
	}
	events := map[string]int{}
	analytics.SetTrackHookForTesting(func(event string, _ map[string]string) { events[event]++ })
	t.Cleanup(func() { analytics.SetTrackHookForTesting(nil) })
	if shown, out := runNotifyCLIUpdate(t); !shown || !strings.Contains(out, "wendy update") || strings.Contains(out, "Remind you tomorrow") {
		t.Fatalf("first run: shown=%v out=%q", shown, out)
	}
	if shown, out := runNotifyCLIUpdate(t); shown || out != "" || prompts != 1 {
		t.Fatalf("second run: shown=%v out=%q prompts=%d", shown, out, prompts)
	}
	if events["cli_update_dismissed"] != 1 || events["cli_update_deferred"] != 0 {
		t.Fatalf("events=%v", events)
	}
}

func TestCLIUpdateJSONNeverPrompts(t *testing.T) {
	setupInteractiveCLIUpdate(t)
	jsonOutput = true
	previous := confirmCLIUpdate
	t.Cleanup(func() { confirmCLIUpdate = previous })
	confirmCLIUpdate = func(*cobra.Command, string) (bool, error) { t.Fatal("JSON opened popup"); return false, nil }
	if shown, out := runNotifyCLIUpdate(t); !shown || !strings.Contains(out, "wendy update") {
		t.Fatal("JSON mode lost its noninteractive guidance")
	}
}
