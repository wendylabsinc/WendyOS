//go:build linux

package commands

import (
	"encoding/json"
	"fmt"
	"os/exec"
)

// drive represents an external disk suitable for image writing.
type drive struct {
	DevicePath  string // e.g. /dev/sdb
	RawPath     string // same as DevicePath on Linux
	Name        string // human-readable name
	Size        string // human-readable size
	IsRemovable bool
}

// lsblkOutput is the JSON output from lsblk.
type lsblkOutput struct {
	Blockdevices []lsblkDevice `json:"blockdevices"`
}

type lsblkDevice struct {
	Name       string `json:"name"`
	Size       string `json:"size"`
	Type       string `json:"type"`
	Removable  string `json:"rm"`
	Mountpoint string `json:"mountpoint"`
}

// listExternalDrives uses lsblk to find removable block devices on Linux.
func listExternalDrives() ([]drive, error) {
	out, err := exec.Command("lsblk", "--json", "-o", "NAME,SIZE,TYPE,RM,MOUNTPOINT").Output()
	if err != nil {
		return nil, fmt.Errorf("running lsblk: %w", err)
	}

	var result lsblkOutput
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("parsing lsblk output: %w", err)
	}

	var drives []drive
	for _, dev := range result.Blockdevices {
		if dev.Type != "disk" {
			continue
		}
		if dev.Removable != "1" {
			continue
		}

		devPath := "/dev/" + dev.Name
		drives = append(drives, drive{
			DevicePath:  devPath,
			RawPath:     devPath,
			Name:        dev.Name,
			Size:        dev.Size,
			IsRemovable: true,
		})
	}

	return drives, nil
}

// unmountDisk unmounts all partitions on a disk before writing.
func unmountDisk(devPath string) error {
	// Unmount all partitions: umount /dev/sdX*
	cmd := exec.Command("sudo", "umount", devPath+"*")
	cmd.Run() //nolint:errcheck — may fail if not mounted, that's fine
	return nil
}

// writeImageToDisk writes an image file to a block device using dd.
func writeImageToDisk(imagePath string, d drive, progressFn func(written int64)) error {
	if err := unmountDisk(d.DevicePath); err != nil {
		return err
	}

	cmd := exec.Command("sudo", "dd", fmt.Sprintf("if=%s", imagePath), fmt.Sprintf("of=%s", d.DevicePath), "bs=4M", "status=progress")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("writing image: %s\n%s", err, string(out))
	}

	// Sync to flush writes.
	exec.Command("sync").Run() //nolint:errcheck

	return nil
}
