package qdl

import (
	"fmt"
	"strings"
)

// DeviceInfo identifies one attached device in EDL mode.
type DeviceInfo struct {
	// Serial is the chip serial, usually parsed out of the product string
	// because these boards leave the USB serial descriptor empty.
	Serial string
	// CID names the SoC family, e.g. "0455". 05c6:9008 is the generic
	// Qualcomm EDL id shared by every Qualcomm phone, modem and dev board,
	// so this is the only hint of which one is attached.
	CID     string
	Bus     int
	Address int
	// Instance is the exact Windows PnP device node. Location is its physical
	// USB port. Neither is substituted for a chip serial.
	Instance string
	Location string
}

// String names the device well enough to tell two boards apart.
func (d DeviceInfo) String() string {
	where := fmt.Sprintf("bus %d device %d", d.Bus, d.Address)
	if d.Instance != "" {
		where = d.Instance
		if d.Location != "" {
			where = d.Location
		}
	}
	if d.CID != "" {
		where = "CID " + d.CID + ", " + where
	}
	if d.Serial == "" {
		return fmt.Sprintf("Qualcomm EDL device (%s)", where)
	}
	return fmt.Sprintf("Qualcomm EDL device %s (%s)", d.Serial, where)
}

func isAlnum(r rune) bool {
	switch {
	case r >= '0' && r <= '9', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		return true
	}
	return false
}

// matches reports whether d is the device want describes. An exact Windows
// instance takes precedence; otherwise a reported chip serial wins, with the
// USB bus/address distinguishing serial-less boards on Unix.
func (d DeviceInfo) matches(want DeviceInfo) bool {
	switch {
	case want.Instance != "":
		return strings.EqualFold(d.Instance, want.Instance)
	case want.Serial != "":
		return d.Serial == want.Serial
	case want.Bus != 0 || want.Address != 0:
		return d.Bus == want.Bus && d.Address == want.Address
	default:
		return true
	}
}

// Key is unique among enumerated devices, including boards without serials.
func (d DeviceInfo) Key() string {
	if d.Instance != "" {
		return d.Instance
	}
	return fmt.Sprintf("%d/%d", d.Bus, d.Address)
}

// edlField extracts one "KEY:value" field from an EDL product string such as
// "QUSB_BULK_CID:0455_SN:CA218394", stopping at the first non-alphanumeric
// byte — which also keeps terminal escapes out of anything we print.
func edlField(product, key string) string {
	_, rest, found := strings.Cut(product, key+":")
	if !found {
		return ""
	}
	end := strings.IndexFunc(rest, func(r rune) bool { return !isAlnum(r) })
	switch {
	case end == 0:
		return ""
	case end < 0:
		return rest
	}
	return rest[:end]
}

// edlSerial extracts the chip serial for a device in EDL mode.
//
// The USB serial-number descriptor is empty on these boards; the serial is
// embedded in the product string instead, e.g. "QUSB_BULK_CID:0455_SN:CA218394".
func edlSerial(usbSerial, product string) string {
	if usbSerial != "" {
		return usbSerial
	}
	return edlField(product, "SN")
}
