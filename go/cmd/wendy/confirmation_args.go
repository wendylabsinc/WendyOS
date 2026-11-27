package main

import "strings"

// normalizeLeadingConfirmationFlag makes the command-scoped boolean explicit
// while Cobra is still discovering the command at the root. Otherwise Cobra
// treats the unregistered bare --yes/-y as value-taking and consumes the next
// command token (e.g. vm). The selected command still owns and validates the
// flag; registering --yes at root would silently accept it on unrelated commands.
func normalizeLeadingConfirmationFlag(args []string) []string {
	result := append([]string{}, args...)
	for i := 0; i < len(result); i++ {
		arg := result[i]
		switch {
		case arg == "--yes", arg == "-y":
			result[i] = arg + "=true"
		case arg == "--device":
			i++ // Its value may itself be --yes or -y.
		case arg == "--json", strings.HasPrefix(arg, "--json="),
			strings.HasPrefix(arg, "--device="), strings.HasPrefix(arg, "--yes="),
			strings.HasPrefix(arg, "-y="):
			// Known prefix options whose value is already inline, or boolean --json.
		default:
			// Stop at the command, --, or an unknown option. Do not rewrite positional
			// arguments, forwarded programs, or values of an option we do not know.
			return result
		}
	}
	return result
}
