package services

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

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
		if err := os.WriteFile(filepath.Join(configPath, f.name), []byte(f.data), f.mode); err != nil {
			return fmt.Errorf("writing %s: %w", f.name, err)
		}
	}

	_ = os.WriteFile(filepath.Join(configPath, ".provisioned"),
		[]byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)

	return nil
}
