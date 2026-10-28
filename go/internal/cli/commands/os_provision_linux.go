//go:build linux

package commands

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/wendyconf"
)

// configPartitionSupported reports whether writeConfigPartition has a working
// implementation on this OS. Callers gate the agent download + config write
// on this so non-supported platforms don't pay the network cost just to fail.
const configPartitionSupported = true

// writeConfigPartition finds, mounts, populates, and unmounts the FAT32 config
// partition on d after a dd write. agentBinary is the arm64 agent binary
// content. creds and deviceName are written to wendy.conf when non-empty.
func writeConfigPartition(d drive, agentBinary []byte, creds []wendyconf.WifiCredential, deviceName string, provisioningJSON []byte) error {
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

	// vfat has no on-disk ownership; the kernel synthesizes each file's uid from
	// the mounting process. Without uid=/gid=, sudo gives every file to root and
	// the subsequent non-root WriteFile calls in writeConfigFiles fail with
	// EACCES. The device applies its own uid when it boots, so this only
	// affects the host-side view of the mount.
	mountOpts := fmt.Sprintf("uid=%d,gid=%d", os.Getuid(), os.Getgid())
	mountCmd := exec.Command("sudo", "mount", "-t", "vfat", "-o", mountOpts, partDev, tmpDir)
	if out, err := mountCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mounting config partition %s: %s: %w", partDev, strings.TrimSpace(string(out)), err)
	}
	defer exec.Command("sudo", "umount", tmpDir).Run() //nolint:errcheck

	return writeConfigFiles(tmpDir, agentBinary, creds, deviceName, provisioningJSON)
}

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

// rescanPartitionTable makes the kernel re-read d's partition table and waits
// for udev to catch up. lsblk reports udev's view, which right after an image
// write can still describe the drive's previous contents (WDY-3974: an NVMe
// that held Orin was fingerprinted with Orin's GUIDs after a Pi image landed).
func rescanPartitionTable(d drive) {
	exec.Command("sudo", "partprobe", d.DevicePath).Run()   //nolint:errcheck
	exec.Command("udevadm", "settle", "--timeout=10").Run() //nolint:errcheck
}

// readDriveIdentity fingerprints d from lsblk: its size, GPT disk GUID,
// serial and model, and the unique GUID of its config partition.
func readDriveIdentity(d drive) (driveIdentity, error) {
	out, err := exec.Command("lsblk", "--json", "--bytes", "-o", "NAME,SIZE,TYPE,PTUUID,PARTUUID,LABEL,SERIAL,MODEL", d.DevicePath).Output()
	if err != nil {
		return driveIdentity{}, fmt.Errorf("lsblk %s: %w", d.DevicePath, err)
	}
	return parseLsblkIdentity(out)
}

// ejectDisk is a no-op on Linux; drives are simply unmounted.
func ejectDisk(_ drive) {}
