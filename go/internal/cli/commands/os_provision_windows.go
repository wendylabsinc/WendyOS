//go:build windows

package commands

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/internal/shared/wendyconf"
)

// configPartitionSupported reports whether writeConfigPartition has a working
// implementation on this OS. Callers gate the agent download + config write
// on this so non-supported platforms don't pay the network cost just to fail.
const configPartitionSupported = true

// writeConfigPartition brings the target disk online, locates the FAT32
// partition labelled "config", assigns it a drive letter, populates it with
// the agent binary and provisioning files via the shared writeConfigFiles
// helper, then unmounts every partition on the disk and takes the disk
// offline. The disk-wide unmount is what stops Windows from leaving phantom
// drive letters for partitions Explorer auto-mounted during the online
// window (EFI, rootfs, config, recovery — there are typically several).
//
// On entry the disk is expected to be offline — writeImageToDisk takes it
// offline at the end of the raw-image phase to suppress auto-mount of every
// partition Windows finds in the freshly-written table.
func writeConfigPartition(d drive, agentBinary []byte, creds []wendyconf.WifiCredential, deviceName string, provisioningJSON []byte) (retErr error) {
	diskNum, err := parseDiskNumber(d.DevicePath)
	if err != nil {
		return err
	}

	if err := setDiskOnline(diskNum); err != nil {
		return fmt.Errorf("bringing disk %d online: %w", diskNum, err)
	}
	// From here the disk is online — register cleanup BEFORE the partition
	// rescan so a failure between online and rescan still triggers
	// unmount + offline. If the main path succeeded, a cleanup failure must
	// be promoted to the returned error: leaving the disk online with
	// phantom letters is the bug this PR exists to prevent, and silently
	// returning nil would let the caller print "Successfully installed"
	// against a half-cleaned-up disk.
	defer func() {
		if cleanupErr := unmountAndOfflineDisk(diskNum); cleanupErr != nil {
			if retErr == nil {
				retErr = fmt.Errorf("config partition write succeeded but cleanup failed (disk may still be online with phantom drive letters): %w", cleanupErr)
			} else {
				fmt.Fprintf(os.Stderr, "warning: failed to unmount/offline disk %d during cleanup: %v\n", diskNum, cleanupErr)
			}
		}
	}()

	if err := updateDisk(diskNum); err != nil {
		return fmt.Errorf("rescanning disk %d: %w", diskNum, err)
	}

	// The storage stack needs a moment after Update-Disk before partitions
	// are reliably enumerable by Get-Volume; mirrors the partprobe sleep on
	// Linux.
	time.Sleep(500 * time.Millisecond)

	partNum, err := findConfigPartitionNumber(diskNum)
	if err != nil {
		return err
	}

	mountPath, err := ensureConfigPartitionMounted(diskNum, partNum)
	if err != nil {
		return err
	}

	if err := writeConfigFiles(mountPath, agentBinary, creds, deviceName, provisioningJSON); err != nil {
		return err
	}

	flushVolume(mountPath)
	return nil
}

// setDiskOnline brings the disk online. Set-Disk -IsOffline does not prompt,
// so we don't pass -Confirm:$false (the legacy Storage module rejects
// -Confirm as unknown — see WINDOWS_REGRESSION_REVIEW.md section 1.1).
func setDiskOnline(diskNum int) error {
	script := fmt.Sprintf("Set-Disk -Number %d -IsOffline $false -ErrorAction Stop", diskNum)
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// updateDisk rescans the disk's partition table so partitions written by
// the dd phase become enumerable. Kept separate from setDiskOnline so a
// rescan failure still triggers the deferred cleanup of the now-online disk.
func updateDisk(diskNum int) error {
	script := fmt.Sprintf("Update-Disk -Number %d -ErrorAction Stop", diskNum)
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// unmountAndOfflineDisk removes drive letters from every partition on the
// disk and then takes the disk offline. Mirrors the cleanup pattern in
// writeImageToDisk (disklister_windows.go) — disk-wide rather than
// partition-scoped because Windows may auto-assign letters to EFI / rootfs
// / recovery partitions when we brought the disk online, and leaving any of
// those in place results in phantom drives in Explorer after the install.
//
// SilentlyContinue on Remove-PartitionAccessPath: the access paths may
// already be gone (e.g. user yanked the SD card) and we don't want a
// secondary error to mask the upstream failure.
func unmountAndOfflineDisk(diskNum int) error {
	script := fmt.Sprintf(
		"Get-Partition -DiskNumber %d -ErrorAction SilentlyContinue | "+
			"Where-Object { $_.DriveLetter } | "+
			"ForEach-Object { "+
			"Remove-PartitionAccessPath -DiskNumber $_.DiskNumber -PartitionNumber $_.PartitionNumber -AccessPath \"$($_.DriveLetter):\\\" -ErrorAction SilentlyContinue "+
			"}; "+
			"Set-Disk -Number %d -IsOffline $true -ErrorAction Stop",
		diskNum, diskNum,
	)
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// findConfigPartitionNumber returns the partition number on diskNum whose
// FAT32 filesystem label matches "config" (case-insensitive, FAT32 padding
// tolerated). The PowerShell script casts DriveLetter to [string] because
// PowerShell hands back a [char] when assigned, whose JSON encoding is the
// integer codepoint — that would silently corrupt the parser. -Depth 3
// suppresses the depth-exceeded warning on nested CIM objects.
func findConfigPartitionNumber(diskNum int) (int, error) {
	script := fmt.Sprintf(
		"Get-Partition -DiskNumber %d -ErrorAction Stop | "+
			"ForEach-Object { "+
			"$vol = $_ | Get-Volume -ErrorAction SilentlyContinue; "+
			"[PSCustomObject]@{ "+
			"PartitionNumber = $_.PartitionNumber; "+
			"DriveLetter = if ($_.DriveLetter) { [string]$_.DriveLetter } else { $null }; "+
			"Label = if ($vol) { $vol.FileSystemLabel } else { $null }; "+
			"Size = $_.Size "+
			"} "+
			"} | ConvertTo-Json -Compress -Depth 3",
		diskNum,
	)
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if err != nil {
		return 0, fmt.Errorf("enumerating partitions on disk %d: %w", diskNum, err)
	}
	return parseConfigPartition(out)
}

// ensureConfigPartitionMounted returns the mount path (e.g. "X:\") for the
// given partition, assigning a drive letter if Windows hasn't already
// auto-assigned one.
func ensureConfigPartitionMounted(diskNum, partNum int) (string, error) {
	letter, err := readPartitionDriveLetter(diskNum, partNum)
	if err != nil {
		return "", fmt.Errorf("reading drive letter for disk %d partition %d: %w", diskNum, partNum, err)
	}
	if letter == "" {
		if err := assignPartitionDriveLetter(diskNum, partNum); err != nil {
			return "", fmt.Errorf("assigning drive letter to disk %d partition %d: %w", diskNum, partNum, err)
		}
		letter, err = readPartitionDriveLetter(diskNum, partNum)
		if err != nil {
			return "", fmt.Errorf("re-reading drive letter for disk %d partition %d: %w", diskNum, partNum, err)
		}
		if letter == "" {
			return "", fmt.Errorf("Windows assigned no drive letter to disk %d partition %d (Group Policy or letter exhaustion?)", diskNum, partNum)
		}
	}
	return letter + `:\`, nil
}

// readPartitionDriveLetter returns the assigned drive letter (e.g. "X") or
// empty string when none is assigned.
func readPartitionDriveLetter(diskNum, partNum int) (string, error) {
	script := fmt.Sprintf(
		"$p = Get-Partition -DiskNumber %d -PartitionNumber %d -ErrorAction Stop; "+
			"if ($p.DriveLetter) { [string]$p.DriveLetter }",
		diskNum, partNum,
	)
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func assignPartitionDriveLetter(diskNum, partNum int) error {
	script := fmt.Sprintf(
		"Add-PartitionAccessPath -DiskNumber %d -PartitionNumber %d -AssignDriveLetter -ErrorAction Stop",
		diskNum, partNum,
	)
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// flushVolume best-effort flushes outstanding FAT32 writes for the given
// mount path. The driver also flushes when the access path is removed, but
// belt-and-suspenders is cheap given the cost of a corrupted boot config.
func flushVolume(mountPath string) {
	if mountPath == "" {
		return
	}
	letter := string(mountPath[0])
	script := fmt.Sprintf("Write-VolumeCache -DriveLetter %s -ErrorAction SilentlyContinue", letter)
	_ = exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).Run()
}

func ejectDisk(devPath string) {
	diskNum, err := parseDiskNumber(devPath)
	if err != nil {
		return
	}
	// Idempotent: writeConfigPartition's deferred cleanup already took the
	// disk offline. This catches the path where the user invoked something
	// that bypassed writeConfigPartition (e.g. a future caller that imaged
	// without provisioning, or an early bail before the online step).
	//
	// No -Confirm:$false: Set-Disk -IsOffline does not prompt, and the
	// legacy Storage module rejects -Confirm as unknown (see
	// WINDOWS_REGRESSION_REVIEW.md §1.1). On the failure paths from
	// writeConfigPartition this is the last attempt to offline the disk —
	// surfacing rather than silently swallowing failure means a stuck
	// online disk is at least visible to the user.
	//
	// Check IsOffline first so the success path (where writeConfigPartition's
	// deferred cleanup already offlined the disk) doesn't emit a spurious
	// warning if the legacy module errors on a redundant Set-Disk.
	script := fmt.Sprintf(
		"$d = Get-Disk -Number %d -ErrorAction Stop; "+
			"if (-not $d.IsOffline) { Set-Disk -Number %d -IsOffline $true -ErrorAction Stop }",
		diskNum, diskNum,
	)
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to take disk %d offline (it may still be online with assigned drive letters): %v: %s\n", diskNum, err, strings.TrimSpace(string(out)))
	}
}
