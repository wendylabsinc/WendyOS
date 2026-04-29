//go:build darwin

package commands

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/wendylabsinc/wendy/internal/shared/wendyconf"
)

// writeConfigPartition finds, mounts, populates, and unmounts the FAT32 config
// partition on d after a dd write. agentBinary is the arm64 agent binary
// content. creds and deviceName are written to wendy.conf when non-empty.
func writeConfigPartition(d drive, agentBinary []byte, creds []wendyconf.WifiCredential, deviceName string, provisioningJSON []byte) error {
	partDev, err := findConfigPartition(d.DevicePath)
	if err != nil {
		return fmt.Errorf("locating config partition on %s: %w", d.DevicePath, err)
	}

	mountPoint, err := mountConfigPartition(partDev)
	if err != nil {
		return fmt.Errorf("mounting config partition %s: %w", partDev, err)
	}
	defer exec.Command("diskutil", "unmount", partDev).Run() //nolint:errcheck

	return writeConfigFiles(mountPoint, agentBinary, creds, deviceName, provisioningJSON)
}

// findConfigPartition runs `diskutil list <diskDev>` (which also rescans the
// partition table after dd) and returns the device node for the partition
// labelled "config".
func findConfigPartition(diskDev string) (string, error) {
	out, err := exec.Command("diskutil", "list", diskDev).Output()
	if err != nil {
		return "", fmt.Errorf("diskutil list %s: %w", diskDev, err)
	}

	// diskutil list output contains lines like:
	//    2:  Microsoft Basic Data  config      67.1 MB    disk4s2
	// We look for a field equal to "config" and take the last field as the
	// partition device (without the /dev/ prefix).
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		for i, f := range fields {
			if strings.EqualFold(f, "config") && i > 0 {
				last := fields[len(fields)-1]
				if strings.HasPrefix(last, "disk") {
					return "/dev/" + last, nil
				}
			}
		}
	}
	return "", fmt.Errorf("config partition not found on %s (is the image fully written?)", diskDev)
}

// mountConfigPartition ensures partDev is mounted and returns its mount point.
// It calls `diskutil mount` (a no-op if already mounted) then queries
// `diskutil info` for the authoritative mount point, avoiding brittle output
// parsing of the mount command itself.
func mountConfigPartition(partDev string) (string, error) {
	// Attempt to mount; ignore errors — the partition may already be auto-mounted
	// by macOS (FAT32 volumes are mounted automatically when they appear).
	exec.Command("diskutil", "mount", partDev).Run() //nolint:errcheck

	out, err := exec.Command("diskutil", "info", partDev).Output()
	if err != nil {
		return "", fmt.Errorf("diskutil info %s: %w", partDev, err)
	}

	// Parse "   Mount Point:               /Volumes/config"
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "Mount Point:") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			if mp := strings.TrimSpace(parts[1]); mp != "" {
				return mp, nil
			}
		}
	}
	return "", fmt.Errorf("config partition %s is not mounted (diskutil mount may have failed)", partDev)
}
