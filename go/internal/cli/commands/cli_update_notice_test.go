package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/pluginmode"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
)

// runNotifyCLIUpdate calls notifyCLIUpdate on a throwaway command and returns
// whether it reported a notice plus what it printed to stderr.
func runNotifyCLIUpdate(t *testing.T) (bool, string) {
	t.Helper()
	cmd := &cobra.Command{}
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

// runUpdateNotice records a pending update from 2026.01.01-000000 to
// 2026.02.01-000000, poses as the binary at exe, and returns what
// notifyCLIUpdate printed on stderr.
func runUpdateNotice(t *testing.T, exe string) string {
	t.Helper()
	t.Setenv("WENDY_SECRET_STORE", "file")
	oldVersion := version.Version
	version.Version = "2026.01.01-000000"
	t.Cleanup(func() { version.Version = oldVersion })
	oldExe := cliExecutable
	cliExecutable = func() (string, error) { return exe, nil }
	t.Cleanup(func() { cliExecutable = oldExe })
	oldTTY := isInteractiveTerminalFn
	isInteractiveTerminalFn = func() bool { return false }
	t.Cleanup(func() { isInteractiveTerminalFn = oldTTY })
	if err := config.Save(&config.Config{AvailableCLIUpdate: "2026.02.01-000000"}); err != nil {
		t.Fatal(err)
	}

	cmd := &cobra.Command{}
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	shown, err := notifyCLIUpdate(cmd)
	if err != nil || !shown {
		t.Fatalf("notifyCLIUpdate = %v, %v; want a shown notice", shown, err)
	}
	return stderr.String()
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

	out := runUpdateNotice(t, managed)
	if !strings.Contains(out, "managed by the Wendy plugin; update the plugin to get 2026.02.01-000000") {
		t.Errorf("notice does not point at the plugin:\n%s", out)
	}
	for _, installer := range []string{"brew", "winget", "cli.sh"} {
		if strings.Contains(out, installer) {
			t.Errorf("notice for a plugin-managed CLI suggests %s:\n%s", installer, out)
		}
	}

	// Like the non-interactive notice, it shows once per release.
	if shown, again := runNotifyCLIUpdate(t); shown || again != "" {
		t.Errorf("second run repeated the plugin notice: shown=%v out=%q", shown, again)
	}
}

// WENDY_PLUGIN alone does not make a CLI plugin-managed: a user whose PATH
// wendy (Homebrew) is new enough runs that binary under the plugin, and
// Homebrew is still how it updates.
func TestNotifyCLIUpdate_HomebrewCLIUnderPluginKeepsInstallerAdvice(t *testing.T) {
	home := isolateAIToolHome(t)
	t.Setenv(pluginmode.EnvVar, "claude")
	brew := filepath.Join(home, "homebrew", "bin", "wendy")

	out := runUpdateNotice(t, brew)
	if strings.Contains(out, "managed by the Wendy plugin") {
		t.Errorf("a Homebrew CLI was reported as plugin-managed:\n%s", out)
	}
	if !strings.Contains(out, "Update with:") {
		t.Errorf("notice lost its installer advice:\n%s", out)
	}
}
