package commands

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// Keep old invocations understandable during the migration without silently
// accepting a command that no longer changes device selection.
func newRemovedDeviceDefaultCmd(name string) *cobra.Command {
	removed := func(*cobra.Command, []string) error {
		return fmt.Errorf("wendy device %s was removed; use --device <device> or set WENDY_DEVICE for this shell", name)
	}
	return &cobra.Command{
		Use:    name,
		Hidden: true,
		Short:  "Removed: select a device with --device or WENDY_DEVICE",
		// Fail before pre-run: no login, discovery, analytics or config writes.
		// Validate old positional arguments with the migration hint too.
		Args: removed,
		RunE: removed,
	}
}

func noteLegacyDefaultDevice(cmd *cobra.Command, cfg *config.Config) {
	if strings.TrimSpace(cfg.LegacyDefaultDevice) == "" || deviceFlag != "" || cmd.Name() == cobra.ShellCompRequestCmd || strings.HasPrefix(cmd.CommandPath(), "wendy completion") {
		return
	}
	// stderr preserves JSON and MCP stdout. No state is changed just to show the
	// notice; normal config saves omit the legacy key and end the migration.
	cmd.PrintErrln("The saved default device is ignored; use --device <device> or set WENDY_DEVICE for this shell.")
}
