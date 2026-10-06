//go:build linux

package commands

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
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
	DevicePath  string // e.g. /dev/sdb
	RawPath     string // same as DevicePath on Linux
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

// lsblkOutput is the JSON output from lsblk.
type lsblkOutput struct {
	Blockdevices []lsblkDevice `json:"blockdevices"`
}

type lsblkDevice struct {
	Name       string        `json:"name"`
	Size       json.Number   `json:"size"`
	Type       string        `json:"type"`
	Removable  flexBool      `json:"rm"`
	Hotplug    flexBool      `json:"hotplug"`
	Transport  string        `json:"tran"`
	Rotational flexBool      `json:"rota"`
	Mountpoint string        `json:"mountpoint"`
	Children   []lsblkDevice `json:"children"`
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
	out, err := exec.Command("lsblk", "--json", "--bytes", "-o", "NAME,SIZE,TYPE,RM,HOTPLUG,TRAN,ROTA,MOUNTPOINT").Output()
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
		} else if dev.Transport == "usb" {
			storageType = StorageUSB
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
			// Non-removable, non-rotational media is an SSD, not an SD card or
			// thumb drive (both report RM=1).
			MediaFixed: !bool(dev.Removable) && !bool(dev.Rotational),
		})
	}

	return drives, nil
}

var buildLsblkArgs = func(devPath string) []string {
	return []string{"lsblk", "--json", "-l", "-o", "NAME,MOUNTPOINT", devPath}
}

// lsblkCmd is the function used to run lsblk for partition enumeration.
// It is a package-level variable so tests can inject a fake implementation
// without spawning a real process.
var lsblkCmd = func(devPath string) ([]byte, error) {
	args := buildLsblkArgs(devPath)
	return exec.Command(args[0], args[1:]...).Output()
}

// umountCmd is the function used to unmount a single mountpoint path.
// It is a package-level variable so tests can inject a mock that records
// calls without invoking privileged host commands.
var umountCmd = func(mountpoint string) ([]byte, error) {
	return exec.Command("sudo", "umount", mountpoint).CombinedOutput()
}

func maxMountpointDepth(dev lsblkDevice) int {
	d := strings.Count(dev.Mountpoint, "/")
	for _, child := range dev.Children {
		if cd := maxMountpointDepth(child); cd > d {
			d = cd
		}
	}
	return d
}

// unmountDisk unmounts all partitions on a disk before writing.
func unmountDisk(devPath string) error {
	// Enumerate partitions via lsblk and unmount each one.
	// The -l flag requests flat (list) output so every partition appears as a
	// top-level entry in blockdevices rather than being nested under a parent
	// disk's "children" array.  Without -l, partitions are silently dropped
	// because older code did not recurse into children.  We also keep the
	// Children field on lsblkDevice and recurse in unmountLsblkDevice as a
	// defence-in-depth measure for lsblk versions that ignore -l.
	out, err := lsblkCmd(devPath)
	if err != nil {
		return fmt.Errorf("lsblk %s: %w", devPath, err)
	}

	var result lsblkOutput
	if err := json.Unmarshal(out, &result); err != nil {
		return fmt.Errorf("parsing lsblk output for %s: %w", devPath, err)
	}

	// Sort devices so that deeper mountpoints are unmounted first.
	// This prevents a parent mount from being busy when we try to unmount it
	// because a child mountpoint beneath it is still active.
	// maxMountpointDepth is used so that a device with no mountpoint itself but
	// with deeply-nested children is still sorted correctly.
	sort.Slice(result.Blockdevices, func(i, j int) bool {
		di := maxMountpointDepth(result.Blockdevices[i])
		dj := maxMountpointDepth(result.Blockdevices[j])
		return di > dj // deeper first; empty mountpoints (di==0) sort last
	})

	var firstErr error
	for _, dev := range result.Blockdevices {
		if err := unmountLsblkDevice(dev); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func unmountLsblkDevice(dev lsblkDevice) error {
	// Children first: a parent mount cannot be unmounted until all child
	// mounts beneath it are gone.  Recurse into children before attempting to
	// unmount this node.  Collect the first error but continue visiting ALL
	// siblings so that a single failure does not leave subsequent partitions
	// mounted.
	// Sort children so that deeper mountpoints are unmounted first.
	// This mirrors the top-level sort in unmountDisk and prevents EBUSY when
	// two sibling children have nested mountpoints (e.g. /mnt/disk and
	// /mnt/disk/sub): the deeper one must be unmounted before the shallower.
	sort.Slice(dev.Children, func(i, j int) bool {
		di := maxMountpointDepth(dev.Children[i])
		dj := maxMountpointDepth(dev.Children[j])
		return di > dj // deeper first; empty mountpoints (di==0) sort last
	})

	var firstErr error
	for _, child := range dev.Children {
		if err := unmountLsblkDevice(child); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	// Then unmount self.  Unmount by mountpoint rather than device path so
	// that device-mapper entries (e.g. mapper/data → /dev/mapper/data) and
	// dm-* nodes are handled correctly.  The mountpoint is always the right
	// argument to pass to umount when it is known.
	if dev.Mountpoint != "" {
		if out, err := umountCmd(dev.Mountpoint); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("unmounting %s: %w\n%s", dev.Mountpoint, err, out)
		}
	}
	return firstErr
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
	cmd := exec.Command("sudo", self, "__bmap-write", "--device", d.DevicePath, "--bmap", bmapPath)

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
		"--device", d.DevicePath, "--bmap", bmapPath, "--source", sourcePath,
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

	return nil
}

// Linux writers already unmount their target; macOS preparation is unnecessary.
func unmountBeforeWrite(_ drive) error { return nil }
