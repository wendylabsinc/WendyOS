// Package executil configures host commands launched by the agent.
package executil

import (
	"os/exec"
	"runtime"
)

// ResetOOMScore removes inherited OOM protection from a host workload before
// executing it. Host shells, durable jobs, and app hooks must not inherit the
// agent's immunity. Existing positive scores remain eligible for earlier kills.
// The fixed shell program never evaluates the workload's arguments as code.
func ResetOOMScore(cmd *exec.Cmd) {
	if runtime.GOOS == "linux" {
		wrapOOMScoreReset(cmd, "/proc/self/oom_score_adj")
	}
}

const resetOOMScoreScript = `IFS= read -r score < "$1" || exit 125
case "$score" in
  -*) printf '0\n' > "$1" || exit 125 ;;
esac
shift
exec "$@"`

func wrapOOMScoreReset(cmd *exec.Cmd, scorePath string) {
	args := []string{"/bin/sh", "-c", resetOOMScoreScript, "wendy-workload", scorePath, cmd.Path}
	cmd.Args = append(args, cmd.Args[1:]...)
	cmd.Path = "/bin/sh"
}
