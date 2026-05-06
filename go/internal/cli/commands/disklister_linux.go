//go:build linux

package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"os/exec"

	"github.com/dustin/go-humanize"
)

// flexBool unmarshals a JSON value that may be a boolean (true/false) or a
// numeric string ("0"/"1"). Older lsblk versions emit "0"/"1" while newer
// versions (util-linux ≥ 2.37) emit native JSON booleans.
type flexBool bool

func (f *flexBool) UnmarshalJSON(data []byte) error {
	// Try bool first (true / false).
	var b bool
	if err := json.Unmarshal(data, &b); err == nil {
		*f = flexBool(b)
		return nil
	}

	// Fall back to string ("0" / "1").
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("flexBool: cannot unmarshal %s", string(data))
	}
	*f = flexBool(s == "1")
	return nil
}

// drive represents an external disk suitable for image writing.
type drive struct {
	DevicePath  string // e.g. /dev/sdb
	RawPath     string // same as DevicePath on Linux
	Name        string // human-readable name
	Size        string // human-readable size
	SizeBytes   int64  // size in bytes
	IsRemovable bool
	StorageType StorageType // underlying storage protocol
}

// lsblkOutput is the JSON output from lsblk.
type lsblkOutput struct {
	Blockdevices []lsblkDevice `json:"blockdevices"`
}

type lsblkDevice struct {
	Name       string      `json:"name"`
	Size       json.Number `json:"size"`
	Type       string      `json:"type"`
	Removable  flexBool    `json:"rm"`
	Hotplug    flexBool    `json:"hotplug"`
	Transport  string      `json:"tran"`
	Mountpoint string      `json:"mountpoint"`
}

// listAllDrives lists external physical drives (USB, SD, hotplug) on Linux.
func listAllDrives() ([]drive, error) {
	return listDrivesLinux()
}

// listExternalDrives lists external drives on Linux.
// A drive is considered external when it is removable, hotpluggable, or
// connected via USB/MMC. This intentionally includes USB-attached SSDs
// which report rm=false but are still external.
func listExternalDrives() ([]drive, error) {
	return listDrivesLinux()
}

func listDrivesLinux() ([]drive, error) {
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
		isExternal := bool(dev.Removable) || bool(dev.Hotplug) ||
			dev.Transport == "usb" || dev.Transport == "mmc"
		if !isExternal {
			continue
		}

		devPath := "/dev/" + dev.Name
		var sizeBytes int64
		if n, err := dev.Size.Int64(); err == nil {
			sizeBytes = n
		}

		storageType := StorageUnknown
		if dev.Transport == "nvme" {
			storageType = StorageNVMe
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
			StorageType: storageType,
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

func writeImageToDisk(r io.Reader, totalSize int64, d drive, progressFn func(written int64)) error {
	if err := unmountDisk(d.DevicePath); err != nil {
		return err
	}

	bs := "bs=4M"
	if d.StorageType == StorageNVMe {
		bs = "bs=64M"
	}
	ddArgs := []string{"dd", fmt.Sprintf("of=%s", d.DevicePath), bs, "status=progress", "conv=fdatasync"}
	if d.StorageType == StorageNVMe {
		ddArgs = append(ddArgs, "oflag=direct")
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

	scannerDone := make(chan struct{})
	go func() {
		defer close(scannerDone)
		scanDDProgress(stderr, progressFn)
	}()

	waitErr := cmd.Wait()
	<-scannerDone

	if waitErr != nil {
		return fmt.Errorf("writing image: %w", waitErr)
	}

	return nil
}
