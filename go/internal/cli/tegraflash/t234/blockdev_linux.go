//go:build linux

package t234

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Linux applies O_EXCL without O_CREAT to block devices: refuse a mounted
// disk (including its partitions) and prevent new mounts while writing.
// Ordinary files remain usable by file-backed writer tests.
const rawWriteFlags = os.O_RDWR | os.O_EXCL

// blockDeviceSize returns the device's capacity in bytes (BLKGETSIZE64).
func blockDeviceSize(dev *os.File) (int64, error) {
	if info, err := dev.Stat(); err == nil && info.Mode().IsRegular() {
		return info.Size(), nil
	}
	var size uint64
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		dev.Fd(),
		uintptr(unix.BLKGETSIZE64),
		uintptr(unsafe.Pointer(&size)),
	)
	if errno != 0 {
		return 0, fmt.Errorf("BLKGETSIZE64: %w", errno)
	}
	return int64(size), nil
}
