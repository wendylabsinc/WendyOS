//go:build !windows

package chat

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func backgroundCommand(executable string, args ...string) *exec.Cmd {
	cmd := exec.Command(executable, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

func backgroundSignal(cmd *exec.Cmd, signal syscall.Signal) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	if err := syscall.Kill(-cmd.Process.Pid, signal); errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	} else {
		return err
	}
}

func interruptBackground(cmd *exec.Cmd) error { return backgroundSignal(cmd, syscall.SIGTERM) }
func killBackground(cmd *exec.Cmd) error      { return backgroundSignal(cmd, syscall.SIGKILL) }
