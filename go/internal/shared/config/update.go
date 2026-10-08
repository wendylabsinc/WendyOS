package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/flock"
)

// lockFileName is the file Update locks beside config.json. It is never
// deleted (see flock.Acquire).
const lockFileName = "config.lock"

// updateLockTimeout bounds how long Update waits for another wendy process. A
// holder keeps the lock for one load-modify-save, so a long wait means a hung
// process, and failing beats hanging every later command behind it.
var updateLockTimeout = 10 * time.Second

// Update loads config.json, applies fn, and saves the result when fn reports a
// change — all under an exclusive lock shared by every wendy process, so two
// processes that each change a different field both keep their change.
// Load-then-Save by hand is last-writer-wins: the second Save writes back the
// snapshot it loaded and silently reverts whatever the first one wrote.
//
// fn returning an error aborts without saving; that error is returned as-is.
// fn must not call Update: the lock is not reentrant, so it would wait out
// updateLockTimeout and fail.
func Update(fn func(cfg *Config) (changed bool, err error)) error {
	dir, err := ConfigDir()
	if err != nil {
		return err
	}
	lockPath := filepath.Join(dir, lockFileName)
	release, err := flock.Acquire(lockPath, updateLockTimeout)
	if err != nil {
		if errors.Is(err, flock.ErrTimeout) {
			return fmt.Errorf("%w: another wendy process has held %s for over %s; retry once it finishes", flock.ErrTimeout, lockPath, updateLockTimeout)
		}
		return fmt.Errorf("locking %s: %w", lockPath, err)
	}
	defer release()

	cfg, err := Load()
	if err != nil {
		return err
	}
	changed, err := fn(cfg)
	if err != nil || !changed {
		return err
	}
	return Save(cfg)
}
