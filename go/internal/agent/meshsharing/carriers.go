package meshsharing

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

// SaveCarrierConfig replaces only local-mesh.json. The caller retains the
// loaded TCP listener and peers while changing NAN/BLE flags. When the last
// radio is disabled and no TCP topology exists, absence represents disabled.
func SaveCarrierConfig(path string, self int32, config localmesh.TCPConfig) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if config.Listen == "" && len(config.Peers) == 0 && !config.NAN && !config.BLE {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return syncDir(dir)
	}
	data, err := json.Marshal(config)
	if err != nil {
		return err
	}
	// Reuse the runtime's strict schema and peer validation before replacing
	// the active document; unknown or invalid TCP topology never gets written.
	check, err := os.CreateTemp(dir, ".local-mesh-check-*")
	if err != nil {
		return err
	}
	defer os.Remove(check.Name())
	if _, err = check.Write(data); err != nil {
		check.Close()
		return err
	}
	if err = check.Close(); err != nil {
		return err
	}
	if _, err = localmesh.LoadTCPConfig(check.Name(), self); err != nil {
		return err
	}
	return atomicConfigFile(path, append(data, '\n'))
}

func syncDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}
