//go:build darwin

package commands

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// drive represents an external disk suitable for image writing.
type drive struct {
	DevicePath  string // e.g. /dev/disk4
	RawPath     string // e.g. /dev/rdisk4
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

// listAllDrives lists external physical drives (NVMe, USB, SD cards) on macOS.
func listAllDrives() ([]drive, error) {
	return listDrivesText()
}

// listExternalDrives uses diskutil to find external removable drives on macOS.
func listExternalDrives() ([]drive, error) {
	return listDrivesText()
}

// listDrivesText parses the text output of `diskutil list external physical`
// and `diskutil list internal physical` to find writable external/removable
// drives. It checks both external and internal physical disks because built-in
// SD card readers present media as internal on macOS.
func listDrivesText() ([]drive, error) {
	out, err := exec.Command("diskutil", "list", "external", "physical").Output()
	if err != nil {
		return nil, fmt.Errorf("running diskutil: %w", err)
	}

	seen := make(map[string]bool)
	drives := parseDiskutilOutput(out, seen, true)

	// Also check internal physical disks for removable media
	// (e.g., built-in SD card readers show as "internal" on macOS).
	internalOut, err := exec.Command("diskutil", "list", "internal", "physical").CombinedOutput()
	if err != nil {
		// Surface a warning instead of silently ignoring the failure so that
		// users can diagnose missing drives (e.g., SD cards in built-in readers).
		fmt.Fprintf(os.Stderr, "warning: failed to list internal physical disks with diskutil: %v\n", err)
	} else {
		for _, d := range parseDiskutilOutput(internalOut, seen, false) {
			if d.IsRemovable {
				drives = append(drives, d)
			}
		}
	}

	return drives, nil
}

// parseDiskutilOutput extracts drive entries from diskutil list output.
// When isExternal is true, all drives are marked removable. When false,
// removability is determined from diskutil info (Removable Media / Ejectable).
func parseDiskutilOutput(out []byte, seen map[string]bool, isExternal bool) []drive {
	var drives []drive
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		// Lines like: /dev/disk4 (external, physical):
		if !strings.HasPrefix(line, "/dev/disk") {
			continue
		}

		parts := strings.SplitN(line, " ", 2)
		devPath := strings.TrimSuffix(parts[0], ":")
		if seen[devPath] {
			continue
		}
		seen[devPath] = true
		rawPath := strings.Replace(devPath, "/dev/disk", "/dev/rdisk", 1)

		// Get disk info for size, name, and removability.
		info, infoErr := getDiskInfo(devPath)
		name := devPath
		size := ""
		var sizeBytes int64
		removable := isExternal
		storageType := StorageUnknown
		if infoErr == nil {
			if info.name != "" {
				name = info.name
			}
			size = info.size
			sizeBytes = info.sizeBytes
			if !isExternal {
				removable = info.removable || info.ejectable
			}
			if strings.EqualFold(info.protocol, "nvme") {
				storageType = StorageNVMe
			} else if strings.EqualFold(info.protocol, "usb") {
				storageType = StorageUSB
			}
		}

		drives = append(drives, drive{
			DevicePath:  devPath,
			RawPath:     rawPath,
			Name:        name,
			Size:        size,
			SizeBytes:   sizeBytes,
			IsRemovable: removable,
			StorageType: storageType,
			// "Removable Media: Fixed" + "Solid State: Yes" is an SSD, not an
			// SD card (which reports removable media).
			MediaFixed: infoErr == nil && !info.removable && info.solidState,
		})
	}
	return drives
}

type diskInfo struct {
	name       string
	size       string
	sizeBytes  int64
	removable  bool   // "Removable Media: Removable"
	ejectable  bool   // "Ejectable: Yes"
	protocol   string // "Protocol:" field, e.g. "NVMe", "USB"
	solidState bool   // "Solid State: Yes"
}

func getDiskInfo(devPath string) (*diskInfo, error) {
	out, err := exec.Command("diskutil", "info", devPath).Output()
	if err != nil {
		return nil, err
	}

	info := &diskInfo{}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "Disk Size:") {
			info.size = strings.TrimSpace(strings.TrimPrefix(line, "Disk Size:"))
			// Parse byte count from e.g. "31.9 GB (31,914,983,424 Bytes)..."
			if start := strings.Index(info.size, "("); start != -1 {
				if end := strings.Index(info.size[start:], " Bytes"); end != -1 {
					// Keep digits only: the thousands separator follows the locale.
					rawBytes := strings.Map(func(r rune) rune {
						if r >= '0' && r <= '9' {
							return r
						}
						return -1
					}, info.size[start+1:start+end])
					fmt.Sscanf(rawBytes, "%d", &info.sizeBytes)
				}
			}
		}
		if strings.HasPrefix(line, "Device / Media Name:") {
			info.name = strings.TrimSpace(strings.TrimPrefix(line, "Device / Media Name:"))
		}
		if strings.HasPrefix(line, "Removable Media:") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "Removable Media:"))
			info.removable = strings.HasPrefix(strings.ToLower(val), "removable")
		}
		if strings.HasPrefix(line, "Ejectable:") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "Ejectable:"))
			info.ejectable = strings.EqualFold(val, "yes")
		}
		if strings.HasPrefix(line, "Protocol:") {
			info.protocol = strings.TrimSpace(strings.TrimPrefix(line, "Protocol:"))
		}
		if strings.HasPrefix(line, "Solid State:") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "Solid State:"))
			info.solidState = strings.EqualFold(val, "yes")
		}
	}
	return info, nil
}

// unmountDisk unmounts all volumes on a disk before writing.
// Falls back to force-unmount if the normal unmount fails.
func unmountDisk(devPath string) error {
	cmd := exec.Command("sudo", "diskutil", "unmountDisk", devPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Retry with force unmount.
		forceCmd := exec.Command("sudo", "diskutil", "unmountDisk", "force", devPath)
		if forceOut, forceErr := forceCmd.CombinedOutput(); forceErr != nil {
			return fmt.Errorf("unmounting %s: %s\nClose Finder windows, Disk Utility, or any apps using the disk, then retry", devPath, string(forceOut)+string(out))
		}
	}
	return nil
}

// writeImageWithBmap flashes the image to d using the block map. It re-execs
// this binary as `sudo wendy __bmap-write`, streaming the decompressed image to
// the helper's stdin; the helper seeks and writes only mapped ranges as root.
// progressFn receives cumulative uncompressed bytes fed to the helper.
func writeImageWithBmap(r io.Reader, totalSize int64, d drive, bmapPath string, progressFn func(written int64)) error {
	if err := unmountDisk(d.DevicePath); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating wendy binary: %w", err)
	}
	cmd := exec.Command("sudo", self, "__bmap-write", "--device", d.RawPath, "--bmap", bmapPath)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting bmap helper: %w", err)
	}

	cw := &countingWriter{w: stdin, progressFn: progressFn}
	copyErr := func() error {
		defer stdin.Close()
		_, err := io.Copy(cw, r)
		return err
	}()

	waitErr := cmd.Wait()
	if copyErr != nil {
		// A failure copying the (decompressed) image into the helper is the
		// root cause; the helper's non-zero exit is just the downstream effect
		// of its stdin closing early. Surface the copy error first.
		if waitErr != nil {
			return fmt.Errorf("streaming image to bmap helper: %w (helper: %v; %s)", copyErr, waitErr, stderr.String())
		}
		return fmt.Errorf("streaming image to bmap helper: %w", copyErr)
	}
	if waitErr != nil {
		if stderr.Len() > 0 {
			return fmt.Errorf("bmap write failed: %w\n%s", waitErr, stderr.String())
		}
		return fmt.Errorf("bmap write failed: %w", waitErr)
	}
	_ = totalSize
	exec.Command("sync").Run() //nolint:errcheck
	return nil
}

// writeImageWithBmapSeekable flashes via the seekable source: it re-execs
// `sudo wendy __bmap-write --source <zst> --bmap <bmap> --device <dev>`; the
// helper reads the source itself and writes mapped ranges as root. progressFn
// receives cumulative mapped bytes (scanned from the helper's stdout).
func writeImageWithBmapSeekable(sourcePath, bmapPath string, d drive, progressFn func(int64)) error {
	if err := unmountDisk(d.DevicePath); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating wendy binary: %w", err)
	}
	cmd := exec.Command("sudo", self, "__bmap-write",
		"--device", d.RawPath, "--bmap", bmapPath, "--source", sourcePath,
		"--writers", strconv.Itoa(writersForStorage(d.StorageType)))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting bmap helper: %w", err)
	}
	scanBmapProgress(stdout, progressFn)
	if err := cmd.Wait(); err != nil {
		if stderr.Len() > 0 {
			return fmt.Errorf("bmap write failed: %w\n%s", err, stderr.String())
		}
		return fmt.Errorf("bmap write failed: %w", err)
	}
	exec.Command("sync").Run() //nolint:errcheck
	return nil
}

func writeImageToDisk(r io.Reader, totalSize int64, d drive, progressFn func(written int64)) error {
	if err := unmountDisk(d.DevicePath); err != nil {
		return err
	}

	bs := "8m"
	if d.StorageType == StorageNVMe {
		bs = "64m"
	}

	ddArgs, err := darwinDDArgs(d.RawPath, bs)
	if err != nil {
		return err
	}

	cmd := exec.Command("sudo", ddArgs...)
	cmd.Stdin = r

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("creating stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting dd: %w", err)
	}

	var ddDiag string
	scannerDone := make(chan struct{})
	go func() {
		defer close(scannerDone)
		ddDiag = scanDDProgress(stderr, progressFn)
	}()

	waitErr := cmd.Wait()
	<-scannerDone

	if waitErr != nil {
		if ddDiag != "" {
			return fmt.Errorf("writing image: %w\n%s", waitErr, ddDiag)
		}
		return fmt.Errorf("writing image: %w", waitErr)
	}

	// Sync to flush any remaining writes.
	exec.Command("sync").Run() //nolint:errcheck

	return nil
}

// ejectDisk ejects the disk so macOS shows the safe-to-remove notification.
// Called by the caller after all post-flash operations are complete.
func ejectDisk(d drive) {
	exec.Command("diskutil", "eject", d.DevicePath).Run() //nolint:errcheck
}
