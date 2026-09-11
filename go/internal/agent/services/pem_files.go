package services

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
)

// syncWriteFile atomically writes data to path: write to a temp file, fsync,
// rename over the target, then fsync the directory. This ensures that a power
// loss mid-write cannot leave the target file empty or partially written —
// critical for security files (private keys, certificates) on embedded devices.
//
// The implementation now lives in internal/shared/atomicfile so the pki-core
// enrolment store writes its second PEM triple through exactly the same
// durability guarantee rather than a second copy of this code.
func syncWriteFile(path string, data []byte, perm os.FileMode) error {
	return atomicfile.Write(path, data, perm)
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
		if err := syncWriteFile(filepath.Join(configPath, f.name), []byte(f.data), f.mode); err != nil {
			return fmt.Errorf("writing %s: %w", f.name, err)
		}
	}

	_ = os.WriteFile(filepath.Join(configPath, ".provisioned"),
		[]byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)

	return nil
}
