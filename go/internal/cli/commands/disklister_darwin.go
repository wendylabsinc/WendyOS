//go:build darwin

package commands

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
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
		if infoErr == nil {
			if info.name != "" {
				name = info.name
			}
			size = info.size
			sizeBytes = info.sizeBytes
			if !isExternal {
				removable = info.removable || info.ejectable
			}
		}

		drives = append(drives, drive{
			DevicePath:  devPath,
			RawPath:     rawPath,
			Name:        name,
			Size:        size,
			SizeBytes:   sizeBytes,
			IsRemovable: removable,
		})
	}
	return drives
}

type diskInfo struct {
	name      string
	size      string
	sizeBytes int64
	removable bool // "Removable Media: Removable"
	ejectable bool // "Ejectable: Yes"
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
					rawBytes := info.size[start+1 : start+end]
					// diskutil may include thousands separators (commas); remove them before parsing.
					rawBytes = strings.ReplaceAll(rawBytes, ",", "")
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

// writeImageToDisk writes an image file to a raw disk device using dd,
// streaming data via stdin in 4 MiB chunks for progress tracking.
func writeImageToDisk(imagePath string, d drive, progressFn func(written int64)) error {
	if err := unmountDisk(d.DevicePath); err != nil {
		return err
	}

	imgFile, err := os.Open(imagePath)
	if err != nil {
		return fmt.Errorf("opening image: %w", err)
	}
	defer imgFile.Close()

	// Use rdisk for faster raw writes on macOS.
	cmd := exec.Command("sudo", "dd", fmt.Sprintf("of=%s", d.RawPath), "bs=4m")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("creating stdin pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting dd: %w", err)
	}

	buf := make([]byte, 4*1024*1024) // 4 MiB
	var totalWritten int64
	for {
		n, readErr := imgFile.Read(buf)
		if n > 0 {
			if _, writeErr := stdin.Write(buf[:n]); writeErr != nil {
				return fmt.Errorf("writing to dd: %w", writeErr)
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

	stdin.Close()
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("writing image: %w", err)
	}

	// Sync to flush any remaining writes, then eject.
	exec.Command("sync").Run()                            //nolint:errcheck
	exec.Command("diskutil", "eject", d.DevicePath).Run() //nolint:errcheck

	return nil
}
