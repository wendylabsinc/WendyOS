package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
)

// syncWriteFile is retained for the other service-owned state files.
func syncWriteFile(path string, data []byte, perm os.FileMode) error {
	return atomicfile.Write(path, data, perm)
}

// provisioningFileMatches avoids replacing an already correct state file on startup.
// Lstat and opened-file identity checks prevent accepting a symlink as the
// state file; a changed, unreadable or incorrectly permissioned path uses the
// same durable atomic writer as a missing file.
func provisioningFileMatches(path string, data []byte, perm os.FileMode) bool {
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

// WriteDeviceKey persists the private key separately from the enrollment JSON.
func WriteDeviceKey(configPath string, keyPEM []byte) error {
	if len(keyPEM) == 0 {
		return fmt.Errorf("device private key is empty")
	}
	return writeProvisioningFile(configPath, "device-key.pem", keyPEM)
}

// WriteProvisioningState persists enrollment JSON after its private key is safe.
// Remove legacy embedded keys while retaining unknown fields for compatibility.
func WriteProvisioningState(configPath string, data []byte) error {
	var state map[string]json.RawMessage
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decoding provisioning state: %w", err)
	}
	if state == nil {
		return fmt.Errorf("provisioning state must be an object")
	}
	removed := false
	for name := range state {
		if strings.EqualFold(name, "keyPem") {
			delete(state, name)
			removed = true
		}
	}
	if removed {
		var err error
		data, err = json.MarshalIndent(state, "", "  ")
		if err != nil {
			return fmt.Errorf("encoding provisioning state: %w", err)
		}
	}
	return writeProvisioningFile(configPath, "provisioning.json", data)
}

func writeProvisioningFile(configPath, name string, data []byte) error {
	if err := os.MkdirAll(configPath, 0o700); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}
	path := filepath.Join(configPath, name)
	if provisioningFileMatches(path, data, 0o600) {
		return nil
	}
	if err := syncWriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	return nil
}
