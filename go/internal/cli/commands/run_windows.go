//go:build windows

package commands

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// configurePostStartProcessGroup makes the postStart cmd.exe its own process
// group leader and overrides exec.CommandContext's default Cancel so that, on
// run-context cancellation, we kill the entire tree (including grandchildren
// spawned via `start /B …`).
//
// Without this, exec.CommandContext only calls TerminateProcess on the direct
// cmd.exe child, leaving any grandchildren orphaned — surprising for any
// long-lived hook.
func configurePostStartProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP

	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		// taskkill /T walks the parent/child relationship maintained by the
		// kernel for us; /F is required to terminate non-cooperative children.
		// Best-effort: if taskkill is unavailable or the tree is already gone,
		// fall back to Process.Kill which mirrors the default behavior.
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		return cmd.Process.Kill()
	}
}
