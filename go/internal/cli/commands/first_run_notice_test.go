package commands

import (
	"bytes"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func firstRunNoticeOutput(t *testing.T, cfg *config.Config) string {
	t.Helper()
	cmd := &cobra.Command{}
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	showFirstRunNotice(cmd, cfg)
	return stderr.String()
}

// A read-only config dir used to make every command fail in PersistentPreRunE
// with "writing config: ... permission denied".
func TestShowFirstRunNotice_ReadOnlyConfigDirWarnsAndContinues(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod cannot make a directory read-only on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	t.Setenv("WENDY_CONFIG_DIR", dir)
	t.Setenv("WENDY_SECRET_STORE", "file")
	stubNonInteractive(t)

	cfg := &config.Config{}
	out := firstRunNoticeOutput(t, cfg)

	if !strings.Contains(out, "wendy analytics disable") {
		t.Errorf("analytics notice missing:\n%s", out)
	}
	if !strings.Contains(out, "could not save the Wendy CLI config") {
		t.Errorf("want a warning about the failed save, got:\n%s", out)
	}
	if strings.Contains(out, "wendy tour") {
		t.Errorf("non-interactive first run must not suggest `wendy tour` (it refuses without a TTY):\n%s", out)
	}
	if cfg.Analytics == nil || !cfg.Analytics.Enabled {
		t.Errorf("cfg.Analytics = %+v, want enabled in memory even when the save fails", cfg.Analytics)
	}
}

func TestShowFirstRunNotice_InteractiveSuggestsTourAndSaves(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	t.Setenv("WENDY_SECRET_STORE", "file")
	stubInteractive(t)

	out := firstRunNoticeOutput(t, &config.Config{})

	if !strings.Contains(out, "Run `wendy tour`") {
		t.Errorf("interactive first run should suggest the tour:\n%s", out)
	}
	if strings.Contains(out, "could not save") {
		t.Errorf("unexpected save warning:\n%s", out)
	}
	saved, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if saved.Analytics == nil || !saved.Analytics.Enabled {
		t.Fatalf("saved Analytics = %+v, want enabled", saved.Analytics)
	}
}
