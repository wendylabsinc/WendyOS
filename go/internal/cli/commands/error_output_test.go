package commands

import (
	"testing"

	"github.com/spf13/cobra"
)

// stubStderrTerminal makes isStderrTerminalFn report tty for the rest of the test.
func stubStderrTerminal(t *testing.T, tty bool) {
	t.Helper()
	prev := isStderrTerminalFn
	isStderrTerminalFn = func() bool { return tty }
	t.Cleanup(func() { isStderrTerminalFn = prev })
}

func TestJSONErrorsRequested(t *testing.T) {
	helper := &cobra.Command{Use: "__bmap-write"}
	info := &cobra.Command{Use: "info"}
	for _, tc := range []struct {
		name        string
		executed    *cobra.Command
		args        []string
		interactive bool // stdin and stdout are terminals
		stderrTTY   bool
		jsonFlag    bool // jsonOutput as PersistentPreRunE (or ros2's argv scan) left it
		want        bool
	}{
		// An agent harness: no terminal anywhere.
		{"no terminal, nothing explicit", nil, []string{"device", "banana"}, false, false, false, true},
		{"no terminal, JSON auto-enabled", info, []string{"device", "info"}, false, false, true, true},
		// A person at a terminal.
		{"terminal, nothing explicit", info, []string{"device", "info", "--bogus"}, true, true, false, false},
		{"explicit --json at a terminal", info, []string{"--json", "device", "info", "--bogus"}, true, true, false, true},
		// `wendy device apps list | grep x`: stdout is piped, so JSON output is
		// on, but the error lands on the person's terminal.
		{"stdout piped, stderr at a terminal", info, []string{"device", "apps", "list"}, false, true, true, false},
		{"stdout piped, stderr at a terminal, before PersistentPreRunE", nil, []string{"device", "banana"}, false, true, false, false},
		{"stdout piped, stderr at a terminal, explicit --json", info, []string{"--json", "device", "apps", "list"}, false, true, true, true},
		// `wendy device info 2>err.log` at a terminal: nothing asked for JSON.
		{"stderr redirected at a terminal", info, []string{"device", "info"}, true, false, false, false},
		{"explicit --json=false without a terminal", info, []string{"--json=false", "device", "info"}, false, false, false, false},
		{"last occurrence wins", info, []string{"--json", "--json=0"}, false, false, false, false},
		{"unparsable value falls back to the terminal rule", info, []string{"--json=maybe"}, true, true, false, false},
		{"--json after -- belongs to another program", info, []string{"device", "exec", "--", "tool", "--json"}, true, true, false, false},
		// A parent wendy reads a helper's stderr and shows it to a person.
		{"hidden helper stays text", helper, []string{"--json", "__bmap-write"}, false, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.interactive {
				stubInteractive(t)
			} else {
				stubNonInteractive(t)
			}
			stubStderrTerminal(t, tc.stderrTTY)
			prev := jsonOutput
			jsonOutput = tc.jsonFlag
			t.Cleanup(func() { jsonOutput = prev })

			if got := JSONErrorsRequested(tc.executed, tc.args); got != tc.want {
				t.Errorf("JSONErrorsRequested(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}
