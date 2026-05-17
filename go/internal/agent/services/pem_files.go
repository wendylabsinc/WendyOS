package services

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// syncWriteFile writes data to path and calls fsync before closing,
// ensuring the data is on disk before returning. Used for security-critical
// files (private keys, certificates) where partial writes would break enrollment.
func syncWriteFile(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
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
