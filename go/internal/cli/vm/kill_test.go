//go:build darwin || linux

package vm

import "syscall"

// syscallKill hard-kills a stubbed emulator, standing in for the VM dying
// without a chance to clean up after itself.
func syscallKill(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }
