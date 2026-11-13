//go:build windows

package mcp

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

func configureRunProcess(*exec.Cmd) {}

// pinRunProcess holds a handle to the CLI while stopRunOnCancel may still
// signal it. taskkill addresses a PID, and os/exec releases its own handle as
// soon as the CLI exits, before Wait returns; without this handle an
// unrelated process could reuse the PID in that window.
func pinRunProcess(cmd *exec.Cmd) func() {
	handle, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return func() {}
	}
	return func() { _ = syscall.CloseHandle(handle) }
}

// reapRunGroup does nothing on Windows. Once Wait has returned the CLI's PID
// is free for reuse, and taskkill /T cannot reach orphaned descendants whose
// parent has exited anyway, so nothing is signalled by PID after exit.
func reapRunGroup(*exec.Cmd) {}

// signalRunProcess ends the run's process tree. Windows cannot deliver an
// interrupt to a console-less child, so every stage terminates.
func signalRunProcess(cmd *exec.Cmd, _ int) error {
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
