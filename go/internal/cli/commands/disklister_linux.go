//go:build linux

package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
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

// writeImageToDisk writes an image file to a block device using dd,
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

	cmd := exec.Command("sudo", "dd", fmt.Sprintf("of=%s", d.DevicePath), "bs=4M")
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

	// Sync to flush writes.
	exec.Command("sync").Run() //nolint:errcheck

	return nil
}
