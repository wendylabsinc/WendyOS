//go:build linux || darwin

package inference

import (
	"os/exec"
	"syscall"
	"time"
)

// ConfigureProcess puts the worker in its own process group and kills the
// whole group when the command is cancelled.
func ConfigureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 3 * time.Second
}
