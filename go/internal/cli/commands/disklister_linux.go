//go:build linux

package commands

import (
	"encoding/json"
	"fmt"
	"os/exec"

	"github.com/dustin/go-humanize"
)

// drive represents an external disk suitable for image writing.
type drive struct {
	DevicePath  string // e.g. /dev/sdb
	RawPath     string // same as DevicePath on Linux
	Name        string // human-readable name
	Size        string // human-readable size
	SizeBytes   int64  // size in bytes
	IsRemovable bool
}

// lsblkOutput is the JSON output from lsblk.
type lsblkOutput struct {
	Blockdevices []lsblkDevice `json:"blockdevices"`
}

type lsblkDevice struct {
	Name       string      `json:"name"`
	Size       json.Number `json:"size"`
	Type       string      `json:"type"`
	Removable  string      `json:"rm"`
	Hotplug    string      `json:"hotplug"`
	Transport  string      `json:"tran"`
	Mountpoint string      `json:"mountpoint"`
}

// listAllDrives lists external physical drives (USB, NVMe, SD) on Linux.
func listAllDrives() ([]drive, error) {
	return listDrivesLinux(false)
}

// listExternalDrives lists removable external drives on Linux.
func listExternalDrives() ([]drive, error) {
	return listDrivesLinux(true)
}

func listDrivesLinux(removableOnly bool) ([]drive, error) {
	out, err := exec.Command("lsblk", "--json", "--bytes", "-o", "NAME,SIZE,TYPE,RM,HOTPLUG,TRAN,MOUNTPOINT").Output()
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
		// Only include external drives: USB or SD card transports, or hotpluggable/removable.
		isExternal := dev.Removable == "1" || dev.Hotplug == "1" ||
			dev.Transport == "usb" || dev.Transport == "mmc"
		if !isExternal {
			continue
		}
		if removableOnly && dev.Removable != "1" {
			continue
		}

		devPath := "/dev/" + dev.Name
		var sizeBytes int64
		if n, err := dev.Size.Int64(); err == nil {
			sizeBytes = n
		}

		drives = append(drives, drive{
			DevicePath: devPath,
			RawPath:    devPath,
			Name:       dev.Name,
			Size:       humanize.Bytes(uint64(sizeBytes)),
			SizeBytes:  sizeBytes,
			// IsRemovable reflects our external-ness predicate so downstream code
			// sees the same classification used to include this device.
			IsRemovable: isExternal,
		})
	}

	return drives, nil
}

// unmountDisk unmounts all partitions on a disk before writing.
func unmountDisk(devPath string) error {
	// Enumerate partitions via lsblk and unmount each one.
	out, err := exec.Command("lsblk", "--json", "-o", "NAME,MOUNTPOINT", devPath).Output()
	if err != nil {
		// If lsblk fails, the disk may not be mounted at all.
		return nil
	}

	var result lsblkOutput
	if err := json.Unmarshal(out, &result); err != nil {
		return nil
	}

	for _, dev := range result.Blockdevices {
		unmountLsblkDevice(dev)
	}
	return nil
}

func unmountLsblkDevice(dev lsblkDevice) {
	if dev.Mountpoint != "" {
		partPath := "/dev/" + dev.Name
		exec.Command("sudo", "umount", partPath).Run() //nolint:errcheck
	}
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
