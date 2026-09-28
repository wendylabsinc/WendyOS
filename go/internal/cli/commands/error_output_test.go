package commands

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestJSONErrorsRequested(t *testing.T) {
	helper := &cobra.Command{Use: "__bmap-write"}
	info := &cobra.Command{Use: "info"}
	for _, tc := range []struct {
		name        string
		executed    *cobra.Command
		args        []string
		interactive bool
		jsonFlag    bool // jsonOutput as PersistentPreRunE (or ros2's argv scan) left it
		want        bool
	}{
		// Failures before PersistentPreRunE: jsonOutput was never set.
		{"no terminal, nothing explicit", nil, []string{"device", "banana"}, false, false, true},
		{"terminal, nothing explicit", info, []string{"device", "info", "--bogus"}, true, false, false},
		{"explicit --json at a terminal", info, []string{"--json", "device", "info", "--bogus"}, true, false, true},
		{"explicit --json=false without a terminal", info, []string{"--json=false", "device", "info"}, false, false, false},
		{"last occurrence wins", info, []string{"--json", "--json=0"}, false, false, false},
		{"unparsable value falls back to the terminal rule", info, []string{"--json=maybe"}, true, false, false},
		{"--json after -- belongs to another program", info, []string{"device", "exec", "--", "tool", "--json"}, true, false, false},
		// After PersistentPreRunE.
		{"auto-enabled JSON at a terminal", info, []string{"device", "info"}, true, true, true},
		// A parent wendy reads a helper's stderr and shows it to a person.
		{"hidden helper stays text", helper, []string{"--json", "__bmap-write"}, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.interactive {
				stubInteractive(t)
			} else {
				stubNonInteractive(t)
			}
			prev := jsonOutput
			jsonOutput = tc.jsonFlag
			t.Cleanup(func() { jsonOutput = prev })

			if got := JSONErrorsRequested(tc.executed, tc.args); got != tc.want {
				t.Errorf("JSONErrorsRequested(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}
