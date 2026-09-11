// Package atomicfile writes a file so that a power loss cannot leave a partial
// one behind.
//
// It is the implementation that used to live as services.syncWriteFile, lifted
// here unchanged so that code outside the agent's services package can share it
// rather than grow a second, subtly different copy. The agent writes private
// keys and certificates on embedded devices that lose power without warning,
// and a half-written key file is indistinguishable from a corrupt one.
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// Write atomically writes data to path: write to a temp file in the same
// directory, fsync it, rename it over the target, then fsync the directory so
// the rename itself is durable.
//
// perm is applied to the temp file before the rename, so the target never
// exists with a wider mode than asked for — which is why a 0o600 key file is
// never briefly world-readable.
func Write(path string, data []byte, perm os.FileMode) error {
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
