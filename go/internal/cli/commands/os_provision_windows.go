//go:build windows

package commands

import (
	"fmt"
	"os/exec"

	"github.com/wendylabsinc/wendy/internal/shared/wendyconf"
)

// configPartitionSupported is false on Windows: writeConfigPartition has no
// implementation, so callers must skip the agent download and refuse to claim
// success when --wifi/--device-name/--pre-enroll were requested.
const configPartitionSupported = false

// writeConfigPartition is a stub on Windows. Callers gate on
// configPartitionSupported and never invoke this; it exists only so the
// cross-platform call site in provisionConfigPartition compiles.
func writeConfigPartition(_ drive, _ []byte, _ []wendyconf.WifiCredential, _ string, _ []byte) error {
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
