//go:build linux

package containerd

import "golang.org/x/sys/unix"

// containerdRootDir is the filesystem containerd stores its content store and
// snapshots under. PruneCache statfs's it before and after a forced GC pass
// to measure the free-space delta the prune reclaimed.
const containerdRootDir = "/var/lib/containerd"

// filesystemFreeBytes reports the free space available on the filesystem
// containing path, in bytes. ok is false when the filesystem could not be
// statted (e.g. the path does not exist), in which case the byte count must
// be ignored.
func filesystemFreeBytes(path string) (uint64, bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, false
	}
	blockSize := uint64(st.Frsize)
	if blockSize == 0 {
		blockSize = uint64(st.Bsize)
	}
	return uint64(st.Bavail) * blockSize, true
}
