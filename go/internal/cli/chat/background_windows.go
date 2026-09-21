//go:build windows

package chat

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

func backgroundCommand(executable string, args ...string) *exec.Cmd {
	cmd := exec.Command(executable, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP, HideWindow: true}
	return cmd
}

func interruptBackground(cmd *exec.Cmd) error { return killBackground(cmd) }

func killBackground(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run(); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
