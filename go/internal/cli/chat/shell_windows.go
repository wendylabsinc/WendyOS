//go:build windows

package chat

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"time"
)

func shellCommand(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "cmd.exe", "/D", "/S", "/C", command)
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		// Kill the tree, including compiler/test children, before the shell.
		killCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = exec.CommandContext(killCtx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		return cmd.Process.Kill()
	}
	return cmd
}
