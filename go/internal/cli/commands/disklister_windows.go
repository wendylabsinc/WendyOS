//go:build windows

package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
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

// drive represents an external disk suitable for image writing.
type drive struct {
	DevicePath  string // e.g. \\.\PhysicalDrive1
	RawPath     string // same as DevicePath on Windows
	Name        string // human-readable name
	Size        string // human-readable size
	SizeBytes   int64  // size in bytes
	IsRemovable bool
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
	out, err := exec.Command("powershell", "-NoProfile", "-Command", script).Output()
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
		})
	}

	return drives, nil
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
	out, err := exec.Command("powershell", "-NoProfile", "-Command", script).Output()
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

// parseDiskNumber extracts the disk number from a \\.\PhysicalDriveN path.
func parseDiskNumber(devPath string) (int, error) {
	var n int
	_, err := fmt.Sscanf(devPath, `\\.\PhysicalDrive%d`, &n)
	if err != nil {
		return 0, fmt.Errorf("parsing disk number from %q: %w", devPath, err)
	}
	return n, nil
}

// clearDiskPartitions uses PowerShell Clear-Disk to remove all partitions,
// volumes, and OEM recovery data from the disk. This releases Windows' hold
// on volumes that have no drive letter (e.g. EFI, recovery, or Jetson
// partitions) which would otherwise block raw disk writes with "Access denied".
func clearDiskPartitions(diskNum int) error {
	script := fmt.Sprintf(
		"Clear-Disk -Number %d -RemoveData -RemoveOEM -Confirm:$false",
		diskNum,
	)
	out, err := exec.Command("powershell", "-NoProfile", "-Command", script).CombinedOutput()
	if err != nil {
		// "not been initialized" means the disk already has no partition
		// table (e.g. from a previous Clear-Disk). That's the state we
		// want, so treat it as success.
		if strings.Contains(string(out), "not been initialized") {
			return nil
		}
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
	defer func() {
		for _, h := range volumeHandles {
			syscall.CloseHandle(h)
		}
	}()

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
	defer syscall.CloseHandle(handle)

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

	// Suppress unused import warning — unsafe is needed for DeviceIoControl pointer args.
	_ = unsafe.Sizeof(0)

	return nil
}
