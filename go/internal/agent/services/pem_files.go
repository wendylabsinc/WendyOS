package services

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// syncWriteFile atomically writes data to path: write to a temp file, fsync,
// rename over the target, then fsync the directory. This ensures that a power
// loss mid-write cannot leave the target file empty or partially written —
// critical for security files (private keys, certificates) on embedded devices.
func syncWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".pem-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	removeOnFail := true
	tmpClosed := false
	defer func() {
		if !tmpClosed {
			_ = tmp.Close() // best-effort: file will be removed in error paths
		}
		if removeOnFail {
			os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(perm); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	tmpClosed = true
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	removeOnFail = false

	// fsync the directory so the rename is durable on power loss. Open/close
	// failures are reported too: skipping the fsync silently would drop the
	// durability guarantee this helper exists to provide.
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open dir for fsync after rename: %w", err)
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		return fmt.Errorf("fsync dir after rename: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close dir after fsync: %w", closeErr)
	}
	return nil
}

// pemFileMatches avoids replacing an already correct projection on startup.
// Lstat and opened-file identity checks prevent accepting a symlink as the
// projection; a changed, unreadable or incorrectly permissioned path uses the
// same durable atomic writer as a missing file.
func pemFileMatches(path string, data []byte, perm os.FileMode) bool {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode() != perm || before.Size() != int64(len(data)) {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	opened, statErr := f.Stat()
	if statErr != nil || !os.SameFile(before, opened) || opened.Mode() != perm {
		_ = f.Close()
		return false
	}
	contents, readErr := io.ReadAll(io.LimitReader(f, int64(len(data))+1))
	closeErr := f.Close()
	after, statErr := os.Lstat(path)
	return readErr == nil && closeErr == nil && statErr == nil &&
		after.Mode() == perm && os.SameFile(opened, after) && bytes.Equal(contents, data)
}

func WritePEMFiles(configPath, keyPEM, certPEM, chainPEM string) error {
	if err := os.MkdirAll(configPath, 0o700); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	files := []struct {
		name string
		data string
		mode os.FileMode
	}{
		{"device-key.pem", keyPEM, 0o600},
		{"device.pem", certPEM, 0o644},
		{"ca.pem", chainPEM, 0o644},
	}

	for _, f := range files {
		if f.data == "" {
			continue
		}
		path := filepath.Join(configPath, f.name)
		data := []byte(f.data)
		if pemFileMatches(path, data, f.mode) {
			continue
		}
		if err := syncWriteFile(path, data, f.mode); err != nil {
			return fmt.Errorf("writing %s: %w", f.name, err)
		}
	}

	// This is an existence sentinel, not the last startup time. Preserve an
	// existing marker; exclusive creation also avoids following a symlink.
	// Marker failures remain best effort, as before.
	if marker, err := os.OpenFile(filepath.Join(configPath, ".provisioned"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644); err == nil {
		_, _ = marker.WriteString(time.Now().UTC().Format(time.RFC3339) + "\n")
		_ = marker.Close()
	}

	return nil
}
