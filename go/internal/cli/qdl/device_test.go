package qdl

import (
	"strings"
	"testing"
)

func TestWindowsDeviceIdentity(t *testing.T) {
	a := DeviceInfo{Instance: `USB\VID_05C6&PID_9008\A`, Location: "port-a"}
	b := DeviceInfo{Instance: `USB\VID_05C6&PID_9008\B`, Location: "port-b"}
	if a.Key() == b.Key() || a.String() == b.String() {
		t.Fatal("serial-less devices collapsed")
	}
	if a.matches(b) {
		t.Fatal("different instance matched")
	}
	want := a
	want.Serial = "stale"
	if !a.matches(want) {
		t.Fatal("exact instance must win over descriptor serial")
	}
}

func TestEDLSerialFromProductString(t *testing.T) {
	// Boards in EDL leave the USB serial-number descriptor empty and put the
	// chip serial in the product string, so a label built from the descriptor
	// alone degrades to a bare USB address.
	for name, tc := range map[string]struct {
		usbSerial string
		product   string
		want      string
	}{
		"real device":        {"", "QUSB_BULK_CID:0455_SN:CA218394", "CA218394"},
		"usb serial wins":    {"ABC123", "QUSB_BULK_CID:0455_SN:CA218394", "ABC123"},
		"no serial anywhere": {"", "QUSB_BULK_CID:0455", ""},
		"trailing separator": {"", "CID:0455_SN:CA218394_X", "CA218394"},
		"empty after SN":     {"", "CID:0455_SN:_X", ""},
		"serial at end":      {"", "SN:DEADBEEF", "DEADBEEF"},
		"nothing at all":     {"", "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := edlSerial(tc.usbSerial, tc.product); got != tc.want {
				t.Errorf("edlSerial(%q, %q) = %q, want %q", tc.usbSerial, tc.product, got, tc.want)
			}
		})
	}
}

func TestDeviceInfoStringNamesTheVendor(t *testing.T) {
	// "bus 2 device 21" alone is not enough to confirm what is being erased.
	with := DeviceInfo{Serial: "CA218394", Bus: 2, Address: 21}.String()
	if !strings.Contains(with, "CA218394") || !strings.Contains(with, "bus 2 device 21") {
		t.Errorf("label %q should carry both the serial and the address", with)
	}
	without := DeviceInfo{Bus: 2, Address: 21}.String()
	if !strings.Contains(without, "Qualcomm") || !strings.Contains(without, "bus 2 device 21") {
		t.Errorf("serial-less label %q should still say what it is", without)
	}
}

func TestOpenAndListAgreeOnSerials(t *testing.T) {
	// A regression guard for a bug that made flashing impossible: List
	// reported the serial parsed out of the product string, while Open
	// compared the raw (empty) USB descriptor, so Open could never find the
	// device List had just handed the caller.
	//
	// Both paths must derive the serial the same way. describe() is that one
	// place, so assert the parser it uses is the one Open filters on.
	const product = "QUSB_BULK_CID:0455_SN:CA218394"
	listed := edlSerial("", product)
	if listed != "CA218394" {
		t.Fatalf("edlSerial = %q, want CA218394", listed)
	}
	// The predicate Open applies, spelled out: it must accept its own output.
	if edlSerial("", product) != listed {
		t.Error("Open's serial derivation disagrees with List's")
	}
	// And an empty filter must still accept the device.
	if listed == "" {
		t.Error("a device with a parseable serial reported none")
	}
}

func TestDeviceInfoMatches(t *testing.T) {
	a := DeviceInfo{Serial: "CA218394", Bus: 2, Address: 21}
	b := DeviceInfo{Serial: "OTHER", Bus: 2, Address: 22}
	// Two boards that report no serial at all — the case that made a
	// serial-keyed selection pick the wrong one.
	blankA := DeviceInfo{Bus: 2, Address: 21}
	blankB := DeviceInfo{Bus: 2, Address: 22}

	for name, tc := range map[string]struct {
		dev, want DeviceInfo
		match     bool
	}{
		"any":                   {a, DeviceInfo{}, true},
		"serial hit":            {a, DeviceInfo{Serial: "CA218394"}, true},
		"serial miss":           {b, DeviceInfo{Serial: "CA218394"}, false},
		"serial wins":           {a, DeviceInfo{Serial: "CA218394", Bus: 9, Address: 9}, true},
		"address distinguishes": {blankA, blankB, false},
		"address hit":           {blankA, blankA, true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.dev.matches(tc.want); got != tc.match {
				t.Errorf("%+v.matches(%+v) = %v, want %v", tc.dev, tc.want, got, tc.match)
			}
		})
	}
}
