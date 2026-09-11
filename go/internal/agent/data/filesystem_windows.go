package data

import "golang.org/x/sys/windows"

// filesystemSpace returns the filesystem's total bytes and bytes available to
// the caller, respecting per-user disk quotas.
func filesystemSpace(path string) (total, available int64, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var totalBytes, availableBytes, freeBytes uint64
	if err := windows.GetDiskFreeSpaceEx(p, &availableBytes, &totalBytes, &freeBytes); err != nil {
		return 0, 0, err
	}
	return int64(totalBytes), int64(availableBytes), nil
}
