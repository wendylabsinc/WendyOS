//go:build windows

package sessionbroker

import (
	"github.com/wendylabsinc/wendy/go/internal/shared/flock"
	"github.com/wendylabsinc/wendy/go/internal/shared/useripc"
	"golang.org/x/sys/windows"
	"os"
	"syscall"
)

func detachedProcessAttributes() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW}
}
func pathOwnedByCurrentUser(path string) bool { return useripc.OwnedPath(path) }
func acquireLock(file *os.File) (bool, error) { return flock.TryLock(file) }
func releaseLock(file *os.File)               { _ = flock.Unlock(file) }
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(process)
	var code uint32
	return windows.GetExitCodeProcess(process, &code) == nil && code == 259
}
