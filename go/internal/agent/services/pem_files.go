package services

import (
	"fmt"
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

// WritePEMFiles writes device certificate PEM files and a provisioned marker to
// configPath. Files with empty content are skipped. Called both by
// ProvisioningService at runtime and by configpartition.Apply on first boot.
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
		if err := syncWriteFile(filepath.Join(configPath, f.name), []byte(f.data), f.mode); err != nil {
			return fmt.Errorf("writing %s: %w", f.name, err)
		}
	}

	_ = os.WriteFile(filepath.Join(configPath, ".provisioned"),
		[]byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)

	return nil
}
