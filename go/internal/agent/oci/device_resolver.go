package oci

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"syscall"
)

// ResolveDeviceNode validates and resolves the same inode. CDI may follow a
// host symlink; entitlement bindings retain their no-symlink policy. Numbers
// from regular files, directories, or a different device type are never used.
func ResolveDeviceNode(path, deviceType string, followSymlinks bool) (int64, int64, error) {
	stat := os.Lstat
	if followSymlinks {
		stat = os.Stat
	}
	info, err := stat(path)
	if err != nil {
		return 0, 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return 0, 0, fmt.Errorf("%s is a symlink", path)
	}
	isChar := info.Mode()&os.ModeCharDevice != 0
	isBlock := info.Mode()&os.ModeDevice != 0 && !isChar
	if (deviceType != "c" || !isChar) && (deviceType != "b" || !isBlock) {
		return 0, 0, fmt.Errorf("%s is not a %s device node", path, deviceType)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("%s has no device stat data", path)
	}
	return int64(unix.Major(uint64(st.Rdev))), int64(unix.Minor(uint64(st.Rdev))), nil
}

var resolvePinnedDevice = func(pin PinnedDevice) (int64, int64, error) {
	return ResolveDeviceNode(pin.sourcePath(), pin.Type, !pin.NoFollow)
}
