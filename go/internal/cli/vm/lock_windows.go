//go:build windows

package vm

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// tryLockFile attempts a non-blocking exclusive lock on f via LockFileEx. It
// returns (true, nil) when the lock was acquired, (false, nil) when another
// process holds it, and a non-nil error for any other failure.
func tryLockFile(f *os.File) (bool, error) {
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, ol,
	)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return false, err
}

// unlockFile releases the lock held on f.
func unlockFile(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
}

// trySharedLock probes without taking the exclusive lock; see the unix version.
func trySharedLock(f *os.File) (bool, error) {
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return false, err
}
