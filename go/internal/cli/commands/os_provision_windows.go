//go:build windows

package commands

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/env"
	"github.com/wendylabsinc/wendy/go/internal/shared/wendyconf"
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
		if cleanupErr := unmountAndOfflineDisk(diskNum, d.IsRemovable); cleanupErr != nil {
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
	out, err := exec.Command(env.PowershellExe(), "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
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
	out, err := exec.Command(env.PowershellExe(), "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// unmountAndOfflineDisk removes drive letters from every partition on the
// disk and (for non-removable disks) takes the disk offline. Mirrors the
// cleanup pattern in writeImageToDisk (disklister_windows.go) — disk-wide
// rather than partition-scoped because Windows may auto-assign letters to
// EFI / rootfs / recovery partitions when we brought the disk online, and
// leaving any of those in place results in phantom drives in Explorer
// after the install.
//
// SilentlyContinue on Remove-PartitionAccessPath: the access paths may
// already be gone (e.g. user yanked the SD card) and we don't want a
// secondary error to mask the upstream failure.
//
// The Set-Disk -IsOffline step is skipped for removable targets — Windows
// rejects it on USB / SD / MMC and on the PCIE card readers that report
// as SCSI but are flagged removable by looksLikeCardReader, with "Not
// Supported: Removable media cannot be set to offline." (WDY-1178).
// Gating in Go on the same drive.IsRemovable predicate used to select the
// install target keeps the cleanup predicate in lockstep with selection.
// Removing the partition access paths above is sufficient for removable
// media: the user pulls the device, and there is no persistent
// online/offline state to worry about.
func unmountAndOfflineDisk(diskNum int, removable bool) error {
	script := fmt.Sprintf(
		"Get-Partition -DiskNumber %d -ErrorAction SilentlyContinue | "+
			"Where-Object { $_.DriveLetter } | "+
			"ForEach-Object { "+
			"Remove-PartitionAccessPath -DiskNumber $_.DiskNumber -PartitionNumber $_.PartitionNumber -AccessPath \"$($_.DriveLetter):\\\" -ErrorAction SilentlyContinue "+
			"}",
		diskNum,
	)
	if !removable {
		script += fmt.Sprintf("; Set-Disk -Number %d -IsOffline $true -ErrorAction Stop", diskNum)
	}
	out, err := exec.Command(env.PowershellExe(), "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

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
	out, err := exec.Command(env.PowershellExe(), "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if err != nil {
		return 0, fmt.Errorf("enumerating partitions on disk %d: %w", diskNum, err)
	}
	return parseConfigPartition(out)
}

// rescanPartitionTable refreshes Windows' cached view of d: writeImageToDisk
// cleared the disk before writing, and until Update-Disk runs Get-Disk can
// still report the cleared disk, without the GUID of the image just written.
func rescanPartitionTable(d drive) {
	diskNum, err := parseDiskNumber(d.DevicePath)
	if err != nil {
		return
	}
	updateDisk(diskNum) //nolint:errcheck
}

// readDriveIdentity fingerprints d from Get-Disk: its size, GPT disk GUID,
// serial number and friendly name. writeImageToDisk leaves a fixed disk
// offline, where volume labels — and with them the config partition — cannot
// be read, so the config-partition GUID is left out on Windows.
func readDriveIdentity(d drive) (driveIdentity, error) {
	diskNum, err := parseDiskNumber(d.DevicePath)
	if err != nil {
		return driveIdentity{}, err
	}
	ids, err := windowsDiskIdentities(fmt.Sprintf("Get-Disk -Number %d -ErrorAction Stop", diskNum))
	if err != nil {
		return driveIdentity{}, err
	}
	id, ok := ids[diskNum]
	if !ok {
		return driveIdentity{}, fmt.Errorf("Get-Disk did not report disk %d", diskNum)
	}
	return id, nil
}

// listIdentifiedDrives fingerprints every drive the installer would offer as a
// target, with one Get-Disk query for all of them.
func listIdentifiedDrives() ([]identifiedDrive, error) {
	drives, err := listAllDrives()
	if err != nil {
		return nil, err
	}
	ids, err := windowsDiskIdentities("Get-Disk")
	if err != nil {
		return nil, err
	}
	var out []identifiedDrive
	for _, d := range drives {
		diskNum, err := parseDiskNumber(d.DevicePath)
		if err != nil {
			continue
		}
		if id, ok := ids[diskNum]; ok {
			out = append(out, identifiedDrive{drive: d, id: id})
		}
	}
	return out, nil
}

// windowsDiskIdentities runs the Get-Disk identity query over the disks the
// selector names.
func windowsDiskIdentities(selector string) (map[int]driveIdentity, error) {
	script := selector + " | ForEach-Object { " +
		"[PSCustomObject]@{ Number=$_.Number; Guid=$_.Guid; Size=$_.Size; " +
		"SerialNumber=$_.SerialNumber; FriendlyName=$_.FriendlyName } " +
		"} | ConvertTo-Json -Compress"
	out, err := exec.Command(env.PowershellExe(), "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if err != nil {
		return nil, fmt.Errorf("running Get-Disk: %w", err)
	}
	return parseWindowsDiskIdentities(out)
}

// drivePresent reports whether Windows still lists d's disk number.
func drivePresent(d drive) bool {
	diskNum, err := parseDiskNumber(d.DevicePath)
	if err != nil {
		return false
	}
	script := fmt.Sprintf("Get-Disk -Number %d -ErrorAction Stop | Out-Null", diskNum)
	return exec.Command(env.PowershellExe(), "-NoProfile", "-NonInteractive", "-Command", script).Run() == nil
}

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

func readPartitionDriveLetter(diskNum, partNum int) (string, error) {
	script := fmt.Sprintf(
		"$p = Get-Partition -DiskNumber %d -PartitionNumber %d -ErrorAction Stop; "+
			"if ($p.DriveLetter) { [string]$p.DriveLetter }",
		diskNum, partNum,
	)
	out, err := exec.Command(env.PowershellExe(), "-NoProfile", "-NonInteractive", "-Command", script).Output()
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
	out, err := exec.Command(env.PowershellExe(), "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
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
	_ = exec.Command(env.PowershellExe(), "-NoProfile", "-NonInteractive", "-Command", script).Run()
}

func ejectDisk(d drive) {
	// Skip the offline call for removable targets — Windows rejects
	// Set-Disk -IsOffline $true on USB / SD / MMC and on the PCIE card
	// readers that report as SCSI but are flagged removable by
	// looksLikeCardReader (WDY-1178). Gating on d.IsRemovable matches the
	// same predicate that put the disk in the selectable-target list,
	// so any path that selected this drive for install will also skip the
	// offline step.
	//
	// For removable media there is nothing useful to do here anyway —
	// writeConfigPartition's deferred cleanup already removed the drive
	// letters, and the user pulls the media to "eject" it physically.
	if d.IsRemovable {
		return
	}
	diskNum, err := parseDiskNumber(d.DevicePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot eject %s: %v\n", d.DevicePath, err)
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
	out, err := exec.Command(env.PowershellExe(), "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to take disk %d offline (it may still be online with assigned drive letters): %v: %s\n", diskNum, err, strings.TrimSpace(string(out)))
	}
}
