//go:build linux

package commands

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

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
	DevicePath  string      // e.g. /dev/sdb
	RawPath     string      // same as DevicePath on Linux
	Name        string      // human-readable name
	Size        string      // human-readable size
	SizeBytes   int64       // size in bytes
	IsRemovable bool
	StorageType StorageType // detected medium: StorageSD, StorageNVMe, or StorageUnknown
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
	Model      string      `json:"model"`
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

// detectStorageTypeLinux infers the physical storage medium from lsblk transport,
// model name, and removable flag.
//
//   - "mmc" transport that is removable is an SD card (e.g. built-in slot).
//   - "mmc" transport that is not removable is onboard eMMC.
//   - "usb" transport whose model contains eMMC keywords is a USB eMMC reader.
//   - "usb" transport whose model contains SD keywords is an SD card reader.
//   - "usb" transport without SD/eMMC indicators is treated as an NVMe enclosure.
func detectStorageTypeLinux(transport, model string, removable bool) StorageType {
	switch strings.ToLower(transport) {
	case "mmc", "sd":
		if !removable {
			return StorageEMMC
		}
		return StorageSD
	case "usb":
		lower := strings.ToLower(model)
		for _, kw := range []string{"emmc", "e-mmc", "embedded mmc"} {
			if strings.Contains(lower, kw) {
				return StorageEMMC
			}
		}
		for _, kw := range []string{"sd card", "sdhc", "sdxc", "sd/mmc", "mmc"} {
			if strings.Contains(lower, kw) {
				return StorageSD
			}
		}
		return StorageNVMe
	default:
		return StorageUnknown
	}
}

func listDrivesLinux() ([]drive, error) {
	out, err := exec.Command("lsblk", "--json", "--bytes", "-o", "NAME,SIZE,TYPE,RM,HOTPLUG,TRAN,MOUNTPOINT,MODEL").Output()
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

		drives = append(drives, drive{
			DevicePath: devPath,
			RawPath:    devPath,
			Name:       dev.Name,
			Size:       humanize.Bytes(uint64(sizeBytes)),
			SizeBytes:  sizeBytes,
			// IsRemovable reflects our external-ness predicate so downstream code
			// sees the same classification used to include this device.
			IsRemovable: isExternal,
			StorageType: detectStorageTypeLinux(dev.Transport, dev.Model, bool(dev.Removable)),
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

// writeImageToDisk writes an image file to a block device using dd. dd reads
// the file directly (rather than via a stdin pipe) so that bs=4M actually
// produces 4 MiB writes to the device — pipe input forces dd to issue a write
// per pipe-buffer-sized read, which is dramatically slower on raw devices.
// Progress is driven by parsing dd's status=progress output on stderr.
func writeImageToDisk(imagePath string, d drive, progressFn func(written int64)) error {
	if err := unmountDisk(d.DevicePath); err != nil {
		return err
	}

	cmd := exec.Command("sudo", "dd",
		fmt.Sprintf("if=%s", imagePath),
		fmt.Sprintf("of=%s", d.DevicePath),
		"bs=4M",
		"status=progress",
	)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("creating stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting dd: %w", err)
	}

	// dd emits progress lines separated by '\r' (overwriting in place) until
	// the final newline-terminated summary. Parse out the leading byte count.
	go scanDDProgress(stderr, progressFn)

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("writing image: %w", err)
	}

	// Sync to flush writes.
	exec.Command("sync").Run() //nolint:errcheck

	return nil
}

// scanDDProgress parses dd's `status=progress` output and invokes progressFn
// with the running byte count. dd separates in-place updates with '\r' and
// terminates the final summary block with '\n', so we split on either.
func scanDDProgress(r io.Reader, progressFn func(written int64)) {
	if progressFn == nil {
		io.Copy(io.Discard, r) //nolint:errcheck
		return
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	scanner.Split(splitCROrLF)
	for scanner.Scan() {
		line := scanner.Text()
		// Progress lines look like:
		//   524288000 bytes (524 MB, 500 MiB) copied, 1 s, 524 MB/s
		// The first whitespace-delimited token is the byte count.
		var token string
		for i, c := range line {
			if c == ' ' || c == '\t' {
				token = line[:i]
				break
			}
		}
		if token == "" {
			continue
		}
		written, err := strconv.ParseInt(token, 10, 64)
		if err != nil {
			continue
		}
		progressFn(written)
	}
}

// splitCROrLF is a bufio.SplitFunc that splits on '\r' or '\n'.
func splitCROrLF(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for i, b := range data {
		if b == '\r' || b == '\n' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}
