//go:build linux

package t234

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// listUMSDisks finds USB mass-storage whole disks via sysfs: the SCSI
// inquiry vendor/model land in /sys/block/sdX/device/{vendor,model}. A LUN
// without a medium keeps its node at size 0 and is skipped.
func listUMSDisks() ([]UMSDisk, error) {
	entries, err := filepath.Glob("/sys/block/sd*")
	if err != nil {
		return nil, err
	}
	var disks []UMSDisk
	for _, e := range entries {
		name := filepath.Base(e)
		exportName, serial, ok := sysfsInquiry(e)
		if !ok {
			continue
		}
		d := UMSDisk{
			DevPath:  "/dev/" + name,
			RawPath:  "/dev/" + name,
			Vendor:   exportName,
			Serial:   serial,
			PortPath: linuxUSBPortPath(e),
		}
		if sectors := sysfsString(filepath.Join(e, "size")); sectors != "" {
			if n, err := strconv.ParseInt(sectors, 10, 64); err == nil {
				d.SizeBytes = n * 512
			}
		}
		if d.SizeBytes == 0 {
			continue
		}
		disks = append(disks, d)
	}
	return disks, nil
}

func linuxUSBPortPath(blockPath string) string {
	p, err := filepath.EvalSymlinks(filepath.Join(blockPath, "device"))
	if err != nil {
		return ""
	}
	for dir := p; dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		if sysfsString(filepath.Join(dir, "idVendor")) == "1d6b" && sysfsString(filepath.Join(dir, "idProduct")) == "0104" {
			return filepath.Base(dir)
		}
	}
	return ""
}

// rawUMSInquiry lists every /sys/block/sd* device's raw SCSI vendor/model — a
// diagnostic for a wait that timed out, showing what the device actually
// advertised before splitInquiry rejoins the fields.
func rawUMSInquiry() string {
	entries, err := filepath.Glob("/sys/block/sd*")
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, e := range entries {
		vendor := sysfsString(filepath.Join(e, "device", "vendor"))
		if vendor == "" {
			continue
		}
		var sizeBytes int64
		if sectors := sysfsString(filepath.Join(e, "size")); sectors != "" {
			if n, perr := strconv.ParseInt(sectors, 10, 64); perr == nil {
				sizeBytes = n * 512
			}
		}
		fmt.Fprintf(&b, "  - vendor=%q model=%q dev=%q size=%d\n",
			vendor, sysfsString(filepath.Join(e, "device", "model")), filepath.Base(e), sizeBytes)
	}
	return b.String()
}

// listUSBDevices reads every USB device from sysfs; the directory name is the
// same bus-port chain linuxUSBPortPath reports.
func listUSBDevices() ([]usbDevice, error) {
	entries, err := filepath.Glob("/sys/bus/usb/devices/*/idVendor")
	if err != nil {
		return nil, err
	}
	var devs []usbDevice
	for _, ve := range entries {
		dir := filepath.Dir(ve)
		v, verr := strconv.ParseUint(sysfsString(ve), 16, 16)
		p, perr := strconv.ParseUint(sysfsString(filepath.Join(dir, "idProduct")), 16, 16)
		if verr != nil || perr != nil {
			continue
		}
		devs = append(devs, usbDevice{
			VID:      uint16(v),
			PID:      uint16(p),
			Serial:   sysfsString(filepath.Join(dir, "serial")),
			PortPath: filepath.Base(dir),
		})
	}
	return devs, nil
}

func sysfsString(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// unmountUMSDisk unmounts anything an automounter grabbed from the LUN.
// Best-effort: the LUNs usually carry no mountable filesystem, so umount's
// exit status is the routine "not mounted" and is not surfaced — only the
// Windows implementation can distinguish that from a lock refusal worth
// reporting.
func unmountUMSDisk(d UMSDisk) error {
	exec.Command("umount", d.DevPath).Run() //nolint:errcheck
	return nil
}

// ejectUMSDisk ejects the LUN's medium (SCSI START STOP UNIT) — the "host is
// done" signal the flashing initrd waits for. The USB device stays attached;
// udisksctl power-off would disconnect it.
func ejectUMSDisk(d UMSDisk) error {
	if out, err := exec.Command("eject", d.DevPath).CombinedOutput(); err != nil {
		return fmt.Errorf("eject %s: %v: %s", d.DevPath, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// sysfsInquiry returns the export name and session serial in a block device's
// SCSI inquiry; ok is false when it has none.
func sysfsInquiry(dir string) (name, serial string, ok bool) {
	vendor := sysfsString(filepath.Join(dir, "device", "vendor"))
	if vendor == "" {
		return "", "", false
	}
	name, serial = splitInquiry(vendor, sysfsString(filepath.Join(dir, "device", "model")))
	return name, serial, true
}

// sessionLUNs lists the sysfs dirs of every LUN this session's gadget exports,
// empty ones included. It matches the session serial, not the port, which can
// change after a replug.
func sessionLUNs(session string) []string {
	entries, _ := filepath.Glob("/sys/block/sd*")
	var luns []string
	for _, e := range entries {
		if _, serial, ok := sysfsInquiry(e); ok && session != "" && strings.EqualFold(serial, session) {
			luns = append(luns, e)
		}
	}
	return luns
}

// pollingOff reports whether a LUN's media polling is off: set to 0, or left at
// the kernel default (-1) while that is 0. Hosts running systemd usually poll
// by default.
func pollingOff(lun string) bool {
	switch sysfsString(filepath.Join(lun, "events_poll_msecs")) {
	case "0":
		return true
	case "-1":
		return sysfsString("/sys/module/block/parameters/events_dfl_poll_msecs") == "0"
	}
	return false
}

// mediaPollingMissing reports whether a LUN of this session would not notice
// the device loading a medium. The attributes are world-readable, so this
// needs no privilege.
func mediaPollingMissing(session string) bool {
	for _, lun := range sessionLUNs(session) {
		if pollingOff(lun) {
			return true
		}
	}
	return false
}

// enableMediaPolling turns on media polling for this session's LUNs that lack
// it. It is re-run while waiting: a re-enumerated LUN starts afresh.
func enableMediaPolling(session string) error {
	var errs []error
	for _, lun := range sessionLUNs(session) {
		if !pollingOff(lun) {
			continue
		}
		if err := os.WriteFile(filepath.Join(lun, "events_poll_msecs"), []byte("1000"), 0o644); err != nil {
			errs = append(errs, fmt.Errorf("enabling media polling on %s: %w", filepath.Base(lun), err))
		}
	}
	return errors.Join(errs...)
}

// CheckHostTools fails early, before anything is sent to the Jetson, when the
// eject tool, which releases the flashing LUNs, is not installed.
func CheckHostTools() error {
	if _, err := exec.LookPath("eject"); err != nil {
		return fmt.Errorf("the eject tool is required to flash this image (Debian/Ubuntu: apt install eject): %w", err)
	}
	return nil
}
