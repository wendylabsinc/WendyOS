//go:build windows

// Package winusb provides Windows USB access for Jetson and Qualcomm flashing.
// It enumerates USB devices and installs family-specific WinUSB bindings (see
// driverinstall_windows.go), and speaks control/bulk USB to them via winusb.dll
// (see winusb_windows.go) — the Windows equivalent of the gousb/libusb transport
// used on macOS and Linux.
//
// Enumeration here uses SetupAPI, which reads device identity from the USB hub,
// so it works whether or not a function driver is bound. That lets wendy show a
// device and its binding state *before* the WinUSB driver is installed, and drive
// the "install the driver now" flow.
package winusb

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/rcm"
)

// USB identifiers for Jetsons. The chip-family PID sets (each T234 module SKU
// has its own recovery PID) live in package rcm, whose identity types compile
// on every OS. Recovery mode is 0955:70xx; the initrd-flash ADB gadget
// re-enumerates as 0955:7100.
const (
	VendorNVIDIA  = 0x0955
	ProductOrin   = rcm.ProductOrinAGX32
	ProductThor   = rcm.ProductThor
	ProductGadget = 0x7100
)

// Device is a USB device as seen by SetupAPI, independent of whether a
// driver is bound. It carries enough identity to select and describe a device
// before we can open it (opening needs the WinUSB driver bound).
type Device struct {
	// InstanceID is the full PnP instance path, e.g.
	// `USB\VID_0955&PID_7026\0C08FF61...`. Stable identifier for this device.
	InstanceID string
	// VID/PID parsed from the hardware ID.
	VID uint16
	PID uint16
	// Serial is the device's USB serial (the trailing element of InstanceID for
	// devices that report one; the Thor bootROM reports its ECID-like string here).
	Serial string
	// LocationPath is the firmware/bus topology path (DEVPKEY_Device_LocationPaths
	// first entry), stable across re-enumeration at the same physical port. Used as
	// the cross-stage PathKey so a multi-device host flashes the chosen board.
	LocationPath  string
	Product       string
	CompatibleIDs []string
	Service       string
	DriverINF     string
	DriverVersion string
	// Bound reports whether a function driver is attached (CM_Get_DevNode_Status
	// without DN_HAS_PROBLEM/CM_PROB_FAILED_INSTALL). A driverless recovery device
	// reports false; after WinUSB install it reports true.
	Bound bool
	// Problem is the CM_PROB_* code when the device has a problem (0 otherwise).
	// CM_PROB_FAILED_INSTALL (28) is the "no driver" state we resolve by install.
	Problem uint32
	// devInst is the CM device-node handle, used for status queries.
	devInst windows.DEVINST
}

// IsRecovery reports whether the device is a Jetson in RCM recovery mode.
func (d Device) IsRecovery() bool { return d.IsT234() || d.IsThor() }

// IsThor reports whether the device is a T264 (AGX Thor) in recovery mode.
func (d Device) IsThor() bool { return d.PID == ProductThor }

// IsT234 reports whether the device is a T234 (Orin family) module in
// recovery mode — any of the per-SKU recovery PIDs.
func (d Device) IsT234() bool { return rcm.IsT234RecoveryPID(d.PID) }

// IsOrinAGX reports whether the device is an AGX Orin module in recovery mode.
func (d Device) IsOrinAGX() bool {
	return d.PID == rcm.ProductOrinAGX32 || d.PID == rcm.ProductOrinAGX64
}

// IsOrinNano reports whether the device is an Orin Nano module in recovery mode.
func (d Device) IsOrinNano() bool {
	return d.PID == rcm.ProductOrinNano8 || d.PID == rcm.ProductOrinNano4
}

// IsGadget reports whether the device is the initrd-flash ADB flashing gadget.
func (d Device) IsGadget() bool { return d.PID == ProductGadget }

// RecoveryDevice converts d to the platform-neutral rcm identity used by the
// shared install flow: the location path becomes the PathKey, the USB serial
// (the bootROM reports the chip ECID there) becomes the ECID, and the PnP
// instance ID rides along as the exact devnode handle for the stage-1 open.
func (d Device) RecoveryDevice() rcm.RecoveryDevice {
	return rcm.RecoveryDevice{PathKey: d.LocationPath, Product: d.PID, ECID: d.Serial, Instance: d.InstanceID}
}

// Describe returns a one-line human label for pickers and logs.
func (d Device) Describe() string {
	var kind string
	switch {
	case d.PID == ProductThor:
		kind = "AGX Thor (T264) recovery"
	case d.IsT234():
		kind = "Orin (T234) recovery"
		if name, ok := rcm.T234ModuleName(d.PID); ok {
			kind = name + " (T234) recovery"
		}
	case d.PID == ProductGadget:
		kind = "initrd-flash gadget"
	default:
		kind = fmt.Sprintf("NVIDIA 0955:%04x", d.PID)
	}
	state := "driver bound"
	if !d.Bound {
		state = "no driver"
		if d.Problem == cmProbFailedInstall {
			state = "no driver (install needed)"
		}
	}
	loc := d.LocationPath
	if loc == "" {
		loc = "unknown location"
	}
	return fmt.Sprintf("%s  [%s, %s]", kind, loc, state)
}

// CM_PROB_ and DN_ status bits we care about (from cfgmgr32.h; not all are in
// x/sys/windows as typed constants).
const (
	cmProbFailedInstall = 28 // CM_PROB_FAILED_INSTALL — no matching/installable driver
	dnHasProblem        = 0x00000400
)

// ListDevices enumerates all present NVIDIA (VID 0955) USB devices, whether or
// not a driver is bound. It never opens a device, so it needs no driver and no
// elevation.
func ListDevices() ([]Device, error) {
	return ListVendor(VendorNVIDIA)
}

// ListVendor enumerates USB devnodes without opening them or requiring a driver.
func ListVendor(vendor uint16) ([]Device, error) {
	// DIGCF_ALLCLASSES so we see devices with no assigned setup class (a
	// driverless recovery device has an empty Class); DIGCF_PRESENT for currently
	// attached only. The "USB" enumerator scopes the walk to the USB bus.
	set, err := windows.SetupDiGetClassDevsEx(nil, "USB", 0, windows.DIGCF_PRESENT|windows.DIGCF_ALLCLASSES, 0, "")
	if err != nil {
		return nil, fmt.Errorf("SetupDiGetClassDevsEx: %w", err)
	}
	defer set.Close()

	var out []Device
	for i := 0; ; i++ {
		info, err := set.EnumDeviceInfo(i)
		if err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_ITEMS) {
				break
			}
			return nil, fmt.Errorf("enumerating USB devices: %w", err)
		}
		instanceID, err := set.DeviceInstanceID(info)
		if err != nil {
			continue
		}
		vid, pid, ok := ParseVIDPID(instanceID)
		if !ok || vid != vendor {
			continue
		}

		d := Device{
			InstanceID: instanceID,
			VID:        vid,
			PID:        pid,
			Serial:     InstanceSerial(instanceID),
			devInst:    info.DevInst,
		}

		// Binding/problem state from the device node.
		var status, problem uint32
		if err := windows.CM_Get_DevNode_Status(&status, &problem, info.DevInst, 0); err == nil {
			d.Problem = problem
			d.Bound = status&dnHasProblem == 0
		}

		// Location path (first entry of SPDRP_LOCATION_PATHS, a REG_MULTI_SZ).
		// Best-effort: a missing path just yields an empty PathKey. This is stable
		// across the RCM→gadget re-enumeration at the same physical port.
		if v, err := set.DeviceRegistryProperty(info, windows.SPDRP_LOCATION_PATHS); err == nil {
			d.LocationPath = FirstString(v)
		}
		if v, err := set.DeviceRegistryProperty(info, windows.SPDRP_SERVICE); err == nil {
			d.Service = FirstString(v)
		}
		if v, err := set.DeviceRegistryProperty(info, windows.SPDRP_COMPATIBLEIDS); err == nil {
			if ids, ok := v.([]string); ok {
				d.CompatibleIDs = ids
			}
		}
		// DEVPKEY_Device_BusReportedDeviceDesc carries the EDL CID/SN product
		// string even when the USB serial descriptor is absent.
		productKey := windows.DEVPROPKEY{FmtID: windows.DEVPROPGUID(mustGUID("{540b947e-8b40-45bc-a8a2-6a0b894cbda2}")), PID: 4}
		if v, err := windows.SetupDiGetDeviceProperty(set, info, &productKey); err == nil {
			d.Product = FirstString(v)
		}
		if key, err := set.OpenDevRegKey(info, windows.DICS_FLAG_GLOBAL, 0, windows.DIREG_DRV, windows.KEY_READ); err == nil {
			r := registry.Key(key)
			d.DriverINF, _, _ = r.GetStringValue("InfPath")
			d.DriverVersion, _, _ = r.GetStringValue("DriverVersion")
			r.Close()
		}

		out = append(out, d)
	}
	return out, nil
}

func (d Device) HardwareID() string { return fmt.Sprintf(`USB\VID_%04X&PID_%04X`, d.VID, d.PID) }

// ParseVIDPID extracts VID and PID from a hardware/instance ID containing
// "VID_XXXX&PID_YYYY" (case-insensitive hex). Exported for package t234's
// gadget-disk discovery, so both packages parse PnP IDs identically.
func ParseVIDPID(id string) (vid, pid uint16, ok bool) {
	up := strings.ToUpper(id)
	v, okv := hexField(up, "VID_")
	p, okp := hexField(up, "PID_")
	if !okv || !okp {
		return 0, 0, false
	}
	return v, p, true
}

// hexField finds "<prefix>XXXX" in s and parses the 4 hex digits after prefix.
func hexField(s, prefix string) (uint16, bool) {
	i := strings.Index(s, prefix)
	if i < 0 || i+len(prefix)+4 > len(s) {
		return 0, false
	}
	n, err := strconv.ParseUint(s[i+len(prefix):i+len(prefix)+4], 16, 16)
	if err != nil {
		return 0, false
	}
	return uint16(n), true
}

// InstanceSerial returns the trailing instance element (after the last backslash),
// which is the device serial for devices that report one. A device with no USB
// serial gets a Windows-generated id prefixed with "&" — we return it as-is; the
// caller uses LocationPath for stable identity, not this. Exported for package
// t234, whose ReleaseUSB must extract the serial exactly the way it was
// reported here.
func InstanceSerial(instanceID string) string {
	i := strings.LastIndex(instanceID, `\`)
	if i < 0 || i+1 >= len(instanceID) {
		return ""
	}
	return instanceID[i+1:]
}

// FirstString normalizes the interface{} returned by DeviceProperty for a
// REG_MULTI_SZ (string slice) or REG_SZ (string) property to its first string.
// Exported for package t234; location paths compared across the two packages
// must be normalized the same way.
func FirstString(v interface{}) string {
	switch t := v.(type) {
	case []string:
		if len(t) > 0 {
			return t[0]
		}
	case string:
		return t
	}
	return ""
}
