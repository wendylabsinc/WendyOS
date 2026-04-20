//go:build windows

package commands

import "fmt"

func writeConfigPartition(d drive, agentBinary []byte, ssid, password string) error {
	return fmt.Errorf("config partition provisioning is not supported on Windows")
}

func ejectDisk(_ string) {}
