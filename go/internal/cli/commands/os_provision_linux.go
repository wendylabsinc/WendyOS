//go:build linux

package commands

import (
	"errors"
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

// How long and how often writeConfigPartition retries finding and mounting the
// config partition. Vars so tests can shorten them.
var (
	configPartitionTimeout      = 10 * time.Second
	configPartitionPollInterval = 250 * time.Millisecond
)

// writeConfigPartition finds, mounts, populates, and unmounts the FAT32 config
// partition on d after a dd write. agentBinary is the arm64 agent binary
// content. creds and deviceName are written to wendy.conf when non-empty. A
// failed unmount is a *configStillMountedError: the drive is not safe to remove yet.
func writeConfigPartition(d drive, agentBinary []byte, creds []wendyconf.WifiCredential, deviceName string, provisioningJSON []byte) error {
	rescanPartitionTable(d)

	tmpDir, err := os.MkdirTemp("", "wendyos-config-*")
	if err != nil {
		return fmt.Errorf("creating temp mount dir: %w", err)
	}

	// vfat has no on-disk ownership; the kernel synthesizes each file's uid from
	// the mounting process. Without uid=/gid=, sudo gives every file to root and
	// the subsequent non-root WriteFile calls in writeConfigFiles fail with
	// EACCES. The device applies its own uid when it boots, so this only
	// affects the host-side view of the mount.
	mountOpts := fmt.Sprintf("uid=%d,gid=%d", os.Getuid(), os.Getgid())
	err = retryConfigPartition(func() (bool, error) {
		partDev, err := findConfigPartition(d.DevicePath)
		if err != nil {
			return true, fmt.Errorf("locating config partition on %s: %w", d.DevicePath, err)
		}
		mountCmd := exec.Command("sudo", "mount", "-t", "vfat", "-o", mountOpts, partDev, tmpDir)
		if out, err := mountCmd.CombinedOutput(); err != nil {
			return mountRetryable(err), fmt.Errorf("mounting config partition %s: %s: %w", partDev, strings.TrimSpace(string(out)), err)
		}
		return false, nil
	}, func() bool { return drivePresent(d) })
	if err != nil {
		os.Remove(tmpDir) //nolint:errcheck
		return err
	}

	writeErr := writeConfigFiles(tmpDir, agentBinary, creds, deviceName, provisioningJSON)
	unmountErr := releaseMount(tmpDir, func() ([]byte, error) {
		return exec.Command("sudo", "umount", tmpDir).CombinedOutput()
	}, time.Sleep)
	if writeErr != nil {
		return errors.Join(writeErr, unmountErr)
	}
	return unmountErr
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

// Some udevd versions re-read the table in a child that settle does not wait
// for, so the partitions can still be unlabelled, or briefly gone, after the rescan.
func retryConfigPartition(attempt func() (retry bool, err error), present func() bool) error {
	deadline := time.Now().Add(configPartitionTimeout)
	for {
		retry, err := attempt()
		if err == nil || !retry || !present() {
			return err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%w (retried for %s)", err, configPartitionTimeout)
		}
		time.Sleep(configPartitionPollInterval)
	}
}

// mountRetryable reports whether err is mount(8)'s "mount failure" exit, which
// covers a device node that is gone; sudo's own failures exit 1.
func mountRetryable(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 32
}

// rescanPartitionTable makes the kernel re-read d's partition table and waits
// for udev to catch up. lsblk reports udev's view, which right after an image
// write can still describe the drive's previous contents (WDY-3974: an NVMe
// that held Orin was fingerprinted with Orin's GUIDs after a Pi image landed).
func rescanPartitionTable(d drive) {
	exec.Command("sudo", "partprobe", d.DevicePath).Run() //nolint:errcheck
	// partprobe's close makes udevd re-read the table, which can drop every
	// partition for a moment. Only a root settle pings udevd, and so can wait for that.
	exec.Command("sudo", "udevadm", "settle", "--timeout=10").Run() //nolint:errcheck
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

// ejectDisk is a no-op on Linux: writeConfigPartition already unmounted the
// only volume this CLI mounted, and reported it if it could not.
func ejectDisk(_ drive) error { return nil }
