//go:build windows

package vm

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func truncateSparse(file *os.File, size int64) error {
	// Windows allocates the extended range unless the file is explicitly sparse.
	// Keep ordinary allocation on file systems that do not support sparse files.
	var returned uint32
	err := windows.DeviceIoControl(windows.Handle(file.Fd()), windows.FSCTL_SET_SPARSE,
		nil, 0, nil, 0, &returned, nil)
	if err != nil && !errors.Is(err, windows.ERROR_INVALID_FUNCTION) && !errors.Is(err, windows.ERROR_NOT_SUPPORTED) {
		return err
	}
	return file.Truncate(size)
}

// IsDiskFull recognizes disk-full errors through wrapped image-write failures.
func IsDiskFull(err error) bool {
	return errors.Is(err, windows.ERROR_DISK_FULL) || errors.Is(err, windows.ERROR_HANDLE_DISK_FULL) ||
		errors.Is(err, windows.ERROR_DISK_QUOTA_EXCEEDED)
}
