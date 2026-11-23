//go:build linux

package services

import (
	"os"

	"golang.org/x/sys/unix"
)

// tapPipeCapacity is how many bytes the tap pipe holds, as the kernel reports
// it (F_GETPIPE_SZ), or defaultTapPipeCapacity when it cannot say. It goes
// through SyscallConn, not Fd: Fd would put the pipe in blocking mode and stop
// the pump's read deadlines from working.
func tapPipeCapacity(r *os.File) int {
	rc, err := r.SyscallConn()
	if err != nil {
		return defaultTapPipeCapacity
	}
	size, sizeErr := 0, error(nil)
	if err := rc.Control(func(fd uintptr) {
		size, sizeErr = unix.FcntlInt(fd, unix.F_GETPIPE_SZ, 0)
	}); err != nil || sizeErr != nil || size <= 0 {
		return defaultTapPipeCapacity
	}
	return size
}
