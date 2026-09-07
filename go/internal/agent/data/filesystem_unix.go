//go:build !windows

package data

import "golang.org/x/sys/unix"

// filesystemSpace returns the filesystem's total bytes and bytes available to
// the caller, excluding space reserved for privileged users.
func filesystemSpace(path string) (total, available int64, err error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, 0, err
	}
	return int64(stat.Blocks) * int64(stat.Bsize), int64(stat.Bavail) * int64(stat.Bsize), nil
}
