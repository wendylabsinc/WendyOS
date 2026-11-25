//go:build !darwin && !linux && !windows

package flock

import (
	"errors"
	"os"
)

// Lock reports that file locking is unavailable on this platform.
func Lock(*os.File) error {
	return errors.ErrUnsupported
}

// TryLock rejects locking on platforms without an advisory lock implementation.
// Browser WASM imports this package through shared config and device pin types,
// but stores credentials in browser storage instead of locked local files.
func TryLock(*os.File) (bool, error) {
	return false, errors.ErrUnsupported
}

// Unlock reports that file locking is unavailable on this platform.
func Unlock(*os.File) error {
	return errors.ErrUnsupported
}

func lookupOwner(string) (uid, gid int, ok bool) {
	return 0, 0, false
}
