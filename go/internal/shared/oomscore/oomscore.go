// Package oomscore protects a process from the OOM killer and keeps that
// protection from leaking into the processes it starts.
package oomscore

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
)

// Protected is the lowest score that still leaves the process killable as a
// last resort.
const Protected = -999

const procScorePath = "/proc/self/oom_score_adj"

// Protect lowers this process's OOM score to Protected. No-op off Linux.
func Protect() error {
	if runtime.GOOS != "linux" {
		return nil
	}
	return protectFile(procScorePath)
}

func protectFile(path string) error {
	return os.WriteFile(path, []byte(strconv.Itoa(Protected)+"\n"), 0o644)
}

// ResetChild makes cmd start with a non-negative OOM score so it does not
// inherit protection. Positive scores are kept. No-op off Linux.
func ResetChild(cmd *exec.Cmd) {
	if runtime.GOOS == "linux" {
		wrapReset(cmd, procScorePath)
	}
}

// The fixed script never evaluates cmd's arguments as code.
const resetScript = `IFS= read -r score < "$1" || exit 125
case "$score" in
  -*) printf '0\n' > "$1" || exit 125 ;;
esac
shift
exec "$@"`

func wrapReset(cmd *exec.Cmd, scorePath string) {
	args := []string{"/bin/sh", "-c", resetScript, "wendy-workload", scorePath, cmd.Path}
	cmd.Args = append(args, cmd.Args[1:]...)
	cmd.Path = "/bin/sh"
}
