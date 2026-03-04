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
	IsRemovable bool
}

// listExternalDrives uses diskutil to find external removable drives on macOS.
func listExternalDrives() ([]drive, error) {
	out, err := exec.Command("diskutil", "list", "-plist", "external").Output()
	if err != nil {
		// Fallback: parse text output.
		return listExternalDrivesText()
	}

	// If plist output is empty or has no disks, try text fallback.
	if len(out) == 0 {
		return listExternalDrivesText()
	}

	return listExternalDrivesText()
}

// listExternalDrivesText parses the text output of `diskutil list external`.
func listExternalDrivesText() ([]drive, error) {
	out, err := exec.Command("diskutil", "list", "external").Output()
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

		desc := ""
		if len(parts) > 1 {
			desc = strings.Trim(parts[1], " ():")
		}

		// Get disk info for size and name.
		info, infoErr := getDiskInfo(devPath)
		name := devPath
		size := ""
		if infoErr == nil {
			if info.name != "" {
				name = info.name
			}
			size = info.size
		}

		drives = append(drives, drive{
			DevicePath:  devPath,
			RawPath:     rawPath,
			Name:        name,
			Size:        size,
			IsRemovable: strings.Contains(desc, "external"),
		})
	}

	return drives, nil
}

type diskInfo struct {
	name string
	size string
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
