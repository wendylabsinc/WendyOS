//go:build linux

package avahibridge

import (
	"os/exec"
	"syscall"
)

// Private buses must not outlive an agent crash and hold stale sockets/namespaces.
func setParentDeathSignal(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
