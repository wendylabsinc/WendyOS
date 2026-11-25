//go:build linux

package services

import (
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// newRebooter wires a rebooter to the real platform calls.
func newRebooter(logger *zap.Logger) rebooter {
	if logger == nil {
		logger = zap.NewNop()
	}
	return rebooter{
		logger:    logger,
		sync:      unix.Sync,
		immediate: func() error { return syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART) },
		clean:     func() error { return systemctlReboot("") },
		grace:     rebootGrace,
		sleep:     time.Sleep,
	}
}

// newArgumentRebooter is newRebooter with both restart paths handing argument
// to the kernel, whose board drivers act on it: "forced-recovery" makes a
// Jetson boot into USB recovery mode. It is what `reboot ARG` does.
func newArgumentRebooter(logger *zap.Logger, argument string) rebooter {
	r := newRebooter(logger)
	r.immediate = func() error { return restartWithArgument(argument) }
	r.clean = func() error { return systemctlReboot(argument) }
	return r
}

// restartWithArgument restarts the kernel now, like LINUX_REBOOT_CMD_RESTART,
// and hands argument to the restart handlers (LINUX_REBOOT_CMD_RESTART2).
// x/sys/unix exports only the argument-less reboot(2).
func restartWithArgument(argument string) error {
	arg, err := unix.BytePtrFromString(argument)
	if err != nil {
		return err
	}
	_, _, errno := unix.Syscall6(unix.SYS_REBOOT,
		unix.LINUX_REBOOT_MAGIC1, unix.LINUX_REBOOT_MAGIC2, unix.LINUX_REBOOT_CMD_RESTART2,
		uintptr(unsafe.Pointer(arg)), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// systemctlReboot asks systemd for an orderly shutdown, which unmounts mounted
// filesystems on the way down, passing argument to the final restart.
func systemctlReboot(argument string) error {
	args := systemctlRebootArgs(argument)
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// rebootSystem flushes filesystems and restarts immediately. This is the
// recovery path (e.g. the health gate's rollback reboot), where the reliable
// immediate restart is preferred over an orderly shutdown that could hang on a
// device already in a bad state.
func rebootSystem() error {
	return newRebooter(nil).reboot()
}
