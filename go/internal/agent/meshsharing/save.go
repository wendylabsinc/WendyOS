package meshsharing

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// SaveConfig durably replaces the existing participation, roaming and sharing
// document. RPC handlers serialize read-modify-write operations above this API.
func SaveConfig(path string, config Config) error {
	if err := config.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(config)
	if err != nil {
		return err
	}
	return atomicConfigFile(path, append(data, '\n'))
}

func atomicConfigFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".local-mesh-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err = file.Chmod(0600); err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	err = errors.Join(writeErr, file.Sync())
	if err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return err
	}
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}
