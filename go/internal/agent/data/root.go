package data

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// migrateDataRoot runs before the manager starts any writers. Episodes,
// campaigns and inference state share a parent and must move together. Copy
// into a private staging directory and publish it atomically; retain the old
// store untouched for rollback. Subsequent starts use the published store.
func migrateDataRoot(oldRoot, newRoot string) (string, error) {
	oldDir, newDir := filepath.Dir(oldRoot), filepath.Dir(newRoot)
	if info, err := os.Stat(newDir); err == nil {
		if !info.IsDir() {
			return "", fmt.Errorf("recording storage %s is not a directory", newDir)
		}
		marker, markerErr := os.ReadFile(filepath.Join(newDir, ".migrated-from"))
		if markerErr == nil && string(marker) == oldDir {
			return newRoot, nil
		}
		if _, oldErr := os.Stat(oldDir); !errors.Is(oldErr, os.ErrNotExist) {
			return "", fmt.Errorf("both %s and %s exist without a completed migration; refusing to hide or overwrite either store", oldDir, newDir)
		}
		return newRoot, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if _, err := os.Stat(oldDir); errors.Is(err, os.ErrNotExist) {
		return newRoot, nil
	} else if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(newDir), 0o750); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(filepath.Dir(newDir), ".data-migration-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage) // Only our unpublished temporary copy.
	if err := os.CopyFS(stage, os.DirFS(oldDir)); err != nil {
		return "", fmt.Errorf("copy recording storage to %s (original retained): %w", newDir, err)
	}
	// CopyFS uses default permissions; restore private grant/config modes and
	// flush payloads before the rename makes the new store authoritative.
	err = filepath.WalkDir(oldDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(oldDir, path)
		if err != nil {
			return err
		}
		target := filepath.Join(stage, rel)
		file, err := os.Open(target)
		if err != nil {
			return err
		}
		syncErr := file.Sync()
		closeErr := file.Close()
		if err := errors.Join(syncErr, closeErr); err != nil {
			return err
		}
		return os.Chmod(target, info.Mode().Perm())
	})
	if err != nil {
		return "", fmt.Errorf("prepare recording storage (original retained): %w", err)
	}
	marker, err := os.OpenFile(filepath.Join(stage, ".migrated-from"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	_, writeErr := marker.WriteString(oldDir)
	err = errors.Join(writeErr, marker.Sync(), marker.Close())
	if err != nil {
		return "", err
	}
	directory, err := os.Open(stage)
	if err != nil {
		return "", err
	}
	err = errors.Join(directory.Sync(), directory.Close())
	if err != nil {
		return "", err
	}
	if err := os.Rename(stage, newDir); err != nil {
		return "", err
	}
	parent, err := os.Open(filepath.Dir(newDir))
	if err != nil {
		return "", err
	}
	err = errors.Join(parent.Sync(), parent.Close())
	if err != nil {
		return "", err
	}
	return newRoot, nil
}
