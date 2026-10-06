//go:build !windows

package atomicfile

import (
	"os"
	"syscall"
)

// lookupOwner returns the uid/gid that owns path, or ok=false if path
// cannot be stat'd (e.g. it does not exist) or its Sys() isn't a
// *syscall.Stat_t.
func lookupOwner(path string) (uid, gid int, ok bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(stat.Uid), int(stat.Gid), true
}
