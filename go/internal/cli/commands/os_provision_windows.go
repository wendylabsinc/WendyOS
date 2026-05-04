//go:build windows

package commands

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/wendylabsinc/wendy/internal/shared/wendyconf"
)

func writeConfigPartition(d drive, agentBinary []byte, creds []wendyconf.WifiCredential, deviceName string, _ []byte) error {
	return fmt.Errorf("config partition provisioning is not supported on Windows")
}

func ejectDisk(devPath string) {
	diskNum, err := parseDiskNumber(devPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot eject %s: %v\n", devPath, err)
		return
	}
	// Take the disk offline so Windows doesn't auto-assign drive letters to
	// the partitions in the freshly written image. -Confirm:$false is omitted
	// because Set-Disk -IsOffline doesn't prompt and the legacy Storage
	// module rejects -Confirm.
	script := fmt.Sprintf("Set-Disk -Number %d -IsOffline $true -ErrorAction SilentlyContinue", diskNum)
	if out, err := exec.Command(powershellExe, "-NoProfile", "-Command", script).CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		// If eject fails the user will see Explorer flooded with
		// auto-mounted phantom drive letters from the new image — surface
		// the cause so they know what to clean up.
		if msg != "" {
			fmt.Fprintf(os.Stderr, "warning: failed to set disk %d offline: %v: %s\n", diskNum, err, msg)
		} else {
			fmt.Fprintf(os.Stderr, "warning: failed to set disk %d offline: %v\n", diskNum, err)
		}
	}
}
