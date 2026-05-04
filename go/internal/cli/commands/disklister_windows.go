//go:build windows

package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unsafe"

	"github.com/dustin/go-humanize"
)

// Windows IOCTL codes for volume management.
const (
	fsctlLockVolume          = 0x00090018
	fsctlDismountVolume      = 0x00090020
	fsctlAllowExtendedDASDIO = 0x00090083
	ioctlDiskGetDriveLayout  = 0x00070050
)

// powershellExe is the absolute path to powershell.exe, resolved once at
// package init time. Looking it up via PATH is unsafe: in a 32-bit wendy.exe
// process running on 64-bit Windows, PATH-resolved `powershell` lands in
// SysWOW64, which ships a legacy Storage module that rejects modern parameters
// like -Confirm on Set-Disk. Resolving through System32 (or Sysnative when
// running under WoW64) ensures we always invoke the host-architecture
// PowerShell with the current Storage module.
var powershellExe = resolvePowershellExe()

func resolvePowershellExe() string {
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}
	// Sysnative is a virtual alias that exists only inside a 32-bit (WoW64)
	// process and points at the real System32. Prefer it so 32-bit builds of
	// wendy.exe still launch 64-bit PowerShell.
	candidates := []string{
		filepath.Join(systemRoot, "Sysnative", "WindowsPowerShell", "v1.0", "powershell.exe"),
		filepath.Join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "powershell"
}

// drive represents an external disk suitable for image writing.
type drive struct {
	DevicePath  string      // e.g. \\.\PhysicalDrive1
	RawPath     string      // same as DevicePath on Windows
	Name        string      // human-readable name
	Size        string      // human-readable size
	SizeBytes   int64       // size in bytes
	IsRemovable bool
	StorageType StorageType // detected medium: StorageSD, StorageNVMe, or StorageUnknown
}

// psDisk is the JSON structure returned by the joined Get-Disk / Get-PhysicalDisk query.
type psDisk struct {
	Number       int    `json:"Number"`
	FriendlyName string `json:"FriendlyName"`
	Size         int64  `json:"Size"`
	BusType      string `json:"BusType"`
	IsSystem     bool   `json:"IsSystem"`
	IsReadOnly   bool   `json:"IsReadOnly"`
	MediaType    string `json:"MediaType"`
}

// listAllDrives lists all physical drives on Windows using PowerShell Get-Disk.
func listAllDrives() ([]drive, error) {
	return listDrivesWindows(false)
}

// listExternalDrives lists removable/USB physical drives on Windows.
func listExternalDrives() ([]drive, error) {
	return listDrivesWindows(true)
}

func listDrivesWindows(externalOnly bool) ([]drive, error) {
	// Join Get-Disk with Get-PhysicalDisk to get both logical and physical
	// properties (BusType, IsSystem from Get-Disk; MediaType from Get-PhysicalDisk).
	script := "Get-Disk | ForEach-Object { " +
		"$pd = Get-PhysicalDisk -DeviceNumber $_.Number -ErrorAction SilentlyContinue; " +
		"$mt = if ($pd) { $pd.MediaType } else { 'Unspecified' }; " +
		"[PSCustomObject]@{ Number=$_.Number; FriendlyName=$_.FriendlyName; Size=$_.Size; " +
		"BusType=$_.BusType; IsSystem=$_.IsSystem; IsReadOnly=$_.IsReadOnly; MediaType=$mt } " +
		"} | ConvertTo-Json -Compress"
	out, err := exec.Command(powershellExe, "-NoProfile", "-Command", script).Output()
	if err != nil {
		return nil, fmt.Errorf("running Get-Disk: %w", err)
	}

	outStr := strings.TrimSpace(string(out))
	if outStr == "" {
		return nil, nil
	}

	// PowerShell returns a single object (not array) when there's only one disk.
	var disks []psDisk
	if strings.HasPrefix(outStr, "[") {
		if err := json.Unmarshal([]byte(outStr), &disks); err != nil {
			return nil, fmt.Errorf("parsing Get-Disk output: %w", err)
		}
	} else {
		var single psDisk
		if err := json.Unmarshal([]byte(outStr), &single); err != nil {
			return nil, fmt.Errorf("parsing Get-Disk output: %w", err)
		}
		disks = []psDisk{single}
	}

	var drives []drive
	for _, d := range disks {
		if d.IsReadOnly || d.IsSystem {
			continue
		}

		external := isExternalBus(d.BusType)
		if externalOnly {
			// Definitely include USB, SD, and MMC bus types.
			// For other bus types (SCSI, SATA, NVMe, etc.), only include
			// if it looks like a card reader: non-fixed media and the
			// friendly name contains "card reader".
			if !external && !looksLikeCardReader(d) {
				continue
			}
		}

		devPath := fmt.Sprintf(`\\.\PhysicalDrive%d`, d.Number)
		drives = append(drives, drive{
			DevicePath:  devPath,
			RawPath:     devPath,
			Name:        d.FriendlyName,
			Size:        humanize.Bytes(uint64(d.Size)),
			SizeBytes:   d.Size,
			IsRemovable: external || looksLikeCardReader(d),
			StorageType: detectStorageTypeWindows(d.BusType, d.FriendlyName),
		})
	}

	return drives, nil
}

// detectStorageTypeWindows infers the physical storage medium from the
// PowerShell BusType and friendly name.
//
//   - "SD" and "MMC" bus types are always SD cards.
//   - "USB" bus type with an SD-keyword name is an SD card reader.
//   - "USB" bus type without SD keywords is assumed to be an NVMe enclosure.
func detectStorageTypeWindows(busType, friendlyName string) StorageType {
	switch strings.ToUpper(busType) {
	case "SD", "MMC":
		return StorageSD
	case "USB":
		lower := strings.ToLower(friendlyName)
		for _, kw := range []string{"emmc", "e-mmc", "embedded mmc"} {
			if strings.Contains(lower, kw) {
				return StorageEMMC
			}
		}
		for _, kw := range []string{"sd card", "sdhc", "sdxc", "sd/mmc", "mmc"} {
			if strings.Contains(lower, kw) {
				return StorageSD
			}
		}
		return StorageNVMe
	default:
		return StorageUnknown
	}
}

// isExternalBus returns true for bus types that indicate a removable/external drive.
func isExternalBus(busType string) bool {
	switch strings.ToUpper(busType) {
	case "USB", "SD", "MMC":
		return true
	default:
		return false
	}
}

// isFixedMedia returns true for media types that are permanently installed
// (SSD, HDD). Returns false for unspecified/removable media (SD cards in
// built-in readers, USB sticks) which often report as "Unspecified".
func isFixedMedia(mediaType string) bool {
	switch strings.ToUpper(mediaType) {
	case "SSD", "HDD":
		return true
	default:
		return false
	}
}

// looksLikeCardReader returns true if a non-USB disk appears to be a
// built-in card reader (e.g., Realtek PCIE readers that report as SCSI).
// This is a heuristic: non-fixed media + name contains "card reader".
func looksLikeCardReader(d psDisk) bool {
	return !isFixedMedia(d.MediaType) &&
		strings.Contains(strings.ToLower(d.FriendlyName), "card reader")
}

// getVolumesForDisk returns the drive letters (e.g. ["E", "F"]) for volumes
// on a given physical disk number.
func getVolumesForDisk(diskNumber int) ([]string, error) {
	script := fmt.Sprintf(
		"Get-Partition -DiskNumber %d -ErrorAction SilentlyContinue | "+
			"Where-Object { $_.DriveLetter } | "+
			"Select-Object -ExpandProperty DriveLetter",
		diskNumber,
	)
	out, err := exec.Command(powershellExe, "-NoProfile", "-Command", script).Output()
	if err != nil {
		return nil, nil // no partitions is fine
	}
	var letters []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		l := strings.TrimSpace(line)
		if len(l) > 0 {
			letters = append(letters, l[:1])
		}
	}
	return letters, nil
}

// lockAndDismountVolume opens a volume by drive letter, locks it with
// FSCTL_LOCK_VOLUME, then dismounts it with FSCTL_DISMOUNT_VOLUME.
// Returns the volume handle which must be kept open until writing is complete.
func lockAndDismountVolume(letter string) (syscall.Handle, error) {
	volPath := `\\.\` + letter + ":"
	pathUTF16, err := syscall.UTF16PtrFromString(volPath)
	if err != nil {
		return syscall.InvalidHandle, err
	}

	h, err := syscall.CreateFile(
		pathUTF16,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return syscall.InvalidHandle, fmt.Errorf("opening volume %s: %w", volPath, err)
	}

	var bytesReturned uint32

	// Lock the volume to get exclusive access.
	err = syscall.DeviceIoControl(h, fsctlLockVolume, nil, 0, nil, 0, &bytesReturned, nil)
	if err != nil {
		syscall.CloseHandle(h)
		return syscall.InvalidHandle, fmt.Errorf("locking volume %s: %w", volPath, err)
	}

	// Dismount the volume's filesystem.
	err = syscall.DeviceIoControl(h, fsctlDismountVolume, nil, 0, nil, 0, &bytesReturned, nil)
	if err != nil {
		syscall.CloseHandle(h)
		return syscall.InvalidHandle, fmt.Errorf("dismounting volume %s: %w", volPath, err)
	}

	return h, nil
}

// physicalDrivePathRE matches a Windows physical-drive path with the disk
// number captured. The end anchor matters: fmt.Sscanf with %d would silently
// accept `\\.\PhysicalDrive1abc` as disk 1, picking up a path the user almost
// certainly didn't intend.
var physicalDrivePathRE = regexp.MustCompile(`^\\\\\.\\PhysicalDrive(\d+)$`)

// parseDiskNumber extracts the disk number from a \\.\PhysicalDriveN path.
func parseDiskNumber(devPath string) (int, error) {
	m := physicalDrivePathRE.FindStringSubmatch(devPath)
	if m == nil {
		return 0, fmt.Errorf("parsing disk number from %q: not a physical drive path", devPath)
	}
	var n int
	if _, err := fmt.Sscanf(m[1], "%d", &n); err != nil {
		return 0, fmt.Errorf("parsing disk number from %q: %w", devPath, err)
	}
	return n, nil
}

// clearDiskPartitions uses PowerShell Clear-Disk to remove all partitions,
// volumes, and OEM recovery data from the disk. This releases Windows' hold
// on volumes that have no drive letter (e.g. EFI, recovery, or Jetson
// partitions) which would otherwise block raw disk writes with "Access denied".
//
// We first inspect Get-Disk's PartitionStyle: an uninitialized disk reports
// "RAW" and Clear-Disk has nothing to do. Skipping in that case avoids a
// non-terminating error whose message text is locale-dependent.
func clearDiskPartitions(diskNum int) error {
	script := fmt.Sprintf(
		"$d = Get-Disk -Number %d -ErrorAction Stop; "+
			"if ($d.PartitionStyle -ne 'RAW') { "+
			"Clear-Disk -Number %d -RemoveData -RemoveOEM -Confirm:$false "+
			"}",
		diskNum, diskNum,
	)
	out, err := exec.Command(powershellExe, "-NoProfile", "-Command", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("clearing disk %d: %s: %w", diskNum, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// writeImageToDisk writes an image file to a physical drive on Windows.
// It clears existing partitions, locks and dismounts remaining volumes,
// opens the raw physical device, and writes in 4 MiB chunks with
// sector-aligned I/O.
func writeImageToDisk(imagePath string, d drive, progressFn func(written int64)) error {
	diskNum, err := parseDiskNumber(d.DevicePath)
	if err != nil {
		return err
	}

	// Ensure the disk is online before clearing partitions — a previous
	// write may have left it offline. Set-Disk -IsOffline doesn't prompt, so
	// no -Confirm switch is required (and the legacy Storage module rejects
	// it outright).
	onlineScript := fmt.Sprintf("Set-Disk -Number %d -IsOffline $false", diskNum)
	_ = exec.Command(powershellExe, "-NoProfile", "-NonInteractive", "-Command", onlineScript).Run()

	// Clear all partitions on the disk first. This is necessary because
	// disks (e.g. from a prior Jetson flash) may contain many partitions
	// without drive letters that getVolumesForDisk cannot enumerate. Those
	// hidden volumes stay mounted and cause "Access is denied" on write.
	if err := clearDiskPartitions(diskNum); err != nil {
		return err
	}

	// Lock and dismount any remaining lettered volumes on this disk. We
	// must keep the volume handles open for the entire duration of the
	// write — closing them would release the lock and let Windows re-mount.
	letters, err := getVolumesForDisk(diskNum)
	if err != nil {
		return fmt.Errorf("enumerating volumes: %w", err)
	}

	var volumeHandles []syscall.Handle
	closeAllHandles := func() {
		for _, h := range volumeHandles {
			syscall.CloseHandle(h)
		}
		volumeHandles = nil
	}
	defer closeAllHandles()

	for _, letter := range letters {
		h, err := lockAndDismountVolume(letter)
		if err != nil {
			return fmt.Errorf("preparing volume %s: %w", letter, err)
		}
		volumeHandles = append(volumeHandles, h)
	}

	imgFile, err := os.Open(imagePath)
	if err != nil {
		return fmt.Errorf("opening image: %w", err)
	}
	defer imgFile.Close()

	// Open the raw physical drive for writing.
	devPathUTF16, err := syscall.UTF16PtrFromString(d.DevicePath)
	if err != nil {
		return fmt.Errorf("encoding device path: %w", err)
	}

	handle, err := syscall.CreateFile(
		devPathUTF16,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL|0x80000000, // FILE_FLAG_WRITE_THROUGH
		0,
	)
	if err != nil {
		return fmt.Errorf("opening %s for writing (are you running as Administrator?): %w", d.DevicePath, err)
	}

	// Allow writes beyond the reported partition layout. Without this,
	// Windows may reject writes that extend past existing partitions.
	var bytesReturned uint32
	_ = syscall.DeviceIoControl(handle, fsctlAllowExtendedDASDIO, nil, 0, nil, 0, &bytesReturned, nil)

	// Lock the physical drive itself for exclusive access.
	_ = syscall.DeviceIoControl(handle, fsctlLockVolume, nil, 0, nil, 0, &bytesReturned, nil)

	diskFile := os.NewFile(uintptr(handle), d.DevicePath)

	buf := make([]byte, 4*1024*1024) // 4 MiB
	var totalWritten int64
	for {
		n, readErr := imgFile.Read(buf)
		if n > 0 {
			// Writes to raw disks on Windows must be sector-aligned.
			// Pad the final chunk to a 512-byte boundary.
			writeLen := n
			if remainder := n % 512; remainder != 0 {
				writeLen = n + (512 - remainder)
				// Zero-fill the padding bytes.
				for i := n; i < writeLen; i++ {
					buf[i] = 0
				}
			}
			if _, writeErr := diskFile.Write(buf[:writeLen]); writeErr != nil {
				return fmt.Errorf("writing to disk: %w", writeErr)
			}
			totalWritten += int64(n)
			if progressFn != nil {
				progressFn(totalWritten)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("reading image: %w", readErr)
		}
	}

	// Flush the file buffers.
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	flushFileBuffers := kernel32.NewProc("FlushFileBuffers")
	flushFileBuffers.Call(uintptr(handle)) //nolint:errcheck

	// Release all our locks (physical drive + volume handles) and then
	// immediately set the disk offline. When locks are released Windows
	// rescans the partition table and auto-assigns drive letters to every
	// partition it finds (EFI, rootfs, recovery, etc.), flooding Explorer
	// with phantom drives. Setting the disk offline right after prevents this.
	//
	// os.NewFile took ownership of the underlying Windows HANDLE (it installs
	// a finalizer that calls CloseHandle), so we close exclusively through
	// diskFile.Close() — calling syscall.CloseHandle separately would
	// double-close once the finalizer ran, with undefined behavior if Windows
	// reused the handle value.
	if cerr := diskFile.Close(); cerr != nil {
		fmt.Fprintf(os.Stderr, "warning: closing %s: %v\n", d.DevicePath, cerr)
	}
	closeAllHandles()

	// Remove any auto-assigned drive letters, then take the disk offline.
	// Set-Disk -IsOffline alone doesn't remove letters that Windows already
	// assigned during the brief window between releasing locks and going offline.
	//
	// Get-Partition -ErrorAction SilentlyContinue: right after Clear-Disk the
	// partition table re-read may not have completed and the cmdlet emits a
	// non-terminating "no MSFT_Partition objects" error we don't want fatal.
	// Set-Disk: no -Confirm (legacy Storage module rejects it; -IsOffline
	// doesn't prompt) and no -ErrorAction Stop (we log exit status below).
	cleanupScript := fmt.Sprintf(
		"Get-Partition -DiskNumber %d -ErrorAction SilentlyContinue | "+
			"Where-Object { $_.DriveLetter } | "+
			"ForEach-Object { Remove-PartitionAccessPath -DiskNumber $_.DiskNumber -PartitionNumber $_.PartitionNumber -AccessPath \"$($_.DriveLetter):\\\" -ErrorAction SilentlyContinue }; "+
			"Set-Disk -Number %d -IsOffline $true",
		diskNum, diskNum,
	)
	if output, err := exec.Command(powershellExe, "-NoProfile", "-NonInteractive", "-Command", cleanupScript).CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(output))
		if msg != "" {
			fmt.Fprintf(os.Stderr, "warning: failed to set disk %d offline: %v: %s\n", diskNum, err, msg)
		} else {
			fmt.Fprintf(os.Stderr, "warning: failed to set disk %d offline: %v\n", diskNum, err)
		}
	}

	// Suppress unused import warning — unsafe is needed for DeviceIoControl pointer args.
	_ = unsafe.Sizeof(0)

	return nil
}
