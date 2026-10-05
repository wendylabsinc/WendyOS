//go:build windows

package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"unsafe"

	"github.com/dustin/go-humanize"
	"github.com/wendylabsinc/wendy/go/internal/shared/env"
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
	StorageType StorageType // underlying storage protocol
	// MediaFixed is positive evidence that the media is fixed, solid-state
	// (an SSD) rather than removable (an SD card / thumb drive). Used to
	// disambiguate a USB-attached SSD enclosure from an SD-card reader.
	MediaFixed bool
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
	out, err := exec.Command(env.PowershellExe(), "-NoProfile", "-Command", script).Output()
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
		storageType := StorageUnknown
		if strings.EqualFold(d.BusType, "NVMe") {
			storageType = StorageNVMe
		} else if strings.EqualFold(d.BusType, "USB") {
			storageType = StorageUSB
		}
		drives = append(drives, drive{
			DevicePath:  devPath,
			RawPath:     devPath,
			Name:        d.FriendlyName,
			Size:        humanize.Bytes(uint64(d.Size)),
			SizeBytes:   d.Size,
			IsRemovable: external || looksLikeCardReader(d),
			StorageType: storageType,
			// An SSD reports MediaType "SSD"; SD cards / thumb drives report
			// "Unspecified" or removable. (USB enclosures often report
			// "Unspecified" too, in which case this stays false and the
			// ambiguous-USB prompt resolves it.)
			MediaFixed: strings.EqualFold(d.MediaType, "SSD"),
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

func getVolumesForDisk(diskNumber int) ([]string, error) {
	script := fmt.Sprintf(
		"Get-Partition -DiskNumber %d -ErrorAction SilentlyContinue | "+
			"Where-Object { $_.DriveLetter } | "+
			"Select-Object -ExpandProperty DriveLetter",
		diskNumber,
	)
	out, err := exec.Command(env.PowershellExe(), "-NoProfile", "-Command", script).Output()
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
	out, err := exec.Command(env.PowershellExe(), "-NoProfile", "-Command", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("clearing disk %d: %s: %w", diskNum, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// lockedDisk holds the resources acquired to write to a physical drive on
// Windows. Call close() when done to flush, release locks, and set the disk
// offline.
type lockedDisk struct {
	handle      syscall.Handle
	diskFile    *os.File
	volumeHs    []syscall.Handle
	diskNum     int
	isRemovable bool
	devPath     string
}

func (ld *lockedDisk) closeVolumeHandles() {
	for _, h := range ld.volumeHs {
		syscall.CloseHandle(h)
	}
	ld.volumeHs = nil
}

// close flushes file buffers, releases all locks, and sets the disk offline
// (for non-removable disks). Mirrors the cleanup sequence at the end of the
// original writeImageToDisk.
func (ld *lockedDisk) close() {
	// Flush the file buffers.
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	flushFileBuffers := kernel32.NewProc("FlushFileBuffers")
	flushFileBuffers.Call(uintptr(ld.handle)) //nolint:errcheck

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
	if cerr := ld.diskFile.Close(); cerr != nil {
		fmt.Fprintf(os.Stderr, "warning: closing %s: %v\n", ld.devPath, cerr)
	}
	ld.closeVolumeHandles()

	// Remove any auto-assigned drive letters, then (for non-removable
	// disks) take the disk offline. Set-Disk -IsOffline alone doesn't
	// remove letters that Windows already assigned during the brief window
	// between releasing locks and going offline.
	//
	// Get-Partition -ErrorAction SilentlyContinue: right after Clear-Disk the
	// partition table re-read may not have completed and the cmdlet emits a
	// non-terminating "no MSFT_Partition objects" error we don't want fatal.
	// Set-Disk: no -Confirm (legacy Storage module rejects it; -IsOffline
	// doesn't prompt) and no -ErrorAction Stop (we log exit status below).
	//
	// Skip Set-Disk -IsOffline for removable targets — Windows rejects it
	// on USB / SD / MMC and on the PCIE card readers that report as SCSI
	// but are flagged removable by looksLikeCardReader, with "Not
	// Supported: Removable media cannot be set to offline." (WDY-1178).
	// Gating in Go on the same drive.IsRemovable predicate used to select
	// the install target keeps the cleanup predicate in lockstep with
	// selection. Removing the partition access paths above is what
	// actually prevents phantom drive letters; the offline step is only
	// meaningful on fixed disks (where Windows would otherwise auto-mount
	// partitions on the next rescan).
	cleanupScript := fmt.Sprintf(
		"Get-Partition -DiskNumber %d -ErrorAction SilentlyContinue | "+
			"Where-Object { $_.DriveLetter } | "+
			"ForEach-Object { Remove-PartitionAccessPath -DiskNumber $_.DiskNumber -PartitionNumber $_.PartitionNumber -AccessPath \"$($_.DriveLetter):\\\" -ErrorAction SilentlyContinue }",
		ld.diskNum,
	)
	if !ld.isRemovable {
		cleanupScript += fmt.Sprintf("; Set-Disk -Number %d -IsOffline $true", ld.diskNum)
	}
	if output, err := exec.Command(env.PowershellExe(), "-NoProfile", "-NonInteractive", "-Command", cleanupScript).CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(output))
		if msg != "" {
			fmt.Fprintf(os.Stderr, "warning: failed to set disk %d offline: %v: %s\n", ld.diskNum, err, msg)
		} else {
			fmt.Fprintf(os.Stderr, "warning: failed to set disk %d offline: %v\n", ld.diskNum, err)
		}
	}
}

// openLockedDisk brings d online, clears its partitions, locks any lettered
// volumes, opens the raw physical drive handle, and applies the DASD / lock
// IOCTLs. The caller must call close() on the returned lockedDisk.
func openLockedDisk(d drive) (*lockedDisk, error) {
	diskNum, err := parseDiskNumber(d.DevicePath)
	if err != nil {
		return nil, err
	}

	// Ensure the disk is online before clearing partitions — a previous
	// write may have left it offline. Set-Disk -IsOffline doesn't prompt, so
	// no -Confirm switch is required (and the legacy Storage module rejects
	// it outright).
	onlineScript := fmt.Sprintf("Set-Disk -Number %d -IsOffline $false", diskNum)
	_ = exec.Command(env.PowershellExe(), "-NoProfile", "-NonInteractive", "-Command", onlineScript).Run()

	// Clear all partitions on the disk first. This is necessary because
	// disks (e.g. from a prior Jetson flash) may contain many partitions
	// without drive letters that getVolumesForDisk cannot enumerate. Those
	// hidden volumes stay mounted and cause "Access is denied" on write.
	if err := clearDiskPartitions(diskNum); err != nil {
		return nil, err
	}

	// Lock and dismount any remaining lettered volumes on this disk. We
	// must keep the volume handles open for the entire duration of the
	// write — closing them would release the lock and let Windows re-mount.
	letters, err := getVolumesForDisk(diskNum)
	if err != nil {
		return nil, fmt.Errorf("enumerating volumes: %w", err)
	}

	ld := &lockedDisk{
		diskNum:     diskNum,
		isRemovable: d.IsRemovable,
		devPath:     d.DevicePath,
	}

	for _, letter := range letters {
		h, err := lockAndDismountVolume(letter)
		if err != nil {
			ld.closeVolumeHandles()
			return nil, fmt.Errorf("preparing volume %s: %w", letter, err)
		}
		ld.volumeHs = append(ld.volumeHs, h)
	}

	// Open the raw physical drive for writing.
	devPathUTF16, err := syscall.UTF16PtrFromString(d.DevicePath)
	if err != nil {
		ld.closeVolumeHandles()
		return nil, fmt.Errorf("encoding device path: %w", err)
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
		ld.closeVolumeHandles()
		return nil, fmt.Errorf("opening %s for writing (are you running as Administrator?): %w", d.DevicePath, err)
	}
	ld.handle = handle
	ld.diskFile = os.NewFile(uintptr(handle), d.DevicePath)

	var bytesReturned uint32
	// Allow writes beyond the reported partition layout. Without this,
	// Windows may reject writes that extend past existing partitions.
	_ = syscall.DeviceIoControl(handle, fsctlAllowExtendedDASDIO, nil, 0, nil, 0, &bytesReturned, nil)
	// Lock the physical drive itself for exclusive access.
	_ = syscall.DeviceIoControl(handle, fsctlLockVolume, nil, 0, nil, 0, &bytesReturned, nil)

	return ld, nil
}

// handleWriterAt adapts a Windows disk handle to io.WriterAt by seeking to off
// then writing. applyBmap calls WriteAt sequentially, so the shared file
// pointer is safe.
type handleWriterAt struct{ h syscall.Handle }

func (hw handleWriterAt) WriteAt(p []byte, off int64) (int, error) {
	if _, err := syscall.Seek(hw.h, off, 0 /* FILE_BEGIN */); err != nil {
		return 0, err
	}
	// Raw physical-drive writes must be a multiple of the sector size. Pad the
	// final sub-sector chunk with zeros (matches writeImageToDisk's behavior).
	const sector = 512
	buf := p
	if rem := len(p) % sector; rem != 0 {
		padded := make([]byte, len(p)+(sector-rem))
		copy(padded, p)
		buf = padded
	}
	var done uint32
	if err := syscall.WriteFile(hw.h, buf, &done, nil); err != nil {
		return 0, err
	}
	if int(done) < len(buf) {
		return 0, fmt.Errorf("short write at offset %d: wrote %d of %d bytes", off, done, len(buf))
	}
	// Report only the caller's requested byte count, not the padding.
	if len(p) < int(done) {
		return len(p), nil
	}
	return int(done), nil
}

// writeImageWithBmap flashes the image to d using the block map. It acquires
// the same locked disk handle as writeImageToDisk and calls applyBmap to write
// only the mapped ranges. progress is driven directly by applyBmap.
func writeImageWithBmap(r io.Reader, totalSize int64, d drive, bmapPath string, progressFn func(written int64)) error {
	data, err := os.ReadFile(bmapPath)
	if err != nil {
		return fmt.Errorf("reading bmap: %w", err)
	}
	b, err := parseBmap(data)
	if err != nil {
		return err
	}

	ld, err := openLockedDisk(d)
	if err != nil {
		return err
	}
	defer ld.close()

	if err := applyBmap(r, handleWriterAt{h: ld.handle}, b, progressFn); err != nil {
		return err
	}
	_ = totalSize
	return nil
}

// writeImageWithBmapSeekable opens the seekable source and writes only mapped
// ranges to the locked disk handle in-process (no helper on Windows).
func writeImageWithBmapSeekable(sourcePath, bmapPath string, d drive, progressFn func(int64)) error {
	data, err := os.ReadFile(bmapPath)
	if err != nil {
		return fmt.Errorf("reading bmap: %w", err)
	}
	b, err := parseBmap(data)
	if err != nil {
		return err
	}
	si, err := openSeekableZstd(sourcePath)
	if err != nil {
		return err
	}
	defer si.Close()
	if si.Size() != b.ImageSize {
		return fmt.Errorf("seekable image size %d != bmap image size %d", si.Size(), b.ImageSize)
	}
	ld, err := openLockedDisk(d)
	if err != nil {
		return err
	}
	defer ld.close()
	// In-process on Windows (no helper subprocess), so set the writer
	// concurrency directly from the storage type: parallel for NVMe, sequential
	// for SD/USB media whose FTL is hurt by scattered concurrent writes.
	bmapWriteConcurrency = writersForStorage(d.StorageType)
	return applyBmapSeekable(si, handleWriterAt{h: ld.handle}, b, progressFn)
}

func writeImageToDisk(r io.Reader, totalSize int64, d drive, progressFn func(written int64)) error {
	ld, err := openLockedDisk(d)
	if err != nil {
		return err
	}
	defer ld.close()

	buf := make([]byte, 4*1024*1024) // 4 MiB
	var totalWritten int64
	for {
		n, readErr := r.Read(buf)
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
			if _, writeErr := ld.diskFile.Write(buf[:writeLen]); writeErr != nil {
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

	// Suppress unused import warning — unsafe is needed for DeviceIoControl pointer args.
	_ = unsafe.Sizeof(0)

	return nil
}

// unmountBeforeWrite is a no-op on Windows: the writers lock and dismount the
// disk's volumes themselves when they open it.
func unmountBeforeWrite(_ drive) error { return nil }
