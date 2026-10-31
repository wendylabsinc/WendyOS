//go:build linux

package services

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// hostRootMedium goes by device number: /proc/mounts calls a root mounted
// without an initramfs /dev/root.
func hostRootMedium() (medium, disk string, err error) {
	var st unix.Stat_t
	if err := unix.Stat("/", &st); err != nil {
		return "", "", fmt.Errorf("stat /: %w", err)
	}
	dev := uint64(st.Dev)
	return rootMediumFromSysfs("/sys", fmt.Sprintf("%d:%d", unix.Major(dev), unix.Minor(dev)))
}
