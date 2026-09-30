package commands

import (
	"os"
	"strconv"
	"strings"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
)

// isStderrTerminalFn reports whether stderr is a terminal, including the
// Cygwin/MSYS pseudo-terminals of Git Bash and mintty, which are pipes to
// the Windows console API. Tests swap it.
var isStderrTerminalFn = func() bool {
	fd := os.Stderr.Fd()
	return isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
}

// JSONErrorsRequested reports whether a failed invocation should be reported
// as a JSON error envelope on stderr rather than as styled text.
//
// An explicit --json or --json=<bool> wins. Otherwise a terminal on stderr
// means a person reads the error, so it is text even when stdout is piped
// (`wendy device apps list | grep x` turns JSON output on). Without a
// terminal on stderr, PersistentPreRunE's rule applies: JSON whenever stdin
// or stdout is not a terminal, which is how agents and scripts run the CLI.
// The decision reads argv itself, because flag, argument and unknown-command
// errors are raised before PersistentPreRunE runs. Hidden "__" helper
// commands always get text: a parent wendy process captures their stderr and
// shows it to a person.
func JSONErrorsRequested(executed *cobra.Command, args []string) bool {
	if executed != nil && strings.HasPrefix(executed.Name(), "__") {
		return false
	}
	if value, ok := explicitJSONFlag(args); ok {
		return value
	}
	if isStderrTerminalFn() {
		return false
	}
	return jsonOutput || !isInteractiveTerminal()
}

// explicitJSONFlag finds the last --json or --json=<bool> before a "--"
// terminator (pflag lets the last occurrence win). An unparsable value is
// ignored here; pflag reports it as a usage error of its own.
func explicitJSONFlag(args []string) (value, ok bool) {
	for _, arg := range args {
		if arg == "--" {
			break
		}
		switch {
		case arg == "--json":
			value, ok = true, true
		case strings.HasPrefix(arg, "--json="):
			if v, err := strconv.ParseBool(strings.TrimPrefix(arg, "--json=")); err == nil {
				value, ok = v, true
			}
		}
	}
	return value, ok
}
