//go:build darwin || linux

package vm

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// detachProcess puts the VM in its own session, so the terminal's SIGINT and
// SIGHUP never reach it: Ctrl-C on the command that started the VM, or closing
// that terminal, must not power the guest off.
// DetachSupported reports that this platform can run a VM in the background.
func DetachSupported() bool { return true }

func detachProcess(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	return nil
}

// killProcess asks the VM to power off, or kills it outright when force is set.
// A process that is already gone is not an error: Stop's caller only cares that
// it is no longer running.
func killProcess(pid int, force bool) error {
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	if err := syscall.Kill(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// attachRunLock hands the run lock to the emulator, so the kernel frees it when
// the emulator exits rather than when this process does.
func attachRunLock(cmd *exec.Cmd, lock *os.File) {
	cmd.ExtraFiles = append(cmd.ExtraFiles, lock)
}

// terminateProcess asks QEMU to exit. SIGTERM, not SIGKILL: QEMU puts the
// terminal in raw mode for the guest console and only restores it on a clean
// shutdown.
func terminateProcess(p *os.Process) error { return p.Signal(syscall.SIGTERM) }
