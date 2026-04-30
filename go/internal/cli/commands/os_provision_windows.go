//go:build windows

package commands

import (
	"fmt"
	"os/exec"

	"github.com/wendylabsinc/wendy/internal/shared/wendyconf"
)

func writeConfigPartition(d drive, agentBinary []byte, creds []wendyconf.WifiCredential, deviceName string, _ []byte) error {
	return fmt.Errorf("config partition provisioning is not supported on Windows")
}

func ejectDisk(devPath string) {
	diskNum, err := parseDiskNumber(devPath)
	if err != nil {
		return
	}
	// Ensure the disk is offline so Windows doesn't assign drive letters
	// to the partitions in the newly written image.
	script := fmt.Sprintf("Set-Disk -Number %d -IsOffline $true -Confirm:$false -ErrorAction SilentlyContinue", diskNum)
	_ = exec.Command("powershell", "-NoProfile", "-Command", script).Run()
}
