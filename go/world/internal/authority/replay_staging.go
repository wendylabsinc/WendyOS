package authority

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

func syncReplayDirectory(path string) error {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func privateReplayEntry(path string, directory bool) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	metadata, ok := info.Sys().(*syscall.Stat_t)
	mode := os.FileMode(0600)
	if directory {
		mode = 0700
	}
	if !ok || metadata.Uid != uint32(os.Geteuid()) || info.Mode().Perm() != mode || (directory && !info.IsDir()) || (!directory && (!info.Mode().IsRegular() || metadata.Nlink != 1)) {
		f.Close()
		return nil, errors.New("replay staging ownership/type/mode mismatch")
	}
	return f, nil
}

// A private directory plus atomic empty-directory owner marker identifies the
// sole staging file. Crashes before marker creation leave an empty directory,
// which can be adopted without deleting anything. No unknown file is removed.
func (c *ReplayCache) ensureStaging() error {
	c.staging = c.path + ReplayStagingSuffix
	digest := sha256.Sum256([]byte(c.path))
	owner := "owner-" + hex.EncodeToString(digest[:])
	if err := os.Mkdir(c.staging, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	dir, err := privateReplayEntry(c.staging, true)
	if err != nil {
		return err
	}
	entries, readErr := dir.ReadDir(3)
	dir.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return readErr
	}
	if len(entries) > 2 {
		return errors.New("unexpected replay staging contents")
	}
	owned, hasNext := false, false
	for _, entry := range entries {
		switch entry.Name() {
		case owner:
			owned = true
		case "next":
			hasNext = true
		default:
			return errors.New("unknown replay staging entry")
		}
	}
	if !owned {
		if len(entries) != 0 {
			return errors.New("replay staging has no owner marker")
		}
		if err = os.Mkdir(filepath.Join(c.staging, owner), 0700); err != nil {
			return err
		}
	}
	marker, err := privateReplayEntry(filepath.Join(c.staging, owner), true)
	if err != nil {
		return err
	}
	children, readErr := marker.ReadDir(1)
	marker.Close()
	if len(children) != 0 || (readErr != nil && !errors.Is(readErr, io.EOF)) {
		return errors.New("invalid replay staging owner marker")
	}
	// Persist ownership before any next-file creation, including recovery after
	// a crash during namespace initialization. The marker has no partial bytes.
	if err = syncReplayDirectory(c.staging); err != nil {
		return err
	}
	if err = syncReplayDirectory(filepath.Dir(c.path)); err != nil {
		return err
	}
	if hasNext {
		next := filepath.Join(c.staging, "next")
		file, err := privateReplayEntry(next, false)
		if err != nil {
			return err
		}
		file.Close()
		if err = os.Remove(next); err != nil {
			return err
		}
		if err = syncReplayDirectory(c.staging); err != nil {
			return err
		}
	}
	return nil
}
