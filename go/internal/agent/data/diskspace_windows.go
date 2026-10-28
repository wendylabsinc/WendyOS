//go:build windows

package data

import (
	"errors"

	"golang.org/x/sys/windows"
)

// diskSpace returns the size of the volume holding path and the bytes still
// available to the caller.
func diskSpace(path string) (total, free int64, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var avail, size, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &size, &totalFree); err != nil {
		return 0, 0, err
	}
	return int64(size), int64(avail), nil
}

// statFilesystem is not implemented on Windows, which runs the CLI and never
// the agent. The store choice treats the error as "cannot confirm" and keeps
// LegacyRoot.
func statFilesystem(string) (fsIdentity, error) {
	return fsIdentity{}, errors.New("filesystem identity is not available on Windows")
}
