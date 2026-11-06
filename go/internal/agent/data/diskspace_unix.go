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

// statFilesystem identifies the filesystem holding path by its device number,
// which a bind mount shares with its source, and reports that filesystem's
// size.
func statFilesystem(path string) (fsIdentity, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return fsIdentity{}, err
	}
	total, _, err := diskSpace(path)
	if err != nil {
		return fsIdentity{}, err
	}
	return fsIdentity{device: uint64(st.Dev), size: total}, nil
}
