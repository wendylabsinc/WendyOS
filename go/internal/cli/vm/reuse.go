package vm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// PrepareForReuse returns true for a complete VM, whether stopped or running.
// An interrupted creation is preserved in a hidden recovery directory, freeing
// the original name for another creation. It never removes an existing disk.
//
// Metadata is the creation commit record. A disk without it may contain only
// part of the image, even when its partition table is readable. Do not boot it
// or manufacture metadata to make it appear complete.
func (s *Store) PrepareForReuse(name string) (ready bool, preservedAt string, err error) {
	lifecycle, err := s.acquireLifecycleLock(name)
	if err != nil {
		return false, "", err
	}
	defer func() {
		if closeErr := lifecycle.Close(); err == nil {
			err = closeErr
		}
	}()

	dir, err := os.Lstat(s.Dir(name))
	if os.IsNotExist(err) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	if !dir.IsDir() {
		return false, "", fmt.Errorf("VM path is not a directory: %s", s.Dir(name))
	}
	complete, err := s.creationComplete(name)
	if err != nil || complete {
		return complete, "", err
	}

	// Hold the lifecycle lock through the move. Once the run lock is acquired,
	// no QEMU owns this directory, and no start can acquire it until we finish.
	// Close the run handle before renaming so recovery also works on Windows.
	run, err := s.acquireRunLockUnderLifecycle(name)
	if err != nil {
		return false, "", err
	}
	if err := run.Close(); err != nil {
		return false, "", err
	}
	backup, err := os.MkdirTemp(s.Root, ".interrupted-"+name+"-")
	if err != nil {
		return false, "", fmt.Errorf("preserving interrupted VM: %w", err)
	}
	preservedAt = filepath.Join(backup, name)
	if err := os.Rename(s.Dir(name), preservedAt); err != nil {
		_ = os.Remove(backup) // This call created it, and it is still empty.
		return false, "", fmt.Errorf("preserving interrupted VM: %w", err)
	}
	return false, preservedAt, nil
}

func (s *Store) creationComplete(name string) (bool, error) {
	metaInfo, err := regularVMFile(s.MetaPath(name))
	if err != nil || metaInfo == nil {
		return false, err
	}
	data, err := os.ReadFile(s.MetaPath(name))
	if err != nil {
		// A permission or I/O error is not evidence of an interrupted create.
		return false, err
	}
	var meta Meta
	if json.Unmarshal(data, &meta) != nil || meta.Name != name {
		return false, nil
	}
	disk, err := regularVMFile(s.DiskPath(name))
	if err != nil || disk == nil {
		return false, err
	}
	if disk.Size() == 0 || disk.Size() < meta.DiskBytes {
		return false, nil
	}
	vars, err := regularVMFile(s.VarsPath(name))
	if err != nil || vars == nil {
		return false, err
	}
	return vars.Size() == pflashBytes, nil
}

func regularVMFile(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("VM file is not a regular file: %s", path)
	}
	return info, nil
}
