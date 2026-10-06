//go:build windows

package flock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// TryLock attempts a non-blocking exclusive lock on f via LockFileEx. It
// returns (true, nil) when the lock was acquired, (false, nil) when another
// process holds it, and a non-nil error for any other failure.
func TryLock(f *os.File) (bool, error) {
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

// Unlock releases the lock held on f.
func Unlock(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
}

// lookupOwner is a no-op on Windows: file ownership there isn't chown-based,
// so there is nothing meaningful to report, and Acquire's geteuid()==0 check
// never fires on this platform anyway.
func lookupOwner(string) (uid, gid int, ok bool) {
	return 0, 0, false
}
