//go:build !darwin && !linux

package vm

import (
	"os"
	"os/exec"
)

func detachProcess(*exec.Cmd) error { return ErrDetachUnsupported }

// DetachSupported lets a caller check for the limitation up front, rather than
// hitting it after an image download it did not need to make.
func DetachSupported() bool { return false }

// killProcess stops a foreground VM. Windows has no SIGTERM, so there is only
// the abrupt form -- the same outcome SIGTERM produces on the other platforms,
// where QEMU exits without telling the guest either.
func killProcess(pid int, _ bool) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

// terminateProcess is what Cmd.Cancel uses; Kill for the same reason.
func terminateProcess(p *os.Process) error { return p.Kill() }

// attachRunLock is a no-op: os/exec cannot pass extra descriptors on Windows,
// so the lock stays with this process. A foreground VM is tied to the CLI's
// lifetime there anyway, which is the same guarantee by another route.
func attachRunLock(*exec.Cmd, *os.File) {}
