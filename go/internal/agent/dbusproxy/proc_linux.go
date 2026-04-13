//go:build linux

package dbusproxy

import (
	"os/exec"
	"syscall"
)

// setPdeathsig configures the command to receive SIGTERM when the parent
// process (the agent) dies, preventing orphaned proxy processes.
func setPdeathsig(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGTERM
}
