package flock

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ErrTimeout reports that Acquire gave up waiting for another holder.
var ErrTimeout = errors.New("timed out waiting for file lock")

// acquirePoll is how often Acquire retries a contended lock.
const acquirePoll = 20 * time.Millisecond

// geteuid and chown are seams so tests can exercise the root-only lock-file
// owner fix-up below without actually running as root.
var (
	geteuid = os.Geteuid
	chown   = os.Chown
)

// Acquire takes an exclusive lock on the file at path, creating it (0600) if
// needed, and waits up to timeout for another holder to let go. The returned
// release unlocks and closes the file; calling it more than once is harmless.
//
// The lock file is never deleted: unlinking it could let a second process lock
// a fresh inode while a waiter still holds the old one. Locks belong to the
// open file, so two Acquire calls in one process exclude each other too — and
// a holder that calls Acquire again on the same path waits on itself until
// timeout.
func Acquire(path string, timeout time.Duration) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	// wendy re-execs itself under `sudo --preserve-env=HOME` for privileged
	// operations, so this can create the lock file as root while still
	// working in the invoking user's ~/.wendy. Fix the owner up before
	// anyone waits on it, or an unprivileged Acquire later on the same path
	// fails to open/lock its own config directory's lock file.
	preserveLockFileOwner(path)
	deadline := time.Now().Add(timeout)
	for {
		locked, err := TryLock(f)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		if locked {
			return sync.OnceFunc(func() {
				_ = Unlock(f)
				_ = f.Close()
			}), nil
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, ErrTimeout
		}
		time.Sleep(acquirePoll)
	}
}

// preserveLockFileOwner chowns path to its directory's owner when running as
// root, so a lock file created (or already owned) by root doesn't block a
// later unprivileged Acquire on the same path with EACCES/EPERM. Best
// effort: a chown failure here must not fail the lock itself, and it is a
// no-op off root, on Windows (lookupOwner always reports ok=false there), or
// when the directory's owner is already root.
func preserveLockFileOwner(path string) {
	if geteuid() != 0 {
		return
	}
	uid, gid, ok := lookupOwner(filepath.Dir(path))
	if !ok || uid == 0 {
		return
	}
	_ = chown(path, uid, gid)
}
