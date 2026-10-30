//go:build linux || darwin

package commands

import (
	"os/exec"
	"syscall"
)

func detachMeshWorker(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
