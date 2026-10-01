//go:build !windows

package mcp

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// runStopSignals are the escalating signals for stopRunOnCancel's stages.
var runStopSignals = [...]syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL}

// configureRunProcess starts the CLI in its own process group so a stop
// signal also reaches the docker, buildx or swift processes it spawns.
func configureRunProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = runSysProcAttr()
}

// pinRunProcess is a no-op: signals address the process group, not a PID.
func pinRunProcess(*exec.Cmd) func() { return func() {} }

// reapRunGroup kills what is left of a cancelled run's process group after
// the CLI has exited.
//
// The leader is already reaped here, so its PID is free, but kill(-pgid)
// addresses the group, not the PID. A process group lives while any member
// does, and the kernel does not hand out a PID that is still in use as a live
// group's ID, so while a descendant remains, -pgid can only reach this run's
// group. Once the group is empty the signal fails with ESRCH, unless a new
// process has since been given the old PID and made itself a group leader
// (setsid/setpgid) in the microseconds since Wait returned, which is
// negligible.
func reapRunGroup(cmd *exec.Cmd) { _ = signalRunProcess(cmd, len(runStopSignals)-1) }

// signalRunProcess sends stage's stop signal (the last one for any later
// stage) to the run's process group.
func signalRunProcess(cmd *exec.Cmd, stage int) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	signal := runStopSignals[min(stage, len(runStopSignals)-1)]
	if err := syscall.Kill(-cmd.Process.Pid, signal); errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	} else {
		return err
	}
}
