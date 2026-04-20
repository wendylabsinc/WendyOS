//go:build linux

package commands

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// writeConfigPartition finds, mounts, populates, and unmounts the FAT32 config
// partition on d after a dd write. agentBinary is the arm64 agent binary content.
// ssid/password and deviceName are written to wendy.conf when non-empty.
func writeConfigPartition(d drive, agentBinary []byte, ssid, password, deviceName string) error {
	// Re-read the partition table after dd.
	exec.Command("sudo", "partprobe", d.DevicePath).Run() //nolint:errcheck
	time.Sleep(500 * time.Millisecond)

	partDev, err := findConfigPartition(d.DevicePath)
	if err != nil {
		return fmt.Errorf("locating config partition on %s: %w", d.DevicePath, err)
	}

	tmpDir, err := os.MkdirTemp("", "wendyos-config-*")
	if err != nil {
		return fmt.Errorf("creating temp mount dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	mountCmd := exec.Command("sudo", "mount", "-t", "vfat", partDev, tmpDir)
	if out, err := mountCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mounting config partition %s: %s: %w", partDev, strings.TrimSpace(string(out)), err)
	}
	defer exec.Command("sudo", "umount", tmpDir).Run() //nolint:errcheck

	return writeConfigFiles(tmpDir, agentBinary, ssid, password, deviceName)
}

// findConfigPartition returns the device path of the partition labelled "config"
// on the given disk (e.g. /dev/sdb → /dev/sdb3).
func findConfigPartition(diskDev string) (string, error) {
	out, err := exec.Command("lsblk", "-o", "NAME,LABEL", "-n", "-r", diskDev).Output()
	if err != nil {
		return "", fmt.Errorf("lsblk %s: %w", diskDev, err)
	}

	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.EqualFold(fields[1], "config") {
			return "/dev/" + fields[0], nil
		}
	}

	return "", fmt.Errorf("config partition not found on %s", diskDev)
}

// ejectDisk is a no-op on Linux; drives are simply unmounted.
func ejectDisk(_ string) {}
