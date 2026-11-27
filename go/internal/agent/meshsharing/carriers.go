package meshsharing

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// SaveCarrierConfig replaces only local-mesh.json. Persist an explicit
// override when the last carrier is disabled so the defaults stay off.
func SaveCarrierConfig(path string, self meshid.Device, config localmesh.TCPConfig) error {
	if config.Listen == "" && config.TLSListen == "" && len(config.Peers) == 0 && !config.NAN && !config.BLE && !config.Ethernet && !config.InfrastructureWiFi {
		return atomicConfigFile(path, []byte("{\"noCarriers\":true}\n"))
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
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
