//go:build unix

package services

import (
	"context"
	"io"
	"os"
	"os/exec"
	"syscall"
)

func runMCUHelper(ctx context.Context, args ...string) (io.ReadCloser, func() error, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.CommandContext(ctx, mcuHelperPath, args...)
	cmd.Stdout, cmd.Stderr = w, w
	// OpenOCD runs as the helper's child, so a timeout must stop the group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	if err := cmd.Start(); err != nil {
		_ = r.Close()
		_ = w.Close()
		return nil, nil, err
	}
	_ = w.Close()
	return r, cmd.Wait, nil
}
