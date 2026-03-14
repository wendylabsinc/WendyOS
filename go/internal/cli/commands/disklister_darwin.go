//go:build darwin

package commands

import (
	"bufio"
	"fmt"
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

// listDrivesText parses the text output of `diskutil list external physical`.
func listDrivesText() ([]drive, error) {
	out, err := exec.Command("diskutil", "list", "external", "physical").Output()
	if err != nil {
		return nil, fmt.Errorf("running diskutil: %w", err)
	}

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
		rawPath := strings.Replace(devPath, "/dev/disk", "/dev/rdisk", 1)

		// Get disk info for size and name.
		info, infoErr := getDiskInfo(devPath)
		name := devPath
		size := ""
		var sizeBytes int64
		if infoErr == nil {
			if info.name != "" {
				name = info.name
			}
			size = info.size
			sizeBytes = info.sizeBytes
		}

		drives = append(drives, drive{
			DevicePath:  devPath,
			RawPath:     rawPath,
			Name:        name,
			Size:        size,
			SizeBytes:   sizeBytes,
			IsRemovable: true,
		})
	}

	return drives, nil
}

type diskInfo struct {
	name      string
	size      string
	sizeBytes int64
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
			// Parse byte count from e.g. "31.9 GB (31914983424 Bytes)..."
			if start := strings.Index(info.size, "("); start != -1 {
				if end := strings.Index(info.size[start:], " Bytes"); end != -1 {
					fmt.Sscanf(info.size[start+1:start+end], "%d", &info.sizeBytes)
				}
			}
		}
		if strings.HasPrefix(line, "Device / Media Name:") {
			info.name = strings.TrimSpace(strings.TrimPrefix(line, "Device / Media Name:"))
		}
	}
	return info, nil
}

// unmountDisk unmounts all volumes on a disk before writing.
func unmountDisk(devPath string) error {
	cmd := exec.Command("sudo", "diskutil", "unmountDisk", devPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("unmounting %s: %s", devPath, string(out))
	}
	return nil
}

// writeImageToDisk writes an image file to a raw disk device using dd.
func writeImageToDisk(imagePath string, d drive, progressFn func(written int64)) error {
	if err := unmountDisk(d.DevicePath); err != nil {
		return err
	}

	// Use rdisk for faster raw writes on macOS.
	cmd := exec.Command("sudo", "dd", fmt.Sprintf("if=%s", imagePath), fmt.Sprintf("of=%s", d.RawPath), "bs=4m")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("writing image: %s\n%s", err, string(out))
	}

	// Eject the disk after writing.
	exec.Command("sudo", "diskutil", "eject", d.DevicePath).Run() //nolint:errcheck

	return nil
}
