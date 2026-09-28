package commands

import (
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// JSONErrorsRequested reports whether a failed invocation should be reported
// as a JSON error envelope on stderr rather than as styled text.
//
// It applies PersistentPreRunE's rule (an explicit --json or --json=<bool>
// wins; otherwise JSON whenever stdin or stdout is not a terminal) but reads
// argv itself, because flag, argument and unknown-command errors are raised
// before PersistentPreRunE runs. Hidden "__" helper commands always get text:
// a parent wendy process captures their stderr and shows it to a person.
func JSONErrorsRequested(executed *cobra.Command, args []string) bool {
	if executed != nil && strings.HasPrefix(executed.Name(), "__") {
		return false
	}
	if value, ok := explicitJSONFlag(args); ok {
		return value
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
