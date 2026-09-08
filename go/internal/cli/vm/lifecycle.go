package vm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var ErrLifecycleBusy = errors.New("VM is being created, started or removed; retry when that operation finishes")

// Keep lifecycle locks outside the removable VM directory. Never unlink them:
// replacing a lock file lets two callers lock different inodes for one name.
// The run lock still belongs to QEMU; this lock only orders filesystem changes
// and acquisition of that run lock, not the lifetime of the emulator.
func (s *Store) acquireLifecycleLock(name string) (*os.File, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.Root, ".locks")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, name+".lock"), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	locked, err := tryLockFile(f)
	if err != nil || !locked {
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s", ErrLifecycleBusy, name)
	}
	return f, nil
}
