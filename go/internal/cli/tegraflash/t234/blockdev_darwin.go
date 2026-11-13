//go:build darwin

package t234

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Darwin disk ioctls (<sys/disk.h>): _IOR('d', 24, uint32) and
// _IOR('d', 25, uint64). x/sys/unix does not define them.
const (
	dkiocGetBlockSize  = 0x40046418
	dkiocGetBlockCount = 0x40086419
)

const rawWriteFlags = os.O_RDWR

// blockDeviceSize returns the device's capacity in bytes.
func blockDeviceSize(dev *os.File) (int64, error) {
	if info, err := dev.Stat(); err == nil && info.Mode().IsRegular() {
		return info.Size(), nil
	}
	var blockSize uint32
	var blockCount uint64
	if err := ioctlPtr(dev, dkiocGetBlockSize, unsafe.Pointer(&blockSize)); err != nil {
		return 0, fmt.Errorf("DKIOCGETBLOCKSIZE: %w", err)
	}
	if err := ioctlPtr(dev, dkiocGetBlockCount, unsafe.Pointer(&blockCount)); err != nil {
		return 0, fmt.Errorf("DKIOCGETBLOCKCOUNT: %w", err)
	}
	return int64(blockCount) * int64(blockSize), nil
}

func ioctlPtr(f *os.File, req uint, arg unsafe.Pointer) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), uintptr(req), uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}
