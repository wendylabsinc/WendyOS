//go:build linux

package hardware

import (
	"bytes"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// gpioChipInfo reads a GPIO chip's label and line count from its character device.
func gpioChipInfo(dev string) (label string, lines uint32, ok bool) {
	f, err := os.Open(dev)
	if err != nil {
		return "", 0, false
	}
	defer f.Close()
	var info unix.GPIOChipInfo
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), unix.GPIO_GET_CHIPINFO_IOCTL, uintptr(unsafe.Pointer(&info))); errno != 0 {
		return "", 0, false
	}
	return string(bytes.TrimRight(info.Label[:], "\x00")), info.Lines, true
}
