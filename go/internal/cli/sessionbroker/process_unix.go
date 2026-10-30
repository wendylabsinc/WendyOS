//go:build !windows

package sessionbroker

import (
	"os"
	"syscall"

	"github.com/wendylabsinc/wendy/go/internal/shared/flock"
	"github.com/wendylabsinc/wendy/go/internal/shared/useripc"
)

func detachedProcessAttributes() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

func pathOwnedByCurrentUser(path string) bool { return useripc.OwnedPath(path) }

func acquireLock(file *os.File) (bool, error) {
	return flock.TryLock(file)
}

func releaseLock(file *os.File) {
	_ = flock.Unlock(file)
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
