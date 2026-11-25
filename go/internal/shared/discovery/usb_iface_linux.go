//go:build linux

package discovery

import (
	"path/filepath"
	"strings"
)

// netClassRoot is the sysfs directory that enumerates network interfaces. It is
// a var so tests can point it at a fixture tree.
var netClassRoot = "/sys/class/net"

// The USB identity every WendyOS image's gadget presents (gadget-setup.sh in
// wendyos-builder). The VID:PID is the generic Linux Foundation composite
// gadget, shared by other Linux gadgets, so the manufacturer is required too.
// The product string carries the per-device name and is not matched.
const (
	wendyGadgetVendorID     = "1d6b"
	wendyGadgetProductID    = "0104"
	wendyGadgetManufacturer = "Wendy Labs Inc"
)

// usbGadgetIdentityFn makes looksLikeUSBConnection judge interfaces by USB
// identity on Linux. Tests of the name rules other platforms use clear it.
var usbGadgetIdentityFn = IsWendyUSBGadget

// IsWendyUSBGadget reports whether the named network interface is the host end
// of a Wendy device's USB link, judged by the identity of the USB device behind
// it in sysfs, never by the interface name: ordinary USB network adapters get
// the same names (enx…, en…u1, usb0) as the gadget.
func IsWendyUSBGadget(name string) bool {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return false
	}
	dir, err := filepath.EvalSymlinks(filepath.Join(netClassRoot, name))
	if err != nil {
		return false
	}
	// Walk up from …/<usb-device>/<usb-device>:1.0/net/<name> to the first
	// directory carrying USB device attributes. Non-USB devices have none.
	for ; dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if vid := readSysfsAttr(dir, "idVendor"); vid != "" {
			return strings.EqualFold(vid, wendyGadgetVendorID) &&
				strings.EqualFold(readSysfsAttr(dir, "idProduct"), wendyGadgetProductID) &&
				readSysfsAttr(dir, "manufacturer") == wendyGadgetManufacturer
		}
	}
	return false
}
