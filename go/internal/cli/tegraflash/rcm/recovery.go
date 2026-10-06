package rcm

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUSBAccess reports that a Jetson in recovery mode is present but the OS
// refused to open it (LIBUSB_ERROR_ACCESS). On Linux this means the current
// user lacks permission on the /dev/bus/usb node — fixed by a udev rule or
// sudo; the caller turns this into actionable guidance. Declared here (not in
// the gousb-tagged files) so the shared install flow can classify it on every
// OS, including Windows.
var ErrUSBAccess = errors.New("USB device access denied")

// ValidChipID excludes Windows-generated location IDs masquerading as serials.
func ValidChipID(id string) bool {
	if id == "" {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// VerifyChipID reads identity from the handle that will receive boot images.
// Empty expected identity preserves the terminal installer's existing behavior.
func VerifyChipID(expected string, read func() (string, error)) error {
	if expected == "" {
		return nil
	}
	if !ValidChipID(expected) {
		return fmt.Errorf("invalid expected recovery chip ID")
	}
	got, err := read()
	if err != nil {
		return fmt.Errorf("reading claimed recovery chip ID: %w", err)
	}
	if !ValidChipID(got) || !strings.EqualFold(got, expected) {
		return fmt.Errorf("recovery chip identity changed: expected %s, observed %s; refusing to send boot images", expected, got)
	}
	return nil
}

// RecoveryDevice identifies a Jetson sitting in USB recovery mode. PathKey is the
// physical USB location (bus + parent-port chain); it is stable across the
// re-enumeration the device undergoes between RCM boot and the ADB flashing gadget,
// so it is the right handle for "flash this specific board".
type RecoveryDevice struct {
	PathKey string // e.g. "20-1.2" (bus 20, hub port 1 → port 2)
	Product uint16 // USB PID: 0x7<module>23 for T234 modules, 0x7026 Thor
	ECID    string // chip BR_CID read over EP0 (may be empty if unreadable)
	// Instance is a platform-specific exact device handle for hosts where
	// PathKey alone cannot single out the devnode: the Windows PnP instance ID
	// (redirected/virtualized USB may report no location path at all). Empty on
	// gousb platforms, where PathKey (bus + port chain) is always present.
	Instance string
}

// IsThor reports whether the device is a T264 (AGX Thor).
func (r RecoveryDevice) IsThor() bool { return r.Product == uint16(ProductThor) }

// IsOrin reports whether the device is a T234 (Orin family: AGX Orin, Orin NX,
// Orin Nano — each module SKU has its own recovery PID).
func (r RecoveryDevice) IsOrin() bool { return IsT234RecoveryPID(r.Product) }

// IsOrinAGX reports whether the device is an AGX Orin module.
func (r RecoveryDevice) IsOrinAGX() bool {
	return r.Product == uint16(ProductOrinAGX32) || r.Product == uint16(ProductOrinAGX64)
}

// IsOrinNano reports whether the device is an Orin Nano module.
func (r RecoveryDevice) IsOrinNano() bool {
	return r.Product == uint16(ProductOrinNano8) || r.Product == uint16(ProductOrinNano4)
}

// Describe returns a one-line human label for pickers/logs.
func (r RecoveryDevice) Describe() string {
	chip := "Jetson"
	if r.IsThor() {
		chip = "AGX Thor (T264)"
	} else if name, ok := T234ModuleName(r.Product); ok {
		chip = name + " (T234)"
	}
	ecid := r.ECID
	if ecid == "" {
		ecid = "unknown"
	}
	return fmt.Sprintf("%s  [usb %s, ECID %s]", chip, r.PathKey, ecid)
}
