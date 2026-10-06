//go:build darwin || linux

// Package flock provides non-blocking, per-file advisory locking shared by
// every CLI feature that needs "at most one process does X": the BuildKit
// build lock, OCI layer staging, and the session broker's identity lock.
// One implementation because the failure modes are subtle and identical
// everywhere — e.g. EWOULDBLOCK must be matched with errors.Is, not ==, to
// survive wrapping.
package flock

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// TryLock attempts a non-blocking exclusive (advisory) lock on f. It returns
// (true, nil) when the lock was acquired, (false, nil) when another process
// holds it, and a non-nil error for any other failure.
func TryLock(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) {
		return false, nil
	}
	return false, err
}

// Unlock releases the advisory lock held on f.
func Unlock(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}

// lookupOwner returns the uid/gid that owns path, or ok=false if path
// cannot be stat'd (e.g. it does not exist) or its Sys() isn't a
// *syscall.Stat_t.
func lookupOwner(path string) (uid, gid int, ok bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(stat.Uid), int(stat.Gid), true
}
