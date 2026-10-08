package commands

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
)

// Under sudo with the user's HOME (the Thor flash's re-exec, or `sudo wendy` on
// macOS) the root command's per-user housekeeping would run as root: the update
// check creates a root-owned ~/.wendy/auth-refresh.lock that later breaks the
// user's token refreshes, and the update prompt would install the CLI as root.
// The user's own runs do that work instead.
func TestRootSkipsHousekeepingUnderSudo(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	t.Setenv("WENDY_SECRET_STORE", "file")
	t.Setenv("WENDY_ANALYTICS", "false")
	stubNonInteractive(t)
	prevJSON := jsonOutput
	t.Cleanup(func() { jsonOutput = prevJSON })
	prevVersion := version.Version
	version.Version = "2026.01.01-000000"
	t.Cleanup(func() { version.Version = prevVersion })
	scheduled := 0
	prevSchedule := scheduleCLIUpdateCheck
	scheduleCLIUpdateCheck = func() { scheduled++ }
	t.Cleanup(func() { scheduleCLIUpdateCheck = prevSchedule })

	run := func() string {
		t.Helper()
		// A pending update notice, and an update check that is due.
		cfg := &config.Config{AvailableCLIUpdate: "2026.02.01-000000"}
		if err := config.Save(cfg); err != nil {
			t.Fatal(err)
		}
		root := NewRootCmd()
		root.AddCommand(&cobra.Command{Use: "noop", RunE: func(*cobra.Command, []string) error { return nil }})
		root.SetArgs([]string{"noop"})
		var stderr bytes.Buffer
		root.SetErr(&stderr)
		root.SetOut(io.Discard)
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		return stderr.String()
	}

	t.Setenv("SUDO_UID", "4242")
	scheduled = 0
	if out := run(); strings.Contains(out, "new version") {
		t.Errorf("under sudo the update notice showed:\n%s", out)
	}
	if scheduled != 0 {
		t.Errorf("under sudo the update check was scheduled %d times", scheduled)
	}

	// Control: the same run as the user does both.
	t.Setenv("SUDO_UID", "")
	scheduled = 0
	if out := run(); !strings.Contains(out, "new version") {
		t.Errorf("as the user the update notice is missing:\n%s", out)
	}
	if scheduled != 1 {
		t.Errorf("as the user the update check was scheduled %d times, want 1", scheduled)
	}
}
