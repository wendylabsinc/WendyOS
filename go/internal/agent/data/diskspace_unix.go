//go:build !windows

package data

import "golang.org/x/sys/unix"

// diskSpace returns the size of the filesystem holding path and the bytes still
// available to unprivileged writers.
func diskSpace(path string) (total, free int64, err error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, 0, err
	}
	return int64(stat.Blocks) * int64(stat.Bsize), int64(stat.Bavail) * int64(stat.Bsize), nil
}
